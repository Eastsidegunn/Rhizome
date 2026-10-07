package deliverable

import (
	"encoding/json"
	"rhizome/internal/events"
	"rhizome/internal/source"
	"testing"
)

func missionStore() *events.Store {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "d", "s"})
	_ = s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: p})
	return s
}
func TestDeliverableValidationFRRHZ068(t *testing.T) {
	s := missionStore()
	svc := Service{Store: s}
	if _, e := svc.Create(Deliverable{ID: "d", Kind: "file", MissionID: "m", SourceRef: "bad:x", Summary: "sum"}); e == nil {
		t.Fatal("bad source accepted")
	}
	if _, e := svc.Create(Deliverable{ID: "d", Kind: "", MissionID: "m", SourceRef: "bad:x", Summary: "sum"}); e == nil {
		t.Fatal("bad kind accepted")
	}
}
func TestDeliverableReplayShapeFRRHZ068(t *testing.T) {
	p, _ := json.Marshal(payload{ID: "d", Kind: "file", MissionID: "m", SourceRef: "sha256:x", Summary: "sum"})
	e := events.Event{AggregateType: "deliverable", AggregateID: "d", Revision: 1, Type: "deliverable.declared", Payload: p}
	if _, err := Replay([]events.Event{e, e}); err == nil {
		t.Fatal("second event accepted")
	}
}

func TestSourceRefReplayAndMutationFRRHZ068(t *testing.T) {
	s := missionStore()
	src, e := source.Service{Store: s}.Register([]byte("body"), "text/plain", "note://x")
	if e != nil {
		t.Fatal(e)
	}
	svc := Service{Store: s}
	d, e := svc.Create(Deliverable{ID: "db", Kind: "file", MissionID: "m", SourceRef: src.BlobID, Summary: "summary"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Replay(s.List("deliverable", d.ID)); e != nil {
		t.Fatal(e)
	}
	before := len(s.All())
	if _, e = svc.Create(Deliverable{ID: "bad", Kind: "file", MissionID: "missing", SourceRef: src.BlobID, Summary: "summary"}); e == nil || len(s.All()) != before {
		t.Fatal("missing mission")
	}
	log := s.List("deliverable", d.ID)
	var p payload
	_ = json.Unmarshal(log[0].Payload, &p)
	p.SourceRef = "bad:x"
	log[0].Payload, _ = json.Marshal(p)
	if _, e = Replay(log); e == nil {
		t.Fatal("bad source replay")
	}
}
func TestTerminalAndDuplicateDeliverableFRRHZ068(t *testing.T) {
	s := missionStore()
	_ = s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"ready"}`)})
	_ = s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"running"}`)})
	_ = s.Append(3, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 4, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"succeeded","DecisionID":"d"}`)})
	sref := source.Service{Store: s}
	src, _ := sref.Register([]byte("x"), "text/plain", "u")
	svc := Service{Store: s}
	d, e := svc.Create(Deliverable{ID: "term", Kind: "f", MissionID: "m", SourceRef: src.BlobID, Summary: "sum"})
	if e != nil {
		t.Fatal(e)
	}
	before := len(s.All())
	if _, e = svc.Create(d); e == nil || len(s.All()) != before {
		t.Fatal("duplicate")
	}
	if d.ID == "" {
		t.Fatal()
	}
}
func TestDeliverableNilAndByMissionFRHZ068(t *testing.T) {
	var s Service
	if _, e := s.Create(Deliverable{}); e == nil {
		t.Fatal()
	}
	if _, e := s.Get("x"); e == nil {
		t.Fatal()
	}
	if _, e := s.ByMission("m"); e == nil {
		t.Fatal()
	}
}
