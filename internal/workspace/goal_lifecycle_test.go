package workspace

// RHZ-061 FR-RHZ-090: goal state emit + 운영자 생명주기 intent(goal.resolve/
// fail/cancel — append-only terminal, 하드 삭제 아님). 테스트 계획
// RHZ-061 W1~W10.

import (
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
)

func fixture061(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-g", "issue goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-ms", "goal-g", "issue mission", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func relay061(t *testing.T, s *events.Store, kind, goalID string) RelayResult {
	t.Helper()
	res, err := RelayIntent(s, Intent{Kind: kind, GoalID: goalID}, "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func goalState061(t *testing.T, s *events.Store, id string) domain.GoalState {
	t.Helper()
	g, err := projector.ReplayGoal(s.List("goal", id))
	if err != nil {
		t.Fatal(err)
	}
	return g.State
}

func workspaceBody061(t *testing.T, s *events.Store) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/workspace %d", rec.Code)
	}
	b, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// W1 (수용 1, 변이 probe): goal state가 Mission 투영·DTO로 방출된다 — 생성
// 직후 active, resolve 후 achieved. 기존 필드(id/name/attention)는 불변.
func TestGoalStateEmittedFRRHZ090(t *testing.T) {
	s := fixture061(t)
	p, err := Snapshot(s)
	if err != nil || len(p.Missions) != 1 || p.Missions[0].State != "active" {
		t.Fatalf("missions %+v err=%v", p.Missions, err)
	}
	body := workspaceBody061(t, s)
	if !strings.Contains(body, `"state":"active"`) || !strings.Contains(body, `"id":"goal-g"`) || !strings.Contains(body, `"name":"issue goal"`) {
		t.Fatalf("dto shape: %s", body)
	}
	if res := relay061(t, s, "goal.resolve", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	if !strings.Contains(workspaceBody061(t, s), `"state":"achieved"`) {
		t.Fatal("resolved state not emitted")
	}
}

// W2 (수용 2, 변이 probe): goal.resolve = terminal decision 이벤트 정확 1건 —
// payload To/DecisionID, correlation, TerminalDecisionID 재생 일치.
func TestGoalResolveAppendsOneDecisionEventFRRHZ090(t *testing.T) {
	s := fixture061(t)
	n := len(s.All())
	if res := relay061(t, s, "goal.resolve", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("journal grew %d, want exactly 1", len(all)-n)
	}
	e := all[len(all)-1]
	if e.AggregateType != "goal" || e.AggregateID != "goal-g" || e.Type != "goal.transitioned" {
		t.Fatalf("event %+v", e)
	}
	var p struct{ To, DecisionID string }
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.To != "achieved" || p.DecisionID != "decision-goal.resolve-goal-g" {
		t.Fatalf("payload %s err=%v", e.Payload, err)
	}
	if e.CorrelationID != "relay:tester" {
		t.Fatalf("correlation %q", e.CorrelationID)
	}
	g, err := projector.ReplayGoal(s.List("goal", "goal-g"))
	if err != nil || g.State != domain.GoalAchieved || g.TerminalDecisionID != p.DecisionID {
		t.Fatalf("replayed %+v err=%v", g, err)
	}
}

// W2 보강: 미검증 actor는 correlation에 기존 프리픽스 규약 적용 후 기록.
func TestGoalResolveUnverifiedActorCorrelationFRRHZ090(t *testing.T) {
	s := fixture061(t)
	res, err := RelayIntent(s, Intent{Kind: "goal.resolve", GoalID: "goal-g"}, "op", false)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	all := s.All()
	if got := all[len(all)-1].CorrelationID; got != "relay:unverified-local-operator:op" {
		t.Fatalf("correlation %q", got)
	}
}

// W3: 재제출 멱등 — 결정론 decisionID 덕에 이벤트 0 추가로 Accepted.
func TestGoalResolveIdempotentFRRHZ090(t *testing.T) {
	s := fixture061(t)
	if res := relay061(t, s, "goal.resolve", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	before, _ := json.Marshal(s.All())
	if res := relay061(t, s, "goal.resolve", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("resubmit appended events")
	}
}

// W4: goal.fail 동형.
func TestGoalFailFRRHZ090(t *testing.T) {
	s := fixture061(t)
	if res := relay061(t, s, "goal.fail", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	all := s.All()
	var p struct{ To, DecisionID string }
	if json.Unmarshal(all[len(all)-1].Payload, &p) != nil || p.To != "failed" || p.DecisionID != "decision-goal.fail-goal-g" {
		t.Fatalf("payload %+v", p)
	}
	if goalState061(t, s, "goal-g") != domain.GoalFailed {
		t.Fatal("state not failed")
	}
}

// W5 (수용 3): goal.cancel = 잔재 sweep — 전이 이벤트 1건 추가, 기존 이벤트
// 바이트 불변(하드 삭제 아님), 다른 goal 무영향.
func TestGoalCancelAppendOnlySweepFRRHZ090(t *testing.T) {
	s := fixture061(t)
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-probe-x", "probe leftover", "done", ""); err != nil {
		t.Fatal(err)
	}
	beforeAll := s.All()
	before, _ := json.Marshal(beforeAll)
	if res := relay061(t, s, "goal.cancel", "goal-probe-x"); !res.Accepted {
		t.Fatal(res)
	}
	all := s.All()
	if len(all) != len(beforeAll)+1 {
		t.Fatalf("grew %d, want 1", len(all)-len(beforeAll))
	}
	prefix, _ := json.Marshal(all[:len(beforeAll)])
	if string(prefix) != string(before) {
		t.Fatal("existing events mutated — not append-only")
	}
	last := all[len(all)-1]
	var p struct{ To, DecisionID string }
	if json.Unmarshal(last.Payload, &p) != nil || p.To != "cancelled" || p.DecisionID != "" {
		t.Fatalf("payload %s", last.Payload)
	}
	if goalState061(t, s, "goal-probe-x") != domain.GoalCancelled {
		t.Fatal("probe goal not cancelled")
	}
	if goalState061(t, s, "goal-g") != domain.GoalActive {
		t.Fatal("unrelated goal affected")
	}
}

// W6 (수용 4 + 발견 1건 핀): terminal 상태에서의 모든 전이 시도는 거부 +
// 저널 불변 + **직후 /v1/workspace 200** — 마지막이 poison 미발생의 증거.
// core 가드 제거 변이 시 cancelled×resolve 케이스가 workspace 500으로 FAIL.
func TestTerminalTransitionsRejectedNoPoisonFRRHZ090(t *testing.T) {
	terminalBy := map[string]func(t *testing.T, s *events.Store){
		"cancelled": func(t *testing.T, s *events.Store) {
			if res := relay061(t, s, "goal.cancel", "goal-g"); !res.Accepted {
				t.Fatal(res)
			}
		},
		"achieved": func(t *testing.T, s *events.Store) {
			if res := relay061(t, s, "goal.resolve", "goal-g"); !res.Accepted {
				t.Fatal(res)
			}
		},
		"failed": func(t *testing.T, s *events.Store) {
			if res := relay061(t, s, "goal.fail", "goal-g"); !res.Accepted {
				t.Fatal(res)
			}
		},
	}
	// achieved/failed에 같은 동사 재시도는 멱등(W3)이므로 "다른 동사"만 위반.
	attempts := map[string][]string{
		"cancelled": {"goal.resolve", "goal.fail", "goal.cancel"},
		"achieved":  {"goal.fail", "goal.cancel"},
		"failed":    {"goal.resolve", "goal.cancel"},
	}
	for state, setup := range terminalBy {
		for _, kind := range attempts[state] {
			s := fixture061(t)
			setup(t, s)
			before, _ := json.Marshal(s.All())
			res := relay061(t, s, kind, "goal-g")
			if res.Accepted {
				t.Fatalf("%s on %s goal accepted", kind, state)
			}
			after, _ := json.Marshal(s.All())
			if string(before) != string(after) {
				t.Fatalf("%s on %s: journal changed", kind, state)
			}
			// poison 미발생의 진짜 증거: 전체 투영이 여전히 서빙된다.
			if body := workspaceBody061(t, s); !strings.Contains(body, `"state":"`+state+`"`) {
				t.Fatalf("%s on %s: workspace lost the terminal state: %s", kind, state, body)
			}
		}
	}
}

// W7: 미존재·무입력 거부, 저널 불변.
func TestGoalIntentUnknownTargetRejectedFRRHZ090(t *testing.T) {
	s := fixture061(t)
	before, _ := json.Marshal(s.All())
	for _, in := range []Intent{
		{Kind: "goal.resolve", GoalID: "goal-ghost"},
		{Kind: "goal.cancel", GoalID: "goal-ghost"},
		{Kind: "goal.resolve"},
		{Kind: "goal.cancel"},
	} {
		res, err := RelayIntent(s, in, "tester", true)
		if err != nil || res.Accepted {
			t.Fatalf("%+v: res=%+v err=%v", in, res, err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejected intents changed journal")
	}
}

// W8 (수용 4): 재생 재계산 — resolve·cancel 후 저널 재생본의 상태 동일.
func TestGoalLifecycleRecomputableFRRHZ090(t *testing.T) {
	s := fixture061(t)
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-probe-x", "probe", "done", ""); err != nil {
		t.Fatal(err)
	}
	if res := relay061(t, s, "goal.resolve", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	if res := relay061(t, s, "goal.cancel", "goal-probe-x"); !res.Accepted {
		t.Fatal(res)
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	if goalState061(t, replayed, "goal-g") != domain.GoalAchieved || goalState061(t, replayed, "goal-probe-x") != domain.GoalCancelled {
		t.Fatal("replay diverged")
	}
	if workspaceBody061(t, s) != workspaceBody061(t, replayed) {
		t.Fatal("projection diverged after replay")
	}
}

// W9: HTTP 왕복 — POST /v1/intent(goal.resolve, goalId 와이어) → workspace에
// achieved 반영.
func TestGoalResolveHTTPFRRHZ090(t *testing.T) {
	s := fixture061(t)
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	out := postIntent057(t, srv, map[string]any{"kind": "goal.resolve", "goalId": "goal-g"})
	if out["Accepted"] != true {
		t.Fatalf("intent rejected: %v", out)
	}
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"state":"achieved"`) {
		t.Fatalf("achieved not visible: %s", b)
	}
}

// W10: cancelled goal에도 note.create about 부착은 성공 — terminal은 참조
// 불능이 아니다(하드 삭제 아님의 실질, RHZ-057 상호작용).
func TestCancelledGoalStillReferencableFRRHZ090(t *testing.T) {
	s := fixture061(t)
	if res := relay061(t, s, "goal.cancel", "goal-g"); !res.Accepted {
		t.Fatal(res)
	}
	res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "postmortem on goal-g", MemoryKind: "observation", GoalID: "goal-g"}, "tester", true)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	found := false
	for _, e := range s.All() {
		if e.AggregateType == "edge" {
			found = true
		}
	}
	if !found {
		t.Fatal("about edge to cancelled goal missing")
	}
}
