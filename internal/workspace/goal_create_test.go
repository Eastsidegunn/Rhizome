package workspace

// RHZ-077 (FR-RHZ-105): goal.create — the operator registers an outcome (goal)
// directly. goal.created only (zero mission events); parentGoalId declares
// one contains(goal:parent → goal:new) edge on the RHZ-066 path in the same
// relay; every rejection leaves the append-only journal byte-identical.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

type wire077 struct {
	Body struct {
		Missions []map[string]any `json:"missions"`
		Tasks    []map[string]any `json:"tasks"`
		Edges    []map[string]any `json:"edges"`
	} `json:"body"`
}

func decode077(t *testing.T, s events.Port) wire077 {
	t.Helper()
	var w wire077
	if err := json.Unmarshal(getWorkspace071(t, s), &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func relay077(t *testing.T, s events.Port, in Intent) RelayResult {
	t.Helper()
	in.Kind = "goal.create"
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func countType077(s events.Port, typ string) int {
	n := 0
	for _, e := range s.All() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// G1: goal only — exactly one goal.created, zero mission events, the goal is
// a /v1/workspace mission with its success criterion, tasks unchanged.
func TestGoalCreateGoalOnlyFRRHZ105(t *testing.T) {
	s := &events.Store{}
	if res, err := RelayIntent(s, Intent{Kind: "mission.create", Name: "existing", Prompt: "p"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("seed: %v %+v", err, res)
	}
	before := decode077(t, s)
	n := len(s.All())
	if res := relay077(t, s, Intent{Name: "example", Success: "example outcome is recorded", Description: "예시 목표 만들기"}); !res.Accepted {
		t.Fatalf("goal.create rejected: %+v", res)
	}
	all := s.All()
	if len(all) != n+1 || all[n].Type != "goal.created" || all[n].AggregateID != "goal-example" {
		t.Fatalf("want exactly one goal.created appended, got %+v", all[n:])
	}
	if countType077(s, "mission.created") != 1 {
		t.Fatal("goal.create must not add mission events (fixture had exactly one)")
	}
	g, err := projector.ReplayGoal(s.List("goal", "goal-example"))
	if err != nil || g.Description != "예시 목표 만들기" || g.Success != "example outcome is recorded" {
		t.Fatalf("goal payload: %+v %v", g, err)
	}
	after := decode077(t, s)
	if len(after.Body.Missions) != len(before.Body.Missions)+1 || len(after.Body.Tasks) != len(before.Body.Tasks) {
		t.Fatalf("projection: missions %d→%d tasks %d→%d", len(before.Body.Missions), len(after.Body.Missions), len(before.Body.Tasks), len(after.Body.Tasks))
	}
	found := false
	for _, m := range after.Body.Missions {
		if m["id"] == "goal-example" && m["success"] == "example outcome is recorded" && m["name"] == "예시 목표 만들기" {
			found = true
		}
	}
	if !found {
		t.Fatalf("goal missing from /v1/workspace missions: %+v", after.Body.Missions)
	}
	// description defaults to name; prompt is accepted as the success alias
	// (mission.create shape).
	if res := relay077(t, s, Intent{Name: "math", Prompt: "math curriculum done"}); !res.Accepted {
		t.Fatalf("prompt alias rejected: %+v", res)
	}
	if g, err := projector.ReplayGoal(s.List("goal", "goal-math")); err != nil || g.Description != "math" || g.Success != "math curriculum done" {
		t.Fatalf("defaults: %+v %v", g, err)
	}
}

// G2: parentGoalId → goal + exactly one contains edge parent→new with the
// edge.declare shape and ID; /v1/workspace edges shows it and the new goal
// is a contains target. The parent may be given as an RHZ-073 handle.
func TestGoalCreateWithParentDeclaresContainsFRRHZ105(t *testing.T) {
	s := goals066(t, "goal-parent")
	n := len(s.All())
	if res := relay077(t, s, Intent{Name: "child", Success: "child done", ParentGoalID: "goal-parent"}); !res.Accepted {
		t.Fatalf("goal.create with parent rejected: %+v", res)
	}
	all := s.All()
	if len(all) != n+2 || all[n].Type != "goal.created" || all[n+1].Type != "edge.declared" {
		t.Fatalf("want goal.created then edge.declared, got %+v", all[n:])
	}
	if countType077(s, "edge.declared") != 1 {
		t.Fatal("exactly one contains edge expected")
	}
	// Same ID/endpoints/kind/actor/verified as the relay's edge.declare path.
	// Correlation differs by decision: edge.declare defaults to the literal
	// "relay", goal.create uses relayCorrelation ("relay:<actor>") like the
	// newer operator intents (goal.resolve, mission.complete) so the journal
	// records who registered the outcome — pinned here.
	want := edge.Edge{ID: "edge-contains-goal-parent-goal-child", From: edge.Endpoint{Type: "goal", ID: "goal-parent"}, To: edge.Endpoint{Type: "goal", ID: "goal-child"}, Kind: edge.Contains, Actor: "unverified-local-operator:tester", Correlation: "relay:unverified-local-operator:tester", Verified: false, Revision: 1}
	got, err := (edge.Service{Store: s}).Get(want.ID)
	if err != nil || got != want {
		t.Fatalf("edge: got %+v want %+v err=%v", got, want, err)
	}
	w := decode077(t, s)
	seen := false
	for _, x := range w.Body.Edges {
		to, _ := x["to"].(map[string]any)
		from, _ := x["from"].(map[string]any)
		if x["id"] == want.ID && x["edgeKind"] == "contains" && from["id"] == "goal-parent" && to["type"] == "goal" && to["id"] == "goal-child" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("contains edge missing from /v1/workspace: %+v", w.Body.Edges)
	}
	// Reachability on the live contains graph (RHZ-066 helper): new goal is
	// a contains target of the parent.
	if ok, err := containsReachable(edge.Service{Store: s}, "goal-parent", "goal-child"); err != nil || !ok {
		t.Fatalf("goal-child must be a contains target of goal-parent: %v %v", ok, err)
	}

	// parentGoalId as a handle (RHZ-073).
	ws := getWorkspace071(t, s)
	ph := handleOf073(t, ws, "missions", "goal-parent")
	if !handleShape(ph) {
		t.Fatalf("not a handle: %q", ph)
	}
	if res := relay077(t, s, Intent{Name: "child2", Success: "child2 done", ParentGoalID: ph}); !res.Accepted {
		t.Fatalf("goal.create with parent handle rejected: %+v", res)
	}
	x, err := (edge.Service{Store: s}).Get("edge-contains-goal-parent-goal-child2")
	if err != nil || x.From.ID != "goal-parent" || x.To.ID != "goal-child2" {
		t.Fatalf("handle parent edge: %+v %v", x, err)
	}
	for _, e := range s.All() {
		if strings.Contains(string(e.Payload), ph) {
			t.Fatalf("journal must store IDs, not handles: %s", e.Payload)
		}
	}
}

// G3 + W1: every rejection path leaves the journal byte-identical (no
// partial write — the parent is validated before CreateGoal).
func TestGoalCreateRejectionsJournalUnchangedFRRHZ105(t *testing.T) {
	s := goals066(t, "goal-live", "goal-dup", "goal-done")
	if res, err := RelayIntent(s, Intent{Kind: "goal.resolve", GoalID: "goal-done"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("goal.resolve: %v %+v", err, res)
	}
	cases := []struct {
		name   string
		in     Intent
		reason string
	}{
		{"empty name", Intent{Success: "s"}, "goal name required"},
		{"empty success", Intent{Name: "x"}, "success required"},
		{"blank success", Intent{Name: "x", Success: "  "}, "success required"},
		{"duplicate name", Intent{Name: "dup", Success: "s"}, events.ErrRevisionConflict.Error()},
		{"parent not found", Intent{Name: "orphan", Success: "s", ParentGoalID: "goal-nope"}, "parent goal not found"},
		{"parent terminal", Intent{Name: "late", Success: "s", ParentGoalID: "goal-done"}, "parent goal is terminal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := journal066(t, s)
			res := relay077(t, s, c.in)
			if res.Accepted || !strings.Contains(res.Reason, c.reason) {
				t.Fatalf("want rejection %q, got %+v", c.reason, res)
			}
			if after := journal066(t, s); after != before {
				t.Fatalf("journal changed on rejection:\n%s\n%s", before, after)
			}
			if _, err := projector.ReplayGoal(s.List("goal", "goal-"+c.in.Name)); c.in.Name != "dup" && c.in.Name != "" && err == nil {
				t.Fatalf("goal %q must not exist after rejection", c.in.Name)
			}
		})
	}
}

// R1: real NDJSON journal round trip preserves goal + contains edge with a
// byte-identical projection.
func TestGoalCreateJournalRoundTripFRRHZ105(t *testing.T) {
	path := t.TempDir() + "/events.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: j}).CreateGoal("goal-root", "root", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if res := relay077(t, j, Intent{Name: "leaf", Success: "leaf ok", ParentGoalID: "goal-root", PolicyRef: "policy-x"}); !res.Accepted {
		t.Fatalf("goal.create: %+v", res)
	}
	wantEvents := len(j.All())
	want := getWorkspace071(t, j)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if got := len(j.All()); got != wantEvents || countType077(j, "mission.created") != 0 {
		t.Fatalf("events %d want %d (missions %d)", got, wantEvents, countType077(j, "mission.created"))
	}
	if g, err := projector.ReplayGoal(j.List("goal", "goal-leaf")); err != nil || g.Success != "leaf ok" || g.PolicyRef != "policy-x" {
		t.Fatalf("goal after reopen: %+v %v", g, err)
	}
	if x, err := (edge.Service{Store: j}).Get("edge-contains-goal-root-goal-leaf"); err != nil || x.Kind != edge.Contains || x.To.ID != "goal-leaf" {
		t.Fatalf("edge after reopen: %+v %v", x, err)
	}
	if got := getWorkspace071(t, j); !bytes.Equal(got, want) {
		t.Fatalf("projection diverged after reopen:\n%s\n%s", want, got)
	}
}
