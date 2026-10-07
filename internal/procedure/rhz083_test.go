package procedure

// RHZ-083 FR-RHZ-114: Step.Recommendation은 additive — 키 없는 legacy
// procedure.created 페이로드는 동일하게 재생되고, recommendation이 빈 템플릿의
// 인코딩은 pre-083과 바이트 동일(omitempty). 값이 있으면 왕복 보존.

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/events"
)

func TestStepRecommendationLegacyReplayFRRHZ114(t *testing.T) {
	// pre-083 형태 그대로의 페이로드(recommendation 키 없음).
	legacy := `{"id":"p-legacy","source_knowledge_id":"k-proc","trigger":"when needed","preconditions":["input exists"],"steps":[{"id":"first","action":"do first","after":null,"needs_gate":false},{"id":"second","action":"do second","after":["first"],"needs_gate":true}],"success_conditions":["done"],"failure_modes":["error"],"recovery_steps":["retry"]}`
	e := events.Event{AggregateType: "procedure", AggregateID: "p-legacy", Revision: 1, Type: "procedure.created", Payload: json.RawMessage(legacy)}
	p, err := Replay([]events.Event{e})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 2 || p.Steps[0].Recommendation != "" || p.Steps[1].Recommendation != "" || !p.Steps[1].NeedsGate || p.Steps[1].After[0] != "first" {
		t.Fatalf("legacy replay: %+v", p.Steps)
	}
	// 같은 템플릿을 오늘 Create하면 페이로드 바이트가 legacy와 동일하다.
	s, _, k, _ := procedureFixtures(t)
	p.Revision = 0
	p.ID = "p-legacy"
	p.SourceKnowledgeID = k.ID
	if _, err := (Service{Store: s}).Create(p); err != nil {
		t.Fatal(err)
	}
	if got := string(s.List("procedure", "p-legacy")[0].Payload); got != legacy {
		t.Fatalf("encoding drifted from legacy\n got %s\nwant %s", got, legacy)
	}
	// 값이 있으면 보존되고 키가 나타난다.
	q := validProcedure(k.ID, "p-rec")
	q.Steps[1].NeedsGate, q.Steps[1].Recommendation = true, "ship"
	created, err := (Service{Store: s}).Create(q)
	if err != nil || created.Steps[1].Recommendation != "ship" || created.Steps[0].Recommendation != "" {
		t.Fatalf("recommendation lost: %+v err=%v", created.Steps, err)
	}
	raw := string(s.List("procedure", "p-rec")[0].Payload)
	if strings.Count(raw, `"recommendation":"ship"`) != 1 || strings.Count(raw, `"recommendation"`) != 1 {
		t.Fatalf("payload: %s", raw)
	}
	replayed, err := Replay(s.List("procedure", "p-rec"))
	if err != nil || replayed.Steps[1].Recommendation != "ship" {
		t.Fatalf("round trip: %+v err=%v", replayed.Steps, err)
	}
}
