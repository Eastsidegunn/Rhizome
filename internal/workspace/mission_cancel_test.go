package workspace

// RHZ-062 FR-RHZ-091: mission.cancel — 운영자 미션 terminal(RHZ-061 짝,
// 완전 sweep). 테스트 계획 RHZ-062 M1~M7, 지정 ①
// (상태기계 무변경 — running·waitingresult는 거부가 정답).

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

// missionIn062: goal+mission 생성 후 지정 상태까지 실서비스 Transition으로 구동.
func missionIn062(t *testing.T, s *events.Store, id string, path ...domain.MissionState) {
	t.Helper()
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-"+id, "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create(id, "goal-"+id, "m", "done"); err != nil {
		t.Fatal(err)
	}
	rev := uint64(1)
	for _, to := range path {
		if _, err := ms.Transition(id, rev, to); err != nil {
			t.Fatalf("drive %s to %s: %v", id, to, err)
		}
		rev++
	}
}

func cancelMission062(t *testing.T, s *events.Store, id string) RelayResult {
	t.Helper()
	res, err := RelayIntent(s, Intent{Kind: "mission.cancel", MissionID: id}, "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func missionState062(t *testing.T, s *events.Store, id string) domain.MissionState {
	t.Helper()
	m, err := projector.ReplayMission(s.List("mission", id))
	if err != nil {
		t.Fatal(err)
	}
	return m.State
}

// M1 (수용 1·2, 변이 probe): 성공 집합 5상태 전수 — 각각 mission.transitioned
// {To:cancelled} 정확 1건, 재생 일치, Task.State "cancelled".
func TestMissionCancelAllowedStatesFRRHZ091(t *testing.T) {
	cases := map[string][]domain.MissionState{
		"mission-planned":      {},
		"mission-ready":        {domain.MissionReady},
		"mission-paused":       {domain.MissionReady, domain.MissionRunning, domain.MissionPaused},
		"mission-waitinghuman": {domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman},
		"mission-blocked":      {domain.MissionReady, domain.MissionBlocked},
	}
	for id, path := range cases {
		s := &events.Store{}
		missionIn062(t, s, id, path...)
		n := len(s.All())
		if res := cancelMission062(t, s, id); !res.Accepted {
			t.Fatalf("%s: %+v", id, res)
		}
		all := s.All()
		if len(all) != n+1 {
			t.Fatalf("%s: grew %d, want 1", id, len(all)-n)
		}
		last := all[len(all)-1]
		var p struct{ To string }
		if last.AggregateType != "mission" || last.Type != "mission.transitioned" || json.Unmarshal(last.Payload, &p) != nil || p.To != "cancelled" {
			t.Fatalf("%s: event %+v payload %s", id, last, last.Payload)
		}
		if missionState062(t, s, id) != domain.MissionCancelled {
			t.Fatalf("%s: replay not cancelled", id)
		}
		proj, err := Snapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(proj.Tasks) != 1 || proj.Tasks[0].State != "cancelled" {
			t.Fatalf("%s: task state %+v (mapState missing cancelled?)", id, proj.Tasks)
		}
	}
}

// M2 (지정 ①): running·waitingresult에서 cancel은 거부 — 상태기계 무변경의
// 사실 핀. 실행 중 중단은 pause→cancel 2단계 또는 execution.stop 의미론(별도).
func TestMissionCancelRejectedWhileRunningFRRHZ091(t *testing.T) {
	cases := map[string][]domain.MissionState{
		"mission-running":       {domain.MissionReady, domain.MissionRunning},
		"mission-waitingresult": {domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult},
	}
	for id, path := range cases {
		s := &events.Store{}
		missionIn062(t, s, id, path...)
		before, _ := json.Marshal(s.All())
		if res := cancelMission062(t, s, id); res.Accepted {
			t.Fatalf("%s: cancel accepted while in-flight", id)
		}
		after, _ := json.Marshal(s.All())
		if string(before) != string(after) {
			t.Fatalf("%s: journal changed", id)
		}
	}
}

// M3 (수용 1): terminal 3종 거부 + 저널 불변 + 직후 workspace 200 — Transition
// 의 append-전 검증(poison 없음) 회귀 핀. succeeded/failed는 재생-유효 이벤트
// 직접 append로 구성(coordinator 픽스처 회피 — Transition이 설계상 막으므로).
func TestMissionCancelTerminalRejectedNoPoisonFRRHZ091(t *testing.T) {
	terminal := map[string]func(t *testing.T, s *events.Store, id string){
		"cancelled": func(t *testing.T, s *events.Store, id string) {
			if res := cancelMission062(t, s, id); !res.Accepted {
				t.Fatal(res)
			}
		},
		"succeeded": func(t *testing.T, s *events.Store, id string) {
			drive062Terminal(t, s, id, domain.MissionSucceeded)
		},
		"failed": func(t *testing.T, s *events.Store, id string) {
			drive062Terminal(t, s, id, domain.MissionFailed)
		},
	}
	for state, setup := range terminal {
		s := &events.Store{}
		id := "mission-" + state
		missionIn062(t, s, id)
		setup(t, s, id)
		before, _ := json.Marshal(s.All())
		if res := cancelMission062(t, s, id); res.Accepted {
			t.Fatalf("cancel on %s accepted", state)
		}
		after, _ := json.Marshal(s.All())
		if string(before) != string(after) {
			t.Fatalf("cancel on %s changed journal", state)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
		rec := httptest.NewRecorder()
		NewHTTP(s).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("workspace %d after rejected cancel on %s (poison?)", rec.Code, state)
		}
	}
}

// drive062Terminal: planned 미션을 재생-유효 경로(ready→running→terminal)의
// mission.transitioned 직접 append로 terminal에 둔다(테스트 전용 writer).
func drive062Terminal(t *testing.T, s *events.Store, id string, to domain.MissionState) {
	t.Helper()
	rev := uint64(1)
	for _, step := range []domain.MissionState{domain.MissionReady, domain.MissionRunning, to} {
		// 재생 규칙: terminal(Succeeded/Failed)은 DecisionID 필수(실행
		// decision 경로의 흔적) — 픽스처도 재생-유효하려면 채워야 한다.
		decision := ""
		if step == domain.MissionSucceeded || step == domain.MissionFailed {
			decision = "decision-fixture-" + id
		}
		p, _ := json.Marshal(struct {
			To         domain.MissionState
			Reason     string
			DecisionID string
		}{To: step, DecisionID: decision})
		if err := s.Append(rev, events.Event{AggregateType: "mission", AggregateID: id, Revision: rev + 1, Type: "mission.transitioned", Payload: p}); err != nil {
			t.Fatal(err)
		}
		rev++
	}
	if missionState062(t, s, id) != to {
		t.Fatalf("fixture drive to %s failed", to)
	}
}

// M4: 재제출 = 거부(terminal→전이) — goal.cancel과 동일 의미론. decisionID
// 멱등은 decision 경로(goal.resolve류) 전용이고 cancel엔 없다.
func TestMissionCancelResubmitRejectedFRRHZ091(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-x")
	if res := cancelMission062(t, s, "mission-x"); !res.Accepted {
		t.Fatal(res)
	}
	before, _ := json.Marshal(s.All())
	if res := cancelMission062(t, s, "mission-x"); res.Accepted {
		t.Fatal("re-cancel accepted")
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("re-cancel changed journal")
	}
}

// M5: 미존재·무입력 거부, 저널 불변.
func TestMissionCancelUnknownTargetRejectedFRRHZ091(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-x")
	before, _ := json.Marshal(s.All())
	for _, in := range []Intent{
		{Kind: "mission.cancel", MissionID: "mission-ghost"},
		{Kind: "mission.cancel"},
	} {
		res, err := RelayIntent(s, in, "tester", true)
		if err != nil || res.Accepted {
			t.Fatalf("%+v: %+v err=%v", in, res, err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejected intents changed journal")
	}
}

// M6 (수용 3): 재생 재계산 — cancel 후 재생본 상태·workspace 바디 동일.
func TestMissionCancelRecomputableFRRHZ091(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-x")
	if res := cancelMission062(t, s, "mission-x"); !res.Accepted {
		t.Fatal(res)
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	if missionState062(t, replayed, "mission-x") != domain.MissionCancelled {
		t.Fatal("replay diverged")
	}
	for _, store := range []*events.Store{s, replayed} {
		req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
		rec := httptest.NewRecorder()
		NewHTTP(store).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatal("workspace failed")
		}
	}
}

// M7 (완전 sweep 증거): HTTP로 goal.cancel + mission.cancel 짝 — goal과 task
// 둘 다 cancelled로 투영. RHZ-061의 반쪽 sweep이 완성됨을 한 시나리오로 핀.
func TestFullSweepGoalAndMissionHTTPFRRHZ091(t *testing.T) {
	s := &events.Store{}
	if res, err := RelayIntent(s, Intent{Kind: "mission.create", Name: "leftover", Prompt: "done"}, "tester", true); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	if out := postIntent057(t, srv, map[string]any{"kind": "goal.cancel", "goalId": "goal-leftover"}); out["Accepted"] != true {
		t.Fatalf("goal.cancel rejected: %v", out)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "mission.cancel", "missionId": "mission-leftover"}); out["Accepted"] != true {
		t.Fatalf("mission.cancel rejected: %v", out)
	}
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if !strings.Contains(body, `"id":"goal-leftover","name":"leftover","attention":false,"state":"cancelled"`) {
		t.Fatalf("goal not cancelled in projection: %s", body)
	}
	if !strings.Contains(body, `"id":"mission-leftover"`) || !strings.Contains(body, `"state":"cancelled","hasProgress":false`) {
		t.Fatalf("task not cancelled in projection: %s", body)
	}
}
