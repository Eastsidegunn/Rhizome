package workspace

// RHZ-063 FR-RHZ-092 A8: procedure.run 수동 트리거 HTTP 왕복 — run·step
// mission이 task 노드로 투영, 거부 경로 왕복.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
)

func TestProcedureRunHTTPFRRHZ092(t *testing.T) {
	s := &events.Store{}
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "src", SourceType: "note", SourceID: "n-1", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	if _, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-dev", Kind: knowledge.Procedure, Statement: "dev loop", SourceMemoryID: "mem-1", Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	if _, err := (procedure.Service{Store: s}).Create(procedure.Procedure{ID: "proc-dev", SourceKnowledgeID: "k-dev", Trigger: "manual", Steps: []procedure.Step{{ID: "impl", Action: "implement"}, {ID: "review", Action: "review", After: []string{"impl"}}}, SuccessConditions: []string{"ci green"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-dev", "dev goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	out := postIntent057(t, srv, map[string]any{"kind": "procedure.run", "id": "proc-dev", "name": "r1", "goalId": "goal-dev", "params": map[string]string{"branch": "feat-x"}})
	if out["Accepted"] != true {
		t.Fatalf("procedure.run rejected: %v", out)
	}
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	for _, want := range []string{`"id":"mission-r1"`, `"id":"mission-r1-impl"`, `"id":"mission-r1-review"`, `"name":"implement"`, `"edgeKind":"spawn"`, `"edgeKind":"dependency"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("workspace missing %s: %s", want, body)
		}
	}
	// 거부 경로: 미존재 procedure — Accepted=false, 저널 불변은 assembly
	// 테스트(A6)가 정밀 핀하므로 여기선 왕복 거부만.
	if out := postIntent057(t, srv, map[string]any{"kind": "procedure.run", "id": "proc-ghost", "name": "r2", "goalId": "goal-dev"}); out["Accepted"] == true {
		t.Fatal("ghost procedure accepted")
	}
}
