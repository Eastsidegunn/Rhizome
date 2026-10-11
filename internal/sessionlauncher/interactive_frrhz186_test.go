package sessionlauncher

// RHZ-142 (FR-RHZ-186): sessionMode interactive → claude --bg with the
// session env and the prompt as the first input; the short id from claude
// agents --json lands in the registry (attachable at once); no child is
// tracked; a re-submission never spawns twice; a failed --bg registers
// nothing and drops its spawn marker.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rhizome/internal/execution"
	"rhizome/internal/session"
)

func bgBody(work string) string {
	return `if [ "$1" = agents ]; then
  if [ -f "$D/bgsid" ]; then printf '[{"id":"ab12","sessionId":"%s","name":"n","kind":"background","status":"idle","cwd":"` + work + `"}]' "$(cat "$D/bgsid")"; else echo '[]'; fi
  exit 0
fi
[ "$1" = --bg ] || exit 1
cp "$D/args" "$D/bgargs"; cp "$D/env" "$D/bgenv"
shift 2
[ "$1" = --session-id ] && echo "$2" > "$D/bgsid"
echo "ab12"`
}

func TestInteractiveLaunchFRRHZ186(t *testing.T) {
	f := newFake(t, "")
	f.setBody(t, bgBody(f.work))
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	l.BoardURL = "http://127.0.0.1:8790"
	req := StartRequest{MissionID: "m1", Instruction: "do it", SessionMode: ModeInteractive}
	if _, reason, err := l.Prepare(req); err != nil || reason != "" {
		t.Fatalf("prepare %q %v", reason, err)
	}
	id, reason, err := l.Start(req)
	if err != nil || reason != "" {
		t.Fatalf("start %q %v", reason, err)
	}
	r := ref(t, s, id)
	if r.State != execution.Accepted || r.ExternalID == "" || l.Running() != 0 {
		t.Fatalf("execution %+v running=%d", r, l.Running())
	}
	x, err := (session.Service{Store: s}).Get(r.ExternalID)
	want := session.Opened{SessionID: r.ExternalID, Name: "m1", MissionID: "m1", Mode: session.Interactive, Cwd: f.work, ExecutionID: id, Kind: session.KindBackground, AttachID: "ab12"}
	if err != nil || x.State != session.Open || x.Opened != want {
		t.Fatalf("session %+v %v", x, err)
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, "bgargs"))
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	wantArgs := l.Cfg.BgArgs(Prompt(l.BoardURL, "m1", "do it"), r.ExternalID)
	if strings.Join(args, "\x00") != strings.Join(wantArgs, "\x00") || args[0] != "--bg" || !strings.HasSuffix(args[1], "\n\ndo it") || args[len(args)-1] != "Bash(git log:*)" {
		t.Fatalf("argv %q", args)
	}
	env, _ := os.ReadFile(filepath.Join(f.dir, "bgenv"))
	for _, kv := range []string{"RHIZOME_MISSION=m1", "RHIZOME_SESSION=session:" + r.ExternalID, "RHIZOME_BOARD=http://127.0.0.1:8790", "TERM=xterm-256color"} {
		if !strings.Contains(string(env), kv+"\n") {
			t.Fatalf("env lacks %s", kv)
		}
	}
	// re-submission: same execution, no second --bg
	before := s.Len()
	if id2, reason, err := l.Start(req); id2 != id || reason != "" || err != nil || s.Len() != before {
		t.Fatalf("resubmit %s %q %v", id2, reason, err)
	}
	// the observation of the same session writes nothing
	agents, err := session.ListAgents(context.Background(), f.bin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := (session.Service{Store: s}).Reconcile(agents); n != 0 || err != nil {
		t.Fatalf("reconcile wrote %d %v", n, err)
	}
	// a different key (other instruction) on a failing --bg: no session, marker dropped
	f.setBody(t, "exit 1")
	id3, reason, err := l.Start(StartRequest{MissionID: "m2", Instruction: "x", SessionMode: ModeInteractive})
	if err != nil || !strings.HasPrefix(reason, "session spawn failed: claude --bg failed") {
		t.Fatalf("failing bg %q %v", reason, err)
	}
	if _, err := os.Lstat(l.markerPath(id3)); !os.IsNotExist(err) {
		t.Fatalf("marker kept: %v", err)
	}
	for _, e := range s.All() {
		if e.AggregateType == session.AggregateType && e.AggregateID != r.ExternalID {
			t.Fatalf("failed --bg registered %s", e.AggregateID)
		}
	}
}
