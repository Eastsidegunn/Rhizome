package projector

import (
	"encoding/json"
	"rhizome/internal/events"
	"testing"
)

func TestReplayGoalRejectsMixedAggregateIDs(t *testing.T) { // FR-RHZ-008
	a, _ := json.Marshal(goalCreated{ID: "g1", Description: "goal", Success: "done"})
	b, _ := json.Marshal(goalTransitioned{To: "paused"})
	_, err := ReplayGoal([]events.Event{
		{AggregateType: "goal", AggregateID: "g1", Revision: 1, Type: "goal.created", Payload: a},
		{AggregateType: "goal", AggregateID: "g2", Revision: 2, Type: "goal.transitioned", Payload: b},
	})
	if err == nil {
		t.Fatal("mixed goal aggregates accepted")
	}
}
