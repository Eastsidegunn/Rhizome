package workspace

import (
	"encoding/json"
	"reflect"
	"rhizome/internal/approval"
	"rhizome/internal/decision"
	"rhizome/internal/domain"
	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/surface"
	"testing"
)

func ws(t *testing.T) *events.Store {
	s := &events.Store{}
	m := mission.Service{Store: s}
	if _, e := m.CreateGoal("g", "goal", "done", "p"); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Create("m", "g", "mission", "ok"); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestSnapshotAggregateAndRevisionFRRHZ070(t *testing.T) {
	s := ws(t)
	m := mission.Service{Store: s}
	_, _ = m.Transition("m", 1, domain.MissionReady)
	p, e := Snapshot(s)
	if e != nil || p.Revision != 3 || len(p.Tasks) != 1 {
		t.Fatal(e, p)
	}
	q, _ := Snapshot(s)
	if len(q.Tasks) != 1 || q.Tasks[0].State != "queued" {
		t.Fatal(q)
	}
}
func TestSurfaceProgressAndCountsFRRHZ070(t *testing.T) {
	s := ws(t)
	m := mission.Service{Store: s}
	_, _ = m.Transition("m", 1, domain.MissionReady)
	_, _ = m.Transition("m", 2, domain.MissionRunning)
	x := surface.Service{Store: s}
	v := .4
	_, e := x.ReportProgress("m", "act", &v, "src")
	if e != nil {
		t.Fatal(e)
	}
	p, e := Snapshot(s)
	if e != nil || p.Tasks[0].State != "running" || p.Counts.Running != 1 {
		t.Fatal(e)
	}
}
func TestEmptyAndNilSnapshotFRRHZ070(t *testing.T) {
	p, e := Snapshot(&events.Store{})
	if e != nil || p.Revision != 0 {
		t.Fatal(e)
	}
	if _, e = Snapshot(nil); e == nil {
		t.Fatal()
	}
}
func TestMissionPayloadFixture(t *testing.T) {
	_, _ = json.Marshal(struct{ To domain.MissionState }{domain.MissionReady})
}

func TestStateMappingAllFRRHZ070(t *testing.T) {
	for in, out := range map[domain.MissionState]string{domain.MissionPlanned: "queued", domain.MissionReady: "queued", domain.MissionRunning: "running", domain.MissionWaitingResult: "waiting", domain.MissionWaitingHuman: "waiting", domain.MissionPaused: "paused", domain.MissionBlocked: "blocked", domain.MissionSucceeded: "completed", domain.MissionFailed: "failed", domain.MissionCancelled: "cancelled"} {
		if got := mapState(in); got != out {
			t.Fatalf("%s", in)
		}
	}
}
func TestProjectionReadOnlyDeepEqualFRRHZ070(t *testing.T) {
	s := ws(t)
	before := s.All()
	a, e := Snapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	b, e := Snapshot(s)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal(e)
	}
	after := s.All()
	if len(before) != len(after) || string(before[0].Payload) != string(after[0].Payload) {
		t.Fatal("mutated")
	}
}
func TestHiddenProgressFRRHZ070(t *testing.T) {
	p, e := Snapshot(ws(t))
	if e != nil || p.Tasks[0].HasProgress {
		t.Fatal(e)
	}
}

func TestGateBeforeAfterObservedFRRHZ070(t *testing.T) {
	s := ws(t)
	k := approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "g"}
	a, e := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "r", "d", "op", "", "", true)
	if e != nil {
		t.Fatal(e)
	}
	p, e := Snapshot(s)
	if e != nil || p.Gates[0].State != "pending" {
		t.Fatal(e)
	}
	_, _ = (approval.Service{Store: s}).Observe(a.ID, 1, approval.Allow, "")
	p, e = Snapshot(s)
	if e != nil || p.Gates[0].State != "approved" {
		t.Fatal(e)
	}
}

