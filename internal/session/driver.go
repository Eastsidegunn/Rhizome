package session

// RHZ-139 (FR-RHZ-183): the terminal driver contract. The core never names a
// terminal: an interactive tab is opened, listed, closed and written to by an
// external executable (the launcher ledger's terminalDriver, an absolute
// path). One invocation per op: one JSON line on stdin, one JSON object on
// stdout, exit 0. See README.md in this directory.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

// DriverTimeout bounds one driver invocation.
const DriverTimeout = 10 * time.Second

// Fixed driver errors: the driver's own output is never echoed (it may carry
// anything), the caller writes nothing on any of them.
var (
	ErrDriverFailed   = errors.New("terminal driver failed")
	ErrDriverTimeout  = errors.New("terminal driver timed out")
	ErrDriverResponse = errors.New("terminal driver returned an invalid response")
	ErrNoDriver       = errors.New("no terminal driver configured")
)

// Tab is one entry of the driver's list op.
type Tab struct {
	DriverRef string `json:"driverRef"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	Alive     bool   `json:"alive"`
}

// OpenRequest is the open op. Env carries RHIZOME_BOARD, RHIZOME_MISSION and
// RHIZOME_SESSION.
type OpenRequest struct {
	Name    string            `json:"name"`
	Cwd     string            `json:"cwd"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
}

// Driver invokes the external terminal driver at Path.
type Driver struct {
	Path    string
	Timeout time.Duration // 0 = DriverTimeout
}

type response struct {
	OK        *bool  `json:"ok"`
	DriverRef string `json:"driverRef"`
	Tabs      *[]Tab `json:"tabs"`
}

func (d Driver) call(req map[string]any) (response, error) {
	var r response
	if d.Path == "" {
		return r, ErrNoDriver
	}
	t := d.Timeout
	if t <= 0 {
		t = DriverTimeout
	}
	line, err := json.Marshal(req)
	if err != nil {
		return r, ErrDriverFailed
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Path)
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return r, ErrDriverTimeout
	}
	if err != nil {
		return r, ErrDriverFailed
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	if dec.Decode(&r) != nil || r.OK == nil {
		return response{}, ErrDriverResponse
	}
	var extra json.RawMessage
	if dec.Decode(&extra) != io.EOF {
		return response{}, ErrDriverResponse
	}
	if !*r.OK {
		return response{}, ErrDriverFailed
	}
	return r, nil
}

// Open asks the driver for a new tab running req.Command and returns its
// driverRef.
func (d Driver) Open(req OpenRequest) (string, error) {
	if strings.TrimSpace(req.Name) == "" || req.Cwd == "" || len(req.Command) == 0 {
		return "", errors.New("open needs name, cwd and command")
	}
	r, err := d.call(map[string]any{"op": "open", "name": req.Name, "cwd": req.Cwd, "command": req.Command, "env": req.Env})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(r.DriverRef) == "" {
		return "", ErrDriverResponse
	}
	return r.DriverRef, nil
}

// List returns the driver's tabs.
func (d Driver) List() ([]Tab, error) {
	r, err := d.call(map[string]any{"op": "list"})
	if err != nil {
		return nil, err
	}
	if r.Tabs == nil {
		return nil, ErrDriverResponse
	}
	for _, t := range *r.Tabs {
		if strings.TrimSpace(t.DriverRef) == "" {
			return nil, ErrDriverResponse
		}
	}
	return *r.Tabs, nil
}

// Close closes one tab.
func (d Driver) Close(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("driverRef required")
	}
	_, err := d.call(map[string]any{"op": "close", "driverRef": ref})
	return err
}

// Send types text into one tab.
func (d Driver) Send(ref, text string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("driverRef required")
	}
	_, err := d.call(map[string]any{"op": "send", "driverRef": ref, "text": text})
	return err
}

// Reconcile is one row of `rhizome session list`: a registry session, a
// driver tab, or both. Missing = registered open with a driverRef the driver
// does not list alive; Unregistered = a driver tab no open session claims.
type Reconcile struct {
	Session      *Session
	Tab          *Tab
	Missing      bool
	Unregistered bool
}

// Match pairs open registry sessions with driver tabs by driverRef.
// Background sessions (no driverRef) are never Missing.
func Match(sessions []Session, tabs []Tab) []Reconcile {
	byRef := map[string]int{}
	for i, t := range tabs {
		byRef[t.DriverRef] = i
	}
	used := map[string]bool{}
	var out []Reconcile
	for i := range sessions {
		s := &sessions[i]
		r := Reconcile{Session: s}
		if s.State == Open && s.DriverRef != "" {
			if j, ok := byRef[s.DriverRef]; ok {
				r.Tab = &tabs[j]
				used[s.DriverRef] = true
				r.Missing = !tabs[j].Alive
			} else {
				r.Missing = true
			}
		}
		out = append(out, r)
	}
	for i := range tabs {
		if !used[tabs[i].DriverRef] {
			out = append(out, Reconcile{Tab: &tabs[i], Unregistered: true})
		}
	}
	return out
}
