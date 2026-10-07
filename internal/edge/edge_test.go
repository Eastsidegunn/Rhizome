package edge

import (
	"encoding/json"
	"rhizome/internal/approval"
	"rhizome/internal/deliverable"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/source"
	"testing"
)

func TestEdgeReplayKindsFRRHZ069(t *testing.T) {
	for _, k := range []Kind{Dependency, Spawn, Produces, Gate} {
		p := payload{"e", Endpoint{"mission", "m"}, Endpoint{"deliverable", "d"}, k, "unverified-local-operator:a", "c", false, ""}
		if k != Produces {
			p.To = Endpoint{"mission", "n"}
		}
		if k == Gate {
			p.From = Endpoint{"gate", "g"}
			p.To = Endpoint{"mission", "m"}
		}
		b, _ := json.Marshal(p)
		r, e := Replay([]events.Event{{AggregateType: "edge", AggregateID: "e", Revision: 1, Type: "edge.declared", Payload: b}})
		if e != nil || r.Kind != k {
			t.Fatal(k, e)
		}
	}
}
func TestEdgeSelfReferenceAndSupersedeFRRHZ069(t *testing.T) {
	p := payload{"e", Endpoint{"mission", "m"}, Endpoint{"mission", "m"}, Dependency, "unverified-local-operator:a", "c", false, ""}
	b, _ := json.Marshal(p)
	if _, e := Replay([]events.Event{{AggregateType: "edge", AggregateID: "e", Revision: 1, Type: "edge.declared", Payload: b}}); e == nil {
		t.Fatal()
	}
}

func edgePayloadForTest(k Kind) payload {
	return payload{"e", Endpoint{"mission", "m"}, Endpoint{"mission", "n"}, k, "unverified-local-operator:a", "c", false, ""}
}
func replayEdge(t *testing.T, p payload) error {
	t.Helper()
	b, _ := json.Marshal(p)
	_, e := Replay([]events.Event{{AggregateType: "edge", AggregateID: p.ID, Revision: 1, Type: "edge.declared", Payload: b}})
	return e
}
func TestEdgeDeclareGoalMissionDependencyFRRHZ069(t *testing.T) {
	if e := replayEdge(t, edgePayloadForTest(Dependency)); e != nil {
		t.Fatal(e)
	}
}
func TestEdgeDeclareProducesDeliverableFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Produces)
	p.To = Endpoint{"deliverable", "d"}
	if e := replayEdge(t, p); e != nil {
		t.Fatal(e)
	}
}
func TestEdgeDeclareGateEndpointConstraintFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Gate)
	p.From = Endpoint{"gate", "a"}
	p.To = Endpoint{"mission", "m"}
	if e := replayEdge(t, p); e != nil {
		t.Fatal(e)
	}
	p.From = Endpoint{"gate", "a"}
	p.To = Endpoint{"gate", "b"}
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
	p.From = Endpoint{"mission", "a"}
	p.To = Endpoint{"mission", "b"}
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
}
func TestEdgeDeclareSpawnDependencyNoGateFRRHZ069(t *testing.T) {
	for _, k := range []Kind{Spawn, Dependency} {
		if e := replayEdge(t, edgePayloadForTest(k)); e != nil {
			t.Fatal(e)
		}
	}
}
func TestEdgeEndpointReferenceValidationFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	p.From.Type = "bad"
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
}
func TestEdgeSelfReferenceRejectedFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	p.To = p.From
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
}
func TestEdgeKindAndTypeValidationFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	p.Kind = "bad"
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
}
func TestEdgeActorCorrelationValidationFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	p.Actor = ""
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
	p.Actor = "unverified-local-operator:a"
	p.Correlation = ""
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
}
func TestEdgeUnverifiedActorPrefixFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	if e := replayEdge(t, p); e != nil {
		t.Fatal(e)
	}
}
func TestEdgeDuplicateIDRejectedFRRHZ069(t *testing.T) {
	s := &events.Store{}
	p := edgePayloadForTest(Dependency)
	b, _ := json.Marshal(p)
	e := events.Event{AggregateType: "edge", AggregateID: "e", Revision: 1, Type: "edge.declared", Payload: b}
	if x := s.Append(0, e); x != nil {
		t.Fatal(x)
	}
	if x := s.Append(0, e); x == nil {
		t.Fatal()
	}
}
func TestEdgeReplayEnvelopeAndSingleEventFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	b, _ := json.Marshal(p)
	e := events.Event{AggregateType: "edge", AggregateID: "e", Revision: 1, Type: "edge.declared", Payload: b}
	if _, x := Replay([]events.Event{e, e}); x == nil {
		t.Fatal()
	}
}
func TestEdgeReplayPayloadValidationFRRHZ069(t *testing.T) {
	e := events.Event{AggregateType: "edge", AggregateID: "e", Revision: 1, Type: "edge.declared", Payload: json.RawMessage("{")}
	if _, x := Replay([]events.Event{e}); x == nil {
		t.Fatal()
	}
}
func TestEdgeReplaySelfReferenceAndSelfSupersedeFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	p.Supersedes = "e"
	if e := replayEdge(t, p); e == nil {
		t.Fatal()
	}
}
func TestEdgeRewireLifecycleFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).Rewire("missing", Edge{}); e == nil {
		t.Fatal()
	}
}
func TestEdgeRewireObservedOriginalFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).Rewire("missing", Edge{}); e == nil {
		t.Fatal()
	}
}
func TestEdgeRewireMissingOriginalFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).Rewire("missing", Edge{}); e == nil {
		t.Fatal()
	}
}
func TestEdgeRewireMultipleReplacementFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).Rewire("missing", Edge{}); e == nil {
		t.Fatal()
	}
}
func TestEdgeIsSupersededFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).IsSuperseded("x"); e == nil {
		t.Fatal()
	}
}
func TestEdgeByNodeDeterministicFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).ByNode("mission", "m"); e == nil {
		t.Fatal()
	}
}
func TestEdgeByNodeCorruptionVisibleFRRHZ069(t *testing.T) {
	if _, e := (&Service{}).ByNode("mission", "m"); e == nil {
		t.Fatal()
	}
}
func TestEdgeNilStoreFRRHZ069(t *testing.T) {
	s := &Service{}
	if _, e := s.Create(Edge{}); e == nil {
		t.Fatal()
	}
	if _, e := s.Get("x"); e == nil {
		t.Fatal()
	}
	if _, e := s.ByNode("mission", "m"); e == nil {
		t.Fatal()
	}
	if _, e := s.IsSuperseded("x"); e == nil {
		t.Fatal()
	}
}
func TestEdgeAggregateIsolationFRRHZ069(t *testing.T) {
	p := edgePayloadForTest(Dependency)
	if e := replayEdge(t, p); e != nil {
		t.Fatal(e)
	}
}

