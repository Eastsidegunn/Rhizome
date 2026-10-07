package workspace

// RHZ-083 FR-RHZ-114: procedure.run이 NeedsGate step마다 만든 게이트 question이
// /v1/context steps[].gates(RHZ-068)·/v1/workspace gateCapabilities(RHZ-070)
// 에 투영되고 gate.approve로 닫힌다 (N4); NDJSON 저널 왕복에 legacy
// procedure.created(recommendation 키 없음)도 포함 (R1).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
)

func gatedStepsWire083() []map[string]any {
	return []map[string]any{
		{"id": "a", "action": "act-a", "needsGate": true},
		{"id": "b", "action": "act-b", "after": []string{"a"}},
		{"id": "c", "action": "act-c", "after": []string{"b"}, "needsGate": true, "recommendation": "ship"},
	}
}

// N4: HTTP define(recommendation 포함) + run → context gates a·c(pending)·b 없음,
// gateCapabilities 3종 enabled, 다이제스트로 gate.approve 성공 → approved/hidden.
func TestProcedureRunGatesProjectedAndApprovableFRRHZ114(t *testing.T) {
	s := fixture068(t)
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.define", "id": "proc-gated", "sourceKnowledgeId": "k-dev", "trigger": "manual", "steps": gatedStepsWire083(), "successConditions": []string{"ci green"}}); out["Accepted"] != true {
		t.Fatalf("define: %v", out)
	}
	p, err := procedure.Replay(s.List("procedure", "proc-gated"))
	if err != nil || p.Steps[2].Recommendation != "ship" || p.Steps[0].Recommendation != "" {
		t.Fatalf("recommendation on wire lost: %+v err=%v", p.Steps, err)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.run", "id": "proc-gated", "name": "r1", "goalId": "goal-dev", "actor": "op", "correlationId": "corr-1"}); out["Accepted"] != true {
		t.Fatalf("run: %v", out)
	}
	h := NewHTTP(s).Handler()
	code, _, steps := contextSteps(t, h, "mission-r1")
	if code != http.StatusOK || !eq(stepIDs(steps), []string{"mission-r1-a", "mission-r1-b", "mission-r1-c"}) {
		t.Fatalf("code=%d steps=%v", code, stepIDs(steps))
	}
	gateOf := map[string]string{}
	for _, st := range steps {
		switch st.ID {
		case "mission-r1-b":
			if len(st.Gates) != 0 {
				t.Fatalf("b has gates %v", st.Gates)
			}
		default:
			if len(st.Gates) != 1 || st.Gates[0].State != "pending" || st.Gates[0].Name != "r1 · "+strings.TrimPrefix(st.ID, "mission-r1-")+" 게이트" {
				t.Fatalf("%s gates=%v", st.ID, st.Gates)
			}
			gateOf[st.ID] = st.Gates[0].ID
		}
	}
	w := decode070(t, getWorkspace070(t, s))
	digests := map[string]string{}
	for _, g := range w.Body.Gates {
		id, _ := g["id"].(string)
		digests[id], _ = g["requestDigest"].(string)
		if id == gateOf["mission-r1-c"] && (g["recommendation"] != "ship" || g["body"] != "act-c" || g["missionId"] != "mission-r1-c") {
			t.Fatalf("gate c projection: %v", g)
		}
	}
	for _, id := range gateOf {
		caps := w.Body.GateCapabilities[id]
		for _, k := range []string{"approve", "reject", "requestChanges"} {
			if caps[k] != "enabled" {
				t.Fatalf("gate %s %s=%q want enabled (%v)", id, k, caps[k], caps)
			}
		}
	}
	if len(w.Body.GateCapabilities) != 2 {
		t.Fatalf("gateCapabilities %d entries, want 2", len(w.Body.GateCapabilities))
	}
	// Approve with the digest returned to the caller.
	approveID := gateOf["mission-r1-a"]
	if out := postIntent057(t, srv, map[string]any{"kind": "gate.approve", "gateId": approveID, "digest": digests[approveID], "actor": "op"}); out["Accepted"] != true {
		t.Fatalf("approve: %v", out)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "gate.approve", "gateId": gateOf["mission-r1-c"], "digest": "rhz-question-v1:bogus", "actor": "op"}); out["Accepted"] == true {
		t.Fatal("approve with wrong digest accepted")
	}
	_, _, steps = contextSteps(t, h, "mission-r1")
	for _, st := range steps {
		switch st.ID {
		case "mission-r1-a":
			if st.Gates[0].State != "approved" {
				t.Fatalf("a gate state %q", st.Gates[0].State)
			}
		case "mission-r1-c":
			if st.Gates[0].State != "pending" {
				t.Fatalf("c gate state %q", st.Gates[0].State)
			}
		}
	}
	w = decode070(t, getWorkspace070(t, s))
	if w.Body.GateCapabilities[approveID]["approve"] != "hidden" || w.Body.GateCapabilities[gateOf["mission-r1-c"]]["approve"] != "enabled" {
		t.Fatalf("capabilities after approve: %v", w.Body.GateCapabilities)
	}
}

