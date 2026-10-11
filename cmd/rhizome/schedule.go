package main

// RHZ-136 (FR-RHZ-180): rhizome schedule add|list — the CLI face of the
// recurring schedule entity. It only talks to a running board over HTTP
// (schedule.create intent, GET /v1/workspace schedules[]).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// scheduleDefaultAddr matches serve's -addr default.
const scheduleDefaultAddr = "127.0.0.1:8080"

const scheduleUsage = `usage:
  rhizome schedule add -name N -weekly <mon..sun> -at HH:MM [-goal G] [-addr A]
  rhizome schedule list [-addr A]

-addr defaults to RHIZOME_BOARD when set, else ` + scheduleDefaultAddr + ` (serve's default).
`

var scheduleWeekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func scheduleCmd(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errOut, scheduleUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprint(out, scheduleUsage)
		return 0
	case "add":
		return scheduleAdd(args[1:], out, errOut)
	case "list":
		return scheduleList(args[1:], out, errOut)
	}
	fmt.Fprintf(errOut, "unknown schedule command %q\n\n%s", args[0], scheduleUsage)
	return 2
}

// scheduleBase resolves the board base URL: explicit -addr, else
// RHIZOME_BOARD, else serve's default. A bare host:port gets http://.
func scheduleBase(f *flag.FlagSet, addr string) string {
	explicit := false
	f.Visit(func(x *flag.Flag) { explicit = explicit || x.Name == "addr" })
	if !explicit {
		if env := strings.TrimSpace(os.Getenv("RHIZOME_BOARD")); env != "" {
			addr = env
		}
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

func scheduleAdd(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("schedule add", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	name := f.String("name", "", "schedule name")
	weekly := f.String("weekly", "", "weekday: mon..sun")
	at := f.String("at", "", "time HH:MM (Asia/Seoul)")
	goal := f.String("goal", "", "goal id or handle")
	if err := f.Parse(args); err != nil {
		return 2
	}
	day, ok := scheduleWeekdays[strings.ToLower(*weekly)]
	if strings.TrimSpace(*name) == "" || !ok || *at == "" || f.NArg() != 0 {
		fmt.Fprint(errOut, scheduleUsage)
		return 2
	}
	body := map[string]any{
		"kind": "schedule.create", "actor": "cli", "name": *name, "tz": "Asia/Seoul",
		"rule": map[string]any{"freq": "weekly", "weekday": day, "time": *at},
	}
	if *goal != "" {
		body["goalId"] = *goal
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(scheduleBase(f, *addr)+"/v1/intent", "application/json", bytes.NewReader(b))
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
	fmt.Fprintf(out, "schedule-%s\n", *name)
	return 0
}

type scheduleRule struct {
	Freq    string
	Weekday int
	Time    string
}

type scheduleRow struct {
	Rule scheduleRule

	ID, Name, GoalID, TZ, State, NextAt string
}

func scheduleList(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("schedule list", flag.ContinueOnError)
	f.SetOutput(errOut)
	addr := f.String("addr", scheduleDefaultAddr, "board address")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprint(errOut, scheduleUsage)
		return 2
	}
	resp, err := http.Get(scheduleBase(f, *addr) + "/v1/workspace")
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer resp.Body.Close()
	var env struct {
		Body struct {
			Schedules []scheduleRow `json:"schedules"`
		} `json:"body"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&env) != nil {
		fmt.Fprintf(errOut, "board answered %s\n", resp.Status)
		return 1
	}
	for _, s := range env.Body.Schedules {
		day := time.Weekday(s.Rule.Weekday).String()[:3]
		next := s.NextAt
		if next == "" {
			next = "-"
		}
		goal := s.GoalID
		if goal == "" {
			goal = "-"
		}
		fmt.Fprintf(out, "%s\t%s\t%s %s %s\t%s\t%s\t%s\n", s.ID, s.Name, s.Rule.Freq, strings.ToLower(day), s.Rule.Time, s.State, next, goal)
	}
	return 0
}
