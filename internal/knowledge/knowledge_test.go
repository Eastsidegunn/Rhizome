package knowledge

import (
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/memory"
)

func memoryStore(t *testing.T) (*events.Store, memory.Memory) {
	t.Helper()
	s := &events.Store{}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "a fact", SourceType: "note", SourceID: "note-1", Confidence: 0.9, Tags: []string{"math"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func candidate(t *testing.T, s *events.Store, id, source string) KnowledgeItem {
	t.Helper()
	i, err := (Service{Store: s}).Create(KnowledgeItem{ID: id, Kind: Claim, Statement: id + " statement", SourceMemoryID: source, Confidence: 0.8, Tags: []string{"tag"}})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestCreateAndReplayStatusFRRHZ040(t *testing.T) {
	s, _ := memoryStore(t)
	i := candidate(t, s, "k-1", "mem-1")
	if i.Status != Candidate || i.Revision != 1 {
		t.Fatalf("candidate projection = %#v", i)
	}
	p, err := (Service{Store: s}).Promote("k-1", "reviewed by user")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != Promoted || p.Revision != 2 {
		t.Fatalf("promoted projection = %#v", p)
	}
	got, err := Replay(s.List("knowledge", "k-1"))
	if err != nil || got.Status != Promoted {
		t.Fatalf("replay = %#v, err=%v", got, err)
	}
}

func TestKnowledgeDoesNotChangeMemoryFRRHZ039(t *testing.T) {
	s, m := memoryStore(t)
	before := append([]events.Event(nil), s.List("memory", m.ID)...)
	_ = candidate(t, s, "k-1", m.ID)
	after := s.List("memory", m.ID)
	if len(after) != len(before) || after[0].Payload == nil || string(after[0].Payload) != string(before[0].Payload) {
		t.Fatalf("memory stream changed: before=%#v after=%#v", before, after)
	}
}

func TestCreateRequiresReplayableMemoryFRRHZ041(t *testing.T) {
	s := &events.Store{}
	if _, err := (Service{Store: s}).Create(KnowledgeItem{ID: "k-1", Kind: Claim, Statement: "x", SourceMemoryID: "missing", Confidence: 0.5}); err == nil {
		t.Fatal("expected missing source memory error")
	}
	if len(s.All()) != 0 {
		t.Fatal("failed create appended an event")
	}
	// A malformed source stream must not be hidden by Create.
	if err := s.Append(0, events.Event{AggregateType: "memory", AggregateID: "bad", Revision: 1, Type: "memory.created", Payload: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Store: s}).Create(KnowledgeItem{ID: "k-2", Kind: Claim, Statement: "x", SourceMemoryID: "bad", Confidence: 0.5}); err == nil {
		t.Fatal("expected malformed source memory error")
	}
}

func TestPromoteRequiresCandidateAndLeavesLogUnchanged(t *testing.T) {
	s, _ := memoryStore(t)
	before := len(s.All())
	if _, err := (Service{Store: s}).Promote("missing", "reason"); err == nil {
		t.Fatal("expected missing candidate error")
	}
	if len(s.All()) != before {
		t.Fatal("failed promotion changed log")
	}
	_ = candidate(t, s, "k-1", "mem-1")
	before = len(s.All())
	if _, err := (Service{Store: s}).Promote("k-1", ""); err == nil {
		t.Fatal("expected missing reason error")
	}
	if len(s.All()) != before {
		t.Fatal("invalid promotion changed log")
	}
	if _, err := (Service{Store: s}).Promote("k-1", "first"); err != nil {
		t.Fatal(err)
	}
	before = len(s.All())
	if _, err := (Service{Store: s}).Promote("k-1", "second"); err == nil {
		t.Fatal("expected duplicate promotion error")
	}
	if len(s.All()) != before {
		t.Fatal("duplicate promotion changed log")
	}
}

func TestSupersedeCreatesNewAggregateAndReportsLink(t *testing.T) {
	s, m := memoryStore(t)
	old := candidate(t, s, "k-1", m.ID)
	replacement, err := (Service{Store: s}).Supersede(old, KnowledgeItem{ID: "k-2", Kind: Claim, Statement: "new", SourceMemoryID: m.ID, Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Supersedes != old.ID || replacement.Revision != 1 {
		t.Fatalf("replacement = %#v", replacement)
	}
	if len(s.List("knowledge", old.ID)) != 1 {
		t.Fatal("original stream was modified")
	}
	sup, err := (Service{Store: s}).IsSuperseded(old.ID)
	if err != nil || !sup {
		t.Fatalf("IsSuperseded = %v, err=%v", sup, err)
	}
	promoted, err := (Service{Store: s}).Promote(replacement.ID, "replacement reviewed")
	if err != nil || promoted.Status != Promoted {
		t.Fatalf("replacement promotion = %#v, err=%v", promoted, err)
	}
	replayed, err := Replay(s.List("knowledge", replacement.ID))
	if err != nil || replayed.Status != Promoted || replayed.Supersedes != old.ID {
		t.Fatalf("replacement replay = %#v, err=%v", replayed, err)
	}
	if _, err := (Service{Store: s}).Supersede(old, KnowledgeItem{ID: old.ID, Kind: Claim, Statement: "bad", SourceMemoryID: m.ID, Confidence: 1}); err == nil {
		t.Fatal("expected self replacement error")
	}
}

func TestOneMemoryCanProduceManyKnowledgeItems(t *testing.T) {
	s, m := memoryStore(t)
	candidate(t, s, "k-2", m.ID)
	candidate(t, s, "k-1", m.ID)
	items, err := (Service{Store: s}).Search("", m.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "k-1" || items[1].ID != "k-2" {
		t.Fatalf("search order = %#v", items)
	}
}

func TestValidationAndSearchCorruption(t *testing.T) {
	s, _ := memoryStore(t)
	for _, tc := range []KnowledgeItem{
		{ID: "bad-kind", Kind: Kind("bad"), Statement: "x", SourceMemoryID: "mem-1", Confidence: .5},
		{ID: "bad-confidence", Kind: Claim, Statement: "x", SourceMemoryID: "mem-1", Confidence: 2},
	} {
		if _, err := (Service{Store: s}).Create(tc); err == nil {
			t.Fatalf("expected validation error for %#v", tc)
		}
	}
	// Replace the valid stream with an invalid revision in a separate store so
	// Search proves that a damaged aggregate is not silently omitted.
	damaged := &events.Store{}
	if err := damaged.Append(0, events.Event{AggregateType: "knowledge", AggregateID: "bad", Revision: 1, Type: "knowledge.promoted", Payload: []byte(`{"id":"bad"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{Store: damaged}).Search("", "", ""); err == nil {
		t.Fatal("expected corrupt aggregate error")
	}
}

func TestCreateDuplicateAndEmptyStatementAreRejected(t *testing.T) {
	// FR-RHZ-039: failed duplicate/invalid creation leaves the append-only log unchanged.
	s, _ := memoryStore(t)
	if _, err := (Service{Store: s}).Create(KnowledgeItem{ID: "k-1", Kind: Claim, Statement: "first", SourceMemoryID: "mem-1", Confidence: .5}); err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	if _, err := (Service{Store: s}).Create(KnowledgeItem{ID: "k-1", Kind: Claim, Statement: "second", SourceMemoryID: "mem-1", Confidence: .5}); err == nil {
		t.Fatal("expected duplicate knowledge ID error")
	}
	if len(s.All()) != before {
		t.Fatal("duplicate create changed log")
	}
	if _, err := (Service{Store: s}).Create(KnowledgeItem{ID: "k-empty", Kind: Claim, Statement: " ", SourceMemoryID: "mem-1", Confidence: .5}); err == nil {
		t.Fatal("expected empty statement error")
	}
	if len(s.All()) != before {
		t.Fatal("empty statement changed log")
	}
}

func TestReplayRejectsThirdEvent(t *testing.T) {
	// FR-RHZ-040: replay rejects a third event in the knowledge aggregate stream.
	s, _ := memoryStore(t)
	_ = candidate(t, s, "k-1", "mem-1")
	if _, err := (Service{Store: s}).Promote("k-1", "reviewed"); err != nil {
		t.Fatal(err)
	}
	log := s.List("knowledge", "k-1")
	log = append(log, events.Event{AggregateType: "knowledge", AggregateID: "k-1", Revision: 3, Type: "knowledge.promoted", Payload: []byte(`{"id":"k-1","kind":"claim","statement":"k-1 statement","source_memory_id":"mem-1","confidence":0.8,"reason":"again"}`)})
	if _, err := Replay(log); err == nil {
		t.Fatal("expected third event rejection")
	}
}

func TestNilStore(t *testing.T) {
	var s Service
	if _, err := s.Create(KnowledgeItem{}); err == nil {
		t.Fatal("expected nil store error")
	}
	if _, err := s.Promote("k", "reason"); err == nil {
		t.Fatal("expected nil store error")
	}
	if _, err := s.Supersede(KnowledgeItem{ID: "a"}, KnowledgeItem{ID: "b"}); err == nil {
		t.Fatal("expected nil store error")
	}
	if _, err := s.Search("", "", ""); err == nil {
		t.Fatal("expected nil store error")
	}
}