// R1: 실제 NDJSON 저널 왕복 — legacy procedure.created(recommendation 키 없음)
// 템플릿과 recommendation 있는 템플릿을 각각 run한 저널이 Close/Open 후 이벤트
// 수·/v1/context·/v1/workspace 바이트 동일, legacy 페이로드 바이트 불변.
func TestProcedureRunGatesJournalRoundTripFRRHZ114(t *testing.T) {
	path := t.TempDir() + "/events.ndjson"
	j, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: j}).CreateGoal("goal-dev", "dev goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	// legacy 템플릿: pre-083 페이로드 그대로(소스 지식 검증은 Create 시점 규칙
	// 이라 Replay엔 무관 — 저널에 이미 있는 과거 이벤트를 흉내낸다).
	legacy := `{"id":"proc-legacy","source_knowledge_id":"k-old","trigger":"manual","preconditions":null,"steps":[{"id":"a","action":"act-a","after":null,"needs_gate":true},{"id":"b","action":"act-b","after":["a"],"needs_gate":false}],"success_conditions":["ok"],"failure_modes":null,"recovery_steps":null}`
	if err := j.Append(0, events.Event{AggregateType: "procedure", AggregateID: "proc-legacy", Revision: 1, Type: "procedure.created", Payload: json.RawMessage(legacy)}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHTTP(j).Handler())
	defer srv.Close()
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.run", "id": "proc-legacy", "name": "rl", "goalId": "goal-dev", "actor": "op"}); out["Accepted"] != true {
		t.Fatalf("legacy run: %v", out)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "note.create", "content": "dev loop note", "memoryKind": "observation", "goalId": "goal-dev", "actor": "op"}); out["Accepted"] != true {
		t.Fatalf("note.create: %v", out)
	}
	note := noteID("dev loop note")
	if out := postIntent057(t, srv, map[string]any{"kind": "knowledge.create", "sourceMemoryId": note, "content": "dev loop how-to", "knowledgeKind": "procedure", "actor": "op"}); out["Accepted"] != true {
		t.Fatalf("knowledge.create: %v", out)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.define", "id": "proc-new", "sourceKnowledgeId": knowID(note, "dev loop how-to"), "trigger": "manual", "steps": gatedStepsWire083(), "successConditions": []string{"ok"}}); out["Accepted"] != true {
		t.Fatalf("define: %v", out)
	}
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.run", "id": "proc-new", "name": "rn", "goalId": "goal-dev", "actor": "op"}); out["Accepted"] != true {
		t.Fatalf("new run: %v", out)
	}
	wantEvents := len(j.All())
	if countAgg071(j, "question") != 3 {
		t.Fatalf("questions %d want 3 (legacy a + new a,c)", countAgg071(j, "question"))
	}
	wantCtxL := snapshotContext083(t, j, "mission-rl")
	wantCtxN := snapshotContext083(t, j, "mission-rn")
	wantWS := getWorkspace071(t, j)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if got := len(j.All()); got != wantEvents {
		t.Fatalf("events %d want %d", got, wantEvents)
	}
	if got := string(j.List("procedure", "proc-legacy")[0].Payload); got != legacy {
		t.Fatalf("legacy payload changed:\n got %s\nwant %s", got, legacy)
	}
	p, err := procedure.Replay(j.List("procedure", "proc-new"))
	if err != nil || p.Steps[2].Recommendation != "ship" {
		t.Fatalf("new template after reopen: %+v err=%v", p.Steps, err)
	}
	if got := snapshotContext083(t, j, "mission-rl"); !bytes.Equal(got, wantCtxL) {
		t.Fatalf("legacy context differs:\n got %s\nwant %s", got, wantCtxL)
	}
	if got := snapshotContext083(t, j, "mission-rn"); !bytes.Equal(got, wantCtxN) {
		t.Fatalf("new context differs:\n got %s\nwant %s", got, wantCtxN)
	}
	if got := getWorkspace071(t, j); !bytes.Equal(got, wantWS) {
		t.Fatalf("workspace differs:\n got %s\nwant %s", got, wantWS)
	}
	var bundle struct {
		Steps []stepView `json:"steps"`
	}
	if err := json.Unmarshal(wantCtxL, &bundle); err != nil || len(bundle.Steps) != 2 || len(bundle.Steps[0].Gates) != 1 || len(bundle.Steps[1].Gates) != 0 {
		t.Fatalf("legacy run gates: %+v err=%v", bundle.Steps, err)
	}
}

func snapshotContext083(t *testing.T, s events.Port, missionID string) []byte {
	t.Helper()
	code, b := getContext(t, NewHTTP(s).Handler(), "?task="+missionID)
	if code != http.StatusOK {
		t.Fatalf("context %s: %d %s", missionID, code, b)
	}
	return b
}
