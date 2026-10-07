package mission

// RHZ-061 FR-RHZ-090 core 가드: command는 재생이 거부할 전이를 append하면 안
// 된다 — cancelled goal에 대한 terminal decision이 스트림을 오염시키던 구멍.

import (
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

func TestApplyGoalDecisionRejectsCancelledFRRHZ090(t *testing.T) {
	s := &events.Store{}
	svc := Service{Store: s}
	if _, err := svc.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TransitionGoal("g", 1, domain.GoalCancelled); err != nil {
		t.Fatal(err)
	}
	n := len(s.All())
	if _, err := svc.ApplyGoalDecision("g", "decision-x", "corr", domain.GoalAchieved); err == nil {
		t.Fatal("terminal decision on cancelled goal accepted")
	}
	if len(s.All()) != n {
		t.Fatal("rejected decision appended an event (poison)")
	}
	// 오염 없음의 직접 증거: 스트림이 여전히 재생 가능하고 cancelled 그대로.
	g, err := projector.ReplayGoal(s.List("goal", "g"))
	if err != nil || g.State != domain.GoalCancelled {
		t.Fatalf("stream poisoned: %+v err=%v", g, err)
	}
}
