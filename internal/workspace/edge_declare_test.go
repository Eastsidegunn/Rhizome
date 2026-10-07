package workspace

// RHZ-066 FR-RHZ-095: edge.declare — contains(goal→goal) 선언 경로. 테스트
// 계획 RHZ-066 D1~D8 (① 전이
// 사이클 포함+D5 과차단 방지, ②a superseded는 사이클 검사 제외, ②b 멱등은
// live-기준 — superseded ID 재선언은 명시 거부).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/mission"
)

func goals066(t *testing.T, ids ...string) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	for _, id := range ids {
		if _, err := ms.CreateGoal(id, "goal "+id, "done", ""); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func declare066(t *testing.T, s *events.Store, from, to string) RelayResult {
	t.Helper()
	res, err := RelayIntent(s, Intent{Kind: "edge.declare", From: "goal:" + from, To: "goal:" + to}, "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func journal066(t *testing.T, s *events.Store) string {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// D1 (변이 probe): 정상 선언 — edge.declared 정확 1건, 결정론 ID, contains
// 모양, 재생 왕복. edgeKind 생략·명시 양쪽 수용.
func TestEdgeDeclareOneEventFRRHZ095(t *testing.T) {
	s := goals066(t, "g-parent", "g-child", "g-other")
	n := len(s.All())
	if res := declare066(t, s, "g-parent", "g-child"); !res.Accepted {
		t.Fatal(res)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("grew %d, want 1", len(all)-n)
	}
	e := all[len(all)-1]
	if e.AggregateType != "edge" || e.Type != "edge.declared" || e.AggregateID != "edge-contains-g-parent-g-child" {
		t.Fatalf("event %+v", e)
	}
	got, err := (edge.Service{Store: s}).Get("edge-contains-g-parent-g-child")
	if err != nil || got.Kind != edge.Contains || got.From != (edge.Endpoint{Type: "goal", ID: "g-parent"}) || got.To != (edge.Endpoint{Type: "goal", ID: "g-child"}) {
		t.Fatalf("replay %+v err=%v", got, err)
	}
	// edgeKind "contains" 명시도 수용.
	res, err := RelayIntent(s, Intent{Kind: "edge.declare", From: "goal:g-parent", To: "goal:g-other", EdgeKind: "contains"}, "tester", true)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
}

// D2 (지정 ②b): 멱등은 live-기준 — live 재선언 무이벤트 Accepted, superseded
// ID 재선언은 명시 거부(거짓 Accept 금지).
func TestEdgeDeclareIdempotentLiveOnlyFRRHZ095(t *testing.T) {
	s := goals066(t, "g1", "g2", "g3")
	if res := declare066(t, s, "g1", "g2"); !res.Accepted {
		t.Fatal(res)
	}
	before := journal066(t, s)
	if res := declare066(t, s, "g1", "g2"); !res.Accepted {
		t.Fatal("live redeclare rejected")
	}
	if journal066(t, s) != before {
		t.Fatal("live redeclare appended")
	}
	// rewire로 치워진 뒤 재선언 → "out of scope" 명시 거부.
	if _, err := (edge.Service{Store: s}).Rewire("edge-contains-g1-g2", edge.Edge{ID: "edge-contains-g1-g3", From: edge.Endpoint{Type: "goal", ID: "g1"}, To: edge.Endpoint{Type: "goal", ID: "g3"}, Kind: edge.Contains, Actor: "tester", Correlation: "test", Verified: true}); err != nil {
		t.Fatal(err)
	}
	mid := journal066(t, s)
	res := declare066(t, s, "g1", "g2")
	if res.Accepted {
		t.Fatal("superseded-id redeclare falsely accepted")
	}
	if !strings.Contains(res.Reason, "out of scope") {
		t.Fatalf("reason %q", res.Reason)
	}
	if journal066(t, s) != mid {
		t.Fatal("rejection changed journal")
	}
}

// D3: 커널 위임 거부 — ghost goal·자기참조·비-goal 끝점, 전부 저널 불변이고
// Reason이 커널 오류를 그대로 전달(relay 중복 검증 없음).
func TestEdgeDeclareKernelDelegationFRRHZ095(t *testing.T) {
	s := goals066(t, "g1")
	before := journal066(t, s)
	for _, in := range []Intent{
		{Kind: "edge.declare", From: "goal:g-ghost", To: "goal:g1"},
		{Kind: "edge.declare", From: "goal:g1", To: "goal:g-ghost"},
		{Kind: "edge.declare", From: "goal:g1", To: "goal:g1"}, // 자기참조 — 커널 valid 소유
		{Kind: "edge.declare", From: "mission:m", To: "goal:g1"},
		{Kind: "edge.declare", From: "goal:", To: "goal:g1"},
	} {
		res, err := RelayIntent(s, in, "tester", true)
		if err != nil || res.Accepted {
			t.Fatalf("%+v: %+v err=%v", in, res, err)
		}
	}
	if journal066(t, s) != before {
		t.Fatal("rejections changed journal")
	}
}

// D4 (지정 ②a): 직접 역방향(2-사이클) 거부; superseded 역방향은 차단 근거가
// 아니다.
func TestEdgeDeclareReverseRejectedFRRHZ095(t *testing.T) {
	s := goals066(t, "g1", "g2", "g3")
	if res := declare066(t, s, "g1", "g2"); !res.Accepted {
		t.Fatal(res)
	}
	before := journal066(t, s)
	res := declare066(t, s, "g2", "g1")
	if res.Accepted || !strings.Contains(res.Reason, "cycle") {
		t.Fatalf("reverse accepted or wrong reason: %+v", res)
	}
	if journal066(t, s) != before {
		t.Fatal("rejection changed journal")
	}
	// g1⊃g2를 rewire로 치우면(→g1⊃g3) 과거 엣지는 live가 아니므로 g2⊃g1 수용.
	if _, err := (edge.Service{Store: s}).Rewire("edge-contains-g1-g2", edge.Edge{ID: "edge-contains-g1-g3", From: edge.Endpoint{Type: "goal", ID: "g1"}, To: edge.Endpoint{Type: "goal", ID: "g3"}, Kind: edge.Contains, Actor: "tester", Correlation: "test", Verified: true}); err != nil {
		t.Fatal(err)
	}
	if res := declare066(t, s, "g2", "g1"); !res.Accepted {
		t.Fatalf("ghost edge blocked a live declaration: %+v", res)
	}
}

// D5 (지정 ① + 과차단 방지): 전이 사이클 거부, 공유 부모(다이아몬드)는 수용.
func TestEdgeDeclareTransitiveCycleFRRHZ095(t *testing.T) {
	s := goals066(t, "g1", "g2", "g3")
	for _, pair := range [][2]string{{"g1", "g2"}, {"g2", "g3"}} {
		if res := declare066(t, s, pair[0], pair[1]); !res.Accepted {
			t.Fatal(res)
		}
	}
	before := journal066(t, s)
	res := declare066(t, s, "g3", "g1")
	if res.Accepted || !strings.Contains(res.Reason, "cycle") {
		t.Fatalf("transitive cycle accepted: %+v", res)
	}
	if journal066(t, s) != before {
		t.Fatal("rejection changed journal")
	}
	// 공유 부모: g3도 g2를 품는 건 사이클이 아니다? (g2⊃g3 존재 — g3⊃g2가
	// 아니라) g1⊃g3: g3에서 g1 도달 불가 → 수용(다중 부모 허용).
	if res := declare066(t, s, "g1", "g3"); !res.Accepted {
		t.Fatalf("diamond over-blocked: %+v", res)
	}
}

// D6: 프로세스 엣지 위조 — edgeKind 전달됐으나 contains 아님 → 명시 거부.
func TestEdgeDeclareContainsOnlyFRRHZ095(t *testing.T) {
	s := goals066(t, "g1", "g2")
	before := journal066(t, s)
	for _, kind := range []string{"spawn", "dependency", "about", "gate", "produces"} {
		res, err := RelayIntent(s, Intent{Kind: "edge.declare", From: "goal:g1", To: "goal:g2", EdgeKind: kind}, "tester", true)
		if err != nil || res.Accepted || !strings.Contains(res.Reason, "contains only") {
			t.Fatalf("%s: %+v err=%v", kind, res, err)
		}
	}
	if journal066(t, s) != before {
		t.Fatal("forgeries changed journal")
	}
}

// D7: HTTP 왕복 + workspace 투영 노출.
func TestEdgeDeclareHTTPFRRHZ095(t *testing.T) {
	s := goals066(t, "g-domain", "g-issue")
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	if out := postIntent057(t, srv, map[string]any{"kind": "edge.declare", "from": "goal:g-domain", "to": "goal:g-issue"}); out["Accepted"] != true {
		t.Fatalf("declare rejected: %v", out)
	}
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"edgeKind":"contains"`) || !strings.Contains(string(b), `"id":"edge-contains-g-domain-g-issue"`) {
		t.Fatalf("contains edge missing from projection: %s", b)
	}
}

// D8: 재생 재계산.
func TestEdgeDeclareRecomputableFRRHZ095(t *testing.T) {
	s := goals066(t, "g1", "g2")
	if res := declare066(t, s, "g1", "g2"); !res.Accepted {
		t.Fatal(res)
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	a, err := (edge.Service{Store: s}).Get("edge-contains-g1-g2")
	if err != nil {
		t.Fatal(err)
	}
	b, err := (edge.Service{Store: replayed}).Get("edge-contains-g1-g2")
	if err != nil || a != b {
		t.Fatalf("replay diverged: %+v vs %+v err=%v", a, b, err)
	}
}
