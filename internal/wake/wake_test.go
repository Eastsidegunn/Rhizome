package wake

import (
	"encoding/json"
	"rhizome/internal/events"
	"testing"
	"time"
)

func TestWakeCreateReplayDuplicate(t *testing.T) {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "x", "y"})
	s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: p})
	w := Wake{ID: "w", Source: Manual, RequestedAt: time.Now(), Payload: []byte("x"), TargetType: "mission", TargetID: "m", CorrelationID: "c", TickKey: "t"}
	got, e := (Service{Store: s}).Create(w)
	if e != nil || got.TickKey != "t" {
		t.Fatal(e)
	}
	got.Payload[0] = 'y'
	if _, e = (Service{Store: s}).Create(w); e == nil {
		t.Fatal("duplicate")
	}
}
