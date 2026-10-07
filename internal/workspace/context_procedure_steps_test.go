package workspace

// RHZ-068 FR-RHZ-097: GET /v1/context steps[] — procedure.run 인스턴스 투영.
// 테스트 계획 RHZ-068 S1~S8.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
)

type stepView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	State string   `json:"state"`
	After []string `json:"after"`
	Gates []struct {
		ID, Name, State string
	} `json:"gates"`
}

// fixture068: goal-dev + 템플릿 소스 지식 + mission-x(무관 mission).
func fixture068(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "src", SourceType: "note", SourceID: "n-1", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	if _, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-dev", Kind: knowledge.Procedure, Statement: "dev loop", SourceMemoryID: "mem-1", Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-dev", "dev goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-x", "goal-dev", "plain mission", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func defineProc(t *testing.T, s *events.Store, id string, steps ...procedure.Step) {
	t.Helper()
	if _, err := (procedure.Service{Store: s}).Create(procedure.Procedure{ID: id, SourceKnowledgeID: "k-dev", Trigger: "manual", Steps: steps, SuccessConditions: []string{"ci green"}}); err != nil {
		t.Fatal(err)
	}
}

func runProc(t *testing.T, srv *httptest.Server, procID, runID string) {
	t.Helper()
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.run", "id": procID, "name": runID, "goalId": "goal-dev"}); out["Accepted"] != true {
		t.Fatalf("procedure.run %s rejected: %v", runID, out)
	}
}

func contextSteps(t *testing.T, h http.Handler, missionID string) (int, []byte, []stepView) {
	t.Helper()
	code, b := getContext(t, h, "?task="+missionID)
	var bundle struct {
		Steps []stepView `json:"steps"`
	}
	if code == http.StatusOK {
		if err := json.Unmarshal(b, &bundle); err != nil {
			t.Fatal(err)
		}
	}
	return code, b, bundle.Steps
}

func stepIDs(steps []stepView) []string {
	ids := make([]string, 0, len(steps))
	for _, st := range steps {
		ids = append(ids, st.ID)
	}
	return ids
}

func eq(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

// S1 — 선형 run: 순서·after·name·state(재생값과 동일).
func TestContextStepsLinearRunFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-lin", procedure.Step{ID: "a", Action: "act-a"}, procedure.Step{ID: "b", Action: "act-b", After: []string{"a"}}, procedure.Step{ID: "c", Action: "act-c", After: []string{"b"}})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-lin", "r1")
	code, _, steps := contextSteps(t, NewHTTP(s).Handler(), "mission-r1")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !eq(stepIDs(steps), []string{"mission-r1-a", "mission-r1-b", "mission-r1-c"}) {
		t.Fatalf("order: %v", stepIDs(steps))
	}
	wantAfter := [][]string{{}, {"mission-r1-a"}, {"mission-r1-b"}}
	for i, st := range steps {
		if !eq(st.After, wantAfter[i]) {
			t.Fatalf("step %s after=%v want %v", st.ID, st.After, wantAfter[i])
		}
		if st.Name != "act-"+string(st.ID[len(st.ID)-1]) {
			t.Fatalf("step %s name=%q", st.ID, st.Name)
		}
		replayed, err := projector.ReplayMission(s.List("mission", st.ID))
		if err != nil {
			t.Fatal(err)
		}
		if st.State != mapState(replayed.State) || st.State == "" {
			t.Fatalf("step %s state=%q want %q", st.ID, st.State, mapState(replayed.State))
		}
		if len(st.Gates) != 0 {
			t.Fatalf("step %s gates=%v want empty", st.ID, st.Gates)
		}
	}
}

