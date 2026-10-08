package workspace

// RHZ-065 FR-RHZ-094: 저술 intent 3종(knowledge.create/promote,
// procedure.define) + 종단 실증. 테스트 계획 RHZ-065.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
)

func knowID(sourceMemoryID, statement string) string {
	sum := sha256.Sum256([]byte(sourceMemoryID + "\x00" + statement))
	return "know-" + hex.EncodeToString(sum[:])[:12]
}

// fixture065: goal+mission + note(=memory, goal에 about) 하나.
func fixture065(t *testing.T) (s *events.Store, noteIDv string) {
	t.Helper()
	s = &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-dev", "dev goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-dev", "goal-dev", "dev mission", "done"); err != nil {
		t.Fatal(err)
	}
	res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "dev loop note", MemoryKind: "observation", GoalID: "goal-dev"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	return s, noteID("dev loop note")
}

func relay065(t *testing.T, s *events.Store, in Intent) RelayResult {
	t.Helper()
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// K1 (수용 1, 변이 probe): knowledge.create → knowledge.candidate 정확 1건,
// 결정론 ID·payload·재생 왕복.
func TestKnowledgeCreateOneEventFRRHZ094(t *testing.T) {
	s, note := fixture065(t)
	n := len(s.All())
	in := Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "always run gofmt", KnowledgeKind: "claim", Confidence: 0.7, Tags: []string{"dev"}}
	if res := relay065(t, s, in); !res.Accepted {
		t.Fatal(res)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("grew %d, want 1", len(all)-n)
	}
	e := all[len(all)-1]
	kid := knowID(note, "always run gofmt")
	if e.AggregateType != "knowledge" || e.Type != "knowledge.candidate" || e.AggregateID != kid {
		t.Fatalf("event %+v", e)
	}
	got, err := knowledge.Replay(s.List("knowledge", kid))
	if err != nil || got.Statement != "always run gofmt" || got.Kind != knowledge.Claim || got.SourceMemoryID != note || got.Confidence != 0.7 || got.Status != knowledge.Candidate {
		t.Fatalf("replay %+v err=%v", got, err)
	}
}

// K2: 멱등 3경로 — 재제출 무이벤트, 승격 후 재제출도 수용, 내용 불일치 거부.
func TestKnowledgeCreateIdempotentFRRHZ094(t *testing.T) {
	s, note := fixture065(t)
	in := Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "always run gofmt", KnowledgeKind: "claim", Confidence: 0.7}
	if res := relay065(t, s, in); !res.Accepted {
		t.Fatal(res)
	}
	before, _ := json.Marshal(s.All())
	if res := relay065(t, s, in); !res.Accepted {
		t.Fatal("resubmit rejected")
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("resubmit appended")
	}
	// 승격 후 동일 재제출 → 여전히 수용·무이벤트.
	if res := relay065(t, s, Intent{Kind: "knowledge.promote", ID: knowID(note, "always run gofmt"), Reason: "test"}); !res.Accepted {
		t.Fatal(res)
	}
	mid, _ := json.Marshal(s.All())
	if res := relay065(t, s, in); !res.Accepted {
		t.Fatal("post-promote resubmit rejected")
	}
	mid2, _ := json.Marshal(s.All())
	if string(mid) != string(mid2) {
		t.Fatal("post-promote resubmit appended")
	}
	// 같은 source+statement, 다른 kind → 결정론 ID 충돌의 정직한 거부.
	bad := in
	bad.KnowledgeKind = "concept"
	if res := relay065(t, s, bad); res.Accepted {
		t.Fatal("conflicting content accepted")
	}
	mid3, _ := json.Marshal(s.All())
	if string(mid2) != string(mid3) {
		t.Fatal("conflict changed journal")
	}
}

