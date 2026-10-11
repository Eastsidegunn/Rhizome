package main

// RHZ-139 (FR-RHZ-183): rhizome session list|close|send — the CLI face of
// the session registry. Registry reads and writes go through a running board
// (GET /v1/workspace sessions[], session.close intent); tabs are reached
// through the launcher ledger's terminalDriver (internal/session/README.md).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"rhizome/internal/session"
	"rhizome/internal/sessionlauncher"
)

const sessionUsage = `usage:
  rhizome session list [-addr A] [-config C]
  rhizome session close <sessionId> [-reason R] [-addr A] [-config C]
  rhizome session send <sessionId> <text> [-addr A] [-config C]

-addr defaults to RHIZOME_BOARD when set, else ` + scheduleDefaultAddr + ` (serve's default).
-config is the session launcher ledger naming terminalDriver; defaults to
RHIZOME_LAUNCHER_CONFIG. Without a driver, list shows the registry only.
list marks "missing" (registered open, no live tab) and "unregistered"
(a driver tab no open session claims).
`

func sessionCmd(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, sessionUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprint(out, sessionUsage)
		return 0
	case "list":
		return sessionList(args[1:], out, errOut)
	case "close":
		return sessionClose(args[1:], out, errOut)
	case "send":
		return sessionSend(args[1:], out, errOut)
	}
	fmt.Fprintf(errOut, "unknown session command %q\n\n%s", args[0], sessionUsage)
	return 2
}

type sessionFlags struct {
	f              *flag.FlagSet
	addr, config   *string
	reason         *string
	positional     []string
	base           string
	driver         session.Driver
	driverAttached bool
}

// parseSessionFlags accepts flags before or after the positional arguments.
func parseSessionFlags(name string, args []string, errOut io.Writer) (*sessionFlags, error) {
	sf := &sessionFlags{f: flag.NewFlagSet(name, flag.ContinueOnError)}
	sf.f.SetOutput(errOut)
	sf.addr = sf.f.String("addr", scheduleDefaultAddr, "board address")
	sf.config = sf.f.String("config", "", "session launcher ledger")
	sf.reason = sf.f.String("reason", "", "close reason")
	rest := args
	for {
		if err := sf.f.Parse(rest); err != nil {
			return nil, err
		}
		rest = sf.f.Args()
		if len(rest) == 0 {
			break
		}
		sf.positional = append(sf.positional, rest[0])
		rest = rest[1:]
	}
	sf.base = scheduleBase(sf.f, *sf.addr)
	path := *sf.config
	if path == "" {
		path = strings.TrimSpace(os.Getenv("RHIZOME_LAUNCHER_CONFIG"))
	}
	if path != "" {
		c, err := sessionlauncher.Load(path)
		if err != nil {
			return nil, err
		}
		sf.driver = session.Driver{Path: c.TerminalDriver}
		sf.driverAttached = c.TerminalDriver != ""
	}
	return sf, nil
}

type sessionRow struct {
	SessionID, Name, MissionID, Mode, State, DriverRef string
}

func fetchSessions(base string) ([]sessionRow, error) {
	resp, err := http.Get(base + "/v1/workspace")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env struct {
		Body struct {
			Sessions []sessionRow `json:"sessions"`
		} `json:"body"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&env) != nil {
		return nil, fmt.Errorf("board answered %s", resp.Status)
	}
	return env.Body.Sessions, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sessionList(args []string, out, errOut io.Writer) int {
	sf, err := parseSessionFlags("session list", args, errOut)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if len(sf.positional) != 0 {
		fmt.Fprint(errOut, sessionUsage)
		return 2
	}
	rows, err := fetchSessions(sf.base)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	reg := make([]session.Session, len(rows))
	for i, r := range rows {
		reg[i] = session.Session{Opened: session.Opened{SessionID: r.SessionID, Name: r.Name, MissionID: r.MissionID, Mode: r.Mode, DriverRef: r.DriverRef}, State: r.State}
	}
	var tabs []session.Tab
	if sf.driverAttached {
		if tabs, err = sf.driver.List(); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
	}
	for _, m := range session.Match(reg, tabs) {
		mark := "-"
		switch {
		case m.Missing && sf.driverAttached:
			mark = "missing"
		case m.Unregistered:
			mark = "unregistered"
		}
		if m.Session != nil {
			s := m.Session
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.SessionID, s.Name, s.Mode, s.State, dash(s.MissionID), dash(s.DriverRef), mark)
			continue
		}
		fmt.Fprintf(out, "-\t%s\t-\t-\t-\t%s\t%s\n", m.Tab.Name, m.Tab.DriverRef, mark)
	}
	return 0
}

// sessionTarget resolves an open registry session by id.
func sessionTarget(base, id string) (sessionRow, error) {
	rows, err := fetchSessions(base)
	if err != nil {
		return sessionRow{}, err
	}
	for _, r := range rows {
		if r.SessionID == id {
			if r.State != session.Open {
				return r, fmt.Errorf("session %s is %s", id, r.State)
			}
			return r, nil
		}
	}
	return sessionRow{}, fmt.Errorf("session %s not found", id)
}

func sessionClose(args []string, out, errOut io.Writer) int {
	sf, err := parseSessionFlags("session close", args, errOut)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if len(sf.positional) != 1 {
		fmt.Fprint(errOut, sessionUsage)
		return 2
	}
	r, err := sessionTarget(sf.base, sf.positional[0])
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	// The tab goes first: a failed driver close writes nothing.
	if r.DriverRef != "" {
		if !sf.driverAttached {
			fmt.Fprintln(errOut, session.ErrNoDriver)
			return 1
		}
		if err := sf.driver.Close(r.DriverRef); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
	}
	body := map[string]any{"kind": "session.close", "actor": "cli", "sessionId": r.SessionID}
	if *sf.reason != "" {
		body["reason"] = *sf.reason
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(sf.base+"/v1/intent", "application/json", bytes.NewReader(b))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer resp.Body.Close()
	var res struct {
		Accepted bool
		Reason   string
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&res) != nil {
		fmt.Fprintf(errOut, "board answered %s\n", resp.Status)
		return 1
	}
	if !res.Accepted {
		fmt.Fprintf(errOut, "rejected: %s\n", res.Reason)
		return 1
	}
	fmt.Fprintf(out, "%s closed\n", r.SessionID)
	return 0
}

func sessionSend(args []string, out, errOut io.Writer) int {
	sf, err := parseSessionFlags("session send", args, errOut)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if len(sf.positional) != 2 {
		fmt.Fprint(errOut, sessionUsage)
		return 2
	}
	r, err := sessionTarget(sf.base, sf.positional[0])
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if r.DriverRef == "" {
		fmt.Fprintf(errOut, "session %s has no terminal tab (mode %s)\n", r.SessionID, r.Mode)
		return 1
	}
	if !sf.driverAttached {
		fmt.Fprintln(errOut, session.ErrNoDriver)
		return 1
	}
	if err := sf.driver.Send(r.DriverRef, sf.positional[1]); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintf(out, "sent to %s\n", r.SessionID)
	return 0
}
