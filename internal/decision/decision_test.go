package decision

import (
	"encoding/json"
	"rhizome/internal/events"
	"testing"
)

func store() *events.Store {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "x", "y"})
	s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: p})
	return s
}
func TestDecisionAndTimeline(t *testing.T) {
	s := Service{Store: store()}
	d, e := s.Create(Decision{ID: "d", MissionID: "m", Kind: StartExecution, Reason: "ready", NextAction: "run"})
	if e != nil || d.Sequence == 0 {
		t.Fatal(e)
	}
	if _, e = s.Create(Decision{ID: "d", MissionID: "m", Kind: StartExecution, Reason: "dup"}); e == nil {
		t.Fatal("duplicate")
	}
	ts, e := s.Timeline()
	if e != nil || len(ts) != 1 {
		t.Fatal(e)
	}
}
