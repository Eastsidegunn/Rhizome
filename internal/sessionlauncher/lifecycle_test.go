package sessionlauncher

// RHZ-124 S1 review follow-up (FR-RHZ-124-S1): running-wait poll,
// accept failure, process-group sweeps (timeout and normal exit), Prepare
// serialized with an in-flight Start, serve shutdown, bounded usage note.
// Fake claude only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

// FakeFailingStore fails every Append of FailType (an injected journal
// fault); everything else goes to the wrapped store.
type FakeFailingStore struct {
	*events.Store
	FailType string
}

func (s *FakeFailingStore) Append(expected uint64, e events.Event) error {
	if s.FailType != "" && e.Type == s.FailType {
		return errors.New("injected append fault")
	}
	return s.Store.Append(expected, e)
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = killPid(pid)
			t.Fatalf("%s %d survived", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func grandchildPid(t *testing.T, f *fakeEnv) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(waitFile(t, filepath.Join(f.dir, "grandchild"))))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitDone(t *testing.T, l *Launcher) {
	t.Helper()
	done := make(chan struct{})
	go func() { l.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("completion did not finish")
	}
}

// forkGrandchild starts a background sleep (optionally ignoring SIGTERM) and
// records its pid atomically.
func forkGrandchild(ignoreTerm bool) string {
	cmd := "sleep 1000 &"
	if ignoreTerm {
		cmd = "(trap '' TERM; exec sleep 1000) &"
	}
	return cmd + "\necho $! > \"$D/grandchild.tmp\"\nmv \"$D/grandchild.tmp\" \"$D/grandchild\""
}

// Deviation #5: the session finishes before the relay moved the mission to
// running; completion waits and still ends at waiting_for_result.
func TestCompletionWaitsForRunningFRRHZ124S1(t *testing.T) {
	f := newFake(t, emit(twoModelResult))
	s := launchStore(t)
	ms := mission.Service{Store: s}
	if _, err := ms.Create("m3", "g", "do m3", "done"); err != nil {
		t.Fatal(err)
	}
	m, _ := projector.ReplayMission(s.List("mission", "m3"))
	if _, err := ms.Transition("m3", m.Revision, domain.MissionReady); err != nil {
		t.Fatal(err)
	}
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	l.RunningWait = 10 * time.Second
	id, reason, err := l.Start(StartRequest{MissionID: "m3", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	// Only after the outcome is journaled does the "relay" move the mission.
	deadline := time.Now().Add(10 * time.Second)
	for ref(t, s, id).State == execution.Accepted {
		if time.Now().After(deadline) {
			t.Fatal("never observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m, _ = projector.ReplayMission(s.List("mission", "m3"))
	if _, err := ms.Transition("m3", m.Revision, domain.MissionRunning); err != nil {
		t.Fatal(err)
	}
	waitDone(t, l)
	if m, _ := projector.ReplayMission(s.List("mission", "m3")); m.State != domain.MissionWaitingResult {
		t.Fatalf("mission %s", m.State)
	}
}

// Accept fails after the spawn: the child is killed and reaped before Start
// returns, the marker stays, and a re-submission asks for a human.
func TestAcceptFailureKillsChildFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	defer f.release(t)
	fs := &FakeFailingStore{Store: launchStore(t), FailType: "execution.accepted"}
	l := newLauncher(fs, f.config(t, nil), &outcomes{})
	req := StartRequest{MissionID: "m1", Instruction: "x"}
	id, _, err := l.Start(req)
	if err == nil || id == "" {
		t.Fatalf("%s %v", id, err)
	}
	marker := filepath.Join(f.logs, id+".pid")
	b, rerr := os.ReadFile(marker)
	if rerr != nil {
		t.Fatal("marker removed", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		t.Fatalf("marker %q", b)
	}
	if alive(pid) {
		_ = killPid(pid)
		t.Fatalf("child %d alive after accept failure", pid)
	}
	if l.Running() != 0 {
		t.Fatal("tracked")
	}
	fs.FailType = ""
	before := len(fs.All())
	if _, reason, err := l.Start(req); err != nil || !strings.HasPrefix(reason, "session spawn outcome unknown") || len(fs.All()) != before || runs(f) > 1 {
		t.Fatalf("%q %v runs=%d", reason, err, runs(f))
	}
}

// Timeout with a grandchild that ignores SIGTERM: only the SIGKILL sweep of
// the group ends it.
func TestTimeoutSweepKillsTermIgnoringGrandchildFRRHZ124S1(t *testing.T) {
	f := newFake(t, forkGrandchild(true)+"\nwait")
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x", Budget: BudgetOverride{TimeMs: i64(1500)}})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	pid := grandchildPid(t, f)
	waitDone(t, l)
	if r := ref(t, s, id); !r.StopRequested || r.State != execution.Cancelled {
		t.Fatalf("%+v", r)
	}
	waitGone(t, pid, "TERM-ignoring grandchild")
}

// Normal exit leaves a background descendant behind: the group is swept.
func TestNormalExitSweepsProcessGroupFRRHZ124S1(t *testing.T) {
	f := newFake(t, forkGrandchild(false)+"\n"+emit(twoModelResult)+"\nexit 0")
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	pid := grandchildPid(t, f)
	waitDone(t, l)
	if r := ref(t, s, id); r.State != execution.Succeeded || r.StopRequested {
		t.Fatalf("%+v", r)
	}
	waitGone(t, pid, "background descendant")
}

// Prepare during an in-flight identical Start waits for it instead of
// reading the half-done claim as an unknown spawn outcome.
func TestPrepareSerializedWithStartFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	defer f.release(t)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, func(m map[string]any) { m["maxConcurrent"] = 2 }), &outcomes{})
	reached, release := make(chan struct{}), make(chan struct{})
	l.beforeAccept = func() { close(reached); <-release }
	req := StartRequest{MissionID: "m1", Instruction: "x"}
	startDone := make(chan string, 1)
	go func() {
		id, reason, err := l.Start(req)
		startDone <- fmt.Sprint(id, "|", reason, "|", err)
	}()
	<-reached
	type res struct {
		plan   StartPlan
		reason string
	}
	prep := make(chan res, 1)
	go func() {
		p, r, _ := l.Prepare(req)
		prep <- res{p, r}
	}()
	select {
	case r := <-prep:
		close(release)
		t.Fatalf("Prepare answered mid-spawn: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if got := <-startDone; !strings.HasSuffix(got, "||<nil>") {
		t.Fatal(got)
	}
	if r := <-prep; r.reason != "" || r.plan.Existing != "accepted" {
		t.Fatalf("%+v", r)
	}
	f.release(t)
	waitDone(t, l)
}

// Shutdown: durable stop (reason user), group gone, outcome journaled as
// Cancelled "stopped: serve shutdown", mission waiting, usage reported, no
// new start accepted.
func TestShutdownStopsChildrenFRRHZ124S1(t *testing.T) {
	f := newFake(t, `trap 'cat "$D/result.json"; exit 0' TERM
`+forkGrandchild(false)+"\nwait")
	if err := os.WriteFile(filepath.Join(f.dir, "result.json"), []byte(twoModelResult+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := launchStore(t)
	o := &outcomes{}
	l := newLauncher(s, f.config(t, nil), o)
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	pid := grandchildPid(t, f)
	l.Shutdown()
	waitDone(t, l)
	r := ref(t, s, id)
	if !r.StopRequested || r.StopReason != "user" || r.StopActor != Actor || r.State != execution.Cancelled || !strings.HasPrefix(r.Summary, "stopped: serve shutdown") {
		t.Fatalf("%+v", r)
	}
	if m, _ := projector.ReplayMission(s.List("mission", "m1")); m.State != domain.MissionWaitingResult {
		t.Fatalf("mission %s", m.State)
	}
	if len(o.got) != 1 || o.got[0].Stopped != "shutdown" || !o.got[0].UsageAvailable || !strings.Contains(o.got[0].NoteContent(), "Stopped on serve shutdown.") {
		t.Fatalf("%+v", o.got)
	}
	waitGone(t, pid, "grandchild")
	before := len(s.All())
	if _, reason, _ := l.Start(StartRequest{MissionID: "m2", Instruction: "y"}); reason != "session launcher shutting down" || len(s.All()) != before {
		t.Fatalf("%q", reason)
	}
	if _, reason, _ := l.Prepare(StartRequest{MissionID: "m2", Instruction: "y"}); reason != "session launcher shutting down" {
		t.Fatalf("%q", reason)
	}
}

func denialsResult(n int, nameLen int) string {
	ds := make([]map[string]any, n)
	for i := range ds {
		ds[i] = map[string]any{"tool_name": "Bash" + strings.Repeat("x", nameLen), "tool_use_id": fmt.Sprintf("toolu_%04d", i), "tool_input": map[string]any{"command": "echo"}}
	}
	b, _ := json.Marshal(ds)
	return strings.Replace(twoModelResult, `"permission_denials":[{"tool_name":"Bash","tool_use_id":"toolu_01","tool_input":{"command":"git push"}}]`, `"permission_denials":`+string(b), 1)
}

// The usage note stays within the relay's 16KiB: 500 denials → first 20 +
// count; still too large → compact without the list; modelUsage intact.
func TestUsageNoteBoundedFRRHZ124S1(t *testing.T) {
	f := newFake(t, emit(denialsResult(500, 0)))
	s := launchStore(t)
	o := &outcomes{}
	l := newLauncher(s, f.config(t, nil), o)
	if _, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l)
	x := o.got[0]
	note := x.NoteContent()
	if x.PermissionDenialCount != 500 || len(x.PermissionDenials) != MaxDenials || x.PermissionDenials[19].ToolUseID != "toolu_0019" || len(note) > MaxNoteBytes ||
		!strings.Contains(note, `"permissionDenialsCount": 500`) || !strings.Contains(note, "0.1000000000000000055") || !strings.Contains(note, "0.023456789012345678") || strings.Contains(note, "toolu_0020") {
		t.Fatalf("count=%d len=%d bytes=%d", x.PermissionDenialCount, len(x.PermissionDenials), len(note))
	}
	// Twenty very long tool names: compact fallback drops the list only.
	big := x
	big.PermissionDenials = nil
	for i := 0; i < MaxDenials; i++ {
		big.PermissionDenials = append(big.PermissionDenials, Denial{ToolName: strings.Repeat("n", 1200), ToolUseID: fmt.Sprint(i)})
	}
	note = big.NoteContent()
	if len(note) > MaxNoteBytes || !strings.Contains(note, `"permissionDenials":null`) || !strings.Contains(note, `"permissionDenialsCount":500`) || !strings.Contains(note, "0.1000000000000000055") || !strings.Contains(note, `"claude-haiku-4-5"`) {
		t.Fatalf("%d bytes: %.300s", len(note), note)
	}
}
