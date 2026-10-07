package trace

import (
	"testing"

	"rhizome/internal/decision"
	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/relation"
	"rhizome/internal/source"
)

type fixtures struct {
	store      *events.Store
	goalID     string
	missionID  string
	memoryID   string
	itemID     string
	decisionID string
	relationID string
	sourceID   string
}

func makeFixtures(t *testing.T, withMemoryEvidence bool) fixtures {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-1", "goal", "success", "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-1", "goal-1", "mission", "success"); err != nil {
		t.Fatal(err)
	}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "fact", SourceType: "note", SourceID: "note-1", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	k, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-1", Kind: knowledge.Claim, Statement: "claim", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (relation.Service{Store: s}).Create(relation.Relation{ID: "rel-1", From: k.ID, Type: relation.Supports, To: k.ID + "-other", SourceMemoryIDs: []string{m.ID}, Confidence: .5})
	if err == nil {
		// The missing endpoint is expected here; create a valid second item and retry.
		t.Fatal("fixture relation unexpectedly succeeded")
	}
	k2, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-1-other", Kind: knowledge.Claim, Statement: "other", SourceMemoryID: m.ID, Confidence: .8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (relation.Service{Store: s}).Create(relation.Relation{ID: "rel-1", From: k.ID, Type: relation.Supports, To: k2.ID, SourceMemoryIDs: []string{m.ID}, Confidence: .5}); err != nil {
		t.Fatal(err)
	}
	src, err := (source.Service{Store: s}).Register([]byte("raw"), "text/plain", "note://raw")
	if err != nil {
		t.Fatal(err)
	}
	evidence := []decision.Evidence{{SourceType: "mission", SourceID: "mission-1"}}
	if withMemoryEvidence {
		evidence = append(evidence, decision.Evidence{SourceType: "memory", SourceID: m.ID})
	}
	d, err := (decision.Service{Store: s}).Create(decision.Decision{ID: "decision-1", MissionID: "mission-1", Kind: decision.AwaitResult, Reason: "inspect", Evidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	return fixtures{store: s, goalID: "goal-1", missionID: "mission-1", memoryID: m.ID, itemID: k.ID, decisionID: d.ID, relationID: "rel-1", sourceID: src.BlobID}
}

func baseTrace(f fixtures) Trace {
	return Trace{ID: "trace-1", Query: "why", RetrievedItemIDs: []string{f.itemID}, UsedItemIDs: []string{f.itemID}, TraversedRelationIDs: []string{f.relationID}, OpenedSourceIDs: []string{f.sourceID}, GoalID: f.goalID, MissionID: f.missionID}
}

func TestTraceCreatePreservesStreamsFRRHZ048(t *testing.T) {
	f := makeFixtures(t, false)
	before := len(f.store.All())
	tr, err := (Service{Store: f.store}).Create(baseTrace(f))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Revision != 1 || len(f.store.List("trace", tr.ID)) != 1 {
		t.Fatalf("trace = %#v", tr)
	}
	if len(f.store.All()) != before+1 || len(f.store.List("knowledge", f.itemID)) != 1 || len(f.store.List("relation", f.relationID)) != 1 || len(f.store.List("source", f.sourceID)) != 1 || len(f.store.List("decision", f.decisionID)) != 1 || len(f.store.List("mission", f.missionID)) != 1 {
		t.Fatal("trace changed a referenced stream")
	}
}

func TestSetConstraintsFRRHZ048(t *testing.T) {
	f := makeFixtures(t, false)
	base := baseTrace(f)
	cases := []Trace{
		func() Trace { x := base; x.UsedItemIDs = []string{"missing"}; return x }(),
		func() Trace { x := base; x.RejectedItemIDs = []string{"missing"}; return x }(),
		func() Trace { x := base; x.RejectedItemIDs = []string{f.itemID}; return x }(),
	}
	for i, tr := range cases {
		tr.ID = "bad-" + string(rune('a'+i))
		before := len(f.store.All())
		if _, err := (Service{Store: f.store}).Create(tr); err == nil {
			t.Fatalf("case %d: expected set error", i)
		}
		if len(f.store.All()) != before {
			t.Fatalf("case %d changed log", i)
		}
	}
}

func TestDecisionMemoryEvidenceLinkFRRHZ049(t *testing.T) {
	f := makeFixtures(t, true)
	tr := baseTrace(f)
	tr.DecisionID = f.decisionID
	if _, err := (Service{Store: f.store}).Create(tr); err != nil {
		t.Fatalf("valid memory evidence trace rejected: %v", err)
	}
	f2 := makeFixtures(t, true)
	bad := baseTrace(f2)
	bad.ID = "trace-bad"
	bad.DecisionID = f2.decisionID
	bad.UsedItemIDs = nil
	if _, err := (Service{Store: f2.store}).Create(bad); err == nil {
		t.Fatal("expected missing used memory evidence error")
	}
	f3 := makeFixtures(t, false)
	missionOnly := baseTrace(f3)
	missionOnly.ID = "trace-mission-only"
	missionOnly.DecisionID = f3.decisionID
	if _, err := (Service{Store: f3.store}).Create(missionOnly); err != nil {
		t.Fatalf("mission evidence incorrectly constrained: %v", err)
	}
}

func TestReferencesAndDuplicateAreAtomic(t *testing.T) {
	f := makeFixtures(t, false)
	base := baseTrace(f)
	for i, mutate := range []func(*Trace){
		func(x *Trace) { x.RetrievedItemIDs = []string{"missing"} },
		func(x *Trace) { x.TraversedRelationIDs = []string{"missing"} },
		func(x *Trace) { x.OpenedSourceIDs = []string{"missing"} },
		func(x *Trace) { x.DecisionID = "missing" },
	} {
		tr := base
		tr.ID = "bad-ref-" + string(rune('a'+i))
		mutate(&tr)
		before := len(f.store.All())
		if _, err := (Service{Store: f.store}).Create(tr); err == nil {
			t.Fatalf("case %d: expected reference error", i)
		}
		if len(f.store.All()) != before {
			t.Fatalf("case %d changed log", i)
		}
	}
	if _, err := (Service{Store: f.store}).Create(base); err != nil {
		t.Fatal(err)
	}
	before := len(f.store.All())
	if _, err := (Service{Store: f.store}).Create(base); err == nil {
		t.Fatal("expected duplicate trace error")
	}
	if len(f.store.All()) != before {
		t.Fatal("duplicate trace changed log")
	}
}

func TestQueriesReplayAndNilStore(t *testing.T) {
	f := makeFixtures(t, false)
	service := Service{Store: f.store}
	for _, id := range []string{"trace-2", "trace-1"} {
		tr := baseTrace(f)
		tr.ID = id
		if _, err := service.Create(tr); err != nil {
			t.Fatal(err)
		}
	}
	byMission, err := service.ByMission(f.missionID)
	if err != nil || len(byMission) != 2 || byMission[0].ID != "trace-1" || byMission[1].ID != "trace-2" {
		t.Fatalf("ByMission = %#v, err=%v", byMission, err)
	}
	byDecision, err := service.ByDecision(f.decisionID)
	if err != nil || len(byDecision) != 0 {
		t.Fatalf("ByDecision = %#v, err=%v", byDecision, err)
	}
	log := f.store.List("trace", "trace-1")
	log = append(log, events.Event{AggregateType: "trace", AggregateID: "trace-1", Revision: 2, Type: "trace.recorded", Payload: log[0].Payload})
	if _, err := Replay(log); err == nil {
		t.Fatal("expected second event rejection")
	}
	if err := f.store.Append(0, events.Event{AggregateType: "trace", AggregateID: "bad", Revision: 1, Type: "trace.recorded", Payload: []byte(`{"trace_id":"bad","query":"","retrieved_item_ids":["k-1"]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ByMission(f.missionID); err == nil {
		t.Fatal("expected corrupt trace error")
	}
	var nilService Service
	if _, err := nilService.Create(Trace{}); err == nil {
		t.Fatal("expected nil create error")
	}
	if _, err := nilService.ByMission("m"); err == nil {
		t.Fatal("expected nil mission query error")
	}
	if _, err := nilService.ByDecision("d"); err == nil {
		t.Fatal("expected nil decision query error")
	}
}
