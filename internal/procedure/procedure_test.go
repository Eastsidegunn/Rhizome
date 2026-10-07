package procedure

import (
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
)

func procedureFixtures(t *testing.T) (*events.Store, memory.Memory, knowledge.KnowledgeItem, knowledge.KnowledgeItem) {
	t.Helper()
	s := &events.Store{}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "procedure source", SourceType: "note", SourceID: "note-1", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	k, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-proc", Kind: knowledge.Procedure, Statement: "how to do it", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	other, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-concept", Kind: knowledge.Concept, Statement: "a concept", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	return s, m, k, other
}

// RHZ-063 (FR-RHZ-092): Steps는 []string에서 Step DAG로 교체됐다(휴면·저널
// 0 이벤트라 레거시 없음). 기존 검증 의도(blank 거부·필수 필드·재생 왕복)는
// Step 구조 기준으로 유지·강화.
func validProcedure(source string, id string) Procedure {
	return Procedure{ID: id, SourceKnowledgeID: source, Trigger: "when needed", Preconditions: []string{"input exists"}, Steps: []Step{{ID: "first", Action: "do first"}, {ID: "second", Action: "do second", After: []string{"first"}}}, SuccessConditions: []string{"done"}, FailureModes: []string{"error"}, RecoverySteps: []string{"retry"}}
}

func TestCreateReplayAndSourceProjectionFRRHZ044(t *testing.T) {
	s, m, k, _ := procedureFixtures(t)
	beforeMemory := string(s.List("memory", m.ID)[0].Payload)
	beforeKnowledge := len(s.List("knowledge", k.ID))
	p, err := (Service{Store: s}).Create(validProcedure(k.ID, "p-1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != 1 || p.SourceKnowledgeID != k.ID || len(p.Steps) != 2 {
		t.Fatalf("procedure = %#v", p)
	}
	if string(s.List("memory", m.ID)[0].Payload) != beforeMemory || len(s.List("knowledge", k.ID)) != beforeKnowledge || len(s.List("relation", "anything")) != 0 {
		t.Fatal("procedure changed source streams")
	}
	got, err := Replay(s.List("procedure", p.ID))
	if err != nil || got.ID != p.ID {
		t.Fatalf("replay = %#v, err=%v", got, err)
	}
}

func TestCreateRejectsMissingCorruptAndWrongKindSourceFRRHZ044(t *testing.T) {
	s, m, k, other := procedureFixtures(t)
	before := len(s.All())
	for _, source := range []string{"missing", other.ID} {
		if _, err := (Service{Store: s}).Create(validProcedure(source, "p-"+source)); err == nil {
			t.Fatalf("expected source rejection for %q", source)
		}
	}
	if len(s.All()) != before {
		t.Fatal("source rejection changed log")
	}
	if err := s.Append(0, events.Event{AggregateType: "knowledge", AggregateID: "bad", Revision: 1, Type: "knowledge.candidate", Payload: []byte(`{"id":"bad","kind":"procedure","statement":"","source_memory_id":"mem-1","confidence":0.5}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Store: s}).Create(validProcedure("bad", "p-bad")); err == nil {
		t.Fatal("expected corrupt source rejection")
	}
	_ = m
	_ = k
}

func TestShapeValidationFRRHZ045(t *testing.T) {
	s, _, k, _ := procedureFixtures(t)
	valid := validProcedure(k.ID, "p-base")
	step := []Step{{ID: "s", Action: "do"}}
	invalid := []Procedure{
		{ID: "empty-trigger", SourceKnowledgeID: k.ID, Trigger: " ", Steps: step},
		{ID: "empty-steps", SourceKnowledgeID: k.ID, Trigger: "trigger"},
		{ID: "blank-step-action", SourceKnowledgeID: k.ID, Trigger: "trigger", Steps: []Step{{ID: "s", Action: " "}}},
		{ID: "blank-step-id", SourceKnowledgeID: k.ID, Trigger: "trigger", Steps: []Step{{ID: " ", Action: "do"}}},
		{ID: "blank-pre", SourceKnowledgeID: k.ID, Trigger: "trigger", Steps: step, Preconditions: []string{" "}},
		{ID: "blank-success", SourceKnowledgeID: k.ID, Trigger: "trigger", Steps: step, SuccessConditions: []string{" "}},
		{ID: "blank-failure", SourceKnowledgeID: k.ID, Trigger: "trigger", Steps: step, FailureModes: []string{" "}},
		{ID: "blank-recovery", SourceKnowledgeID: k.ID, Trigger: "trigger", Steps: step, RecoverySteps: []string{" "}},
	}
	for _, p := range invalid {
		before := len(s.All())
		if _, err := (Service{Store: s}).Create(p); err == nil {
			t.Fatalf("expected shape error for %#v", p)
		}
		if len(s.All()) != before {
			t.Fatalf("shape error changed log for %#v", p)
		}
	}
	if _, err := (Service{Store: s}).Create(valid); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateAndReplaySecondEventRejected(t *testing.T) {
	s, _, k, _ := procedureFixtures(t)
	service := Service{Store: s}
	if _, err := service.Create(validProcedure(k.ID, "p-1")); err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	if _, err := service.Create(validProcedure(k.ID, "p-1")); err == nil {
		t.Fatal("expected duplicate procedure error")
	}
	if len(s.All()) != before {
		t.Fatal("duplicate procedure changed log")
	}
	log := s.List("procedure", "p-1")
	log = append(log, events.Event{AggregateType: "procedure", AggregateID: "p-1", Revision: 2, Type: "procedure.created", Payload: log[0].Payload})
	if _, err := Replay(log); err == nil {
		t.Fatal("expected second event rejection")
	}
}

func TestQueriesSortedAndCorruptNotHidden(t *testing.T) {
	s, _, k, _ := procedureFixtures(t)
	service := Service{Store: s}
	for _, id := range []string{"p-2", "p-1"} {
		if _, err := service.Create(validProcedure(k.ID, id)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := service.BySourceKnowledge(k.ID)
	if err != nil || len(items) != 2 || items[0].ID != "p-1" || items[1].ID != "p-2" {
		t.Fatalf("query = %#v, err=%v", items, err)
	}
	if _, err := service.Get("p-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(0, events.Event{AggregateType: "procedure", AggregateID: "bad", Revision: 1, Type: "procedure.created", Payload: []byte(`{"id":"bad","source_knowledge_id":"k-proc","trigger":"x","preconditions":[],"steps":[],"success_conditions":[],"failure_modes":[],"recovery_steps":[]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BySourceKnowledge(k.ID); err == nil {
		t.Fatal("expected corrupt procedure error")
	}
}

func TestNilStore(t *testing.T) {
	var service Service
	if _, err := service.Create(Procedure{}); err == nil {
		t.Fatal("expected nil create error")
	}
	if _, err := service.Get("p"); err == nil {
		t.Fatal("expected nil get error")
	}
	if _, err := service.BySourceKnowledge("k"); err == nil {
		t.Fatal("expected nil query error")
	}
}