// S2 — 위상 순서: 역순 삽입·다이아몬드 동률 사전순 (변이 probe b).
func TestContextStepsTopoOrderFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-rev", procedure.Step{ID: "c", Action: "c", After: []string{"b"}}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}}, procedure.Step{ID: "a", Action: "a"})
	defineProc(t, s, "proc-dia", procedure.Step{ID: "d", Action: "d", After: []string{"c", "b"}}, procedure.Step{ID: "c", Action: "c", After: []string{"a"}}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}}, procedure.Step{ID: "a", Action: "a"})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-rev", "rev")
	runProc(t, srv, "proc-dia", "dia")
	h := NewHTTP(s).Handler()
	if _, _, steps := contextSteps(t, h, "mission-rev"); !eq(stepIDs(steps), []string{"mission-rev-a", "mission-rev-b", "mission-rev-c"}) {
		t.Fatalf("rev order: %v", stepIDs(steps))
	}
	_, _, steps := contextSteps(t, h, "mission-dia")
	if !eq(stepIDs(steps), []string{"mission-dia-a", "mission-dia-b", "mission-dia-c", "mission-dia-d"}) {
		t.Fatalf("dia order: %v", stepIDs(steps))
	}
	if !eq(steps[3].After, []string{"mission-dia-b", "mission-dia-c"}) {
		t.Fatalf("dia d after=%v", steps[3].After)
	}
	// 저널 순서 ≠ 위상 순서: 독립 step x,y는 x,y 순으로 spawn되지만, 이후
	// 선언된 dependency(x after y)가 투영 순서를 [y,x]로 뒤집어야 한다 —
	// assembly.Run이 이미 TopoOrder로 spawn하므로 이 경우만이 "spawn 조회순"
	// 구현과 TopoOrder 구현을 가른다.
	defineProc(t, s, "proc-ind", procedure.Step{ID: "x", Action: "x"}, procedure.Step{ID: "y", Action: "y"})
	runProc(t, srv, "proc-ind", "ind")
	if _, err := (edge.Service{Store: s}).Create(edge.Edge{ID: "e-late", From: edge.Endpoint{Type: "mission", ID: "mission-ind-x"}, To: edge.Endpoint{Type: "mission", ID: "mission-ind-y"}, Kind: edge.Dependency, Actor: "t", Correlation: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, _, steps := contextSteps(t, h, "mission-ind"); !eq(stepIDs(steps), []string{"mission-ind-y", "mission-ind-x"}) {
		t.Fatalf("ind order: %v (journal order must not win over topo order)", stepIDs(steps))
	}
}

// S3 — gates 매칭·정렬·상태 반영 (변이 probe c).
func TestContextStepsGatesFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-lin", procedure.Step{ID: "a", Action: "a"}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}}, procedure.Step{ID: "c", Action: "c", After: []string{"b"}})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-lin", "r1")
	ask := func(name, missionID string) {
		if out := postIntent057(t, srv, map[string]any{"kind": "question.ask", "name": name, "body": "body " + name, "recommendation": "approve", "missionId": missionID, "actor": "tester"}); out["Accepted"] != true {
			t.Fatalf("ask %s: %v", name, out)
		}
	}
	ask("b-two", "mission-r1-b")
	ask("b-one", "mission-r1-b")
	ask("a-only", "mission-r1-a")
	ask("unrelated", "mission-x")
	h := NewHTTP(s).Handler()
	_, body, steps := contextSteps(t, h, "mission-r1")
	if strings.Contains(string(body), "unrelated") {
		t.Fatal("unrelated mission's question leaked into steps")
	}
	if len(steps[0].Gates) != 1 || steps[0].Gates[0].Name != "a-only" || steps[0].Gates[0].State != "pending" {
		t.Fatalf("step a gates=%v", steps[0].Gates)
	}
	if len(steps[1].Gates) != 2 || steps[1].Gates[0].ID >= steps[1].Gates[1].ID {
		t.Fatalf("step b gates=%v (want 2, id-sorted)", steps[1].Gates)
	}
	if len(steps[2].Gates) != 0 {
		t.Fatalf("step c gates=%v", steps[2].Gates)
	}
	// approve / reject via the relay, digest from the workspace projection.
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for _, g := range p.Gates {
		digests[g.ID] = g.RequestDigest
	}
	g0, g1 := steps[1].Gates[0], steps[1].Gates[1]
	if out := postIntent057(t, srv, map[string]any{"kind": "gate.approve", "gateId": g0.ID, "digest": digests[g0.ID], "actor": "test-operator"}); out["Accepted"] != true {
		t.Fatalf("approve: %v", out)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "gate.reject", "gateId": g1.ID, "digest": digests[g1.ID], "reason": "no", "actor": "test-operator"}); out["Accepted"] != true {
		t.Fatalf("reject: %v", out)
	}
	_, _, steps = contextSteps(t, h, "mission-r1")
	if steps[1].Gates[0].State != "approved" || steps[1].Gates[1].State != "rejected" || steps[0].Gates[0].State != "pending" {
		t.Fatalf("after decisions: a=%v b=%v", steps[0].Gates, steps[1].Gates)
	}
}

