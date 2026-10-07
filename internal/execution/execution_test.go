package execution

import (
	"encoding/json"
	"rhizome/internal/events"
	"rhizome/internal/policy"
	"testing"
)

func missionStore() *events.Store {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m1", "g", "work", "done"})
	_ = s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", Payload: p})
	return s
}

func intent(s Service, id, key string) (Ref, error) {
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	return s.IntentWithPolicy(id, key, p, p)
}
func TestIntentAcceptObserve(t *testing.T) {
	s := Service{Store: missionStore()}
	r, e := intent(s, "m1", "k1")
	if e != nil {
		t.Fatal(e)
	}
	if r.ExternalID != "" {
		t.Fatal("external before accept")
	}
	r, e = s.ClaimDispatch(r.ID, "janus", "corr-1")
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Accept(r.ID, "ext")
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Observe(r.ID, "0000000000000000001", "done", "hx:1", false)
	if e != nil || r.State != Succeeded {
		t.Fatalf("%+v %v", r, e)
	}
	if _, e = s.Observe(r.ID, "0000000000000000001", "dup", "hx:2", false); e == nil {
		t.Fatal("nonmonotonic cursor accepted")
	}
}
func TestDuplicateKeyRejected(t *testing.T) {
	s := Service{Store: missionStore()}
	if _, e := intent(s, "m1", "k"); e != nil {
		t.Fatal(e)
	}
	if _, e := intent(s, "m2", "k"); e == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestDispatchClaimFRRHZ060(t *testing.T) {
	s := Service{Store: missionStore()}
	r, e := intent(s, "m1", "claim-key")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Accept(r.ID, "ext"); e == nil {
		t.Fatal("accept without claim")
	}
	r, e = s.ClaimDispatch(r.ID, "janus", "corr")
	if e != nil {
		t.Fatal(e)
	}
	before := len(s.Store.All())
	if _, e = s.ClaimDispatch(r.ID, "janus", "corr"); e == nil {
		t.Fatal("duplicate claim")
	}
	if len(s.Store.All()) != before {
		t.Fatal("log mutated")
	}
}

func TestBindingAndCursorFRRHZ061(t *testing.T) {
	s := Service{Store: missionStore()}
	r, _ := intent(s, "m1", "bind-key")
	r, _ = s.ClaimDispatch(r.ID, "janus", "c")
	r, _ = s.Accept(r.ID, "0123456789abcdef0123456789abcdef")
	b := Binding{SessionDB: "/tmp/session.db", TraceID: r.ExternalID, PolicyHash: "p", RequestFingerprint: "f"}
	r, e := s.Bind(r.ID, b)
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.ObserveState(r.ID, "0000000000000000009", "x", "src", Observing)
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.ObserveState(r.ID, "0000000000000000010", "y", "src", Succeeded)
	if e != nil || r.State != Succeeded {
		t.Fatal(e)
	}
}

func TestUnknownClassesFRRHZ060(t *testing.T) {
	classes := []UnknownClass{ExternalEffectPossible, ObservationDelay, DeadlineNear, Repeated, NeedsHuman}
	for _, c := range classes {
		s := Service{Store: missionStore()}
		r, _ := intent(s, "m1", string(c))
		r, _ = s.ClaimDispatch(r.ID, "janus", "c")
		r, _ = s.Accept(r.ID, "ext")
		incident := ""
		if c == Repeated {
			incident = "inc"
		}
		r, e := s.MarkUnknown(r.ID, c, incident, "ref", "uncertain")
		if e != nil || r.State != Unknown {
			t.Fatalf("%s: %v", c, e)
		}
		if _, e = s.MarkUnknown(r.ID, "", "", "ref", "x"); e == nil {
			t.Fatal("missing class accepted")
		}
	}
}

func TestUnknownResolutionFRRHZ060(t *testing.T) {
	s := Service{Store: missionStore()}
	r, _ := intent(s, "m1", "resolve")
	r, _ = s.ClaimDispatch(r.ID, "janus", "c")
	r, _ = s.Accept(r.ID, "ext")
	r, _ = s.MarkUnknown(r.ID, ObservationDelay, "", "ref", "x")
	if _, e := s.ObserveState(r.ID, "0000000000000000001", "", "", Succeeded); e == nil {
		t.Fatal("unknown succeeded")
	}
	if _, e := s.ResolveUnknown(r.ID, Failed, ""); e == nil {
		t.Fatal("human id required")
	}
	var e error
	r, e = s.ResolveUnknown(r.ID, Failed, "decision-1")
	if e != nil || r.State != Failed {
		t.Fatal(e)
	}
}

func TestObserveUnknownRequiresClassificationFRRHZ060(t *testing.T) {
	s := Service{Store: missionStore()}
	r, _ := intent(s, "m1", "observe-unknown")
	r, _ = s.ClaimDispatch(r.ID, "janus", "c")
	r, _ = s.Accept(r.ID, "ext")
	before := len(s.Store.All())
	if _, e := s.ObserveState(r.ID, "0000000000000000001", "", "", Unknown); e == nil {
		t.Fatal("unclassified unknown accepted")
	}
	if len(s.Store.All()) != before {
		t.Fatal("log mutated")
	}
}

func TestClaimUnknownReconcileAdoptsExternalIDFRRHZ060(t *testing.T) {
	s := Service{Store: missionStore()}
	r, _ := intent(s, "m1", "reconcile-id")
	r, _ = s.ClaimDispatch(r.ID, "janus", "c")
	r, _ = s.MarkUnknown(r.ID, ObservationDelay, "", "ref", "x")
	b := Binding{SessionDB: "/tmp/session.db", TraceID: "0123456789abcdef0123456789abcdef", PolicyHash: "p", RequestFingerprint: "f"}
	r, e := s.Reconcile(r.ID, ReconciliationEvidence{Binding: b, Target: "janus", IdempotencyKey: "reconcile-id", Cursor: "0000000000000000001", SourceRef: "obs"})
	if e != nil {
		t.Fatal(e)
	}
	if r.ExternalID != b.TraceID {
		t.Fatalf("external id not adopted: %q", r.ExternalID)
	}
}

func TestReconcileReplayExternalIDMismatchFRRHZ060(t *testing.T) {
	s := Service{Store: missionStore()}
	r, _ := intent(s, "m1", "tamper-reconcile")
	r, _ = s.ClaimDispatch(r.ID, "janus", "c")
	r, _ = s.MarkUnknown(r.ID, ObservationDelay, "", "ref", "x")
	b := Binding{SessionDB: "/tmp/session.db", TraceID: "0123456789abcdef0123456789abcdef", PolicyHash: "p", RequestFingerprint: "f"}
	_, e := s.Reconcile(r.ID, ReconciliationEvidence{Binding: b, Target: "janus", IdempotencyKey: "tamper-reconcile", Cursor: "0000000000000000001", SourceRef: "obs"})
	if e != nil {
		t.Fatal(e)
	}
	log := s.Store.List("execution", r.ID)
	var p reconcilePayload
	if e = json.Unmarshal(log[len(log)-1].Payload, &p); e != nil {
		t.Fatal(e)
	}
	p.ExternalID = "evil"
	log[len(log)-1].Payload, _ = json.Marshal(p)
	if _, e = Replay(log); e == nil {
		t.Fatal("tampered reconciled external id accepted")
	}
}