func TestHumanJanusDecisionSeparationFRRHZ070(t *testing.T) {
	s := ws(t)
	k := approval.RequestKey{TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SpanID: "bbbbbbbbbbbbbbbb", RequestID: "sep"}
	a, e := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "r", "d", "op", "", "", true)
	if e != nil {
		t.Fatal(e)
	}
	_, e = (approval.Service{Store: s}).Observe(a.ID, 1, approval.Deny, "deadline")
	if e != nil {
		t.Fatal(e)
	}
	p, e := Snapshot(s)
	if e != nil || p.Gates[0].HumanDecision != "allow" || p.Gates[0].JanusDecision != "deny" || p.Gates[0].State == "approved" {
		t.Fatal(e)
	}
}

func TestRelaySessionRejectedFRRHZ071(t *testing.T) {
	s := ws(t)
	for _, k := range []string{"session.pause", "session.resume", "session.fork", "session.stdin", "session.kill"} {
		before := len(s.All())
		r, e := RelayIntent(s, Intent{Kind: k}, "op", true)
		if e != nil || r.Accepted || r.Reason != "JANUS T17-19 표면 의존" || len(s.All()) != before {
			t.Fatal(k, e)
		}
	}
}
func TestRelayGateRequestChangesNoMissionFRRHZ071(t *testing.T) {
	s := ws(t)
	r, e := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: "missing", Instruction: "x"}, "op", true)
	if e != nil || r.Accepted || r.Reason != "gate에 mission 연결 없음" {
		t.Fatal(e)
	}
}

func TestRelayMissionCreateFRRHZ071(t *testing.T) {
	s := &events.Store{}
	r, e := RelayIntent(s, Intent{Kind: "mission.create", Name: "n", Prompt: "success"}, "op", true)
	if e != nil || !r.Accepted || len(s.List("goal", "goal-n")) != 1 {
		t.Fatal(e)
	}
}
func TestRelayTaskPauseResumeFRRHZ071(t *testing.T) {
	s := ws(t)
	m := mission.Service{Store: s}
	_, _ = m.Transition("m", 1, domain.MissionReady)
	_, _ = m.Transition("m", 2, domain.MissionRunning)
	r, e := RelayIntent(s, Intent{Kind: "task.pause", TaskID: "m"}, "op", true)
	if e != nil || !r.Accepted {
		t.Fatal(e)
	}
	r, e = RelayIntent(s, Intent{Kind: "task.resume", TaskID: "m"}, "op", true)
	if e != nil || !r.Accepted {
		t.Fatal(e)
	}
}
func TestRelayTaskInstructAcceptedFRRHZ071(t *testing.T) {
	s := ws(t)
	r, e := RelayIntent(s, Intent{Kind: "task.instruct", TaskID: "m", Instruction: "do"}, "op", false)
	if e != nil || !r.Accepted {
		t.Fatal(e)
	}
	if len(s.List("surface", "surface-m")) != 1 {
		t.Fatal()
	}
}
func TestRelayGateRequestChangesAcceptedFRRHZ071(t *testing.T) {
	s := ws(t)
	d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "dec", MissionID: "m", Reason: "wait", Kind: decision.WaitHuman})
	if e != nil {
		t.Fatal(e)
	}
	k := approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "r"}
	a, e := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "r", "d", "op", "", "dec", true)
	if e != nil {
		t.Fatal(e)
	}
	r, e := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: a.ID, Instruction: "change"}, "op", true)
	if e != nil || !r.Accepted || len(s.List("surface", "surface-m")) != 1 {
		t.Fatal(e, d)
	}
}
func TestRelayEdgeRewireAcceptedFRRHZ071(t *testing.T) {
	s := ws(t)
	mm := mission.Service{Store: s}
	_, _ = mm.Create("m2", "g", "m2", "ok")
	r, e := (edge.Service{Store: s}).Create(edge.Edge{ID: "e1", From: edge.Endpoint{Type: "mission", ID: "m"}, To: edge.Endpoint{Type: "mission", ID: "m2"}, Kind: edge.Dependency, Actor: "op", Correlation: "c", Verified: true})
	_ = r
	if e == nil {
		_ = e
	}
	r2, e := RelayIntent(s, Intent{Kind: "edge.rewire", EdgeID: "e1", ID: "e2", From: "mission:m", To: "mission:m2", EdgeKind: "dependency"}, "op", true)
	if e != nil || !r2.Accepted {
		t.Fatal(e)
	}
}
