package workspace

// RHZ-135 (FR-RHZ-177): a pending internal gate bound to a terminal mission
// leaves attention (gate_pending) and counts.needsYou; the gate itself stays
// pending — only the projection drops it.

import (
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

func TestTerminalMissionDropsGatePendingFRRHZ177(t *testing.T) {
	for _, c := range []struct {
		close string
		path  []domain.MissionState
	}{
		{"mission.complete", []domain.MissionState{domain.MissionReady, domain.MissionRunning}},
		{"mission.fail", []domain.MissionState{domain.MissionReady, domain.MissionRunning}},
		{"mission.cancel", []domain.MissionState{domain.MissionReady}}, // cancel has no edge from running
	} {
		close := c.close
		s := &events.Store{}
		missionIn062(t, s, "mission-078", c.path...)
		id, _ := askBound078(t, s, "t177-"+close)
		if ny, items := wire132(t, s); ny != 1 || len(ofKind132(items, "gate_pending")) != 1 {
			t.Fatalf("%s: before needsYou=%d items=%+v", close, ny, items)
		}
		before := len(s.List("question", id))
		relay132(t, s, Intent{Kind: close, MissionID: "mission-078", Reason: "closed"})
		if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
			t.Fatalf("%s: after terminal needsYou=%d items=%+v", close, ny, items)
		}
		if n := len(s.List("question", id)); n != before {
			t.Fatalf("%s: question stream %d → %d events, want unchanged", close, before, n)
		}
		p, err := Snapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, g := range p.Gates {
			if g.ID == id {
				found = true
				if g.State != "pending" {
					t.Fatalf("%s: gate state %q, want pending", close, g.State)
				}
			}
		}
		if !found {
			t.Fatalf("%s: gate %s missing from projection", close, id)
		}
	}
}

// A pending gate on another, still-open mission keeps its attention item.
func TestTerminalMissionKeepsOtherGatesFRRHZ177(t *testing.T) {
	s := store078(t)
	missionIn062(t, s, "mission-177", domain.MissionReady, domain.MissionRunning)
	askBound078(t, s, "t177-closed")
	relay132(t, s, Intent{Kind: "question.ask", Name: "t177-open", Body: "b", Recommendation: "r", MissionID: "mission-177"})
	relay132(t, s, Intent{Kind: "mission.complete", MissionID: "mission-078", Reason: "closed"})
	ny, items := wire132(t, s)
	got := ofKind132(items, "gate_pending")
	if ny != 1 || len(got) != 1 || got[0].MissionID != "mission-177" || got[0].Cause != "t177-open" {
		t.Fatalf("needsYou=%d items=%+v", ny, items)
	}
}
