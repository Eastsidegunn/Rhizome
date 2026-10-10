package workspace

// RHZ-067 FR-RHZ-096: /v1/workspace goal 투영에 success 추가(additive, 순수
// 투영). 테스트 계획 RHZ-067 G1~G4 (G5는 전체
// 스위트 green이 증거 — 별도 테스트 없음). 판정: description은 생략 —
// domain.Goal에 Name 필드가 없고 투영 name==g.Description이라 모든 경로에서
// 바이트 중복(G2 키 집합이 판정을 고정).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/mission"
)

func fixture067(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-g", "dev goal", "ci green + review pass", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-m", "goal-g", "dev mission", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func workspaceBody067(t *testing.T, s *events.Store) []byte {
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
	return b
}

// G1 (변이 probe): goal.Success가 Snapshot과 wire에 그대로 노출.
func TestGoalSuccessExposedFRRHZ096(t *testing.T) {
	s := fixture067(t)
	p, err := Snapshot(s)
	if err != nil || len(p.Missions) != 1 || p.Missions[0].Success != "ci green + review pass" {
		t.Fatalf("missions %+v err=%v", p.Missions, err)
	}
	body := string(workspaceBody067(t, s))
	if !strings.Contains(body, `"success":"ci green + review pass"`) {
		t.Fatalf("success missing from wire: %s", body)
	}
	for _, want := range []string{`"id":"goal-g"`, `"name":"dev goal"`, `"attention":false`, `"state":"active"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("existing field missing: %s", want)
		}
	}
}

// G2 (additive + 판정 고정): missions[0] 키 집합이 정확히
// {id, name, attention, state, success, handle} — description 등 다른 신규
// 키 없음 (handle은 RHZ-073/FR-RHZ-103 additive, 항상 존재).
// 제로값 missionDTO 직렬화에 success 키 부재 = omitempty 와이어 불변 증거.
func TestMissionDTOKeySetFRRHZ096(t *testing.T) {
	s := fixture067(t)
	var env struct {
		Body struct {
			Missions []map[string]any `json:"missions"`
		} `json:"body"`
	}
	if err := json.Unmarshal(workspaceBody067(t, s), &env); err != nil || len(env.Body.Missions) != 1 {
		t.Fatalf("decode: %v", err)
	}
	keys := env.Body.Missions[0]
	// RHZ-133 (FR-RHZ-173) adds changedAtRevision, lastActivityTs and steps
	// (the fixture goal has one task).
	for _, want := range []string{"id", "name", "attention", "state", "success", "handle", "changedAtRevision", "lastActivityTs", "steps"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("key %s missing", want)
		}
	}
	if len(keys) != 9 {
		t.Fatalf("unexpected keys: %v", keys)
	}
	zero, err := json.Marshal(missionDTO{})
	if err != nil || strings.Contains(string(zero), "success") || strings.Contains(string(zero), "changedAtRevision") || strings.Contains(string(zero), "steps") {
		t.Fatalf("omitempty broken: %s err=%v", zero, err)
	}
}

// G3: 순수 투영 — GET 전후 저널 완전 불변(신규 이벤트 0).
func TestGoalSuccessPureProjectionFRRHZ096(t *testing.T) {
	s := fixture067(t)
	before, _ := json.Marshal(s.All())
	workspaceBody067(t, s)
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("projection wrote to journal")
	}
}

// G4: 재생 왕복 — 재생본에서 같은 GET 바이트 동일(success 포함).
func TestGoalSuccessRecomputableFRRHZ096(t *testing.T) {
	s := fixture067(t)
	b1 := workspaceBody067(t, s)
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(b1, workspaceBody067(t, replayed)) {
		t.Fatal("replay diverged")
	}
}
