package workspace

import (
	"strings"
	"testing"

	"rhizome/internal/assembly"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

// slotStarter is a launcher stand-in with maxConcurrent slots.
type slotStarter struct {
	max     int
	running map[string]bool
	started []ExecStartRequest
}

func (f *slotStarter) Prepare(r ExecStartRequest) (ExecStartPlan, string, error) {
	if len(f.running) >= f.max {
		return ExecStartPlan{ExecutionID: "exec-" + r.MissionID}, "session launcher busy", nil
	}
	return ExecStartPlan{ExecutionID: "exec-" + r.MissionID}, "", nil
}

func (f *slotStarter) Start(r ExecStartRequest) (string, string, error) {
	f.started = append(f.started, r)
	f.running[r.MissionID] = true
	return "exec-" + r.MissionID, "", nil
}

func (f *slotStarter) names() []string {
	var out []string
	for _, r := range f.started {
		out = append(out, r.MissionID[strings.LastIndex(r.MissionID, "-")+1:])
	}
	return out
}

type procFix struct {
	t  *testing.T
	s  *events.Store
	st *slotStarter
}

func newProcFix(t *testing.T, slots int, steps []procedure.Step, execute bool, params map[string]string) procFix {
	t.Helper()
	s := &events.Store{}
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "src", SourceType: "note", SourceID: "n-1", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	if _, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-p", Kind: knowledge.Procedure, Statement: "p", SourceMemoryID: "mem-1", Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	if _, err := (procedure.Service{Store: s}).Create(procedure.Procedure{ID: "proc-x", SourceKnowledgeID: "k-p", Trigger: "manual", Steps: steps}); err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-x", "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	res, err := RelayIntent(s, Intent{Kind: "procedure.run", ID: "proc-x", Name: "r1", GoalID: "goal-x", Params: params, Execute: execute}, "ops", trust.Authority{})
	if err != nil || !res.Accepted {
		t.Fatalf("procedure.run: %v %+v", err, res)
	}
	return procFix{t: t, s: s, st: &slotStarter{max: slots, running: map[string]bool{}}}
}

// pass runs one pass of a FRESH executor (restart = no carried state).
func (f procFix) pass() int {
	x := &ProcedureExecutor{Store: f.s, Hooks: func() RelayHooks { return RelayHooks{Start: f.st} }, Models: []string{"opus"}, OnError: func(msg string) { f.t.Log(msg) }}
	return x.Pass()
}

func (f procFix) finish(step string, to domain.MissionState) {
	f.t.Helper()
	// Completion stays with people/ops: the same relays they use.
	id := "mission-r1-" + step
	kind := map[domain.MissionState]string{domain.MissionSucceeded: "mission.complete", domain.MissionFailed: "mission.fail"}[to]
	res, err := RelayIntent(f.s, Intent{Kind: kind, MissionID: id, Reason: "test " + string(to)}, "ops", trust.Authority{})
	if err != nil || !res.Accepted {
		f.t.Fatalf("%s %s: %v %+v", kind, id, err, res)
	}
	delete(f.st.running, id)
}

// steady asserts a pass over an unchanged journal appends nothing.
func (f procFix) steady() {
	f.t.Helper()
	before := len(f.s.All())
	if n := f.pass(); n != 0 || len(f.s.All()) != before {
		f.t.Fatalf("same state wrote: pass=%d events %d→%d", n, before, len(f.s.All()))
	}
}

func (f procFix) wantStarted(want ...string) {
	f.t.Helper()
	if got := strings.Join(f.st.names(), ","); got != strings.Join(want, ",") {
		f.t.Fatalf("started %q, want %q", got, strings.Join(want, ","))
	}
}

func step(id string, after ...string) procedure.Step {
	return procedure.Step{ID: id, Action: "do " + id, After: after}
}

func TestProcedureExecutorLinearFRRHZ189(t *testing.T) {
	f := newProcFix(t, 3, []procedure.Step{step("a"), step("b", "a"), step("c", "b")}, true, nil)
	f.pass()
	f.wantStarted("a")
	f.steady()
	f.finish("a", domain.MissionSucceeded)
	f.pass()
	f.wantStarted("a", "b")
	f.steady()
	f.finish("b", domain.MissionSucceeded)
	f.pass()
	f.wantStarted("a", "b", "c")
	f.finish("c", domain.MissionSucceeded)
	f.steady()
	rs, err := assembly.Status(f.s, "r1")
	if err != nil || rs.Done != 3 || rs.Total != 3 || rs.Stalled != "" {
		t.Fatalf("status %+v %v", rs, err)
	}
	// The executor never completes missions itself: the run mission stays running.
	if rs.State != domain.MissionRunning {
		t.Fatalf("run state %s", rs.State)
	}
}

func TestProcedureExecutorBranchFRRHZ189(t *testing.T) {
	f := newProcFix(t, 3, []procedure.Step{step("a"), step("b", "a"), step("c", "a"), step("d", "b", "c")}, true, nil)
	f.pass()
	f.finish("a", domain.MissionSucceeded)
	f.pass()
	f.wantStarted("a", "b", "c")
	f.finish("b", domain.MissionSucceeded)
	f.steady() // d waits for c
	f.finish("c", domain.MissionSucceeded)
	f.pass()
	f.wantStarted("a", "b", "c", "d")
}

func TestProcedureExecutorGateFRRHZ189(t *testing.T) {
	b := step("b", "a")
	b.NeedsGate = true
	f := newProcFix(t, 3, []procedure.Step{step("a"), b}, true, nil)
	f.pass()
	f.finish("a", domain.MissionSucceeded)
	f.steady() // gate open: no start
	rs, _ := assembly.Status(f.s, "r1")
	if rs.Steps[1].Phase != assembly.StepGatePending || rs.Steps[1].Gate != "open" {
		t.Fatalf("gate step %+v", rs.Steps[1])
	}
	q, err := question.Replay(f.s.List("question", rs.Steps[1].GateID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (question.Service{Store: f.s}).Answer(q.ID, question.Approve, "", "ops", q.Digest); err != nil {
		t.Fatal(err)
	}
	f.pass()
	f.wantStarted("a", "b")
}

func TestProcedureExecutorStallFRRHZ189(t *testing.T) {
	f := newProcFix(t, 3, []procedure.Step{step("a"), step("b", "a")}, true, nil)
	f.pass()
	f.finish("a", domain.MissionFailed)
	if n := f.pass(); n != 1 {
		t.Fatalf("stall pass wrote %d", n)
	}
	f.wantStarted("a")
	rm, _ := projector.ReplayMission(f.s.List("mission", "mission-r1"))
	if rm.State != domain.MissionBlocked || !strings.Contains(rm.BlockedReason, "step a failed") {
		t.Fatalf("run %s %q", rm.State, rm.BlockedReason)
	}
	p, err := Snapshot(f.s, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range p.Attention {
		found = found || (a.Kind == attentionNoteBlocked && a.MissionID == "mission-r1")
	}
	if !found {
		t.Fatalf("no stalled attention: %+v", p.Attention)
	}
	f.steady()
	f.wantStarted("a")
}

func TestProcedureExecutorSlotQueueFRRHZ189(t *testing.T) {
	f := newProcFix(t, 1, []procedure.Step{step("b"), step("c"), step("d")}, true, nil)
	f.pass()
	f.wantStarted("b")
	f.steady() // full launcher: c, d queued, zero writes
	f.finish("b", domain.MissionSucceeded)
	f.pass()
	f.wantStarted("b", "c")
	f.finish("c", domain.MissionSucceeded)
	f.pass()
	f.wantStarted("b", "c", "d")
}

func TestProcedureExecutorNotExecutedRunFRRHZ189(t *testing.T) {
	f := newProcFix(t, 3, []procedure.Step{step("a")}, false, nil)
	f.steady()
	f.wantStarted()
}

func TestProcedureExecutorLaunchInputsFRRHZ189(t *testing.T) {
	a := step("a")
	a.Workdir = "rhizome"
	b := step("b")
	b.Model = "sonnet"
	f := newProcFix(t, 3, []procedure.Step{a, b}, true, map[string]string{"workdir": "gunnflow", "sessionMode": "interactive"})
	f.pass()
	f.wantStarted("a")
	r := f.st.started[0]
	if r.Workdir != "rhizome" || r.SessionMode != "interactive" || !strings.Contains(r.Instruction, "do a") || !strings.Contains(r.Instruction, "workdir=gunnflow") {
		t.Fatalf("start request %+v", r)
	}
	// b names a model the launcher cannot run: the run stalls, nothing starts.
	rm, _ := projector.ReplayMission(f.s.List("mission", "mission-r1"))
	if rm.State != domain.MissionBlocked || !strings.Contains(rm.BlockedReason, "sonnet") {
		t.Fatalf("run %s %q", rm.State, rm.BlockedReason)
	}
}
