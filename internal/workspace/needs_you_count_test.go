package workspace

// RHZ-085 ③ (FR-RHZ-115): counts.needsYou includes pending internal gates
// (questions). Rule: needsYou = missions waiting_for_human + questions with
// state pending. changes_requested (RHZ-078) is NOT counted — the worker has
// the ball. JANUS gate contribution unchanged. Determinism and no writes.

import (
	"bytes"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

// C1: pending question bound to a running mission → needsYou 1; approve → 0;
// reject → 0 (fresh store per decision).
func TestNeedsYouCountsPendingQuestionFRRHZ115(t *testing.T) {
	for _, kind := range []string{"gate.approve", "gate.reject"} {
		s := store078(t)
		if ny, _, _ := workspaceCounts079(t, s); ny != 0 {
			t.Fatalf("%s: needsYou=%d before ask, want 0", kind, ny)
		}
		id, digest := askBound078(t, s, "c1-"+kind)
		if ny, _, _ := workspaceCounts079(t, s); ny != 1 {
			t.Fatalf("%s: needsYou=%d with pending question, want 1", kind, ny)
		}
		res, err := RelayIntent(s, Intent{Kind: kind, GateID: id, Digest: digest, Reason: "decided"}, "alice", noAuthority())
		if err != nil || !res.Accepted {
			t.Fatalf("%s: %v %+v", kind, err, res)
		}
		if ny, _, _ := workspaceCounts079(t, s); ny != 0 {
			t.Fatalf("%s: needsYou=%d after decision, want 0", kind, ny)
		}
	}
}

// C2: changes_requested is not counted (worker has the ball); a later
// approve keeps it at 0.
func TestNeedsYouExcludesChangesRequestedFRRHZ115(t *testing.T) {
	s := store078(t)
	id, digest := askBound078(t, s, "c2")
	if ny, _, _ := workspaceCounts079(t, s); ny != 1 {
		t.Fatalf("needsYou=%d pending, want 1", ny)
	}
	if res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: id, Digest: digest, Reason: "shorter"}, "alice", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("requestChanges: %v %+v", err, res)
	}
	w := decode078(t, s)
	if g := gateByID075(t, w.Body.Gates, id); g["state"] != "changes_requested" {
		t.Fatalf("gate state %v", g["state"])
	}
	if ny, _, _ := workspaceCounts079(t, s); ny != 0 {
		t.Fatalf("needsYou=%d after changes_requested, want 0", ny)
	}
	if res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: id, Digest: digest}, "alice", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("approve: %v %+v", err, res)
	}
	if ny, _, _ := workspaceCounts079(t, s); ny != 0 {
		t.Fatalf("needsYou=%d after approve, want 0", ny)
	}
}

// C3: waiting_for_human mission + pending question on another mission → 2.
func TestNeedsYouSumsMissionAndQuestionFRRHZ115(t *testing.T) {
	s := store078(t)
	missionIn062(t, s, "mission-085h", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	if ny, _, _ := workspaceCounts079(t, s); ny != 1 {
		t.Fatalf("needsYou=%d with waiting mission only, want 1", ny)
	}
	askBound078(t, s, "c3")
	if ny, _, _ := workspaceCounts079(t, s); ny != 2 {
		t.Fatalf("needsYou=%d with waiting mission + pending question, want 2", ny)
	}
}

// C4: determinism — two Snapshots (and two wire bodies) over the same journal are equal.
// C5: Snapshot and GET /v1/workspace write nothing to the journal.
func TestNeedsYouDeterministicAndReadOnlyFRRHZ115(t *testing.T) {
	s := store078(t)
	missionIn062(t, s, "mission-085h", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	askBound078(t, s, "c4")
	before := journalBytes075(t, s)
	n := len(s.All())
	p1, err := Snapshot(events.Port(s))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Snapshot(events.Port(s))
	if err != nil {
		t.Fatal(err)
	}
	if p1.Counts != p2.Counts || p1.Counts.NeedsYou != 2 {
		t.Fatalf("counts %+v vs %+v", p1.Counts, p2.Counts)
	}
	w1, w2 := getWorkspace071(t, s), getWorkspace071(t, s)
	if !bytes.Equal(w1, w2) {
		t.Fatalf("wire diverged:\n%s\n%s", w1, w2)
	}
	if len(s.All()) != n || !bytes.Equal(journalBytes075(t, s), before) {
		t.Fatalf("journal changed by read: %d → %d", n, len(s.All()))
	}
}
