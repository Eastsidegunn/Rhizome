package evaluation

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
	store                    *events.Store
	missionID                string
	memoryID                 string
	a, b, c                  string
	fails, applies, supports string
}

func fixtures(t *testing.T) fixture {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem", Kind: memory.Fact, Content: "fact", SourceType: "note", SourceID: "n", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	ks := knowledge.Service{Store: s}
	ids := []string{"a", "b", "c"}
	items := make([]knowledge.KnowledgeItem, 3)
	for i, id := range ids {
		items[i], err = ks.Create(knowledge.KnowledgeItem{ID: id, Kind: knowledge.Claim, Statement: id, SourceMemoryID: m.ID, Confidence: .8})
		if err != nil {
			t.Fatal(err)
		}
	}
	rs := relation.Service{Store: s}
	makeRel := func(id string, typ relation.Type, from, to string) {
		if _, e := rs.Create(relation.Relation{ID: id, From: from, Type: typ, To: to, SourceMemoryIDs: []string{m.ID}, Confidence: .7}); e != nil {
			t.Fatal(e)
		}
	}
	makeRel("r-fail", relation.FailsWhen, "a", "b")
	makeRel("r-apply", relation.AppliesWhen, "a", "c")
	makeRel("r-support", relation.Supports, "b", "c")
	return fixture{store: s, missionID: "m", memoryID: m.ID, a: items[0].ID, b: items[1].ID, c: items[2].ID, fails: "r-fail", applies: "r-apply", supports: "r-support"}
}

func TestCaseGeneratorsFRRHZ052(t *testing.T) {
	f := fixtures(t)
	r, err := RelationRecallCase(f.store, f.fails)
	if err != nil || len(r.RequiredItemIDs) != 2 || len(r.RequiredRelationIDs) != 1 {
		t.Fatalf("recall=%#v err=%v", r, err)
	}
	a, err := ApplicationCase(f.store, f.applies)
	if err != nil || a.ExpectedOutcome != "applicable" {
		t.Fatalf("application=%#v err=%v", a, err)
	}
	failsApplication, err := ApplicationCase(f.store, f.fails)
	if err != nil || failsApplication.ExpectedOutcome != "not_applicable" {
		t.Fatalf("fails application=%#v err=%v", failsApplication, err)
	}
	p, err := PathReconstructionCase(f.store, f.fails, f.supports)
	if err != nil || len(p.RequiredItemIDs) != 3 || len(p.RequiredRelationIDs) != 2 {
		t.Fatalf("path=%#v err=%v", p, err)
	}
	if _, err := ApplicationCase(f.store, f.supports); err == nil {
		t.Fatal("expected non-applies rejection")
	}
	if _, err := PathReconstructionCase(f.store, f.fails, f.applies); err != nil {
		t.Fatalf("shared endpoint path rejected: %v", err)
	}
	if _, err := RelationRecallCase(f.store, "missing"); err == nil {
		t.Fatal("expected corrupt reference rejection")
	}
	if _, err := PathReconstructionCase(f.store, f.fails, "missing"); err == nil {
		t.Fatal("expected missing path relation rejection")
	}
}

