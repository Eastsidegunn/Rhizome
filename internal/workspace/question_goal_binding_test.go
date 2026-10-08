package workspace

// RHZ-075 (FR-RHZ-108): question.ask accepts goalId as the alternative to
// missionId — exactly one of the two at the relay entrance; the gate is
// projected with gates[].goalId (omitempty, additive). Replay rules are
// unchanged: a legacy question.asked without the key still round-trips.
// Q1 goal path, Q2 exactly-one + existence/terminal, Q3 handle resolution,
// Q4 missionId path byte-identical, R1 NDJSON round trip incl. legacy.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/question"
)

func journalBytes075(t *testing.T, s events.Port) []byte {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gateByID075(t *testing.T, gates []map[string]any, id string) map[string]any {
	t.Helper()
	for _, g := range gates {
		if g["id"] == id {
			return g
		}
	}
	t.Fatalf("gate %s not projected: %v", id, gates)
	return nil
}

// Q1: question.ask{goalId} accepted → gates[].goalId projected, missionId key
// absent on that gate, gateCapabilities present as pending.
func TestQuestionAskGoalIDProjectsGoalGateFRRHZ108(t *testing.T) {
	s := &events.Store{}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-q1", "q1", "done", ""); err != nil {
		t.Fatal(err)
	}
	res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r", GoalID: "goal-q1"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("question.ask goalId: %v %+v", err, res)
	}
	qid := mustQuestionID("t", "b", "r")
	raw := getWorkspace071(t, s)
	var w struct {
		Body struct {
			Gates            []map[string]any             `json:"gates"`
			GateCapabilities map[string]map[string]string `json:"gateCapabilities"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	g := gateByID075(t, w.Body.Gates, qid)
	if g["goalId"] != "goal-q1" || g["state"] != "pending" {
		t.Fatalf("gate: %v", g)
	}
	if _, has := g["missionId"]; has {
		t.Fatalf("missionId must be omitted on a goal-bound gate: %v", g)
	}
	want := gateCapabilities("pending", true) // goal-bound → requestChanges enabled (RHZ-078, FR-RHZ-109)
	if caps := w.Body.GateCapabilities[qid]; caps["approve"] != want.Approve || caps["reject"] != want.Reject || caps["requestChanges"] != want.RequestChanges {
		t.Fatalf("gateCapabilities[%s] = %v want %+v", qid, caps, want)
	}
	p, err := Snapshot(s)
	if err != nil || len(p.Gates) != 1 || p.Gates[0].GoalID != "goal-q1" || p.Gates[0].MissionID != "" {
		t.Fatalf("snapshot: %+v %v", p.Gates, err)
	}
}

// Q2: neither → "missionId or goalId required"; both → "both missionId and
// goalId given"; unknown goal → "goal not found"; terminal goal → kernel's
// "goal is terminal". Journal byte-identical after every rejection.
func TestQuestionAskExactlyOneBindingFRRHZ108(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	missionIn062(t, s, "mission-q2", domain.MissionReady, domain.MissionRunning)
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
	before := journalBytes075(t, s)
	cases := []struct {
		name   string
		in     Intent
		reason string
	}{
		{"neither", Intent{}, "missionId or goalId required"},
		{"blank both", Intent{MissionID: " ", GoalID: " "}, "missionId or goalId required"},
		{"both", Intent{MissionID: "mission-q2", GoalID: "goal-mission-q2"}, "both missionId and goalId given"},
		{"unknown goal", Intent{GoalID: "goal-missing"}, "goal not found"},
		{"achieved goal", Intent{GoalID: "goal-done"}, "goal is terminal"},
		{"cancelled goal", Intent{GoalID: "goal-gone"}, "goal is terminal"},
	}
	for _, tc := range cases {
		in := tc.in
		in.Kind, in.Name, in.Body, in.Recommendation = "question.ask", "t", "b", "r"
		res, err := RelayIntent(s, in, "tester", noAuthority())
		if err != nil || res.Accepted || res.Reason != tc.reason {
			t.Fatalf("%s: %v %+v (want reason %q)", tc.name, err, res, tc.reason)
		}
		if got := journalBytes075(t, s); !bytes.Equal(got, before) {
			t.Fatalf("%s: journal changed", tc.name)
		}
	}
}

// Q3: goalId given as an RHZ-073 handle resolves to the goal ID; the journal
// never contains the handle string.
func TestQuestionAskGoalIDHandleResolvesFRRHZ108(t *testing.T) {
	s := &events.Store{}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-q3", "q3", "done", ""); err != nil {
		t.Fatal(err)
	}
	h := "g-" + sha8("goal-q3")
	if got := handleOf073(t, getWorkspace071(t, s), "missions", "goal-q3"); got != h {
		t.Fatalf("goal handle %q want %q", got, h)
	}
	res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r", GoalID: h}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("question.ask by handle: %v %+v", err, res)
	}
	q, err := (question.Service{Store: s}).Get(mustQuestionID("t", "b", "r"))
	if err != nil || q.GoalID != "goal-q3" {
		t.Fatalf("resolved binding: %+v %v", q, err)
	}
	if strings.Contains(string(journalBytes075(t, s)), h) {
		t.Fatal("handle string leaked into the journal")
	}
}

// Q4: the existing missionId path is byte-identical to a golden built on a
// separate store through the kernel with the same mission binding, and the
// projected gate carries no goalId key.
func TestQuestionAskMissionIDPathUnchangedFRRHZ108(t *testing.T) {
	golden := &events.Store{}
	missionIn062(t, golden, "mission-q4", domain.MissionReady, domain.MissionRunning)
	if _, err := (question.Service{Store: golden}).Ask("t", "b", "r", "mission-q4", "", "unverified-local-operator:tester", "corr-q4"); err != nil {
		t.Fatal(err)
	}
	want := getWorkspace071(t, golden)

	s := &events.Store{}
	missionIn062(t, s, "mission-q4", domain.MissionReady, domain.MissionRunning)
	res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "t", Body: "b", Recommendation: "r", MissionID: "mission-q4", CorrelationID: "corr-q4"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("question.ask missionId: %v %+v", err, res)
	}
	got := getWorkspace071(t, s)
	if !bytes.Equal(got, want) {
		t.Fatalf("/v1/workspace differs from golden:\n got %s\nwant %s", got, want)
	}
	w := decode071(t, got)
	g := gateByID075(t, w.Body.Gates, mustQuestionID("t", "b", "r"))
	if g["missionId"] != "mission-q4" {
		t.Fatalf("gate: %v", g)
	}
	if _, has := g["goalId"]; has {
		t.Fatalf("goalId must be omitted on a mission-bound gate: %v", g)
	}
}

// R1: real NDJSON journal round trip — a goal-bound question, a mission-bound
// question and a legacy question.asked (no GoalID key, appended directly)
// survive Close/Open with the same event count and byte-identical
// /v1/workspace; the legacy gate carries neither goalId nor missionId.
func TestJournalRoundTripGoalBoundAndLegacyQuestionFRRHZ108(t *testing.T) {
	path := t.TempDir() + "/events.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("goal-r", "r", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-r", "goal-r", "m", "ok"); err != nil {
		t.Fatal(err)
	}
	if res, err := RelayIntent(j, Intent{Kind: "question.ask", Name: "qg", Body: "b", Recommendation: "r", GoalID: "goal-r"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("goal-bound ask: %v %+v", err, res)
	}
	if res, err := RelayIntent(j, Intent{Kind: "question.ask", Name: "qm", Body: "b", Recommendation: "r", MissionID: "mission-r"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("mission-bound ask: %v %+v", err, res)
	}
	// Legacy question.asked exactly as a pre-RHZ-075 kernel wrote it: no GoalID key.
	d := question.Digest("ql", "b", "r")
	legacyID := mustQuestionIDForDigest(d)
	legacy, _ := json.Marshal(map[string]string{
		"Title": "ql", "Body": "b", "Recommendation": "r", "MissionID": "",
		"RequestedBy": "unverified-local-operator:legacy", "CorrelationID": "", "Digest": d,
	})
	if err := j.Append(0, events.Event{AggregateType: "question", AggregateID: legacyID, Revision: 1, Type: "question.asked", Payload: legacy, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	wantEvents := len(j.All())
	want := getWorkspace071(t, j)
	wantSnap, err := Snapshot(j)
	if err != nil {
		t.Fatal(err)
	}
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
	if got := getWorkspace071(t, j); !bytes.Equal(got, want) {
		t.Fatalf("projection after reopen differs:\n got %s\nwant %s", got, want)
	}
	gotSnap, err := Snapshot(j)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(wantSnap)
	b, _ := json.Marshal(gotSnap)
	if !bytes.Equal(a, b) {
		t.Fatalf("Snapshot after reopen differs:\n got %s\nwant %s", b, a)
	}
	w := decode071(t, want)
	if len(w.Body.Gates) != 3 {
		t.Fatalf("gates: %v", w.Body.Gates)
	}
	gg := gateByID075(t, w.Body.Gates, mustQuestionID("qg", "b", "r"))
	if gg["goalId"] != "goal-r" || gg["missionId"] != nil {
		t.Fatalf("goal-bound gate: %v", gg)
	}
	gm := gateByID075(t, w.Body.Gates, mustQuestionID("qm", "b", "r"))
	if gm["missionId"] != "mission-r" || gm["goalId"] != nil {
		t.Fatalf("mission-bound gate: %v", gm)
	}
	gl := gateByID075(t, w.Body.Gates, legacyID)
	if gl["goalId"] != nil || gl["missionId"] != nil || gl["state"] != "pending" {
		t.Fatalf("legacy gate: %v", gl)
	}
}
