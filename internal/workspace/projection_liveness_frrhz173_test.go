package workspace

// RHZ-133 (FR-RHZ-173): per-node liveness / version / provenance / progress
// facts on GET /v1/workspace — changedAtRevision + lastActivityTs (own
// streams only), tasks[].active, tasks[].originNodeId, steps — all derived
// from the journal and omitted when unknown.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
)

// pinnedTimes173 re-appends a store's events with a fixed envelope time per
// position, so two journals built through different paths at different wall
// times can still be compared byte-for-byte once lastActivityTs is on the wire.
func pinnedTimes173(t *testing.T, s events.Port) *events.Store {
	t.Helper()
	out := &events.Store{}
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for i, e := range s.All() {
		e.CreatedAt = base.Add(time.Duration(i) * time.Second)
		if err := out.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

type wire173 struct {
	Revision uint64
	Sections map[string]map[string]map[string]any // section → id → object
}

func getWire173(t *testing.T, s events.Port) wire173 {
	t.Helper()
	raw := getWorkspace071(t, s)
	var env struct {
		Revision uint64                     `json:"revision"`
		Body     map[string]json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	w := wire173{Revision: env.Revision, Sections: map[string]map[string]map[string]any{}}
	for _, section := range []string{"missions", "tasks", "gates", "deliverables", "requests"} {
		w.Sections[section] = map[string]map[string]any{}
		data, ok := env.Body[section]
		if !ok {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var items []map[string]any
		if err := dec.Decode(&items); err != nil {
			t.Fatalf("decode %s: %v", section, err)
		}
		for _, it := range items {
			w.Sections[section][it["id"].(string)] = it
		}
	}
	return w
}

func (w wire173) node(t *testing.T, section, id string) map[string]any {
	t.Helper()
	n := w.Sections[section][id]
	if n == nil {
		t.Fatalf("%s %s missing: %v", section, id, w.Sections[section])
	}
	return n
}

func num173(t *testing.T, n map[string]any, key string) (int64, bool) {
	t.Helper()
	v, ok := n[key]
	if !ok {
		return 0, false
	}
	x, err := v.(json.Number).Int64()
	if err != nil {
		t.Fatalf("%s not an integer: %v", key, v)
	}
	return x, true
}

// lastOf173 is the expected activity: the latest event across the streams.
func lastOf173(s events.Port, streams ...[2]string) (uint64, int64) {
	var seq uint64
	var ms int64
	for _, k := range streams {
		for _, e := range s.List(k[0], k[1]) {
			if e.Sequence > seq {
				seq, ms = e.Sequence, e.CreatedAt.UnixMilli()
			}
		}
	}
	return seq, ms
}

func assertActivity173(t *testing.T, w wire173, section, id string, s events.Port, streams ...[2]string) {
	t.Helper()
	n := w.node(t, section, id)
	wantSeq, wantMs := lastOf173(s, streams...)
	gotSeq, okSeq := num173(t, n, "changedAtRevision")
	gotMs, okMs := num173(t, n, "lastActivityTs")
	if !okSeq || !okMs || uint64(gotSeq) != wantSeq || gotMs != wantMs || wantMs <= 0 {
		t.Fatalf("%s %s activity = (%d,%v)/(%d,%v), want (%d,%d)", section, id, gotSeq, okSeq, gotMs, okMs, wantSeq, wantMs)
	}
}

func assertRevisionBound173(t *testing.T, w wire173) {
	t.Helper()
	for section, items := range w.Sections {
		for id, n := range items {
			if v, ok := num173(t, n, "changedAtRevision"); ok && (v < 1 || uint64(v) > w.Revision) {
				t.Fatalf("%s %s changedAtRevision %d outside [1,%d]", section, id, v, w.Revision)
			}
		}
	}
}

func steps173(t *testing.T, n map[string]any) (done, total int64, ok bool) {
	t.Helper()
	raw, has := n["steps"]
	if !has {
		return 0, 0, false
	}
	m := raw.(map[string]any)
	d, _ := m["done"].(json.Number).Int64()
	tt, _ := m["total"].(json.Number).Int64()
	if len(m) != 2 {
		t.Fatalf("steps keys %v", m)
	}
	return d, tt, true
}

func drive173(t *testing.T, s events.Port, id string, path ...domain.MissionState) {
	t.Helper()
	ms := mission.Service{Store: s}
	for _, to := range path {
		m, err := projector.ReplayMission(s.List("mission", id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ms.Transition(id, m.Revision, to); err != nil {
			t.Fatalf("drive %s to %s: %v", id, to, err)
		}
	}
}

// L1: every node kind reports the latest event of its own stream(s); a task's
// surface progress bumps it, an edge never does; every changedAtRevision is
// within [1, envelope revision].
func TestLivenessActivityOwnStreamsFRRHZ173(t *testing.T) {
	s := requestWorkspaceFixture(t)
	relay132(t, s, createRequestIntent())
	if res := register081(t, s, Intent{MissionID: "mission-request", DeliverableKind: "code", Summary: "patch"}); !res.Accepted {
		t.Fatalf("deliverable: %+v", res)
	}
	qid, _ := func() (string, string) {
		res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "q173", Body: "b", Recommendation: "r", MissionID: "mission-request"}, "tester", noAuthority())
		if err != nil || !res.Accepted {
			t.Fatalf("ask: %v %+v", err, res)
		}
		return mustQuestionID("q173", "b", "r"), ""
	}()
	// pendingGate's decision names mission "m".
	if _, err := (mission.Service{Store: s}).Create("m", "goal-request", "m", "done"); err != nil {
		t.Fatal(err)
	}
	gr := pendingGate(t, s, "r173", gwDigest)

	w := getWire173(t, s)
	assertActivity173(t, w, "missions", "goal-request", s, [2]string{"goal", "goal-request"})
	assertActivity173(t, w, "tasks", "mission-request", s, [2]string{"mission", "mission-request"}, [2]string{"surface", "surface-mission-request"})
	assertActivity173(t, w, "gates", qid, s, [2]string{"question", qid})
	assertActivity173(t, w, "gates", gr.ID, s, [2]string{"approval", gr.ID}, [2]string{"approvalrequest", gr.ID})
	var rid, did string
	for id := range w.Sections["requests"] {
		rid = id
	}
	for id := range w.Sections["deliverables"] {
		did = id
	}
	assertActivity173(t, w, "requests", rid, s, [2]string{"request", rid})
	assertActivity173(t, w, "deliverables", did, s, [2]string{"deliverable", did})
	assertRevisionBound173(t, w)
	taskBefore, _ := num173(t, w.node(t, "tasks", "mission-request"), "changedAtRevision")
	goalBefore, _ := num173(t, w.node(t, "missions", "goal-request"), "changedAtRevision")

	// Surface progress lives on surface/surface-<id>: the task moves.
	drive173(t, s, "mission-request", domain.MissionReady, domain.MissionRunning)
	if res := progress082(t, s, "mission-request", Intent{CurrentAction: "working"}); !res.Accepted {
		t.Fatalf("progress: %+v", res)
	}
	w = getWire173(t, s)
	taskAfter, _ := num173(t, w.node(t, "tasks", "mission-request"), "changedAtRevision")
	if taskAfter <= taskBefore || uint64(taskAfter) != w.Revision {
		t.Fatalf("surface progress did not bump task: %d → %d (rev %d)", taskBefore, taskAfter, w.Revision)
	}
	if last := s.All()[len(s.All())-1]; last.AggregateType != "surface" {
		t.Fatalf("last event %s/%s, want surface", last.AggregateType, last.AggregateID)
	}
	assertActivity173(t, w, "tasks", "mission-request", s, [2]string{"mission", "mission-request"}, [2]string{"surface", "surface-mission-request"})

	// A contains edge touching the goal is not the goal's own stream.
	relay077(t, s, Intent{Name: "child", Success: "ok", ParentGoalID: "goal-request"})
	w = getWire173(t, s)
	if v, _ := num173(t, w.node(t, "missions", "goal-request"), "changedAtRevision"); v != goalBefore {
		t.Fatalf("edge bumped goal activity: %d → %d", goalBefore, v)
	}
	assertRevisionBound173(t, w)
}

// L2: the empty journal golden and the top-level key set are unchanged.
func TestLivenessEmptyJournalGoldenFRRHZ173(t *testing.T) {
	const golden = `{"revision":0,"body":{"missions":[],"tasks":[],"gates":[],"deliverables":[],"edges":[],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{},"gateCapabilities":{}}}` + "\n"
	if got := string(getWorkspace071(t, &events.Store{})); got != golden {
		t.Fatalf("empty golden changed:\n got %s\nwant %s", got, golden)
	}
	s := fixture068(t)
	var env struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(getWorkspace071(t, s), &env); err != nil {
		t.Fatal(err)
	}
	want := "missions,tasks,gates,deliverables,edges,counts,attention,capabilities,gateCapabilities"
	if got := strings.Join(objectKeys070(t, env.Body), ","); got != want {
		t.Fatalf("top-level keys %s, want %s", got, want)
	}
}

// L3: active is present on every task — true only while running — and on no
// other node kind.
func TestTaskActiveFRRHZ173(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "m-planned")
	missionIn062(t, s, "m-running", domain.MissionReady, domain.MissionRunning)
	missionIn062(t, s, "m-waiting", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	missionIn062(t, s, "m-done")
	drive062Terminal(t, s, "m-done", domain.MissionSucceeded)
	w := getWire173(t, s)
	want := map[string]bool{"m-planned": false, "m-running": true, "m-waiting": false, "m-done": false}
	for id, active := range want {
		v, ok := w.node(t, "tasks", id)["active"]
		if !ok || v != active {
			t.Fatalf("%s active=%v (present %v), want %v", id, v, ok, active)
		}
	}
	for id, n := range w.Sections["missions"] {
		if _, ok := n["active"]; ok {
			t.Fatalf("goal %s carries active", id)
		}
	}
}

// L4: originNodeId is the single live spawn source; none → absent; two
// distinct sources → absent; superseding the extra edge restores it.
func TestTaskOriginNodeFRRHZ173(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-o", procedure.Step{ID: "a", Action: "act-a"}, procedure.Step{ID: "b", Action: "act-b", After: []string{"a"}})
	relay132(t, s, Intent{Kind: "procedure.run", ID: "proc-o", Name: "o", GoalID: "goal-dev"})
	w := getWire173(t, s)
	for _, id := range []string{"mission-o-a", "mission-o-b"} {
		if got := w.node(t, "tasks", id)["originNodeId"]; got != "mission-o" {
			t.Fatalf("%s originNodeId=%v, want mission-o", id, got)
		}
	}
	for _, id := range []string{"mission-o", "mission-x"} {
		if v, ok := w.node(t, "tasks", id)["originNodeId"]; ok {
			t.Fatalf("%s has originNodeId %v without a spawn source", id, v)
		}
	}
	es := edge.Service{Store: s}
	extra := edge.Spec{ID: "e-extra", From: edge.Endpoint{Type: "mission", ID: "mission-x"}, To: edge.Endpoint{Type: "mission", ID: "mission-o-a"}, Kind: edge.Spawn, Actor: "t", Correlation: "t"}
	if _, err := es.Create(extra, noAuthority()); err != nil {
		t.Fatal(err)
	}
	// A duplicate edge from the same source is not ambiguity.
	dup := edge.Spec{ID: "e-dup", From: edge.Endpoint{Type: "mission", ID: "mission-o"}, To: edge.Endpoint{Type: "mission", ID: "mission-o-b"}, Kind: edge.Spawn, Actor: "t", Correlation: "t"}
	if _, err := es.Create(dup, noAuthority()); err != nil {
		t.Fatal(err)
	}
	w = getWire173(t, s)
	if v, ok := w.node(t, "tasks", "mission-o-a")["originNodeId"]; ok {
		t.Fatalf("ambiguous origin emitted: %v", v)
	}
	if got := w.node(t, "tasks", "mission-o-b")["originNodeId"]; got != "mission-o" {
		t.Fatalf("duplicate same-source spawn broke origin: %v", got)
	}
	rewired := edge.Spec{ID: "e-extra-2", From: edge.Endpoint{Type: "mission", ID: "mission-x"}, To: edge.Endpoint{Type: "mission", ID: "mission-o"}, Kind: edge.Spawn, Actor: "t", Correlation: "t"}
	if _, err := es.Rewire("e-extra", rewired, noAuthority()); err != nil {
		t.Fatal(err)
	}
	w = getWire173(t, s)
	if got := w.node(t, "tasks", "mission-o-a")["originNodeId"]; got != "mission-o" {
		t.Fatalf("superseded edge still counted: %v", got)
	}
	if got := w.node(t, "tasks", "mission-o")["originNodeId"]; got != "mission-x" {
		t.Fatalf("rewired edge origin: %v", got)
	}
	for id, n := range w.Sections["tasks"] {
		if n["originNodeId"] == id {
			t.Fatalf("%s is its own origin", id)
		}
	}
}

// L5: a run's steps count its spawn children: cancelled leave the total,
// succeeded are done; total 0 → absent; leaf tasks carry no steps.
func TestTaskStepsSpawnChildrenFRRHZ173(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-s", procedure.Step{ID: "a", Action: "act-a"}, procedure.Step{ID: "b", Action: "act-b"}, procedure.Step{ID: "c", Action: "act-c"})
	relay132(t, s, Intent{Kind: "procedure.run", ID: "proc-s", Name: "s", GoalID: "goal-dev"})
	check := func(wantDone, wantTotal int64, wantPresent bool) {
		t.Helper()
		w := getWire173(t, s)
		d, tt, ok := steps173(t, w.node(t, "tasks", "mission-s"))
		if ok != wantPresent || d != wantDone || tt != wantTotal {
			t.Fatalf("run steps = %d/%d (present %v), want %d/%d (%v)", d, tt, ok, wantDone, wantTotal, wantPresent)
		}
		for _, id := range []string{"mission-s-a", "mission-s-b", "mission-s-c", "mission-x"} {
			if _, ok := w.node(t, "tasks", id)["steps"]; ok {
				t.Fatalf("leaf %s carries steps", id)
			}
		}
	}
	check(0, 3, true)
	drive062Terminal(t, s, "mission-s-a", domain.MissionSucceeded)
	check(1, 3, true)
	if res := cancelMission062(t, s, "mission-s-b"); !res.Accepted {
		t.Fatalf("cancel: %+v", res)
	}
	check(1, 2, true)
	if res := cancelMission062(t, s, "mission-s-c"); !res.Accepted {
		t.Fatalf("cancel: %+v", res)
	}
	check(1, 1, true)

	// All children cancelled → total 0 → absent.
	s2 := fixture068(t)
	defineProc(t, s2, "proc-z", procedure.Step{ID: "a", Action: "act-a"})
	relay132(t, s2, Intent{Kind: "procedure.run", ID: "proc-z", Name: "z", GoalID: "goal-dev"})
	if res := cancelMission062(t, s2, "mission-z-a"); !res.Accepted {
		t.Fatalf("cancel: %+v", res)
	}
	if _, _, ok := steps173(t, getWire173(t, s2).node(t, "tasks", "mission-z")); ok {
		t.Fatal("total 0 must omit steps")
	}
}

// L6: a goal's steps cover its tasks and those of every contained goal; a
// contains cycle terminates; a goal without tasks has no steps.
func TestGoalStepsContainsClosureFRRHZ173(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	for _, g := range []string{"goal-root", "goal-mid", "goal-leaf", "goal-empty"} {
		if _, err := ms.CreateGoal(g, g, "done", ""); err != nil {
			t.Fatal(err)
		}
	}
	for id, g := range map[string]string{"m-root": "goal-root", "m-mid": "goal-mid", "m-leaf-1": "goal-leaf", "m-leaf-2": "goal-leaf", "m-leaf-x": "goal-leaf"} {
		if _, err := ms.Create(id, g, id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	relay132(t, s, Intent{Kind: "edge.declare", From: "goal:goal-root", To: "goal:goal-mid"})
	relay132(t, s, Intent{Kind: "edge.declare", From: "goal:goal-mid", To: "goal:goal-leaf"})
	drive062Terminal(t, s, "m-leaf-1", domain.MissionSucceeded)
	if res := cancelMission062(t, s, "m-leaf-x"); !res.Accepted {
		t.Fatalf("cancel: %+v", res)
	}
	want := map[string][2]int64{"goal-root": {1, 4}, "goal-mid": {1, 3}, "goal-leaf": {1, 2}}
	check := func(want map[string][2]int64) {
		t.Helper()
		w := getWire173(t, s)
		for g, dt := range want {
			d, tt, ok := steps173(t, w.node(t, "missions", g))
			if !ok || d != dt[0] || tt != dt[1] {
				t.Fatalf("%s steps %d/%d (present %v), want %v", g, d, tt, ok, dt)
			}
		}
		if _, _, ok := steps173(t, w.node(t, "missions", "goal-empty")); ok {
			t.Fatal("goal without tasks carries steps")
		}
	}
	check(want)
	// Cycle leaf → root (published directly; the relay refuses cycles):
	// every goal on the cycle now reaches all three, and the scan ends.
	if _, err := (edge.Service{Store: s}).Create(edge.Spec{ID: "e-cycle", From: edge.Endpoint{Type: "goal", ID: "goal-leaf"}, To: edge.Endpoint{Type: "goal", ID: "goal-root"}, Kind: edge.Contains, Actor: "t", Correlation: "t"}, noAuthority()); err != nil {
		t.Fatal(err)
	}
	check(map[string][2]int64{"goal-root": {1, 4}, "goal-mid": {1, 4}, "goal-leaf": {1, 4}})
}

// L7: the envelope time is durable — identical after a journal reopen — and a
// legacy line with the zero time reports no lastActivityTs (unknown) while its
// changedAtRevision stays.
func TestLivenessJournalDurableAndLegacyZeroTimeFRRHZ173(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.ndjson")
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("goal-j", "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-j", "goal-j", "m", "done"); err != nil {
		t.Fatal(err)
	}
	want := getWorkspace071(t, j)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	got := getWorkspace071(t, j)
	_ = j.Close()
	if !bytes.Equal(got, want) || !bytes.Contains(got, []byte(`"lastActivityTs":`)) {
		t.Fatalf("activity not durable:\n%s\n%s", want, got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e["aggregate_type"] == "goal" {
			e["created_at"] = time.Time{}
		}
		b, _ := json.Marshal(e)
		lines = append(lines, string(b))
	}
	legacy := filepath.Join(t.TempDir(), "legacy.ndjson")
	if err := os.WriteFile(legacy, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err = openTestJournal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	w := getWire173(t, j)
	first := getWorkspace071(t, j)
	_ = j.Close()
	goal := w.node(t, "missions", "goal-j")
	if _, ok := goal["lastActivityTs"]; ok {
		t.Fatalf("legacy zero time reported as activity: %v", goal)
	}
	if v, ok := num173(t, goal, "changedAtRevision"); !ok || v != 1 {
		t.Fatalf("legacy changedAtRevision %v %v", v, ok)
	}
	if _, ok := num173(t, w.node(t, "tasks", "mission-j"), "lastActivityTs"); !ok {
		t.Fatal("stamped mission lost lastActivityTs")
	}
	// Replay never re-stamps a legacy line with the load time.
	time.Sleep(2 * time.Millisecond)
	j, err = openTestJournal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if again := getWorkspace071(t, j); !bytes.Equal(again, first) {
		t.Fatalf("legacy projection differs across reopen:\n%s\n%s", first, again)
	}
}

// L8: the facts are pure projection — a GET appends nothing.
func TestLivenessPureProjectionFRRHZ173(t *testing.T) {
	s := fixture068(t)
	before := len(s.All())
	_ = getWire173(t, s)
	if len(s.All()) != before {
		t.Fatal("GET wrote to the journal")
	}
}