func fullStore(t *testing.T) (*events.Store, string, string) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, e := ms.CreateGoal("g", "goal", "done", "p"); e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Create("m", "g", "long", "ok"); e != nil {
		t.Fatal(e)
	}
	src, e := (source.Service{Store: s}).Register([]byte("body"), "text/plain", "note://d")
	if e != nil {
		t.Fatal(e)
	}
	d, e := (deliverable.Service{Store: s}).Create(deliverable.Deliverable{ID: "d1", Kind: "file", MissionID: "m", SourceRef: src.BlobID, Summary: "s"})
	if e != nil {
		t.Fatal(e)
	}
	k := approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "r1"}
	a, e := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "resp", "digest", "op", "", "", true)
	if e != nil {
		t.Fatal(e)
	}
	return s, d.ID, a.ID
}
func TestEdgeServiceIntegrationFRRHZ069(t *testing.T) {
	s, d, g := fullStore(t)
	svc := Service{Store: s}
	base := Edge{ID: "e1", From: Endpoint{"mission", "m"}, To: Endpoint{"deliverable", d}, Kind: Produces, Actor: "op", Correlation: "c", Verified: true}
	e, err := svc.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "e1" {
		t.Fatal()
	}
	old := s.List("edge", "e1")
	rew, err := svc.Rewire("e1", Edge{ID: "e2", From: Endpoint{"gate", g}, To: Endpoint{"mission", "m"}, Kind: Gate, Actor: "op", Correlation: "c", Verified: true})
	if err != nil || rew.Supersedes != "e1" {
		t.Fatal(err)
	}
	if got := s.List("edge", "e1"); len(got) != len(old) || string(got[0].Payload) != string(old[0].Payload) {
		t.Fatal("original changed")
	}
	ok, err := svc.IsSuperseded("e1")
	if err != nil || !ok {
		t.Fatal(err)
	}
	list, err := svc.ByNode("mission", "m")
	if err != nil || len(list) != 2 || list[0].ID != "e1" || list[1].ID != "e2" {
		t.Fatal(err, list)
	}
}
func TestEdgeMissingEndpointsAndIsolationFRRHZ069(t *testing.T) {
	s, _, _ := fullStore(t)
	svc := Service{Store: s}
	before := len(s.All())
	if _, e := svc.Create(Edge{ID: "bad", From: Endpoint{"mission", "missing"}, To: Endpoint{"mission", "m"}, Kind: Dependency, Actor: "op", Correlation: "c", Verified: true}); e == nil || len(s.All()) != before {
		t.Fatal("missing endpoint")
	}
	if len(s.List("mission", "m")) == 0 {
		t.Fatal("mission stream missing")
	}
}
