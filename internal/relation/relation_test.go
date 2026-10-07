package relation

import (
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
)

func relationFixtures(t *testing.T) (*events.Store, memory.Memory, knowledge.KnowledgeItem, knowledge.KnowledgeItem) {
	t.Helper()
	s := &events.Store{}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "fact", SourceType: "note", SourceID: "n-1", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	k1, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-1", Kind: knowledge.Concept, Statement: "one", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-2", Kind: knowledge.Claim, Statement: "two", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	return s, m, k1, k2
}

func TestCreateReplayAndProjectionFRRHZ042(t *testing.T) {
	s, m, k1, k2 := relationFixtures(t)
	beforeMemory := string(s.List("memory", m.ID)[0].Payload)
	beforeK1 := len(s.List("knowledge", k1.ID))
	r, err := (Service{Store: s}).Create(Relation{ID: "r-1", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .7})
	if err != nil {
		t.Fatal(err)
	}
	if r.Revision != 1 || r.ID != "r-1" {
		t.Fatalf("relation = %#v", r)
	}
	if string(s.List("memory", m.ID)[0].Payload) != beforeMemory || len(s.List("knowledge", k1.ID)) != beforeK1 {
		t.Fatal("relation changed source streams")
	}
	got, err := Replay(s.List("relation", "r-1"))
	if err != nil || got.From != k1.ID {
		t.Fatalf("replay = %#v, err=%v", got, err)
	}
}

func TestCreateRejectsMissingOrCorruptKnowledgeFRRHZ043(t *testing.T) {
	s, m, k1, _ := relationFixtures(t)
	before := len(s.All())
	for _, r := range []Relation{
		{ID: "missing", From: "unknown", Type: Supports, To: k1.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5},
	} {
		if _, err := (Service{Store: s}).Create(r); err == nil {
			t.Fatal("expected missing knowledge error")
		}
	}
	if len(s.All()) != before {
		t.Fatal("failed endpoint validation changed log")
	}
	if err := s.Append(0, events.Event{AggregateType: "knowledge", AggregateID: "bad", Revision: 1, Type: "knowledge.candidate", Payload: []byte(`{"id":"bad","kind":"claim","statement":"","source_memory_id":"mem-1","confidence":0.5}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Store: s}).Create(Relation{ID: "bad-ref", From: "bad", Type: Supports, To: k1.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5}); err == nil {
		t.Fatal("expected corrupt knowledge error")
	}
}

func TestValidationLeavesLogUnchanged(t *testing.T) {
	s, m, k1, k2 := relationFixtures(t)
	bad := []Relation{
		{ID: "self", From: k1.ID, Type: Supports, To: k1.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5},
		{ID: "type", From: k1.ID, Type: Type("unknown"), To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5},
		{ID: "source", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: nil, Confidence: .5},
		{ID: "memory", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: []string{"missing"}, Confidence: .5},
		{ID: "confidence", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: 2},
	}
	for _, r := range bad {
		before := len(s.All())
		if _, err := (Service{Store: s}).Create(r); err == nil {
			t.Fatalf("expected validation error for %#v", r)
		}
		if len(s.All()) != before {
			t.Fatalf("invalid relation changed log for %#v", r)
		}
	}
}

func TestDuplicateAndReplayCorruption(t *testing.T) {
	s, m, k1, k2 := relationFixtures(t)
	service := Service{Store: s}
	if _, err := service.Create(Relation{ID: "r-1", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5}); err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	if _, err := service.Create(Relation{ID: "r-1", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5}); err == nil {
		t.Fatal("expected duplicate relation error")
	}
	if len(s.All()) != before {
		t.Fatal("duplicate relation changed log")
	}
	log := s.List("relation", "r-1")
	log = append(log, events.Event{AggregateType: "relation", AggregateID: "r-1", Revision: 2, Type: "relation.created", Payload: log[0].Payload})
	if _, err := Replay(log); err == nil {
		t.Fatal("expected second event rejection")
	}
	log[0].Type = "relation.unknown"
	if _, err := Replay(log[:1]); err == nil {
		t.Fatal("expected unknown event rejection")
	}
}

func TestQueriesAreSortedAndFilterContradictions(t *testing.T) {
	s, m, k1, k2 := relationFixtures(t)
	service := Service{Store: s}
	for _, r := range []Relation{
		{ID: "r-2", From: k1.ID, Type: Supports, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5},
		{ID: "r-1", From: k1.ID, Type: Contradicts, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5},
		{ID: "r-3", From: k2.ID, Type: DerivedFrom, To: k1.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5},
	} {
		if _, err := service.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	from, err := service.From(k1.ID)
	if err != nil || len(from) != 2 || from[0].ID != "r-1" || from[1].ID != "r-2" {
		t.Fatalf("from = %#v, err=%v", from, err)
	}
	to, err := service.To(k2.ID, Supports)
	if err != nil || len(to) != 1 || to[0].ID != "r-2" {
		t.Fatalf("to = %#v, err=%v", to, err)
	}
	contra, err := service.Contradicts(k2.ID)
	if err != nil || len(contra) != 1 || contra[0].ID != "r-1" {
		t.Fatalf("contradicts = %#v, err=%v", contra, err)
	}
}

func TestQueriesSurfaceCorruptRelationsAndNilStore(t *testing.T) {
	s, m, k1, k2 := relationFixtures(t)
	if err := s.Append(0, events.Event{AggregateType: "relation", AggregateID: "bad", Revision: 1, Type: "relation.created", Payload: []byte(`{"relation_id":"bad","from":"k-1","type":"unknown","to":"k-2","source_memory_ids":["mem-1"],"confidence":0.5}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Store: s}).From(k1.ID); err == nil {
		t.Fatal("expected corrupt relation error")
	}
	var nilService Service
	if _, err := nilService.Create(Relation{}); err == nil {
		t.Fatal("expected nil create error")
	}
	if _, err := nilService.From(k1.ID); err == nil {
		t.Fatal("expected nil from error")
	}
	if _, err := nilService.To(k2.ID); err == nil {
		t.Fatal("expected nil to error")
	}
	if _, err := nilService.Contradicts(m.ID); err == nil {
		t.Fatal("expected nil contradicts error")
	}
}
