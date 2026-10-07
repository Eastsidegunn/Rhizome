package projector

// RHZ-087 FR-RHZ-117 P1: goal.updated 재생 — created→updated(desc)→
// updated(success)→transitioned 순서대로 적용, ID·state 불변; legacy
// 스트림(updated 없음)은 그대로; 두 필드 다 없는 payload·creation 전·
// terminal 뒤는 에러; 미지 타입은 여전히 에러(기존 동작 pin).

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

func created087() json.RawMessage {
	b, _ := json.Marshal(goalCreated{ID: "goal-x", Description: "old desc", Success: "old success"})
	return b
}

func ev087(rev uint64, typ string, payload json.RawMessage) events.Event {
	return events.Event{AggregateType: "goal", AggregateID: "goal-x", Revision: rev, Type: typ, Payload: payload}
}

func TestReplayGoalUpdatedOrderedFRRHZ117(t *testing.T) {
	paused, _ := json.Marshal(goalTransitioned{To: domain.GoalPaused})
	log := []events.Event{
		ev087(1, "goal.created", created087()),
		ev087(2, "goal.updated", json.RawMessage(`{"Description":"new desc","Reason":"r"}`)),
		ev087(3, "goal.updated", json.RawMessage(`{"Success":"new success","Reason":""}`)),
		ev087(4, "goal.transitioned", paused),
	}
	got, err := ReplayGoal(log)
	if err != nil || got.ID != "goal-x" || got.Description != "new desc" || got.Success != "new success" || got.State != domain.GoalPaused || got.Revision != 4 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	// prefix replay: description changed, success still the created one, state active.
	mid, err := ReplayGoal(log[:2])
	if err != nil || mid.Description != "new desc" || mid.Success != "old success" || mid.State != domain.GoalActive || mid.Revision != 2 {
		t.Fatalf("prefix %+v, err %v", mid, err)
	}
	// an absent key and an explicit empty string both mean unchanged.
	both, err := ReplayGoal([]events.Event{
		ev087(1, "goal.created", created087()),
		ev087(2, "goal.updated", json.RawMessage(`{"Description":"","Success":"s2","Reason":""}`)),
	})
	if err != nil || both.Description != "old desc" || both.Success != "s2" {
		t.Fatalf("empty-string field: %+v err=%v", both, err)
	}
	// revision gap is still a conflict.
	if _, err := ReplayGoal([]events.Event{ev087(1, "goal.created", created087()), ev087(3, "goal.updated", json.RawMessage(`{"Description":"d"}`))}); err == nil {
		t.Fatal("revision gap accepted")
	}
}

func TestReplayGoalLegacyNoUpdatedFRRHZ117(t *testing.T) {
	paused, _ := json.Marshal(goalTransitioned{To: domain.GoalPaused})
	got, err := ReplayGoal([]events.Event{ev087(1, "goal.created", created087()), ev087(2, "goal.transitioned", paused)})
	if err != nil || got.Description != "old desc" || got.Success != "old success" || got.State != domain.GoalPaused || got.Revision != 2 {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestReplayGoalUpdatedPoisonRejectedFRRHZ117(t *testing.T) {
	for _, payload := range []string{`{}`, `{"Reason":"only"}`, `{"Description":"","Success":""}`} {
		_, err := ReplayGoal([]events.Event{ev087(1, "goal.created", created087()), ev087(2, "goal.updated", json.RawMessage(payload))})
		if err == nil || !strings.Contains(err.Error(), "goal update without fields") {
			t.Fatalf("payload %s: err = %v", payload, err)
		}
	}
	if _, err := ReplayGoal([]events.Event{ev087(1, "goal.created", created087()), ev087(2, "goal.updated", json.RawMessage(`nope`))}); err == nil {
		t.Fatal("malformed goal.updated accepted")
	}
	if _, err := ReplayGoal([]events.Event{ev087(1, "goal.updated", json.RawMessage(`{"Description":"d"}`))}); err == nil || !strings.Contains(err.Error(), "update before creation") {
		t.Fatalf("update before creation: err = %v", err)
	}
	cancelled, _ := json.Marshal(goalTransitioned{To: domain.GoalCancelled})
	if _, err := ReplayGoal([]events.Event{ev087(1, "goal.created", created087()), ev087(2, "goal.transitioned", cancelled), ev087(3, "goal.updated", json.RawMessage(`{"Description":"d"}`))}); err == nil || !strings.Contains(err.Error(), "update on terminal goal") {
		t.Fatalf("update on terminal: err = %v", err)
	}
}

func TestReplayGoalUnknownTypeStillRejectedFRRHZ117(t *testing.T) {
	_, err := ReplayGoal([]events.Event{ev087(1, "goal.created", created087()), ev087(2, "goal.renamed", json.RawMessage(`{"Description":"d"}`))})
	if err == nil || !strings.Contains(err.Error(), "unknown goal event") {
		t.Fatalf("err = %v", err)
	}
}
