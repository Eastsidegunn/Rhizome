package workspace

// RHZ-071 (FR-RHZ-100): mission.create takes an optional goalId (no goal
// pair when given; the goal must exist and be non-terminal) and question.ask
// requires an existing missionId. Validation lives at the relay entrance
// only — replay rules and projectors are unchanged.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
	"rhizome/internal/question"
)

type wire071 struct {
	Body struct {
		Missions []map[string]any `json:"missions"`
		Tasks    []map[string]any `json:"tasks"`
		Gates    []map[string]any `json:"gates"`
	} `json:"body"`
}

func getWorkspace071(t *testing.T, s events.Port) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode071(t *testing.T, b []byte) wire071 {
	t.Helper()
	var w wire071
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return w
}

func countAgg071(s events.Port, aggType string) int {
	n := 0
	for _, e := range s.All() {
		if e.AggregateType == aggType {
			n++
		}
	}
	return n
}

// M1: goalId given → zero goal events added, one mission bound to that goal,
// /v1/workspace tasks[].missionId == goalId, missions count unchanged.
func TestMissionCreateWithGoalIDSkipsGoalPairFRRHZ100(t *testing.T) {
	s := &events.Store{}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-shared", "shared", "done", ""); err != nil {
		t.Fatal(err)
	}
	goalsBefore, missionsBefore := countAgg071(s, "goal"), countAgg071(s, "mission")
	snapBefore := decode071(t, getWorkspace071(t, s))
	res, err := RelayIntent(s, Intent{Kind: "mission.create", Name: "child", Prompt: "do it", GoalID: "goal-shared"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("mission.create with goalId: %v %+v", err, res)
	}
	if got := countAgg071(s, "goal"); got != goalsBefore {
		t.Fatalf("goal events added: %d → %d", goalsBefore, got)
	}
	if got := countAgg071(s, "mission"); got != missionsBefore+1 {
		t.Fatalf("mission events: %d → %d, want +1", missionsBefore, got)
	}
	if len(s.List("goal", "goal-child")) != 0 {
		t.Fatal("goal-child must not be created when goalId is given")
	}
	m, err := projector.ReplayMission(s.List("mission", "mission-child"))
	if err != nil || m.GoalID != "goal-shared" || m.Description != "child" || m.Success != "do it" {
		t.Fatalf("mission: %+v %v", m, err)
	}
	w := decode071(t, getWorkspace071(t, s))
	if len(w.Body.Missions) != len(snapBefore.Body.Missions) {
		t.Fatalf("missions count changed: %d → %d", len(snapBefore.Body.Missions), len(w.Body.Missions))
	}
	found := false
	for _, task := range w.Body.Tasks {
		if task["id"] == "mission-child" {
			found = true
			if task["missionId"] != "goal-shared" {
				t.Fatalf("task.missionId = %v want goal-shared", task["missionId"])
			}
		}
	}
	if !found {
		t.Fatalf("mission-child not projected: %v", w.Body.Tasks)
	}
}

// M2: no goalId → the existing pair behaviour, byte-identical /v1/workspace
// to the pre-change path (the CreateGoal+Create pair the old relay issued,
// driven through the kernel on a separate store).
func TestMissionCreateWithoutGoalIDKeepsPairFRRHZ100(t *testing.T) {
	golden := &events.Store{}
	ms := mission.Service{Store: golden}
	if _, err := ms.CreateGoal("goal-n", "n", "success", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-n", "goal-n", "n", "success"); err != nil {
		t.Fatal(err)
	}
	// RHZ-133 (FR-RHZ-173): lastActivityTs reads envelope time; both stores
	// are compared with the same pinned times.
	want := getWorkspace071(t, pinnedTimes173(t, golden))

	s := &events.Store{}
	res, err := RelayIntent(s, Intent{Kind: "mission.create", Name: "n", Prompt: "success"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("mission.create: %v %+v", err, res)
	}
	if len(s.List("goal", "goal-n")) != 1 || len(s.List("mission", "mission-n")) != 1 {
		t.Fatal("pair not created")
	}
	if got := getWorkspace071(t, pinnedTimes173(t, s)); !bytes.Equal(got, want) {
		t.Fatalf("/v1/workspace differs from pre-change golden:\n got %s\nwant %s", got, want)
	}
}

// M3: unknown goal → "goal not found"; terminal goal → "goal is terminal";
// journal unchanged in both cases.
func TestMissionCreateGoalValidationFRRHZ100(t *testing.T) {
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
	res, err := RelayIntent(s, Intent{Kind: "mission.create", Name: "x", Prompt: "p", GoalID: "goal-missing"}, "tester", noAuthority())
	if err != nil || res.Accepted || res.Reason != "goal not found" {
		t.Fatalf("unknown goal: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on unknown goal")
	}
	res, err = RelayIntent(s, Intent{Kind: "mission.create", Name: "x", Prompt: "p", GoalID: "goal-done"}, "tester", noAuthority())
	if err != nil || res.Accepted || res.Reason != "goal is terminal" {
		t.Fatalf("terminal goal: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on terminal goal")
	}
	res, err = RelayIntent(s, Intent{Kind: "mission.create", Name: "x", Prompt: "p", GoalID: "goal-gone"}, "tester", noAuthority())
	if err != nil || res.Accepted || res.Reason != "goal is terminal" {
		t.Fatalf("cancelled goal: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on cancelled goal")
	}
}

// M4: same name twice with goalId → the second is rejected with Create's
// error, journal unchanged.
func TestMissionCreateDuplicateNameWithGoalIDFRRHZ100(t *testing.T) {
	s := &events.Store{}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-g", "g", "ok", ""); err != nil {
		t.Fatal(err)
	}
	in := Intent{Kind: "mission.create", Name: "dup", Prompt: "p", GoalID: "goal-g"}
	if res, err := RelayIntent(s, in, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("first: %v %+v", err, res)
	}
	before := len(s.All())
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil || res.Accepted || res.Reason == "" {
		t.Fatalf("second must be rejected with Create's error: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on duplicate")
	}
}

