package question_test

// RHZ-075 (FR-RHZ-108): a question may bind to a goal instead of a mission.
// K1 goal binding, K2 both given rejected, K3 goal existence/terminal,
// K4 legacy question.asked payloads (no GoalID key) replay unchanged.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	question "rhizome/internal/question"
)

// K1: Ask with goalID → Ref.GoalID set, MissionID empty, payload carries GoalID.
func TestAskWithGoalIDBindsGoalFRRHZ108(t *testing.T) {
	s := &events.Store{}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-k1", "k1", "done", ""); err != nil {
		t.Fatal(err)
	}
	q, err := (question.Service{Store: s}).Ask("t", "b", "r", "", "goal-k1", "operator", "corr")
	if err != nil {
		t.Fatal(err)
	}
	if q.GoalID != "goal-k1" || q.MissionID != "" {
		t.Fatalf("ref binding: %+v", q)
	}
	log := s.List("question", q.ID)
	if len(log) != 1 || log[0].Type != "question.asked" {
		t.Fatalf("journal: %+v", log)
	}
	var p map[string]any
	if err := json.Unmarshal(log[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["GoalID"] != "goal-k1" {
		t.Fatalf("payload GoalID: %s", log[0].Payload)
	}
	got, err := (question.Service{Store: s}).Get(q.ID)
	if err != nil || got != q {
		t.Fatalf("replay: %+v %v", got, err)
	}
}

// K2: both missionID and goalID → error, nothing appended.
func TestAskRejectsBothMissionAndGoalFRRHZ108(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-k2", "k2", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-k2", "goal-k2", "m", "done"); err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	_, err := (question.Service{Store: s}).Ask("t", "b", "r", "mission-k2", "goal-k2", "operator", "")
	if err == nil || err.Error() != "both missionId and goalId given" {
		t.Fatalf("both given: %v", err)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on both given")
	}
}

// K3: terminal goal → "goal is terminal"; missing goal → "goal reference:"
// prefix; nothing appended either way.
func TestAskGoalMustExistAndBeLiveFRRHZ108(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-done", "done", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.ApplyGoalDecision("goal-done", "decision-done", "corr-done", domain.GoalAchieved); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.CreateGoal("goal-gone", "gone", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.TransitionGoal("goal-gone", 1, domain.GoalCancelled); err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	svc := question.Service{Store: s}
	for _, g := range []string{"goal-done", "goal-gone"} {
		if _, err := svc.Ask("t", "b", "r", "", g, "operator", ""); err == nil || err.Error() != "goal is terminal" {
			t.Fatalf("%s: %v", g, err)
		}
	}
	if _, err := svc.Ask("t", "b", "r", "", "goal-missing", "operator", ""); err == nil || !strings.HasPrefix(err.Error(), "goal reference:") {
		t.Fatalf("missing goal: %v", err)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on rejected goal binding")
	}
}

// K4: a hand-written legacy question.asked payload WITHOUT the GoalID key
// (and one with MissionID only) replays to GoalID "" — the Ref equals the
// pre-change shape exactly. Replay adds no validation (journal poison guard).
func TestLegacyAskedPayloadWithoutGoalIDReplaysFRRHZ108(t *testing.T) {
	actor := "unverified-local-operator:legacy"
	for _, tc := range []struct{ name, missionID string }{{"orphan", ""}, {"mission-bound", "mission-old"}} {
		t.Run(tc.name, func(t *testing.T) {
			title, body, rec := "legacy "+tc.name, "old body", "r"
			d := question.Digest(title, body, rec)
			id := mustQuestionIDForDigest(d)
			payload, _ := json.Marshal(map[string]string{
				"Title": title, "Body": body, "Recommendation": rec, "MissionID": tc.missionID,
				"RequestedBy": actor, "CorrelationID": "corr-old", "Digest": d,
			})
			if strings.Contains(string(payload), "GoalID") {
				t.Fatal("fixture must not carry GoalID")
			}
			ref, err := question.Replay([]events.Event{{
				AggregateType: "question", AggregateID: id, Revision: 1, Type: "question.asked",
				Payload: payload, CorrelationID: "corr-old", CreatedAt: time.Now().UTC(),
			}})
			if err != nil {
				t.Fatal(err)
			}
			want := question.Ref{ID: id, Title: title, Body: body, Recommendation: rec, MissionID: tc.missionID, RequestedBy: actor, CorrelationID: "corr-old", Digest: d, Revision: 1}
			if ref != want {
				t.Fatalf("legacy replay drift:\n got %+v\nwant %+v", ref, want)
			}
			if ref.GoalID != "" {
				t.Fatalf("GoalID must zero-fill: %q", ref.GoalID)
			}
		})
	}
}
