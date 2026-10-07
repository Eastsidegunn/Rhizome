package mission

// RHZ-087 FR-RHZ-117 K1: 커널 writer UpdateGoal — goal.updated 1건 append
// (description / success / 둘 다), 재생으로 재확인, ID·state 불변; terminal
// goal·둘 다 빈값·revision 불일치는 append 전 거부(저널 바이트 불변);
// 동일 값은 멱등 수용(쓰기 0).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

func fixtureGoal087(t *testing.T, s *events.Store, id string) Service {
	t.Helper()
	svc := Service{Store: s}
	if _, err := svc.CreateGoal(id, "old desc", "old success", ""); err != nil {
		t.Fatal(err)
	}
	return svc
}

func lastPayload087(t *testing.T, s *events.Store) (events.Event, map[string]any) {
	t.Helper()
	last := s.All()[len(s.All())-1]
	var p map[string]any
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return last, p
}

func TestUpdateGoalDescriptionSuccessBothFRRHZ117(t *testing.T) {
	s := &events.Store{}
	svc := fixtureGoal087(t, s, "goal-x")
	n := len(s.All())
	// description only: payload has Description+Reason, no Success key.
	g, err := svc.UpdateGoal("goal-x", 1, "new desc", "", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "goal-x" || g.Description != "new desc" || g.Success != "old success" || g.State != domain.GoalActive || g.Revision != 2 {
		t.Fatalf("after description update: %+v", g)
	}
	if len(s.All()) != n+1 {
		t.Fatalf("journal grew %d, want 1", len(s.All())-n)
	}
	last, p := lastPayload087(t, s)
	if last.AggregateType != "goal" || last.AggregateID != "goal-x" || last.Type != "goal.updated" || last.Revision != 2 {
		t.Fatalf("event %+v", last)
	}
	if p["Description"] != "new desc" || p["Reason"] != "r1" {
		t.Fatalf("payload %s", last.Payload)
	}
	if _, has := p["Success"]; has {
		t.Fatalf("description-only update must omit Success: %s", last.Payload)
	}
	// success only: no Description key.
	g, err = svc.UpdateGoal("goal-x", 2, "", "  new success  ", "")
	if err != nil || g.Description != "new desc" || g.Success != "new success" || g.Revision != 3 {
		t.Fatalf("after success update: %+v err=%v", g, err)
	}
	last, p = lastPayload087(t, s)
	if last.Type != "goal.updated" || last.Revision != 3 || p["Success"] != "new success" {
		t.Fatalf("event %+v payload %s", last, last.Payload)
	}
	if _, has := p["Description"]; has {
		t.Fatalf("success-only update must omit Description: %s", last.Payload)
	}
	// both.
	g, err = svc.UpdateGoal("goal-x", 3, "d3", "s3", "both")
	if err != nil || g.Description != "d3" || g.Success != "s3" || g.Revision != 4 || g.State != domain.GoalActive {
		t.Fatalf("after both: %+v err=%v", g, err)
	}
	last, p = lastPayload087(t, s)
	if p["Description"] != "d3" || p["Success"] != "s3" || p["Reason"] != "both" {
		t.Fatalf("payload %s", last.Payload)
	}
	if len(s.All()) != n+3 {
		t.Fatalf("journal grew %d, want 3", len(s.All())-n)
	}
	// reassert via replay: ID immutable, latest values, state untouched.
	r, err := projector.ReplayGoal(s.List("goal", "goal-x"))
	if err != nil || r.ID != "goal-x" || r.Description != "d3" || r.Success != "s3" || r.State != domain.GoalActive || r.Revision != 4 {
		t.Fatalf("replay %+v err=%v", r, err)
	}
	// a later transition keeps the edited values (value copy).
	if g, err = svc.TransitionGoal("goal-x", 4, domain.GoalPaused); err != nil || g.Description != "d3" || g.Success != "s3" || g.State != domain.GoalPaused {
		t.Fatalf("transition after update: %+v err=%v", g, err)
	}
}

func TestUpdateGoalTerminalRejectedFRRHZ117(t *testing.T) {
	s := &events.Store{}
	svc := fixtureGoal087(t, s, "goal-c")
	if _, err := svc.TransitionGoal("goal-c", 1, domain.GoalCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateGoal("goal-a", "d", "s", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyGoalDecision("goal-a", "dec", "corr", domain.GoalAchieved); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	for _, id := range []string{"goal-c", "goal-a"} {
		g, _ := projector.ReplayGoal(s.List("goal", id))
		if _, err := svc.UpdateGoal(id, g.Revision, "x", "y", ""); !errors.Is(err, domain.ErrInvalidState) {
			t.Fatalf("%s: err = %v, want ErrInvalidState", id, err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on rejected update")
	}
	if g, err := projector.ReplayGoal(s.List("goal", "goal-c")); err != nil || g.Description != "old desc" {
		t.Fatalf("replay %+v err=%v", g, err)
	}
}

func TestUpdateGoalBothEmptyRejectedFRRHZ117(t *testing.T) {
	s := &events.Store{}
	svc := fixtureGoal087(t, s, "goal-x")
	before, _ := json.Marshal(s.All())
	for _, c := range [][2]string{{"", ""}, {"  ", "\t\n"}, {" ", ""}} {
		_, err := svc.UpdateGoal("goal-x", 1, c[0], c[1], "r")
		if err == nil || !strings.Contains(err.Error(), "description or success required") {
			t.Fatalf("%q/%q: err = %v", c[0], c[1], err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on rejected update")
	}
}

func TestUpdateGoalRevisionConflictAndUnknownFRRHZ117(t *testing.T) {
	s := &events.Store{}
	svc := fixtureGoal087(t, s, "goal-x")
	before, _ := json.Marshal(s.All())
	if _, err := svc.UpdateGoal("goal-x", 7, "new", "", ""); !errors.Is(err, events.ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
	// stale revision + 동일 값도 멱등 성공이 아니라 conflict여야
	// 한다(커맨드 가드가 Append 전에 잡는다 — 가드 제거 시 이 단언이 FAIL).
	cur, err := projector.ReplayGoal(s.List("goal", "goal-x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateGoal("goal-x", cur.Revision+5, cur.Description, "", ""); !errors.Is(err, events.ErrRevisionConflict) {
		t.Fatalf("stale+identical: err = %v, want ErrRevisionConflict", err)
	}
	if _, err := svc.UpdateGoal("goal-nope", 0, "new", "", ""); err == nil {
		t.Fatal("unknown goal accepted")
	}
	if _, err := (Service{}).UpdateGoal("goal-x", 1, "new", "", ""); err == nil {
		t.Fatal("nil store accepted")
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on rejected update")
	}
}

func TestUpdateGoalIdenticalIdempotentFRRHZ117(t *testing.T) {
	s := &events.Store{}
	svc := fixtureGoal087(t, s, "goal-x")
	before, _ := json.Marshal(s.All())
	cases := [][2]string{{"old desc", "old success"}, {"old desc", ""}, {"", "old success"}, {" old desc ", ""}}
	for _, c := range cases {
		g, err := svc.UpdateGoal("goal-x", 1, c[0], c[1], "noop")
		if err != nil || g.Revision != 1 || g.Description != "old desc" || g.Success != "old success" {
			t.Fatalf("%q/%q: %+v err=%v", c[0], c[1], g, err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on identical update")
	}
	// one field identical, the other changed → only the changed key rides.
	g, err := svc.UpdateGoal("goal-x", 1, "old desc", "s2", "")
	if err != nil || g.Revision != 2 || g.Description != "old desc" || g.Success != "s2" {
		t.Fatalf("%+v err=%v", g, err)
	}
	_, p := lastPayload087(t, s)
	if _, has := p["Description"]; has || p["Success"] != "s2" {
		t.Fatalf("payload %v", p)
	}
}
