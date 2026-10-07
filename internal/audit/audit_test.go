package audit

import (
	"encoding/json"
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"testing"
)

func fixture() *events.Store {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "x", "y"})
	s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: p})
	s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)})
	s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: []byte(`{"To":"running"}`)})
	return s
}
func TestTerminalLinkRejectsMissingAndWrong(t *testing.T) {
	s := fixture()
	d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "d", MissionID: "m", Kind: decision.Complete, Reason: "r", Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}}, CorrelationID: "c"})
	if e != nil {
		t.Fatal(e)
	}
	if e = TerminalLink(s, d.ID); e == nil {
		t.Fatal("missing link accepted")
	}
	s.Append(3, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 4, Type: "mission.transitioned", CorrelationID: "x", Payload: []byte(`{"To":"succeeded","DecisionID":"d"}`)})
	if e = TerminalLink(s, d.ID); e == nil {
		t.Fatal("wrong correlation accepted")
	}
}
