package approval

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/events"
	"testing"
)

func TestApprovalFlowFRRHZ062(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "r1"}
	r, e := s.RecordInput(k, Allow, "", "resp", "digest", "alice", "c", "", noAuthority())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Dispatch(r.ID, "resp"); e != nil {
		t.Fatal(e)
	}
	r, e = s.Dispatch(r.ID, "resp")
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Observe(r.ID, 1, Deny, "deadline")
	if e != nil || r.HumanDecision != Allow || r.JanusDecision != Deny {
		t.Fatal(e)
	}
	if _, e = s.Dispatch(r.ID, "resp"); e == nil {
		t.Fatal("redispatch after observed")
	}
}
func TestApprovalValidationFRRHZ062(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"bad", "x", ""}
	if _, e := s.RecordInput(k, Allow, "", "r", "d", "a", "", "", noAuthority()); e == nil {
		t.Fatal("bad key")
	}
	k = RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "r"}
	if _, e := s.RecordInput(k, Deny, "", "r", "d", "a", "", "", noAuthority()); e == nil {
		t.Fatal("deny without reason")
	}
}
func TestApprovalReplayNoRedispatchFRRHZ063(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "x"}
	r, _ := s.RecordInput(k, Allow, "", "r", "d", "a", "", "", noAuthority())
	r, _ = s.Dispatch(r.ID, "r")
	r, _ = s.Observe(r.ID, 2, Allow, "")
	if _, e := s.Dispatch(r.ID, "r"); e == nil {
		t.Fatal("terminal replay redispatch")
	}
}

func TestApprovalValidationMatrixFRRHZ062(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "matrix"}
	if _, e := s.Dispatch("none", "r"); e == nil {
		t.Fatal("dispatch without input")
	}
	if _, e := s.Observe("none", 1, Allow, ""); e == nil {
		t.Fatal("observe without input")
	}
	if _, e := s.RecordInput(k, Allow, "", "r", "", "alice", "", "", noAuthority()); e == nil {
		t.Fatal("missing digest")
	}
	r, e := s.RecordInput(k, Allow, "", "r", "d", "alice", "", "", noAuthority())
	if e != nil {
		t.Fatal(e)
	}
	before := len(s.Store.All())
	if _, e = s.RecordInput(k, Allow, "", "r", "d", "alice", "", "", noAuthority()); e == nil {
		t.Fatal("duplicate input")
	}
	if len(s.Store.All()) != before {
		t.Fatal("log changed")
	}
	if _, e = s.Dispatch(r.ID, "other"); e == nil {
		t.Fatal("response mismatch")
	}
	r, _ = s.Dispatch(r.ID, "r")
	if _, e = s.Observe(r.ID, 1, Deny, ""); e == nil {
		t.Fatal("deny without janus reason")
	}
}
func TestActorAndByTraceFRRHZ062(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "a"}
	r, e := s.RecordInput(k, Allow, "", "r", "d", "operator", "", "", noAuthority())
	if e != nil || r.ActorRef != "unverified-local-operator:operator" {
		t.Fatal(e)
	}
	k.RequestID = "b"
	r, e = s.RecordInput(k, Allow, "", "r2", "d2", "unverified-local-operator:operator", "", "", noAuthority())
	if e != nil || r.ActorRef != "unverified-local-operator:operator" {
		t.Fatal(e)
	}
	got, e := s.ByTrace(k.TraceID)
	if e != nil || len(got) != 2 || got[0].ID > got[1].ID {
		t.Fatal(e, got)
	}
}
func TestNilStoreFRRHZ062(t *testing.T) {
	s := Service{}
	k := RequestKey{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "x"}
	if _, e := s.RecordInput(k, Allow, "", "r", "d", "a", "", "", noAuthority()); e == nil {
		t.Fatal()
	}
	for _, f := range []func() error{func() error { _, e := s.Dispatch("x", "r"); return e }, func() error { _, e := s.Observe("x", 1, Allow, ""); return e }, func() error { _, e := s.Get("x"); return e }, func() error { _, e := s.ByTrace(k.TraceID); return e }} {
		if f() == nil {
			t.Fatal("nil store accepted")
		}
	}
}

func TestObserveBeforeDispatchCrashFRRHZ062(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "crash"}
	r, e := s.RecordInput(k, Allow, "", "r", "d", "a", "", "", noAuthority())
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Observe(r.ID, 1, Deny, "deadline")
	if e != nil || r.State != Observed {
		t.Fatal(e)
	}
}
func TestTamperedDispatchReplayFRRHZ062(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "tamper"}
	r, _ := s.RecordInput(k, Allow, "", "r", "d", "a", "", "", noAuthority())
	r, _ = s.Dispatch(r.ID, "r")
	log := s.Store.List("approval", r.ID)
	var p dispatched
	_ = json.Unmarshal(log[1].Payload, &p)
	p.Decision = Deny
	log[1].Payload, _ = json.Marshal(p)
	if _, e := Replay(log); e == nil {
		t.Fatal("tampered dispatch accepted")
	}
}

