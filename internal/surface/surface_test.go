package surface

import (
	"encoding/json"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
	"testing"
)

func ms() *events.Store {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "d", "s"})
	_ = s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: p})
	return s
}
func TestProgressAndInstructionFRRHZ064(t *testing.T) {
	s := Service{Store: ms()}
	p := .5
	r, e := s.ReportProgress("m", "step", &p, "src")
	if e != nil || r.Progress != p {
		t.Fatal(e)
	}
	r, e = s.Instruct("m", "do", "bob", noAuthority(), "c")
	if e != nil || r.InstructionActor != "unverified-local-operator:bob" {
		t.Fatal(e)
	}
}
func TestProgressValidationFRRHZ064(t *testing.T) {
	s := Service{Store: ms()}
	if _, e := s.ReportProgress("m", "", nil, "src"); e == nil {
		t.Fatal()
	}
	p := 2.0
	if _, e := s.ReportProgress("m", "x", &p, "src"); e == nil {
		t.Fatal()
	}
}
func TestPausedTransitionFRRHZ065(t *testing.T) {
	m := domain.Mission{State: domain.MissionRunning}
	if _, e := m.Transition(domain.MissionPaused); e != nil {
		t.Fatal(e)
	}
	m.State = domain.MissionReady
	if _, e := m.Transition(domain.MissionPaused); e == nil {
		t.Fatal()
	}
}

func TestSurfaceReplayMissionMismatchFRRHZ064(t *testing.T) {
	s := Service{Store: ms()}
	p := .2
	r, _ := s.ReportProgress("m", "a", &p, "src")
	log := s.Store.List("surface", "surface-m")
	var x progress
	_ = json.Unmarshal(log[0].Payload, &x)
	x.MissionID = "other"
	log[0].Payload, _ = json.Marshal(x)
	if _, e := Replay(log); e == nil {
		t.Fatal("mismatched mission accepted")
	}
	_ = r
}
func TestSurfaceNilAndRevisionFRRHZ064(t *testing.T) {
	var s Service
	p := .1
	if _, e := s.ReportProgress("m", "a", &p, "s"); e == nil {
		t.Fatal()
	}
	if _, e := s.Instruct("m", "x", "a", noAuthority(), "c"); e == nil {
		t.Fatal()
	}
	if _, e := s.ByMission("m"); e == nil {
		t.Fatal()
	}
}

func missionWithState(state domain.MissionState) *events.Store {
	s := ms()
	if state == domain.MissionRunning {
		_ = s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"ready"}`)})
		_ = s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"running"}`)})
	}
	if state == domain.MissionCancelled {
		_ = s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"cancelled"}`)})
	}
	if state == domain.MissionSucceeded {
		_ = s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"succeeded","DecisionID":"d"}`)})
	}
	return s
}
func TestTransitionWithReasonFRRHZ064(t *testing.T) {
	s := missionWithState(domain.MissionRunning)
	m := mission.Service{Store: s}
	before := len(s.All())
	if _, e := m.TransitionWithReason("m", 3, domain.MissionBlocked, ""); e == nil || len(s.All()) != before {
		t.Fatal("empty reason accepted")
	}
	x, e := m.TransitionWithReason("m", 3, domain.MissionBlocked, "blocked")
	if e != nil || x.BlockedReason != "blocked" {
		t.Fatal(e)
	}
	x, e = m.Transition("m", 4, domain.MissionReady)
	if e != nil || x.BlockedReason != "" {
		t.Fatal(e)
	}
}
func TestLegacyBlockedReasonReplayFRRHZ064(t *testing.T) {
	s := ms()
	_ = s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"ready"}`)})
	_ = s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"blocked"}`)})
	m, e := projector.ReplayMission(s.List("mission", "m"))
	if e != nil || m.BlockedReason != "" {
		t.Fatal(e)
	}
}
func TestSurfaceMissionStreamImmutableFRRHZ064(t *testing.T) {
	s := ms()
	before := s.List("mission", "m")
	x := Service{Store: s}
	p := .3
	_, _ = x.ReportProgress("m", "a", &p, "src")
	_, _ = x.Instruct("m", "do", "op", noAuthority(), "c")
	after := s.List("mission", "m")
	if len(before) != len(after) || string(before[0].Payload) != string(after[0].Payload) {
		t.Fatal("mission stream changed")
	}
}
func TestSurfaceMissingAndTerminalMissionFRRHZ064(t *testing.T) {
	for _, state := range []domain.MissionState{"missing", domain.MissionSucceeded, domain.MissionCancelled} {
		var s *events.Store
		if state == "missing" {
			s = &events.Store{}
		} else {
			s = missionWithState(state)
		}
		x := Service{Store: s}
		n := len(s.All())
		p := .1
		if _, e := x.ReportProgress("m", "a", &p, "s"); e == nil || len(s.All()) != n {
			t.Fatal(state)
		}
		if _, e := x.Instruct("m", "a", "op", noAuthority(), "c"); e == nil || len(s.All()) != n {
			t.Fatal(state)
		}
	}
}
func TestProgressMultipleReportsFRRHZ064(t *testing.T) {
	s := ms()
	x := Service{Store: s}
	p := .2
	r, _ := x.ReportProgress("m", "first", &p, "s")
	if r.Revision != 1 {
		t.Fatal()
	}
	r, _ = x.ReportProgress("m", "second", nil, "s")
	if r.Revision != 2 || r.CurrentAction != "second" || r.HasProgress {
		t.Fatal()
	}
}

// RHZ-082 (FR-RHZ-113): Progress validates before append, merges present
// fields only on replay, and an identical repeat writes nothing.
func TestProgressedMergeAndValidationFRRHZ113(t *testing.T) {
	s := Service{Store: ms()}
	bad := 1.5
	for _, c := range []struct {
		action, blocked string
		p               *float64
	}{{"", "", nil}, {" ", " ", nil}, {"x", "", &bad}} {
		if _, e := s.Progress("m", "test-operator", c.action, c.p, c.blocked); e == nil {
			t.Fatalf("expected rejection for %+v", c)
		}
	}
	if _, e := s.Progress("m", "", "x", nil, ""); e == nil {
		t.Fatal("actor required")
	}
	if n := len(s.Store.List("surface", "surface-m")); n != 0 {
		t.Fatalf("rejected Progress wrote %d events", n)
	}
	st, e := s.Progress("m", "test-operator", "step", nil, "")
	if e != nil || st.CurrentAction != "step" || st.HasProgress || st.Revision != 1 {
		t.Fatalf("%+v %v", st, e)
	}
	zero := 0.0
	if st, e = s.Progress("m", "test-operator", "", &zero, "reason"); e != nil || !st.HasProgress || st.Progress != 0 || st.CurrentAction != "step" || st.BlockedReason != "reason" || st.ProgressSource != "relay:test-operator" {
		t.Fatalf("merge: %+v %v", st, e)
	}
	if st, e = s.Progress("m", "test-operator", "step", &zero, "reason"); e != nil || st.Revision != 2 {
		t.Fatalf("identical repeat must not write: %+v %v", st, e)
	}
	log := s.Store.List("surface", "surface-m")
	var x progressed
	_ = json.Unmarshal(log[1].Payload, &x)
	x.MissionID = "other"
	log[1].Payload, _ = json.Marshal(x)
	if _, e := Replay(log); e == nil {
		t.Fatal("mismatched mission id must fail replay")
	}
}