func TestPathDirectionAndNonChainFRRHZ052(t *testing.T) {
	f := fixtures(t)
	path, err := PathReconstructionCase(f.store, f.fails, f.applies)
	if err != nil {
		t.Fatal(err)
	}
	if len(path.RequiredItemIDs) != 3 {
		t.Fatalf("directional shared endpoint not accepted: %#v", path)
	}
	if _, err := PathReconstructionCase(f.store, f.fails, "r-support"); err != nil {
		t.Fatal(err)
	}
	// Add a disjoint relation pair and ensure they are rejected as a path.
	if _, err := (knowledge.Service{Store: f.store}).Create(knowledge.KnowledgeItem{ID: "d", Kind: knowledge.Claim, Statement: "d", SourceMemoryID: f.memoryID, Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	if _, err := (relation.Service{Store: f.store}).Create(relation.Relation{ID: "r-disjoint", From: "d", Type: relation.Supports, To: "c", SourceMemoryIDs: []string{f.memoryID}, Confidence: .5}); err != nil {
		t.Fatal(err)
	}
	if _, err := PathReconstructionCase(f.store, f.fails, "r-disjoint"); err == nil {
		t.Fatal("expected disjoint path rejection")
	}
}

func TestScoresFRRHZ053(t *testing.T) {
	f := fixtures(t)
	c, err := RelationRecallCase(f.store, f.fails)
	if err != nil {
		t.Fatal(err)
	}
	full, err := Score(c, trace.Trace{UsedItemIDs: []string{f.a, f.b}, TraversedRelationIDs: []string{f.fails}})
	if err != nil {
		t.Fatal(err)
	}
	if full.EvidenceRecall != 1 || full.EvidencePrecision != 1 || full.NodeCoverage != 1 || full.EdgeCoverage != 1 || !full.ConditionOK {
		t.Fatalf("full score=%#v", full)
	}
	partial, err := Score(c, trace.Trace{UsedItemIDs: []string{f.a}, TraversedRelationIDs: []string{f.fails}})
	if err != nil {
		t.Fatal(err)
	}
	if partial.EvidenceRecall != .5 || partial.EvidencePrecision != 1 || partial.NodeCoverage != .5 || partial.EdgeCoverage != 1 {
		t.Fatalf("partial score=%#v", partial)
	}
	precision, err := Score(c, trace.Trace{UsedItemIDs: []string{f.a, "unrelated"}})
	if err != nil {
		t.Fatal(err)
	}
	if precision.EvidenceRecall != .5 || precision.EvidencePrecision != .5 {
		t.Fatalf("precision score=%#v", precision)
	}
	zero, err := Score(c, trace.Trace{})
	if err != nil {
		t.Fatal(err)
	}
	if zero.EvidenceRecall != 0 || zero.EvidencePrecision != 0 || zero.EdgeCoverage != 0 {
		t.Fatalf("zero score=%#v", zero)
	}
	app, err := ApplicationCase(f.store, f.applies)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Score(app, trace.Trace{UsedItemIDs: []string{f.a, f.c}, TraversedRelationIDs: []string{f.applies}, Outcome: "applicable"})
	if err != nil || !ok.ConditionOK {
		t.Fatalf("application success=%#v err=%v", ok, err)
	}
	bad, err := Score(app, trace.Trace{UsedItemIDs: []string{f.a, f.c}, TraversedRelationIDs: []string{f.applies}, Outcome: "not_applicable"})
	if err != nil || bad.ConditionOK {
		t.Fatalf("application failure=%#v err=%v", bad, err)
	}
	failsApp, err := ApplicationCase(f.store, f.fails)
	if err != nil {
		t.Fatal(err)
	}
	failsOK, err := Score(failsApp, trace.Trace{UsedItemIDs: []string{f.a, f.b}, TraversedRelationIDs: []string{f.fails}, Outcome: "not_applicable"})
	if err != nil || !failsOK.ConditionOK {
		t.Fatalf("fails_when success=%#v err=%v", failsOK, err)
	}
	failsBad, err := Score(failsApp, trace.Trace{UsedItemIDs: []string{f.a, f.b}, TraversedRelationIDs: []string{f.fails}, Outcome: "applicable"})
	if err != nil || failsBad.ConditionOK {
		t.Fatalf("fails_when mismatch=%#v err=%v", failsBad, err)
	}
	repeat, _ := Score(c, trace.Trace{UsedItemIDs: []string{f.a}, TraversedRelationIDs: []string{f.fails}})
	if !reflect.DeepEqual(partial, repeat) {
		t.Fatal("score is not deterministic")
	}
}

func TestRecordQueriesAndAtomicity(t *testing.T) {
	f := fixtures(t)
	c, err := RelationRecallCase(f.store, f.fails)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := (trace.Service{Store: f.store}).Create(trace.Trace{ID: "t-1", Query: "q", RetrievedItemIDs: []string{f.a, f.b}, UsedItemIDs: []string{f.a, f.b}, TraversedRelationIDs: []string{f.fails}, MissionID: f.missionID})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: f.store}
	before := len(f.store.All())
	ev, err := service.Record("e-2", c, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Scores.EvidenceRecall != 1 || len(f.store.All()) != before+1 {
		t.Fatalf("record=%#v", ev)
	}
	if _, err := service.Record("e-2", c, tr.ID); err == nil {
		t.Fatal("expected duplicate evaluation")
	}
	if len(f.store.All()) != before+1 {
		t.Fatal("duplicate changed log")
	}
	if _, err := service.Record("missing", c, "missing-trace"); err == nil {
		t.Fatal("expected missing trace")
	}
	byTrace, err := service.ByTrace(tr.ID)
	if err != nil || len(byTrace) != 1 {
		t.Fatalf("by trace=%#v err=%v", byTrace, err)
	}
	byKind, err := service.ByKind(RelationRecall)
	if err != nil || len(byKind) != 1 {
		t.Fatalf("by kind=%#v err=%v", byKind, err)
	}
	log := f.store.List("evaluation", "e-2")
	log = append(log, events.Event{AggregateType: "evaluation", AggregateID: "e-2", Revision: 2, Type: "evaluation.recorded", Payload: log[0].Payload})
	if _, err := Replay(log); err == nil {
		t.Fatal("expected second event rejection")
	}
	if err := f.store.Append(0, events.Event{AggregateType: "evaluation", AggregateID: "bad", Revision: 1, Type: "evaluation.recorded", Payload: []byte(`{"evaluation_id":"bad","kind":"relation_recall","question":"","required_item_ids":["a"],"required_relation_ids":["r"],"trace_id":"t"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ByKind(RelationRecall); err == nil {
		t.Fatal("expected corrupt evaluation error")
	}
}

func TestNilStore(t *testing.T) {
	var service Service
	if _, err := service.Record("e", Case{}, "t"); err == nil {
		t.Fatal("expected nil record")
	}
	if _, err := service.ByTrace("t"); err == nil {
		t.Fatal("expected nil by trace")
	}
	if _, err := service.ByKind(RelationRecall); err == nil {
		t.Fatal("expected nil by kind")
	}
}