func TestGateFieldsFRRHZ066(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"0123456789abcdef0123456789abcdef", "0123456789abcdef", "gate"}
	g := GateFields{GateName: "deploy", GateType: "approval", RequestedAction: "run", RiskTier: "logged", ReasonRequired: true, Request: Request{Target: "prod", RequestedBy: "alice", RequestedAt: "2000-01-01T00:00:00Z"}}
	if _, e := s.RecordInputWithGate(k, Allow, "", "r", "d", "alice", "", "", noAuthority(), g); e == nil {
		t.Fatal("reason required bypass")
	}
	r, e := s.RecordInputWithGate(k, Allow, "because", "r", "d", "alice", "", "", noAuthority(), g)
	if e != nil {
		t.Fatal(e)
	}
	log := s.Store.List("approval", r.ID)
	var p input
	_ = json.Unmarshal(log[0].Payload, &p)
	p.RiskTier = "bad"
	log[0].Payload, _ = json.Marshal(p)
	if _, e = Replay(log); e == nil {
		t.Fatal("bad risk tier replayed")
	}
}

func TestSupersedeLifecycleFRRHZ067(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"11111111111111111111111111111111", "2222222222222222", "old"}
	old, e := s.RecordInput(k, Allow, "", "r", "d", "a", "", "", noAuthority())
	if e != nil {
		t.Fatal(e)
	}
	before := s.Store.List("approval", old.ID)
	nk := RequestKey{"11111111111111111111111111111111", "2222222222222222", "new"}
	g := GateFields{GateName: "g", GateType: "t", RequestedAction: "a", RiskTier: "logged", Request: Request{Target: "x", RequestedBy: "a", RequestedAt: "2000-01-01T00:00:00Z"}}
	n, e := s.Supersede(old.ID, nk, Allow, "", "r2", "d2", "a", "", "", noAuthority(), g)
	if e != nil || n.Supersedes != old.ID {
		t.Fatal(e)
	}
	after := s.Store.List("approval", old.ID)
	if len(before) != len(after) || string(before[0].Payload) != string(after[0].Payload) {
		t.Fatal("original changed")
	}
	ok, e := s.IsSuperseded(old.ID)
	if e != nil || !ok {
		t.Fatal(e)
	}
	if _, e = s.Supersede(n.ID, nk, Allow, "", "r", "d", "a", "", "", noAuthority(), g); e == nil {
		t.Fatal("self supersede")
	}
	var p input
	log := s.Store.List("approval", n.ID)
	_ = json.Unmarshal(log[0].Payload, &p)
	p.Supersedes = n.ID
	log[0].Payload, _ = json.Marshal(p)
	if _, e = Replay(log); e == nil {
		t.Fatal("self supersede replayed")
	}
	_, e = s.Observe(old.ID, 1, Allow, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Supersede(old.ID, RequestKey{"11111111111111111111111111111111", "2222222222222222", "after"}, Allow, "", "r3", "d3", "a", "", "", noAuthority(), g); e != nil {
		t.Fatal("observed supersede rejected")
	}
}
func TestGateFieldRejectionMatrixFRRHZ066(t *testing.T) {
	fields := []func(*GateFields){func(g *GateFields) { g.GateName = "" }, func(g *GateFields) { g.GateType = "" }, func(g *GateFields) { g.RequestedAction = "" }, func(g *GateFields) { g.RiskTier = "bad" }, func(g *GateFields) { g.Urgency = "bad" }, func(g *GateFields) { g.Request.RequestedAt = "bad" }, func(g *GateFields) { g.Request.ExpiresAt = "bad" }, func(g *GateFields) { g.Request.Target = "" }, func(g *GateFields) { g.Request.RequestedBy = "" }, func(g *GateFields) { g.Request.RequestedAt = "" }}
	for i, f := range fields {
		s := Service{Store: &events.Store{}}
		k := RequestKey{"33333333333333333333333333333333", "4444444444444444", fmt.Sprint(i)}
		g := GateFields{GateName: "g", GateType: "t", RequestedAction: "a", RiskTier: "logged", Request: Request{Target: "x", RequestedBy: "a", RequestedAt: "2000-01-01T00:00:00Z"}}
		f(&g)
		if _, e := s.RecordInputWithGate(k, Allow, "", "r", "d", "a", "", "", noAuthority(), g); e == nil || len(s.Store.All()) != 0 {
			t.Fatalf("field %d", i)
		}
	}
}
func TestReasonRequiredTamperedReplayFRHZ066(t *testing.T) {
	s := Service{Store: &events.Store{}}
	k := RequestKey{"55555555555555555555555555555555", "6666666666666666", "rr"}
	g := GateFields{GateName: "g", GateType: "t", RequestedAction: "a", RiskTier: "logged", ReasonRequired: true, Request: Request{Target: "x", RequestedBy: "a", RequestedAt: "2000-01-01T00:00:00Z"}}
	r, e := s.RecordInputWithGate(k, Allow, "ok", "r", "d", "a", "", "", noAuthority(), g)
	if e != nil {
		t.Fatal(e)
	}
	log := s.Store.List("approval", r.ID)
	var p input
	_ = json.Unmarshal(log[0].Payload, &p)
	p.Reason = ""
	log[0].Payload, _ = json.Marshal(p)
	if _, e = Replay(log); e == nil {
		t.Fatal("tampered reason accepted")
	}
}
