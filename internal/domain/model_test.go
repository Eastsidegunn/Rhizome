package domain

import "testing"

func TestMissionTransitions(t *testing.T) {
	m, err := NewMission("m1", "g1", "do work", "work is complete")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []MissionState{MissionReady, MissionRunning, MissionWaitingResult, MissionSucceeded} {
		m, err = m.Transition(state)
		if err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	if m.Revision != 4 {
		t.Fatalf("revision = %d, want 4", m.Revision)
	}
	if _, err = m.Transition(MissionRunning); err == nil {
		t.Fatal("terminal mission transitioned")
	}
}