// K3: 실재·필수 검증 — ghost source 거부(커널 validateSource), 빈 필드 거부,
// confidence 미지정 → 0.5 기본 기록.
func TestKnowledgeCreateValidationFRRHZ094(t *testing.T) {
	s, note := fixture065(t)
	before, _ := json.Marshal(s.All())
	for _, in := range []Intent{
		{Kind: "knowledge.create", SourceMemoryID: "note-ghost", Content: "x", KnowledgeKind: "claim"},
		{Kind: "knowledge.create", Content: "x", KnowledgeKind: "claim"},
		{Kind: "knowledge.create", SourceMemoryID: note, KnowledgeKind: "claim"},
		{Kind: "knowledge.create", SourceMemoryID: note, Content: "x"},
	} {
		if res := relay065(t, s, in); res.Accepted {
			t.Fatalf("%+v accepted", in)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejections changed journal")
	}
	if res := relay065(t, s, Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "default conf", KnowledgeKind: "claim"}); !res.Accepted {
		t.Fatal(res)
	}
	got, err := knowledge.Replay(s.List("knowledge", knowID(note, "default conf")))
	if err != nil || got.Confidence != 0.5 {
		t.Fatalf("default confidence %+v err=%v", got, err)
	}
}

// P1 (수용 1): promote 정상 1건·재생, 재승격·ghost·빈 reason 거부(저널 불변).
func TestKnowledgePromoteFRRHZ094(t *testing.T) {
	s, note := fixture065(t)
	if res := relay065(t, s, Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "to promote", KnowledgeKind: "claim"}); !res.Accepted {
		t.Fatal(res)
	}
	kid := knowID(note, "to promote")
	n := len(s.All())
	if res := relay065(t, s, Intent{Kind: "knowledge.promote", ID: kid, Reason: "validated"}); !res.Accepted {
		t.Fatal(res)
	}
	if len(s.All()) != n+1 {
		t.Fatal("promote appended != 1")
	}
	got, err := knowledge.Replay(s.List("knowledge", kid))
	if err != nil || got.Status != knowledge.Promoted {
		t.Fatalf("replay %+v err=%v", got, err)
	}
	before, _ := json.Marshal(s.All())
	for _, in := range []Intent{
		{Kind: "knowledge.promote", ID: kid, Reason: "again"}, // 재승격
		{Kind: "knowledge.promote", ID: "know-ghost", Reason: "x"},
		{Kind: "knowledge.promote", ID: kid},
	} {
		if res := relay065(t, s, in); res.Accepted {
			t.Fatalf("%+v accepted", in)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejections changed journal")
	}
}

func defineIntent(sourceKnowledgeID string) Intent {
	return Intent{Kind: "procedure.define", ID: "proc-dev", SourceKnowledgeID: sourceKnowledgeID, Trigger: "manual",
		Steps:             []IntentStep{{ID: "impl", Action: "implement"}, {ID: "review", Action: "review", After: []string{"impl"}, NeedsGate: true}},
		SuccessConditions: []string{"ci green"}}
}

// 승격된 procedure-kind 지식까지 저술된 픽스처.
func fixture065Knowledge(t *testing.T) (*events.Store, string) {
	t.Helper()
	s, note := fixture065(t)
	if res := relay065(t, s, Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "dev loop how-to", KnowledgeKind: "procedure"}); !res.Accepted {
		t.Fatal(res)
	}
	kid := knowID(note, "dev loop how-to")
	if res := relay065(t, s, Intent{Kind: "knowledge.promote", ID: kid, Reason: "validated"}); !res.Accepted {
		t.Fatal(res)
	}
	return s, kid
}

// D1 (수용 1, 변이 probe): procedure.define → procedure.created 정확 1건
// The event type is procedure.created; replay preserves the Step DAG.
func TestProcedureDefineOneEventFRRHZ094(t *testing.T) {
	s, kid := fixture065Knowledge(t)
	n := len(s.All())
	if res := relay065(t, s, defineIntent(kid)); !res.Accepted {
		t.Fatal(res)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("grew %d, want 1", len(all)-n)
	}
	e := all[len(all)-1]
	if e.AggregateType != "procedure" || e.Type != "procedure.created" || e.AggregateID != "proc-dev" {
		t.Fatalf("event %+v", e)
	}
	got, err := procedure.Replay(s.List("procedure", "proc-dev"))
	if err != nil || len(got.Steps) != 2 || got.Steps[1].ID != "review" || !got.Steps[1].NeedsGate || got.Steps[1].After[0] != "impl" {
		t.Fatalf("replay %+v err=%v", got, err)
	}
}

