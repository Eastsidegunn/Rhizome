package main

// RHZ-139 (FR-RHZ-183): rhizome session list|send|close against a served
// board and a fake terminal driver written into t.TempDir (no testdata).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/session"
)

func TestSessionCLIFRRHZ183(t *testing.T) {
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' "$line" >> "` + dir + `/requests"
case "$line" in
*'"op":"list"'*) echo '{"ok":true,"tabs":[{"driverRef":"tab-1","name":"a","cwd":"/tmp","alive":true},{"driverRef":"tab-9","name":"stray","cwd":"/","alive":true}]}' ;;
*) echo '{"ok":true}' ;;
esac
`
	if err := os.WriteFile(driver, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "claude")
	_ = os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o700)
	ledger, _ := json.Marshal(map[string]any{"claudePath": claude, "workdirs": map[string]any{"w": dir}, "defaultWorkdir": "w", "model": "m",
		"permissionMode": "acceptEdits", "allowedTools": []any{}, "logDir": dir, "maxConcurrent": 1,
		"ceiling": map[string]any{"usd": 1, "timeMs": 60000}, "terminalDriver": driver})
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &events.Store{}
	reg := session.Service{Store: s}
	for _, o := range []session.Opened{
		{SessionID: "s-1", Name: "a", Mode: session.Interactive, Cwd: "/tmp", DriverRef: "tab-1"},
		{SessionID: "s-2", Name: "b", Mode: session.Interactive, Cwd: "/tmp", DriverRef: "tab-gone"},
		{SessionID: "s-3", Name: "c", Mode: session.Background, Cwd: "/tmp"},
	} {
		if _, err := reg.Open(o); err != nil {
			t.Fatal(err)
		}
	}
	handler, _ := assembleServeWith(s, nil, "", "", nil, nil, io.Discard, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	cli := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(append(append([]string{"session"}, args...), "-addr", srv.URL, "-config", cfg), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	code, out, errOut := cli("list")
	want := "s-1\ta\tinteractive\topen\t-\ttab-1\t-\n" +
		"s-2\tb\tinteractive\topen\t-\ttab-gone\tmissing\n" +
		"s-3\tc\tbackground\topen\t-\t-\t-\n" +
		"-\tstray\t-\t-\t-\ttab-9\tunregistered\n"
	if code != 0 || out != want {
		t.Fatalf("list %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := cli("send", "s-3", "hi"); code != 1 || !strings.Contains(errOut, "no terminal tab") {
		t.Fatalf("send to background: %d %s", code, errOut)
	}
	if code, out, errOut := cli("send", "s-1", "hello there"); code != 0 || out != "sent to s-1\n" {
		t.Fatalf("send %d %s %s", code, out, errOut)
	}
	if code, out, errOut := cli("close", "s-1", "-reason", "done"); code != 0 || out != "s-1 closed\n" {
		t.Fatalf("close %d %s %s", code, out, errOut)
	}
	if x, err := reg.Get("s-1"); err != nil || x.State != session.Closed || x.Reason != "done" {
		t.Fatalf("s-1 = %+v %v", x, err)
	}
	if code, _, _ := cli("close", "s-1"); code != 1 {
		t.Fatal("closed a closed session")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "requests"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], `"op":"send"`) || !strings.Contains(lines[1], `"text":"hello there"`) || !strings.Contains(lines[2], `{"driverRef":"tab-1","op":"close"}`) {
		t.Fatalf("driver requests %q", lines)
	}
}
