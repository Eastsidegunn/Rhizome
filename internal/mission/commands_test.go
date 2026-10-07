package mission

import (
	"encoding/json"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"testing"
)

func TestCommandsAppendAndReplay(t *testing.T) { // FR-RHZ-003
	s := Service{Store: &events.Store{}}
	if _, err := s.CreateGoal("g1", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	m, err := s.Create("m1", "g1", "work", "done")
	if err != nil || m.Revision != 1 {
		t.Fatalf("create: %#v %v", m, err)
	}
	m, err = s.Transition("m1", 1, domain.MissionReady)
	if err != nil || m.State != domain.MissionReady {
		t.Fatalf("transition: %#v %v", m, err)
	}
}

func TestInvalidTransitionLeavesLogUnchanged(t *testing.T) { // FR-RHZ-004
	s := Service{Store: &events.Store{}}
	if _, err := s.CreateGoal("g1", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("m1", "g1", "work", "done"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Transition("m1", 1, domain.MissionSucceeded)
	if err == nil {
		t.Fatal("invalid transition accepted")
	}
	if got := len(s.Store.List("mission", "m1")); got != 1 {
		t.Fatalf("events = %d", got)
	}
}

func TestStaleRevisionConflicts(t *testing.T) { // FR-RHZ-004
	s := Service{Store: &events.Store{}}
	if _, err := s.CreateGoal("g1", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Create("m1", "g1", "work", "done")
	_, _ = s.Transition("m1", 1, domain.MissionReady)
	if _, err := s.Transition("m1", 0, domain.MissionRunning); err != events.ErrRevisionConflict {
		t.Fatalf("err = %v", err)
	}
}
func TestApplyGoalDecisionLifecycle(t *testing.T) {
	s := Service{Store: &events.Store{}}
	s.CreateGoal("g", "goal", "done", "")
	g, e := s.ApplyGoalDecision("g", "d", "c", domain.GoalAchieved)
	if e != nil || g.TerminalDecisionID != "d" {
		t.Fatal(e)
	}
	n := len(s.Store.All())
	if _, e = s.ApplyGoalDecision("g", "d", "c", domain.GoalAchieved); e != nil || len(s.Store.All()) != n {
		t.Fatal("not idempotent")
	}
	if _, e = s.ApplyGoalDecision("g", "x", "c", domain.GoalFailed); e == nil || len(s.Store.All()) != n {
		t.Fatal("conflict")
	}
}
func TestApplyGoalDecisionInvalid(t *testing.T) {
	s := Service{Store: &events.Store{}}
	if _, e := s.ApplyGoalDecision("missing", "d", "c", domain.GoalAchieved); e == nil {
		t.Fatal("missing accepted")
	}
	s.CreateGoal("g", "goal", "done", "")
	n := len(s.Store.All())
	if _, e := s.ApplyGoalDecision("g", "", "c", domain.GoalAchieved); e == nil || len(s.Store.All()) != n {
		t.Fatal("empty accepted")
	}
}

// RHZ-069 (FR-RHZ-098): ApplyMissionDecision — 운영자 종결 writer 커널 가드.
func fixtureMission098(t *testing.T, path ...domain.MissionState) Service {
	t.Helper()
	s := Service{Store: &events.Store{}}
	if _, err := s.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("m", "g", "work", "done"); err != nil {
		t.Fatal(err)
	}
	rev := uint64(1)
	for _, to := range path {
		if _, err := s.Transition("m", rev, to); err != nil {
			t.Fatal(err)
		}
		rev++
	}
	return s
}

func TestApplyMissionDecisionTerminalRejectedFRRHZ098(t *testing.T) {
	// succeeded: 재생-유효 terminal 이벤트 직접 append(DecisionID 포함).
	s := fixtureMission098(t, domain.MissionReady, domain.MissionRunning)
	p, _ := json.Marshal(struct {
		To         domain.MissionState
		DecisionID string
	}{domain.MissionSucceeded, "d0"})
	if err := s.Store.Append(3, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 4, Type: "mission.transitioned", Payload: p}); err != nil {
		t.Fatal(err)
	}
	n := len(s.Store.All())
	if _, err := s.ApplyMissionDecision("m", 4, domain.MissionFailed, "d1", "c", "r"); err != domain.ErrInvalidState || len(s.Store.All()) != n {
		t.Fatalf("succeeded→failed: err=%v grew=%d", err, len(s.Store.All())-n)
	}
	// cancelled
	s = fixtureMission098(t, domain.MissionCancelled)
	n = len(s.Store.All())
	if _, err := s.ApplyMissionDecision("m", 2, domain.MissionSucceeded, "d1", "c", ""); err != domain.ErrInvalidState || len(s.Store.All()) != n {
		t.Fatalf("cancelled→succeeded: err=%v grew=%d", err, len(s.Store.All())-n)
	}
}

func TestApplyMissionDecisionPlannedDirectRejectedFRRHZ098(t *testing.T) {
	s := fixtureMission098(t)
	n := len(s.Store.All())
	if _, err := s.ApplyMissionDecision("m", 1, domain.MissionSucceeded, "d1", "c", ""); err != domain.ErrInvalidState || len(s.Store.All()) != n {
		t.Fatalf("planned→succeeded: err=%v grew=%d", err, len(s.Store.All())-n)
	}
	if m, err := s.Transition("m", 1, domain.MissionReady); err != nil || m.State != domain.MissionReady {
		t.Fatalf("stream poisoned after rejected decision: %+v %v", m, err)
	}
}

func TestApplyMissionDecisionToCancelledRejectedFRRHZ098(t *testing.T) {
	s := fixtureMission098(t, domain.MissionReady, domain.MissionRunning)
	n := len(s.Store.All())
	_, err := s.ApplyMissionDecision("m", 3, domain.MissionCancelled, "d1", "c", "")
	if err == nil || err.Error() != "invalid terminal decision" || len(s.Store.All()) != n {
		t.Fatalf("to=cancelled: err=%v grew=%d", err, len(s.Store.All())-n)
	}
}

func TestApplyMissionDecisionEmptyIDsRejectedFRRHZ098(t *testing.T) {
	s := fixtureMission098(t, domain.MissionReady, domain.MissionRunning)
	n := len(s.Store.All())
	for _, c := range [][2]string{{"", "c"}, {"d1", ""}} {
		_, err := s.ApplyMissionDecision("m", 3, domain.MissionSucceeded, c[0], c[1], "")
		if err == nil || err.Error() != "invalid terminal decision" || len(s.Store.All()) != n {
			t.Fatalf("ids %q: err=%v grew=%d", c, err, len(s.Store.All())-n)
		}
	}
	// 정상 경로(대조): running + 유효 id → Succeeded, TerminalDecisionID 일치.
	m, err := s.ApplyMissionDecision("m", 3, domain.MissionSucceeded, "d1", "c", "ok")
	if err != nil || m.State != domain.MissionSucceeded || m.TerminalDecisionID != "d1" || len(s.Store.All()) != n+1 {
		t.Fatalf("valid decision: %+v %v", m, err)
	}
}

func TestApplyMissionDecisionRevisionConflictFRRHZ098(t *testing.T) {
	s := fixtureMission098(t, domain.MissionReady, domain.MissionRunning)
	n := len(s.Store.All())
	if _, err := s.ApplyMissionDecision("m", 2, domain.MissionSucceeded, "d1", "c", ""); err != events.ErrRevisionConflict || len(s.Store.All()) != n {
		t.Fatalf("stale: err=%v grew=%d", err, len(s.Store.All())-n)
	}
}
