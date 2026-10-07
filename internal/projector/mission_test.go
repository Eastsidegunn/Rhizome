package projector

import (
	"encoding/json"
	"testing"

	"rhizome/internal/events"
)

func TestReplayMissionFromEvents(t *testing.T) { // FR-RHZ-001
	created, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	transition, _ := json.Marshal(missionTransitioned{To: "ready"})
	got, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.transitioned", Payload: transition},
	})
	if err != nil || got.State != "ready" || got.Revision != 2 {
		t.Fatalf("got %#v, err %v", got, err)
	}
}

func TestReplayRejectsRevisionGap(t *testing.T) { // FR-RHZ-002
	_, err := ReplayMission([]events.Event{{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.created"}})
	if err != events.ErrRevisionConflict {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayRejectsMixedAggregateIDs(t *testing.T) {
	a, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	b, _ := json.Marshal(missionTransitioned{To: "ready"})
	_, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: a},
		{AggregateType: "mission", AggregateID: "m2", Revision: 2, Type: "mission.transitioned", Payload: b},
	})
	if err == nil {
		t.Fatal("mixed aggregate IDs accepted")
	}
}

func TestReplayRejectsDuplicateCreation(t *testing.T) {
	a, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	_, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: a},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.created", Payload: a},
	})
	if err == nil {
		t.Fatal("duplicate creation accepted")
	}
}
