package sessionlauncher

// RHZ-143 (FR-RHZ-187): launched --bg sessions count against maxInteractive
// (refusal writes nothing); past the effective time ceiling they are stopped
// with claude stop <attachId> and closed ceiling-time (a failed stop writes
// nothing; the observation never reopens them); startMu is not held while
// claude --bg runs (an identical start is refused as in progress, another
// key proceeds).

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/execution"
	"rhizome/internal/session"
)

// bgFleetBody fakes claude --bg / agents --json / stop for many sessions:
// agents lists one interactive entry in claude's real shape (no ids) and
// every started session with its first 8 uuid characters as short id.
func bgFleetBody(work string, stopExit int) string {
	return `case "$1" in
agents)
  printf '[{"name":"tty","kind":"interactive","status":"busy","cwd":"/tmp","pid":99}'
  [ -f "$D/bgsids" ] && while read s; do printf ',{"id":"%s","sessionId":"%s","kind":"background","status":"idle","cwd":"` + work + `"}' "$(echo "$s" | cut -c1-8)" "$s"; done < "$D/bgsids"
  echo ']'; exit 0;;
stop) echo "$2" >> "$D/stops"; exit ` + strconv.Itoa(stopExit) + `;;
--bg) shift 2; [ "$1" = --session-id ] && echo "$2" >> "$D/bgsids"; exit 0;;
esac
exit 1`
}

func TestInteractiveCapAndCeilingFRRHZ187(t *testing.T) {
	f := newFake(t, "")
	f.setBody(t, bgFleetBody(f.work, 0))
	s := launchStore(t)
	c := f.config(t, nil)
	if c.InteractiveCap() != DefaultMaxInteractive || DefaultMaxInteractive != 2 {
		t.Fatalf("default cap %d", c.InteractiveCap())
	}
	if _, err := Parse(f.ledger(func(m map[string]any) { m["maxInteractive"] = -1 })); err == nil {
		t.Fatal("maxInteractive -1 accepted")
	}
	if c1 := f.config(t, func(m map[string]any) { m["maxInteractive"] = 1 }); c1.InteractiveCap() != 1 || c1.Digest() == c.Digest() {
		t.Fatalf("cap 1: %d", c1.InteractiveCap())
	}
	l := newLauncher(s, c, &outcomes{})
	sids := []string{}
	for _, m := range []string{"m1", "m2"} {
		id, reason, err := l.Start(StartRequest{MissionID: m, Instruction: "do it", SessionMode: ModeInteractive})
		if err != nil || reason != "" {
			t.Fatalf("start %s %q %v", m, reason, err)
		}
		sids = append(sids, ref(t, s, id).ExternalID)
	}
	// the cap counts launched sessions, not -p children: a third is refused
	// with zero writes, in Prepare and Start alike
	third := StartRequest{MissionID: "m1", Instruction: "more", SessionMode: ModeInteractive}
	want := "session launcher busy: 2 interactive open (maxInteractive 2)"
	before := s.Len()
	if _, reason, _ := l.Prepare(third); reason != want {
		t.Fatalf("prepare third %q", reason)
	}
	if _, reason, err := l.Start(third); reason != want || err != nil || s.Len() != before {
		t.Fatalf("start third %q %v (%d → %d events)", reason, err, before, s.Len())
	}
	// before the ceiling: nothing
	ctx := context.Background()
	if n, err := l.EnforceCeilings(ctx, time.Now()); n != 0 || err != nil || s.Len() != before {
		t.Fatalf("early enforce %d %v", n, err)
	}
	late := time.Now().Add(61 * time.Second) // ceiling.timeMs 60000
	// a failed claude stop writes nothing
	f.setBody(t, bgFleetBody(f.work, 1))
	if n, err := l.EnforceCeilings(ctx, late); n != 0 || err != errStop || s.Len() != before {
		t.Fatalf("failed stop %d %v", n, err)
	}
	_ = os.Remove(filepath.Join(f.dir, "stops"))
	f.setBody(t, bgFleetBody(f.work, 0))
	if n, err := l.EnforceCeilings(ctx, late); n != 2 || err != nil {
		t.Fatalf("enforce %d %v", n, err)
	}
	stops, _ := os.ReadFile(filepath.Join(f.dir, "stops"))
	for _, sid := range sids {
		if !strings.Contains(string(stops), sid[:8]+"\n") {
			t.Fatalf("no claude stop %s: %q", sid[:8], stops)
		}
		if x, _ := (session.Service{Store: s}).Get(sid); x.State != session.Closed || x.Reason != session.ReasonCeilingTime {
			t.Fatalf("session %+v", x)
		}
	}
	// idempotent; the observation (daemons still listed) never reopens them
	after := s.Len()
	if n, _ := l.EnforceCeilings(ctx, late); n != 0 || s.Len() != after {
		t.Fatalf("second enforce %d", n)
	}
	agents, err := session.ListAgents(ctx, f.bin, 0)
	if err != nil || len(agents) != 3 {
		t.Fatalf("agents %+v %v", agents, err)
	}
	if n, err := (session.Service{Store: s}).Reconcile(agents); n != 0 || err != nil || s.Len() != after {
		t.Fatalf("reconcile reopened %d %v", n, err)
	}
	// the slots are free again
	if _, reason, err := l.Start(third); reason != "" || err != nil {
		t.Fatalf("start after ceiling %q %v", reason, err)
	}
}

func TestInteractiveLockScopeFRRHZ187(t *testing.T) {
	f := newFake(t, "")
	f.setBody(t, bgFleetBody(f.work, 0))
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	l.beforeAccept = func() {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
		}
	}
	req := StartRequest{MissionID: "m1", Instruction: "do it", SessionMode: ModeInteractive}
	type res struct {
		id, reason string
		err        error
	}
	done := make(chan res, 1)
	go func() {
		id, reason, err := l.Start(req)
		done <- res{id, reason, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first launch never reached the acceptance")
	}
	// the first launch is between claude --bg and accept, outside startMu
	plan, reason, err := l.Prepare(req)
	if err != nil || reason != launchingReason(plan.ExecutionID) {
		t.Fatalf("identical prepare %q %v", reason, err)
	}
	if _, reason, err := l.Start(req); err != nil || reason != launchingReason(plan.ExecutionID) {
		t.Fatalf("identical start %q %v", reason, err)
	}
	// with one launch in flight and the cap at 2, another key proceeds
	id2, reason, err := l.Start(StartRequest{MissionID: "m2", Instruction: "do it", SessionMode: ModeInteractive})
	if err != nil || reason != "" || ref(t, s, id2).State != execution.Accepted {
		t.Fatalf("concurrent start %q %v", reason, err)
	}
	// the cap counts the in-flight launch
	if _, reason, _ := l.Prepare(StartRequest{MissionID: "m2", Instruction: "x", SessionMode: ModeInteractive}); !strings.HasPrefix(reason, "session launcher busy: 2 interactive open") {
		t.Fatalf("cap with in-flight %q", reason)
	}
	close(release)
	r := <-done
	if r.err != nil || r.reason != "" || ref(t, s, r.id).State != execution.Accepted {
		t.Fatalf("first start %+v", r)
	}
	l.Wait()
	if x, _ := (session.Service{Store: s}).Get(ref(t, s, r.id).ExternalID); x.State != session.Open || x.AttachID == "" {
		t.Fatalf("first session %+v", x)
	}
}
