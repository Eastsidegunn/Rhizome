package workspace

// RHZ-064 FR-RHZ-093: GET /v1/context — 결정론 컨텍스트 번들 + UseTrace.
// 테스트 계획 RHZ-064 C1~C7 (C8은 archtest P4가
// 자동 커버 — 별도 테스트 없음).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/relation"
	"rhizome/internal/trace"
)

// fixture064: goal·mission + about 시드 memory 2개(mem-a→mission, mem-b→goal)
// + mem-a에서 파생된 확정 지식 2개 + supports 관계로 당겨지는 제3 지식.
func fixture064(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-g", "dev goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-x", "goal-g", "dev mission", "done"); err != nil {
		t.Fatal(err)
	}
	mems := memory.Service{Store: s}
	for _, id := range []string{"mem-a", "mem-b", "mem-c"} {
		if _, err := mems.Create(memory.Memory{ID: id, Kind: memory.Fact, Content: "content of " + id, SourceType: "note", SourceID: "n-" + id, Confidence: .9, Tags: []string{"dev"}}); err != nil {
			t.Fatal(err)
		}
	}
	ks := knowledge.Service{Store: s}
	for id, src := range map[string]string{"k-1": "mem-a", "k-2": "mem-a", "k-3": "mem-c"} {
		if _, err := ks.Create(knowledge.KnowledgeItem{ID: id, Kind: knowledge.Claim, Statement: "statement " + id, SourceMemoryID: src, Confidence: .8}); err != nil {
			t.Fatal(err)
		}
		if _, err := ks.Promote(id, "confirmed for test"); err != nil {
			t.Fatal(err)
		}
	}
	// k-3은 시드(mem-a) 지식이 아니지만 supports 관계로 당겨진다.
	if _, err := (relation.Service{Store: s}).Create(relation.Relation{ID: "rel-1", From: "k-1", To: "k-3", Type: relation.Supports, SourceMemoryIDs: []string{"mem-a"}, Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	es := edge.Service{Store: s}
	for id, target := range map[string]edge.Endpoint{"e-about-m": {Type: "mission", ID: "mission-x"}, "e-about-g": {Type: "goal", ID: "goal-g"}} {
		from := "mem-a"
		if id == "e-about-g" {
			from = "mem-b"
		}
		if _, err := es.Create(edge.Spec{ID: id, From: edge.Endpoint{Type: "memory", ID: from}, To: target, Kind: edge.About, Actor: "tester", Correlation: "test"}, noAuthority()); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func getContext(t *testing.T, h http.Handler, query string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/context"+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, b
}

func traceEvents064(s *events.Store) []events.Event {
	out := []events.Event{}
	for _, e := range s.All() {
		if e.AggregateType == "trace" {
			out = append(out, e)
		}
	}
	return out
}

// C1 (수용 1, 변이 probe): 3회 연속 조회 바이트 완전 동일 — 2·3회차엔
// trace가 이미 쌓여 있으므로 trace 자기-영향 차단까지 같은 단언이 핀한다.
func TestContextDeterministicBytesFRRHZ093(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	code, b1 := getContext(t, h, "?task=mission-x")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, b1)
	}
	for i := 0; i < 2; i++ {
		code, b := getContext(t, h, "?task=mission-x")
		if code != http.StatusOK || !bytes.Equal(b, b1) {
			t.Fatalf("requery %d: bytes differ (trace feedback?)", i)
		}
	}
	// 번들 내용: 시드 memory 2, 지식 3(k-3은 관계로), 관계 1, revision 없음.
	body := string(b1)
	for _, want := range []string{`"id":"mem-a"`, `"id":"mem-b"`, `"id":"k-1"`, `"id":"k-2"`, `"id":"k-3"`, `"id":"rel-1"`, `"state":"queued"`, `"state":"active"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("bundle missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"revision"`) {
		t.Fatal("bundle carries a revision (self-feedback hazard)")
	}
}

func TestContextRelationWireOrderFRRHZ093(t *testing.T) {
	s := fixture064(t)
	code, body := getContext(t, NewHTTP(s).Handler(), "?task=mission-x")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	want := []byte(`"relations":[{"id":"rel-1","type":"supports","from":"k-1","to":"k-3"}]`)
	if !bytes.Contains(body, want) {
		t.Fatalf("context relation wire changed\n got: %s\nwant fragment: %s", body, want)
	}
}

// C2 (수용 2, 변이 probe): UseTrace 정확 1건 — payload 순서까지 일치, trace
// 외 aggregate 증가 0, Replay 왕복.
func TestContextRecordsOneUseTraceFRRHZ093(t *testing.T) {
	s := fixture064(t)
	n := len(s.All())
	code, _ := getContext(t, NewHTTP(s).Handler(), "?task=mission-x")
	if code != http.StatusOK {
		t.Fatal("query failed")
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("journal grew %d, want exactly 1", len(all)-n)
	}
	e := all[len(all)-1]
	if e.AggregateType != "trace" || e.Type != "trace.recorded" {
		t.Fatalf("event %+v", e)
	}
	var p struct {
		ID        string   `json:"trace_id"`
		Query     string   `json:"query"`
		Retrieved []string `json:"retrieved_item_ids"`
		Traversed []string `json:"traversed_relation_ids"`
		GoalID    string   `json:"goal_id"`
		MissionID string   `json:"mission_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.ID != "trace-task-mission-x-1" || p.Query != "task:mission-x" || p.MissionID != "mission-x" || p.GoalID != "goal-g" {
		t.Fatalf("payload %+v", p)
	}
	if fmt.Sprint(p.Retrieved) != "[k-1 k-2 k-3]" || fmt.Sprint(p.Traversed) != "[rel-1]" {
		t.Fatalf("retrieved=%v traversed=%v", p.Retrieved, p.Traversed)
	}
	got, err := trace.Replay(s.List("trace", p.ID))
	if err != nil || got.MissionID != "mission-x" || len(got.RetrievedItemIDs) != 3 {
		t.Fatalf("replay %+v err=%v", got, err)
	}
	for _, e := range all[n:] {
		if e.AggregateType != "trace" {
			t.Fatalf("non-trace event appended: %s/%s", e.AggregateType, e.Type)
		}
	}
}

// C3: 재조회마다 trace 1건씩 — 상태-결정론 ID가 유일하게 증가.
func TestContextTraceIDsUniqueFRRHZ093(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	for i := 1; i <= 3; i++ {
		if code, _ := getContext(t, h, "?task=mission-x"); code != http.StatusOK {
			t.Fatal("query failed")
		}
	}
	evs := traceEvents064(s)
	if len(evs) != 3 {
		t.Fatalf("traces %d, want 3", len(evs))
	}
	for i, e := range evs {
		want := fmt.Sprintf("trace-task-mission-x-%d", i+1)
		if e.AggregateID != want {
			t.Fatalf("trace %d id %q want %q", i, e.AggregateID, want)
		}
		if _, err := trace.Replay(s.List("trace", e.AggregateID)); err != nil {
			t.Fatal(err)
		}
	}
}

// C4: 지식 0 조회 = 200 빈 번들 + trace 생략·저널 완전 불변
// (trace 스키마의 RetrievedItemIDs 필수 제약의 귀결).
func TestContextEmptyResultNoTraceFRRHZ093(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-empty", "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-empty", "goal-empty", "m", "done"); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	code, b := getContext(t, NewHTTP(s).Handler(), "?task=mission-empty")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`"memories":[]`, `"knowledge":[]`, `"relations":[]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("empty bundle shape: %s", b)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("empty query wrote to journal")
	}
}

// C5: 오류 경로 — 미존재 404·파람 없음 400·비GET 404, 전부 저널 불변.
func TestContextErrorPathsFRRHZ093(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	before, _ := json.Marshal(s.All())
	if code, _ := getContext(t, h, "?task=mission-ghost"); code != http.StatusNotFound {
		t.Fatalf("ghost: %d", code)
	}
	if code, _ := getContext(t, h, ""); code != http.StatusBadRequest {
		t.Fatalf("no param: %d", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/context?task=mission-x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST: %d", rec.Code)
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("error paths wrote to journal")
	}
}

// C6: 동시 조회 8개 — 전부 200·동일 바이트, trace 정확 8건·ID 유일
// (뮤텍스 임계구역; -race는 make ci의 test-race가 핀).
func TestContextConcurrentQueriesFRRHZ093(t *testing.T) {
	s := fixture064(t)
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	bodies := make([][]byte, 8)
	codes := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Get(srv.URL + "/v1/context?task=mission-x")
			if err != nil {
				return
			}
			bodies[i], _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range codes {
		if codes[i] != http.StatusOK || !bytes.Equal(bodies[i], bodies[0]) {
			t.Fatalf("request %d: code=%d equal=%v", i, codes[i], bytes.Equal(bodies[i], bodies[0]))
		}
	}
	evs := traceEvents064(s)
	ids := map[string]bool{}
	for _, e := range evs {
		ids[e.AggregateID] = true
	}
	if len(evs) != 8 || len(ids) != 8 {
		t.Fatalf("traces=%d unique=%d, want 8/8", len(evs), len(ids))
	}
}

// C7: 재생 재계산 — 재생본 store에 같은 조회 = 같은 바이트.
func TestContextRecomputableFRRHZ093(t *testing.T) {
	s := fixture064(t)
	code, b1 := getContext(t, NewHTTP(s).Handler(), "?task=mission-x")
	if code != http.StatusOK {
		t.Fatal("query failed")
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	code, b2 := getContext(t, NewHTTP(replayed).Handler(), "?task=mission-x")
	if code != http.StatusOK || !bytes.Equal(b1, b2) {
		t.Fatal("replayed store produced different bytes")
	}
}