// Q1: question.ask — empty missionId → "missionId or goalId required" (RHZ-075; journal
// unchanged); unknown → "mission not found" (journal unchanged); existing →
// accepted and gates[].missionId projected.
func TestQuestionAskRequiresMissionIDFRRHZ100(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-q1", domain.MissionReady, domain.MissionRunning)
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r"}, "tester", noAuthority())
	if err != nil || res.Accepted || res.Reason != "missionId or goalId required" { // RHZ-075 (FR-RHZ-108): goalId is the alternative; reason string widened.
		t.Fatalf("empty missionId: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on empty missionId")
	}
	res, err = RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r", MissionID: "mission-nope"}, "tester", noAuthority())
	if err != nil || res.Accepted || res.Reason != "mission not found" {
		t.Fatalf("unknown mission: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on unknown mission")
	}
	// terminal mission: the relay passes it through and the kernel (question.go
	// "mission is terminal") rejects — pinned as current behaviour
	// (커널 무변경, 사후 결정은 goal 또는 살아있는 mission에).
	missionIn062(t, s, "mission-done", domain.MissionReady, domain.MissionRunning)
	if r, e := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-done"}, "tester", noAuthority()); e != nil || !r.Accepted {
		t.Fatalf("complete fixture: %v %+v", e, r)
	}
	before = len(s.All())
	res, err = RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r", MissionID: "mission-done"}, "tester", noAuthority())
	if err != nil || res.Accepted || !strings.Contains(res.Reason, "mission is terminal") {
		t.Fatalf("terminal mission: %v %+v", err, res)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on terminal mission")
	}
	res, err = RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r", MissionID: "mission-q1"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("existing mission: %v %+v", err, res)
	}
	w := decode071(t, getWorkspace071(t, s))
	if len(w.Body.Gates) != 1 || w.Body.Gates[0]["missionId"] != "mission-q1" {
		t.Fatalf("gates: %v", w.Body.Gates)
	}
}

// Q2: a fixture journal holding a question WITHOUT missionId (appended
// through the kernel, as the old relay would have) still replays and
// Snapshot works — replay rules unchanged.
func TestLegacyQuestionWithoutMissionIDReplaysFRRHZ100(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-l")
	q, err := (question.Service{Store: s}).Ask("legacy", "orphan body", "r", "", "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := (question.Service{Store: s}).Get(q.ID)
	if err != nil || got.MissionID != "" {
		t.Fatalf("legacy replay: %+v %v", got, err)
	}
	p, err := Snapshot(s)
	if err != nil || len(p.Gates) != 1 || p.Gates[0].MissionID != "" || p.Gates[0].State != "pending" {
		t.Fatalf("snapshot: %+v %v", p, err)
	}
}

// R1: real NDJSON journal round trip — a goalId mission and a bound question
// survive Close/Open with the same event count and a byte-identical
// projection, and no goal pair leaks into the file.
func TestJournalRoundTripGoalIDAndMissionIDFRRHZ100(t *testing.T) {
	path := t.TempDir() + "/events.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: j}).CreateGoal("goal-r", "r", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if res, err := RelayIntent(j, Intent{Kind: "mission.create", Name: "r1", Prompt: "p", GoalID: "goal-r"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("mission.create: %v %+v", err, res)
	}
	if res, err := RelayIntent(j, Intent{Kind: "question.ask", Name: "q", Body: "b", Recommendation: "r", MissionID: "mission-r1"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("question.ask: %v %+v", err, res)
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
	if got := len(j.All()); got != wantEvents {
		t.Fatalf("events %d want %d", got, wantEvents)
	}
	if countAgg071(j, "goal") != 1 {
		t.Fatal("goal pair leaked into journal")
	}
	if got := getWorkspace071(t, j); !bytes.Equal(got, want) {
		t.Fatalf("projection after reopen differs:\n got %s\nwant %s", got, want)
	}
	w := decode071(t, want)
	if len(w.Body.Tasks) != 1 || w.Body.Tasks[0]["missionId"] != "goal-r" || len(w.Body.Gates) != 1 || w.Body.Gates[0]["missionId"] != "mission-r1" {
		t.Fatalf("projection: tasks=%v gates=%v", w.Body.Tasks, w.Body.Gates)
	}
}
