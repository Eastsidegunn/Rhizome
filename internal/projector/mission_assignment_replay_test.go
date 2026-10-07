package projector

// RHZ-080 FR-RHZ-111 P1: mission.assigned 재생 — 순서대로 마지막 값이 이기고
// 상태는 그대로; legacy 스트림은 Assignee ""; 빈 assignee payload는 에러;
// 미지 타입은 여전히 에러(기존 동작 pin).

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

func TestReplayMissionAssignedOrderedFRRHZ111(t *testing.T) {
	created, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	a1, _ := json.Marshal(missionAssigned{Assignee: "agent-a", Reason: "r1"})
	ready, _ := json.Marshal(missionTransitioned{To: domain.MissionReady})
	a2, _ := json.Marshal(missionAssigned{Assignee: "agent-b"})
	got, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.assigned", Payload: a1},
		{AggregateType: "mission", AggregateID: "m1", Revision: 3, Type: "mission.transitioned", Payload: ready},
		{AggregateType: "mission", AggregateID: "m1", Revision: 4, Type: "mission.assigned", Payload: a2},
	})
	if err != nil || got.State != domain.MissionReady || got.Assignee != "agent-b" || got.Revision != 4 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	// prefix replay: first assignment visible, state unchanged by assign.
	mid, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.assigned", Payload: a1},
	})
	if err != nil || mid.State != domain.MissionPlanned || mid.Assignee != "agent-a" || mid.Revision != 2 {
		t.Fatalf("prefix %+v, err %v", mid, err)
	}
}

func TestReplayMissionLegacyNoAssigneeFRRHZ111(t *testing.T) {
	created, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	ready, _ := json.Marshal(missionTransitioned{To: domain.MissionReady})
	got, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.transitioned", Payload: ready},
	})
	if err != nil || got.Assignee != "" || got.State != domain.MissionReady {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestReplayMissionAssignedEmptyRejectedFRRHZ111(t *testing.T) {
	created, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	for _, payload := range []string{`{"Assignee":"","Reason":"x"}`, `{}`, `{"Reason":"only"}`} {
		_, err := ReplayMission([]events.Event{
			{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
			{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.assigned", Payload: json.RawMessage(payload)},
		})
		if err == nil || !strings.Contains(err.Error(), "assignee missing") {
			t.Fatalf("payload %s: err = %v", payload, err)
		}
	}
	// malformed payload is a decode error, not a silent no-op.
	if _, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.assigned", Payload: json.RawMessage(`nope`)},
	}); err == nil {
		t.Fatal("malformed mission.assigned accepted")
	}
}

func TestReplayMissionUnknownTypeStillRejectedFRRHZ111(t *testing.T) {
	created, _ := json.Marshal(missionCreated{ID: "m1", GoalID: "g1", Description: "work", Success: "done"})
	_, err := ReplayMission([]events.Event{
		{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m1", Revision: 2, Type: "mission.unassigned", Payload: json.RawMessage(`{}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown mission event") {
		t.Fatalf("err = %v", err)
	}
}

// P2 (FR-RHZ-111): 생성 전 assigned·terminal 뒤 assigned는 재생 거부.
func TestReplayMissionAssignedGuardsFRRHZ111(t *testing.T) {
	assigned := []byte(`{"Assignee":"agent-a","Reason":""}`)
	if _, err := ReplayMission([]events.Event{{AggregateType: "mission", AggregateID: "m-x", Revision: 1, Type: "mission.assigned", Payload: assigned}}); err == nil {
		t.Fatal("assignment before creation must fail replay")
	}
	created := []byte(`{"ID":"m-x","GoalID":"g","Description":"d","Success":"s"}`)
	cancelled := []byte(`{"To":"cancelled"}`)
	log := []events.Event{
		{AggregateType: "mission", AggregateID: "m-x", Revision: 1, Type: "mission.created", Payload: created},
		{AggregateType: "mission", AggregateID: "m-x", Revision: 2, Type: "mission.transitioned", Payload: cancelled},
		{AggregateType: "mission", AggregateID: "m-x", Revision: 3, Type: "mission.assigned", Payload: assigned},
	}
	if _, err := ReplayMission(log); err == nil {
		t.Fatal("assignment on a terminal mission must fail replay")
	}
}
