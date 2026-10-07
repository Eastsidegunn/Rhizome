package mission

// RHZ-079 FR-RHZ-110 K1: 커널 writer ApplyMissionDecision이 waiting_for_human·
// blocked에서 succeeded/failed를 1건 append로 쓰고, 재생이 TerminalDecisionID·
// BlockedReason 소거를 돌려준다.

import (
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

func TestApplyMissionDecisionFromWaitingHumanBlockedFRRHZ110(t *testing.T) {
	cases := []struct {
		id      string
		path    []domain.MissionState
		blocked bool
		to      domain.MissionState
	}{
		{"h-ok", []domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman}, false, domain.MissionSucceeded},
		{"h-fail", []domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman}, false, domain.MissionFailed},
		{"b-ok", []domain.MissionState{domain.MissionReady}, true, domain.MissionSucceeded},
		{"b-fail", []domain.MissionState{domain.MissionReady}, true, domain.MissionFailed},
	}
	for _, c := range cases {
		s := &events.Store{}
		svc := Service{Store: s}
		if _, err := svc.CreateGoal("g", "g", "d", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Create(c.id, "g", "m", "d"); err != nil {
			t.Fatal(err)
		}
		rev := uint64(1)
		for _, st := range c.path {
			if _, err := svc.Transition(c.id, rev, st); err != nil {
				t.Fatalf("%s: drive %s: %v", c.id, st, err)
			}
			rev++
		}
		if c.blocked {
			if _, err := svc.TransitionWithReason(c.id, rev, domain.MissionBlocked, "dependency unavailable"); err != nil {
				t.Fatal(err)
			}
			rev++
			if m, _ := projector.ReplayMission(s.List("mission", c.id)); m.BlockedReason != "dependency unavailable" {
				t.Fatalf("%s: blocked reason not projected %+v", c.id, m)
			}
		}
		n := len(s.All())
		m, err := svc.ApplyMissionDecision(c.id, rev, c.to, "decision-"+c.id, "relay:tester", "closed")
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if len(s.All()) != n+1 {
			t.Fatalf("%s: journal grew %d, want 1", c.id, len(s.All())-n)
		}
		if m.State != c.to || m.Revision != rev+1 || m.TerminalDecisionID != "decision-"+c.id || m.BlockedReason != "" {
			t.Fatalf("%s: %+v", c.id, m)
		}
	}
}
