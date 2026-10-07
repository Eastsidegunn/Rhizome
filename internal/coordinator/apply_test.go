package coordinator

import (
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"testing"
)

func TestApplyDecisionCompleteAndIdempotent(t *testing.T) {
	s := ms()
	s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)})
	s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: []byte(`{"To":"running"}`)})
	d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "d", MissionID: "m", Kind: decision.Complete, Reason: "done", Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}}})
	if e != nil {
		t.Fatal(e)
	}
	c := &Coordinator{Store: s}
	m, e := c.ApplyDecision(d.ID)
	if e != nil || m.State != "succeeded" {
		t.Fatalf("%+v %v", m, e)
	}
	n := len(s.All())
	m, e = c.ApplyDecision(d.ID)
	if e != nil || m.State != "succeeded" || len(s.All()) != n {
		t.Fatal("not idempotent")
	}
}
func TestApplyDecisionRejectsNonTerminal(t *testing.T) {
	s := ms()
	s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)})
	s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: []byte(`{"To":"running"}`)})
	d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "d", MissionID: "m", Kind: decision.StartExecution, Reason: "go"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = (&Coordinator{Store: s}).ApplyDecision(d.ID); e == nil {
		t.Fatal("accepted")
	}
}
func TestApplyDecisionFailAndMalformed(t *testing.T) {
	s := ms()
	s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)})
	s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: []byte(`{"To":"running"}`)})
	d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "f", MissionID: "m", Kind: decision.Fail, Reason: "bad", Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}}})
	if e != nil {
		t.Fatal(e)
	}
	m, e := (&Coordinator{Store: s}).ApplyDecision(d.ID)
	if e != nil || m.State != "failed" {
		t.Fatal(e)
	}
	n := len(s.All())
	s.Append(0, events.Event{AggregateType: "decision", AggregateID: "bad", Revision: 1, Type: "decision.created", Payload: []byte("{")})
	if _, e = (&Coordinator{Store: s}).ApplyDecision("bad"); e == nil || len(s.All()) != n+1 {
		t.Fatal("malformed accepted")
	}
}

func TestApplyDecisionRejectsFutureEvidence(t *testing.T) {
	s := ms()
	d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "future", MissionID: "m", Kind: decision.Complete, Reason: "r", Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}}})
	if e != nil {
		t.Fatal(e)
	}
	s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)})
	before := len(s.All())
	if _, e = (&Coordinator{Store: s}).ApplyDecision(d.ID); e == nil || len(s.All()) != before {
		t.Fatal("future evidence accepted")
	}
}