// D2: define 멱등(전체 동치)·불일치 거부(procedure.updated 비범위 경계).
func TestProcedureDefineIdempotentFRRHZ094(t *testing.T) {
	s, kid := fixture065Knowledge(t)
	if res := relay065(t, s, defineIntent(kid)); !res.Accepted {
		t.Fatal(res)
	}
	before, _ := json.Marshal(s.All())
	if res := relay065(t, s, defineIntent(kid)); !res.Accepted {
		t.Fatal("identical resubmit rejected")
	}
	changed := defineIntent(kid)
	changed.Steps = append(changed.Steps, IntentStep{ID: "merge", Action: "merge", After: []string{"review"}})
	res := relay065(t, s, changed)
	if res.Accepted {
		t.Fatal("changed template accepted (procedure.updated out of scope)")
	}
	if !strings.Contains(res.Reason, "out of scope") {
		t.Fatalf("reason %q", res.Reason)
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed")
	}
}

// D3: define 검증 위임 — ghost 지식·비procedure kind·사이클 전부 거부(커널
// Create 경유, relay 중복 검증 없음), 저널 불변.
func TestProcedureDefineValidationFRRHZ094(t *testing.T) {
	s, note := fixture065(t)
	if res := relay065(t, s, Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "a mere claim", KnowledgeKind: "claim"}); !res.Accepted {
		t.Fatal(res)
	}
	claimID := knowID(note, "a mere claim")
	if res := relay065(t, s, Intent{Kind: "knowledge.promote", ID: claimID, Reason: "x"}); !res.Accepted {
		t.Fatal(res)
	}
	before, _ := json.Marshal(s.All())
	ghost := defineIntent("know-ghost")
	nonProc := defineIntent(claimID)
	cyclic := defineIntent(claimID)
	cyclic.Steps = []IntentStep{{ID: "a", Action: "a", After: []string{"b"}}, {ID: "b", Action: "b", After: []string{"a"}}}
	for _, in := range []Intent{ghost, nonProc, cyclic, {Kind: "procedure.define", ID: "p", SourceKnowledgeID: claimID, Trigger: "t"}} {
		if res := relay065(t, s, in); res.Accepted {
			t.Fatalf("%+v accepted", in)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejections changed journal")
	}
}

