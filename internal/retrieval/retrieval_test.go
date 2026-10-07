package retrieval

import (
	"reflect"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/relation"
	"rhizome/internal/trace"
)

type fixture struct {
	store               *events.Store
	missionID           string
	itemA, itemB, itemC string
	memoryID            string
}

func makeFixture(t *testing.T) fixture {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-1", "goal", "done", "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-1", "goal-1", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "fact", SourceType: "note", SourceID: "n", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	ks := knowledge.Service{Store: s}
	a, err := ks.Create(knowledge.KnowledgeItem{ID: "k-a", Kind: knowledge.Claim, Statement: "a", SourceMemoryID: m.ID, Confidence: .8, Tags: []string{"math"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ks.Create(knowledge.KnowledgeItem{ID: "k-b", Kind: knowledge.Claim, Statement: "b", SourceMemoryID: m.ID, Confidence: .8, Tags: []string{"physics"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ks.Create(knowledge.KnowledgeItem{ID: "k-c", Kind: knowledge.Concept, Statement: "c", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (relation.Service{Store: s}).Create(relation.Relation{ID: "rel-a-b", From: a.ID, Type: relation.Supports, To: b.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .7}); err != nil {
		t.Fatal(err)
	}
	if _, err := (relation.Service{Store: s}).Create(relation.Relation{ID: "rel-a-c", From: a.ID, Type: relation.DerivedFrom, To: c.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .7}); err != nil {
		t.Fatal(err)
	}
	if _, err := (relation.Service{Store: s}).Create(relation.Relation{ID: "rel-b-c", From: b.ID, Type: relation.Supports, To: c.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .7}); err != nil {
		t.Fatal(err)
	}
	return fixture{store: s, missionID: "mission-1", itemA: a.ID, itemB: b.ID, itemC: c.ID, memoryID: m.ID}
}

func TestCompileTimePortAndDeterminismFRRHZ050(t *testing.T) {
	f := makeFixture(t)
	var r Retriever = Deterministic{Store: f.store}
	q := Query{IncludeCandidates: true, Tag: "math"}
	one, err := r.Retrieve(q)
	if err != nil {
		t.Fatal(err)
	}
	two, err := r.Retrieve(q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, two) || len(one.Items) != 1 || one.Items[0].Knowledge.ID != f.itemA {
		t.Fatalf("non-deterministic result: one=%#v two=%#v", one, two)
	}
}

func TestFiltersAndSupersededProjectionFRRHZ050(t *testing.T) {
	f := makeFixture(t)
	ks := knowledge.Service{Store: f.store}
	old, err := knowledge.Replay(f.store.List("knowledge", f.itemB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Supersede(old, knowledge.KnowledgeItem{ID: "k-b-new", Kind: knowledge.Claim, Statement: "new b", SourceMemoryID: f.memoryID, Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	r := Deterministic{Store: f.store}
	base, err := r.Retrieve(Query{IncludeCandidates: true, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range base.Items {
		if item.Knowledge.ID == f.itemB {
			found = item.Superseded
		}
	}
	if !found {
		t.Fatal("superseded item was not marked")
	}
	without, err := r.Retrieve(Query{IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range without.Items {
		if item.Knowledge.ID == f.itemB {
			t.Fatal("superseded item included by default")
		}
	}
	promotedOnly, err := r.Retrieve(Query{IncludeCandidates: false, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range promotedOnly.Items {
		if item.Knowledge.ID == f.itemA || item.Knowledge.ID == f.itemB {
			t.Fatal("candidate included when disabled")
		}
	}
	combo, err := r.Retrieve(Query{Kind: knowledge.Claim, Tag: "physics", SourceMemoryID: f.memoryID, IncludeCandidates: true, IncludeSuperseded: true})
	if err != nil || len(combo.Items) != 1 || combo.Items[0].Knowledge.ID != f.itemB {
		t.Fatalf("combined filter = %#v, err=%v", combo, err)
	}
}

func TestOneHopRelationsAndSupersededExpansionFRRHZ050(t *testing.T) {
	f := makeFixture(t)
	r := Deterministic{Store: f.store}
	positive, err := r.Retrieve(Query{Tag: "math", IncludeCandidates: true, FollowRelations: []relation.Type{relation.Supports}})
	if err != nil || len(positive.Items) != 2 || positive.Items[0].Knowledge.ID != f.itemA || positive.Items[1].Knowledge.ID != f.itemB || len(positive.Relations) != 1 || positive.Relations[0].ID != "rel-a-b" {
		t.Fatalf("positive one-hop result = %#v, err=%v", positive, err)
	}
	for i := 0; i < 3; i++ {
		repeated, repeatErr := r.Retrieve(Query{Tag: "math", IncludeCandidates: true, FollowRelations: []relation.Type{relation.Supports}})
		if repeatErr != nil || !reflect.DeepEqual(positive, repeated) {
			t.Fatalf("one-hop result was not deterministic: %#v vs %#v", positive, repeated)
		}
	}
	ks := knowledge.Service{Store: f.store}
	b, err := knowledge.Replay(f.store.List("knowledge", f.itemB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Supersede(b, knowledge.KnowledgeItem{ID: "k-b-new", Kind: knowledge.Claim, Statement: "new", SourceMemoryID: f.memoryID, Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	result, err := r.Retrieve(Query{Kind: knowledge.Claim, IncludeCandidates: true, FollowRelations: []relation.Type{relation.Supports}})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.Knowledge.ID == f.itemB {
			t.Fatal("superseded opposite endpoint expanded")
		}
	}
	if len(result.Relations) != 0 {
		t.Fatalf("relation with excluded endpoint returned: %#v", result.Relations)
	}
}

func TestResultFeedsTraceFRRHZ051(t *testing.T) {
	f := makeFixture(t)
	result, err := (Deterministic{Store: f.store}).Retrieve(Query{IncludeCandidates: true, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := (trace.Service{Store: f.store}).Create(trace.Trace{ID: "trace-1", Query: "retrieve", RetrievedItemIDs: result.KnowledgeIDs(), UsedItemIDs: []string{f.itemA}, MissionID: f.missionID, TraversedRelationIDs: result.RelationIDs()})
	if err != nil {
		t.Fatalf("trace integration failed: %v", err)
	}
	if tr.ID != "trace-1" {
		t.Fatal("trace not recorded")
	}
}

func TestCorruptionAndNilStore(t *testing.T) {
	f := makeFixture(t)
	if err := f.store.Append(0, events.Event{AggregateType: "knowledge", AggregateID: "bad", Revision: 1, Type: "knowledge.candidate", Payload: []byte(`{"id":"bad","kind":"claim","statement":"","source_memory_id":"mem-1","confidence":0.5}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Deterministic{Store: f.store}).Retrieve(Query{IncludeCandidates: true}); err == nil {
		t.Fatal("expected corrupt knowledge error")
	}
	var nilRetriever Deterministic
	if _, err := nilRetriever.Retrieve(Query{}); err == nil {
		t.Fatal("expected nil store error")
	}
}