// S4 — 일반 mission은 steps:[] 이고 마지막 키(additive 바이트 증거).
func TestContextStepsEmptyAdditiveFRRHZ097(t *testing.T) {
	s := fixture064(t)
	code, b := getContext(t, NewHTTP(s).Handler(), "?task=mission-x")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !bytes.HasSuffix(b, []byte(`,"steps":[]}`+"\n")) {
		t.Fatalf("steps must be the trailing key: %s", b)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	want := []string{"goal", "knowledge", "memories", "relations", "steps", "task"}
	got := make([]string, 0, len(top))
	for k := range top {
		got = append(got, k)
	}
	sort.Strings(got)
	if !eq(got, want) {
		t.Fatalf("keys=%v want %v", got, want)
	}
}

// S5 — 결정론·저널 재생 왕복 (gate 혼합 상태).
func TestContextStepsDeterministicFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-lin", procedure.Step{ID: "a", Action: "a"}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-lin", "r1")
	for _, n := range []string{"q2", "q1"} {
		postIntent057(t, srv, map[string]any{"kind": "question.ask", "name": n, "body": n, "recommendation": "ok", "missionId": "mission-r1-b", "actor": "tester"})
	}
	h := NewHTTP(s).Handler()
	_, b1, _ := contextSteps(t, h, "mission-r1")
	_, b2, _ := contextSteps(t, h, "mission-r1")
	if !bytes.Equal(b1, b2) {
		t.Fatal("same journal, different bytes")
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	_, b3, _ := contextSteps(t, NewHTTP(replayed).Handler(), "mission-r1")
	if !bytes.Equal(b1, b3) {
		t.Fatal("replayed store produced different bytes")
	}
	if !strings.Contains(string(b1), `"steps":[{"id":"mission-r1-a"`) {
		t.Fatalf("steps missing: %s", b1)
	}
}

// S6 — 사이클·미해결 after(spawn 집합 밖의 실재 mission) → 500, 실패 경로 쓰기 0.
// edge.Service가 endpoint 실재를 검증하므로 "유령" mission은 저널에 존재할 수 없다.
func TestContextStepsCycleAndUnresolvedFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-lin", procedure.Step{ID: "a", Action: "a"}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}}, procedure.Step{ID: "c", Action: "c", After: []string{"b"}})
	defineProc(t, s, "proc-one", procedure.Step{ID: "a", Action: "a"})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-lin", "r1")
	runProc(t, srv, "proc-one", "r3")
	es := edge.Service{Store: s}
	if _, err := es.Create(edge.Edge{ID: "e-cycle", From: edge.Endpoint{Type: "mission", ID: "mission-r1-a"}, To: edge.Endpoint{Type: "mission", ID: "mission-r1-c"}, Kind: edge.Dependency, Actor: "t", Correlation: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.Create(edge.Edge{ID: "e-ghost", From: edge.Endpoint{Type: "mission", ID: "mission-r3-a"}, To: edge.Endpoint{Type: "mission", ID: "mission-x"}, Kind: edge.Dependency, Actor: "t", Correlation: "t"}); err != nil {
		t.Fatal(err)
	}
	h := NewHTTP(s).Handler()
	before := len(s.All())
	for _, id := range []string{"mission-r1", "mission-r3"} {
		code, b := getContext(t, h, "?task="+id)
		if code != http.StatusInternalServerError || !strings.Contains(string(b), "context assembly failed") {
			t.Fatalf("%s: code=%d body=%s", id, code, b)
		}
	}
	if len(s.All()) != before {
		t.Fatal("failure path wrote events")
	}
}

// S7 — spawn 필터: 들어오는 spawn·다른 kind는 step이 아니다 (변이 probe a).
func TestContextStepsSpawnFilterFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-lin", procedure.Step{ID: "a", Action: "a"}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}}, procedure.Step{ID: "c", Action: "c", After: []string{"b"}})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-lin", "r1")
	es := edge.Service{Store: s}
	for _, x := range []edge.Edge{
		{ID: "e-in-spawn", From: edge.Endpoint{Type: "mission", ID: "mission-x"}, To: edge.Endpoint{Type: "mission", ID: "mission-r1"}, Kind: edge.Spawn},
		{ID: "e-out-dep", From: edge.Endpoint{Type: "mission", ID: "mission-r1"}, To: edge.Endpoint{Type: "mission", ID: "mission-x"}, Kind: edge.Dependency},
	} {
		x.Actor, x.Correlation = "t", "t"
		if _, err := es.Create(x); err != nil {
			t.Fatal(err)
		}
	}
	code, _, steps := contextSteps(t, NewHTTP(s).Handler(), "mission-r1")
	if code != http.StatusOK || !eq(stepIDs(steps), []string{"mission-r1-a", "mission-r1-b", "mission-r1-c"}) {
		t.Fatalf("code=%d steps=%v", code, stepIDs(steps))
	}
}

// S8 — 순수 투영: 조회로 저널이 변하지 않는다.
func TestContextStepsNoWriteFRRHZ097(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-lin", procedure.Step{ID: "a", Action: "a"}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-lin", "r1")
	before := len(s.All())
	if code, _, _ := contextSteps(t, NewHTTP(s).Handler(), "mission-r1"); code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if len(s.All()) != before {
		t.Fatal("read path appended events")
	}
}