// E2E (수용 3): HTTP 6단 저술 체인 → 작업 트리 스폰 + /v1/context가 이제
// knowledge≥1 반환 + UseTrace 정확 1건 — 드라이런의 두 빈자리 폐쇄.
func TestAuthoringChainEndToEndFRRHZ094(t *testing.T) {
	s := &events.Store{}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	post := func(body map[string]any) map[string]any { return postIntent057(t, srv, body) }
	if out := post(map[string]any{"kind": "mission.create", "name": "dev", "prompt": "done"}); out["Accepted"] != true {
		t.Fatalf("mission.create: %v", out)
	}
	if out := post(map[string]any{"kind": "note.create", "content": "dev loop note", "memoryKind": "observation", "goalId": "goal-dev"}); out["Accepted"] != true {
		t.Fatalf("note.create: %v", out)
	}
	note := noteID("dev loop note")
	if out := post(map[string]any{"kind": "knowledge.create", "sourceMemoryId": note, "content": "dev loop how-to", "knowledgeKind": "procedure"}); out["Accepted"] != true {
		t.Fatalf("knowledge.create: %v", out)
	}
	kid := knowID(note, "dev loop how-to")
	if out := post(map[string]any{"kind": "knowledge.promote", "id": kid, "reason": "validated"}); out["Accepted"] != true {
		t.Fatalf("promote: %v", out)
	}
	if out := post(map[string]any{"kind": "procedure.define", "id": "proc-dev", "sourceKnowledgeId": kid, "trigger": "manual",
		"steps": []map[string]any{{"id": "impl", "action": "implement"}, {"id": "review", "action": "review", "after": []string{"impl"}, "needsGate": true}}}); out["Accepted"] != true {
		t.Fatalf("define: %v", out)
	}
	// RHZ-083 (FR-RHZ-114): review는 needsGate라 게이트 question이 자동 생성되고
	// question은 actor를 요구하므로 run에 actor를 싣는다 (어댑터는 항상 싣는다).
	if out := post(map[string]any{"kind": "procedure.run", "id": "proc-dev", "name": "r1", "goalId": "goal-dev", "actor": "tester"}); out["Accepted"] != true {
		t.Fatalf("run: %v", out)
	}
	// 스폰 확인: run mission + step 2 + spawn 2 + dep 1 (+ review 게이트 question 1).
	for _, id := range []string{"mission-r1", "mission-r1-impl", "mission-r1-review"} {
		if len(s.List("mission", id)) == 0 {
			t.Fatalf("mission %s not spawned", id)
		}
	}
	// context: knowledge ≥1 + UseTrace 정확 1건.
	resp, err := http.Get(srv.URL + "/v1/context?task=mission-dev")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var bundle struct {
		Knowledge []struct{ ID string } `json:"knowledge"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil || len(bundle.Knowledge) < 1 || bundle.Knowledge[0].ID != kid {
		t.Fatalf("context knowledge %s err=%v", b, err)
	}
	traces := traceEvents064(s)
	if len(traces) != 1 {
		t.Fatalf("use traces %d, want exactly 1", len(traces))
	}
}

// W1: steps 포함 full camelCase JSON 와이어 왕복 — D1과 동일 결과가 HTTP로.
func TestDefineWireCamelCaseFRRHZ094(t *testing.T) {
	s, kid := fixture065Knowledge(t)
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	out := postIntent057(t, srv, map[string]any{"kind": "procedure.define", "id": "proc-wire", "sourceKnowledgeId": kid, "trigger": "manual",
		"steps":             []map[string]any{{"id": "s1", "action": "act", "needsGate": true}},
		"successConditions": []string{"ok"}, "recoverySteps": []string{"retry"}})
	if out["Accepted"] != true {
		t.Fatalf("wire define rejected: %v", out)
	}
	got, err := procedure.Replay(s.List("procedure", "proc-wire"))
	if err != nil || !got.Steps[0].NeedsGate || got.RecoverySteps[0] != "retry" {
		t.Fatalf("wire fields lost: %+v err=%v", got, err)
	}
}

// R1: 재생 재계산 — E2E 체인 저널의 재생본에서 지식·절차 상태 동일, 같은
// context 조회 같은 바이트.
func TestAuthoringRecomputableFRRHZ094(t *testing.T) {
	s, kid := fixture065Knowledge(t)
	if res := relay065(t, s, defineIntent(kid)); !res.Accepted {
		t.Fatal(res)
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	a, err := knowledge.Replay(s.List("knowledge", kid))
	if err != nil {
		t.Fatal(err)
	}
	b, err := knowledge.Replay(replayed.List("knowledge", kid))
	if err != nil || a.Status != b.Status || a.Statement != b.Statement {
		t.Fatal("knowledge replay diverged")
	}
	c1, b1 := getContext(t, NewHTTP(s).Handler(), "?task=mission-dev")
	c2, b2 := getContext(t, NewHTTP(replayed).Handler(), "?task=mission-dev")
	if c1 != http.StatusOK || c2 != http.StatusOK || !bytes.Equal(b1, b2) {
		t.Fatal("context diverged after replay")
	}
}
