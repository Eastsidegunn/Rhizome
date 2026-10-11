package session

// RHZ-139 (FR-RHZ-183): the registry stream and the terminal driver contract,
// pinned with a fake driver written into t.TempDir (no testdata).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
)

func TestRegistryOpenCloseReplayFRRHZ183(t *testing.T) {
	s := &events.Store{}
	svc := Service{Store: s}
	o := Opened{SessionID: "s-1", Name: "m", MissionID: "mission-1", Mode: Background, Cwd: "/tmp", ExecutionID: "exec-1"}
	for _, bad := range []Opened{
		{SessionID: "", Name: "m", Mode: Background, Cwd: "/tmp"},
		{SessionID: "s", Name: " ", Mode: Background, Cwd: "/tmp"},
		{SessionID: "s", Name: "m", Mode: "oneshot", Cwd: "/tmp"},
		{SessionID: "s", Name: "m", Mode: Interactive, Cwd: "rel"},
	} {
		if _, err := svc.Open(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := svc.Close("s-1", "x"); err == nil || s.Len() != 0 {
		t.Fatal("closed an unknown session or wrote")
	}
	if x, err := svc.Open(o); err != nil || x.State != Open || x.ExecutionID != "exec-1" {
		t.Fatalf("open = %+v %v", x, err)
	}
	if _, err := svc.Open(o); err == nil || s.Len() != 1 {
		t.Fatal("double open accepted or wrote")
	}
	if _, err := svc.Close("s-1", ""); err == nil || s.Len() != 1 {
		t.Fatal("empty reason accepted")
	}
	if x, err := svc.Close("s-1", "exited"); err != nil || x.State != Closed || x.Reason != "exited" {
		t.Fatalf("close = %+v %v", x, err)
	}
	if _, err := svc.Close("s-1", "again"); err == nil || s.Len() != 2 {
		t.Fatal("double close accepted or wrote")
	}
	// A gate resume reopens the same session id.
	if x, err := svc.Open(o); err != nil || x.State != Open || x.Revision != 3 {
		t.Fatalf("reopen = %+v %v", x, err)
	}
	log := s.List(AggregateType, "s-1")
	bad := append([]events.Event{}, log...)
	bad[1].Type = OpenedType
	if _, err := Replay(bad); err == nil {
		t.Fatal("replayed opened after opened")
	}
}

// fakeDriver writes a POSIX sh driver that answers per op from files under
// its directory; behaviour selects a failure mode.
func fakeDriver(t *testing.T, behaviour string) (Driver, string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d="$(dirname "$0")"
IFS= read -r line
printf '%s\n' "$line" >> "$d/requests"
case "` + behaviour + `" in
fail) exit 3 ;;
hang) sleep 5; exit 0 ;;
garbage) echo 'not json'; exit 0 ;;
notok) echo '{"ok":false,"error":"secret detail"}'; exit 0 ;;
trailing) echo '{"ok":true,"driverRef":"r"} {}'; exit 0 ;;
noref) echo '{"ok":true}'; exit 0 ;;
esac
case "$line" in
*'"op":"open"'*) echo '{"ok":true,"driverRef":"tab-1"}' ;;
*'"op":"list"'*) echo '{"ok":true,"tabs":[{"driverRef":"tab-1","name":"m","cwd":"/tmp","alive":true},{"driverRef":"tab-9","name":"stray","cwd":"/","alive":true}]}' ;;
*) echo '{"ok":true}' ;;
esac
`
	path := filepath.Join(dir, "driver")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return Driver{Path: path, Timeout: 2 * time.Second}, dir
}

func TestDriverContractFRRHZ183(t *testing.T) {
	d, dir := fakeDriver(t, "ok")
	env := map[string]string{"RHIZOME_BOARD": "http://b", "RHIZOME_MISSION": "mission-1", "RHIZOME_SESSION": "s-1"}
	ref, err := d.Open(OpenRequest{Name: "m", Cwd: "/tmp", Command: []string{"claude", "--session-id", "s-1"}, Env: env})
	if err != nil || ref != "tab-1" {
		t.Fatalf("open = %q %v", ref, err)
	}
	tabs, err := d.List()
	if err != nil || len(tabs) != 2 || !tabs[0].Alive {
		t.Fatalf("list = %+v %v", tabs, err)
	}
	if err := d.Send("tab-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close("tab-1"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "requests"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 {
		t.Fatalf("requests = %q", b)
	}
	var open struct {
		Op, Name, Cwd string
		Command       []string
		Env           map[string]string
	}
	if json.Unmarshal([]byte(lines[0]), &open) != nil || open.Op != "open" || open.Command[2] != "s-1" || open.Env["RHIZOME_SESSION"] != "s-1" {
		t.Fatalf("open request = %s", lines[0])
	}
	for i, want := range []string{`"op":"list"`, `"op":"send"`, `"op":"close"`} {
		if !strings.Contains(lines[i+1], want) {
			t.Fatalf("request %d = %s, want %s", i+1, lines[i+1], want)
		}
	}
}

func TestDriverFixedErrorsFRRHZ183(t *testing.T) {
	for _, c := range []struct {
		behaviour string
		want      error
	}{
		{"fail", ErrDriverFailed}, {"garbage", ErrDriverResponse}, {"notok", ErrDriverFailed},
		{"trailing", ErrDriverResponse}, {"noref", ErrDriverResponse}, {"hang", ErrDriverTimeout},
	} {
		d, _ := fakeDriver(t, c.behaviour)
		if c.behaviour == "hang" {
			d.Timeout = 500 * time.Millisecond
		}
		ref, err := d.Open(OpenRequest{Name: "m", Cwd: "/tmp", Command: []string{"x"}})
		if !errors.Is(err, c.want) || ref != "" {
			t.Fatalf("%s: %q %v, want %v", c.behaviour, ref, err, c.want)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("%s: driver output echoed: %v", c.behaviour, err)
		}
	}
	if _, err := (Driver{}).List(); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("no driver: %v", err)
	}
}

func TestMatchFRRHZ183(t *testing.T) {
	reg := []Session{
		{Opened: Opened{SessionID: "a", DriverRef: "tab-1"}, State: Open},
		{Opened: Opened{SessionID: "b", DriverRef: "tab-gone"}, State: Open},
		{Opened: Opened{SessionID: "c"}, State: Open},
		{Opened: Opened{SessionID: "d", DriverRef: "tab-dead"}, State: Open},
	}
	tabs := []Tab{{DriverRef: "tab-1", Alive: true}, {DriverRef: "tab-9", Alive: true}, {DriverRef: "tab-dead"}}
	got := Match(reg, tabs)
	if len(got) != 5 || got[0].Missing || !got[1].Missing || got[2].Missing || !got[3].Missing || !got[4].Unregistered || got[4].Tab.DriverRef != "tab-9" {
		t.Fatalf("match = %+v", got)
	}
}
