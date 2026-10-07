package execution

import (
	"encoding/json"
	"reflect"
	"testing"

	"rhizome/internal/events"
)

func activeExecution(t *testing.T, key string) (Service, Ref) {
	t.Helper()
	s := Service{Store: missionStore()}
	r, e := intent(s, "m1", key)
	if e != nil {
		t.Fatal(e)
	}
	if r, e = s.ClaimDispatch(r.ID, "janus", "corr-1"); e != nil {
		t.Fatal(e)
	}
	if r, e = s.Accept(r.ID, "trace-ext"); e != nil {
		t.Fatal(e)
	}
	return s, r
}

func allEvents(t *testing.T, s Service, id string) []events.Event {
	t.Helper()
	return s.Store.List("execution", id)
}

// Plan A1: durable stop intent, no state transition, service/replay symmetry.
func TestRequestStopDurableFRRHZ076(t *testing.T) {
	s, r := activeExecution(t, "stop-a1")
	got, err := s.RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	log := allEvents(t, s, r.ID)
	if log[len(log)-1].Type != "execution.stop_requested" {
		t.Fatal(log[len(log)-1].Type)
	}
	if !got.StopRequested || got.StopReason != "user" || got.StopActor != "operator" || got.StopEvidenceSeq != 0 || got.StopID != stopIDFor(r.ID) {
		t.Fatalf("%+v", got)
	}
	if got.State != Accepted {
		t.Fatal("stop request must not transition state:", got.State)
	}
	replayed, err := Replay(log)
	if err != nil || !reflect.DeepEqual(got, replayed) {
		t.Fatalf("service/replay asymmetry: %v %+v %+v", err, got, replayed)
	}
	// Observing is also active: a fresh execution advanced to observing accepts.
	s2, r2 := activeExecution(t, "stop-a1b")
	if _, err := s2.ObserveState(r2.ID, "0000000000000000001", "tick", "src", Observing); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.RequestStop(r2.ID, "user", 0, "operator"); err != nil {
		t.Fatal(err)
	}
}

// Plan A2: only accepted/observing accept a stop request.
func TestRequestStopActiveOnlyFRRHZ076(t *testing.T) {
	cases := map[string]func(t *testing.T) (Service, Ref){
		"intent": func(t *testing.T) (Service, Ref) {
			s := Service{Store: missionStore()}
			r, e := intent(s, "m1", "stop-a2-intent")
			if e != nil {
				t.Fatal(e)
			}
			return s, r
		},
		"dispatch_claimed": func(t *testing.T) (Service, Ref) {
			s := Service{Store: missionStore()}
			r, e := intent(s, "m1", "stop-a2-claim")
			if e != nil {
				t.Fatal(e)
			}
			if r, e = s.ClaimDispatch(r.ID, "janus", "corr"); e != nil {
				t.Fatal(e)
			}
			return s, r
		},
		"succeeded": terminalFixture(Succeeded), "failed": terminalFixture(Failed),
		"cancelled": terminalFixture(Cancelled),
		"unknown": func(t *testing.T) (Service, Ref) {
			s, r := activeExecution(t, "stop-a2-unknown")
			r, e := s.MarkUnknown(r.ID, NeedsHuman, "", "src", "lost")
			if e != nil {
				t.Fatal(e)
			}
			return s, r
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s, r := build(t)
			before := allEvents(t, s, r.ID)
			if _, err := s.RequestStop(r.ID, "user", 0, "operator"); err == nil {
				t.Fatal("inactive execution accepted stop request")
			}
			if !reflect.DeepEqual(before, allEvents(t, s, r.ID)) {
				t.Fatal("log changed")
			}
		})
	}
}

func terminalFixture(st State) func(t *testing.T) (Service, Ref) {
	return func(t *testing.T) (Service, Ref) {
		s, r := activeExecution(t, "stop-a2-"+string(st))
		r, e := s.ObserveState(r.ID, "0000000000000000001", "done", "src", st)
		if e != nil {
			t.Fatal(e)
		}
		return s, r
	}
}

// Plan A3: reason enum and actor identity are both validated.
func TestRequestStopReasonAndActorFRRHZ076(t *testing.T) {
	valid := []struct {
		reason string
		seq    int64
	}{{"user", 0}, {"budget_exceeded", 0}, {"policy", 3}, {"parent_done", 0}}
	for _, tc := range valid {
		s, r := activeExecution(t, "stop-a3-"+tc.reason)
		got, err := s.RequestStop(r.ID, tc.reason, tc.seq, "operator")
		if err != nil || got.StopReason != tc.reason {
			t.Fatal(tc.reason, err)
		}
		var p struct{ Reason string }
		log := allEvents(t, s, r.ID)
		if json.Unmarshal(log[len(log)-1].Payload, &p) != nil || p.Reason != tc.reason {
			t.Fatal("reason not preserved verbatim")
		}
	}
	s, r := activeExecution(t, "stop-a3-bad")
	before := allEvents(t, s, r.ID)
	for _, reason := range []string{"", "USER", " user", "malicious", "timeout"} {
		if _, err := s.RequestStop(r.ID, reason, 0, "operator"); err == nil {
			t.Fatalf("reason %q accepted", reason)
		}
	}
	if _, err := s.RequestStop(r.ID, "user", 0, ""); err == nil {
		t.Fatal("empty actor accepted")
	}
	if _, err := s.RequestStop(r.ID, "user", 0, " "); err == nil {
		t.Fatal("blank actor accepted")
	}
	if !reflect.DeepEqual(before, allEvents(t, s, r.ID)) {
		t.Fatal("log changed")
	}
}

// Plan A4 (D5 confirmed): evidence_seq is required for policy, refused otherwise.
func TestRequestStopPolicyEvidenceFRRHZ076(t *testing.T) {
	s, r := activeExecution(t, "stop-a4")
	before := allEvents(t, s, r.ID)
	for _, seq := range []int64{0, -1} {
		if _, err := s.RequestStop(r.ID, "policy", seq, "operator"); err == nil {
			t.Fatal("policy stop without evidence accepted:", seq)
		}
	}
	if _, err := s.RequestStop(r.ID, "user", 7, "operator"); err == nil {
		t.Fatal("evidence_seq on non-policy reason accepted")
	}
	if !reflect.DeepEqual(before, allEvents(t, s, r.ID)) {
		t.Fatal("log changed")
	}
	got, err := s.RequestStop(r.ID, "policy", 7, "operator")
	if err != nil || got.StopEvidenceSeq != 7 {
		t.Fatal(got, err)
	}
	var p struct{ EvidenceSeq int64 }
	log := allEvents(t, s, r.ID)
	if json.Unmarshal(log[len(log)-1].Payload, &p) != nil || p.EvidenceSeq != 7 {
		t.Fatal("evidence seq not preserved")
	}
}

// Plan A5: replay rejects every tampered stream the service would refuse
// (the service and replay apply the same strict validation rules).
func TestStopRequestedReplaySymmetryFRRHZ076(t *testing.T) {
	s, r := activeExecution(t, "stop-a5")
	if _, err := s.RequestStop(r.ID, "user", 0, "operator"); err != nil {
		t.Fatal(err)
	}
	base := allEvents(t, s, r.ID)
	tamper := func(t *testing.T, change func(m map[string]any)) []events.Event {
		t.Helper()
		log := append([]events.Event(nil), base...)
		last := log[len(log)-1]
		var m map[string]any
		if err := json.Unmarshal(last.Payload, &m); err != nil {
			t.Fatal(err)
		}
		change(m)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		last.Payload = raw
		log[len(log)-1] = last
		return log
	}
	cases := map[string]func(m map[string]any){
		"bad_reason":              func(m map[string]any) { m["Reason"] = "malicious" },
		"policy_without_evidence": func(m map[string]any) { m["Reason"] = "policy" },
		"evidence_on_user":        func(m map[string]any) { m["EvidenceSeq"] = 9 },
		"stop_id_mismatch":        func(m map[string]any) { m["StopID"] = "stop-forged" },
		"empty_actor":             func(m map[string]any) { m["Actor"] = "" },
		"identity_change":         func(m map[string]any) { m["MissionID"] = "other" },
		"external_id_change":      func(m map[string]any) { m["ExternalID"] = "other-trace" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Replay(tamper(t, change)); err == nil {
				t.Fatal("tampered stop_requested replayed")
			}
		})
	}
	t.Run("after_intent_only", func(t *testing.T) {
		moved := []events.Event{base[0], base[len(base)-1]}
		moved[1].Revision = 2
		if _, err := Replay(moved); err == nil {
			t.Fatal("stop_requested accepted before acceptance")
		}
	})
	t.Run("duplicate_event", func(t *testing.T) {
		dup := base[len(base)-1]
		dup.Revision++
		if _, err := Replay(append(append([]events.Event(nil), base...), dup)); err == nil {
			t.Fatal("second stop_requested replayed")
		}
	})
}

// Plan A6 (D4 confirmed): same content is a no-event idempotent read,
// different content is a conflict.
func TestRequestStopIdempotenceFRRHZ076(t *testing.T) {
	s, r := activeExecution(t, "stop-a6")
	first, err := s.RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	before := allEvents(t, s, r.ID)
	again, err := s.RequestStop(r.ID, "user", 0, "operator")
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal(again, err)
	}
	if !reflect.DeepEqual(before, allEvents(t, s, r.ID)) {
		t.Fatal("idempotent repeat appended an event")
	}
	for _, tc := range []struct {
		reason string
		seq    int64
		actor  string
	}{{"parent_done", 0, "operator"}, {"policy", 4, "operator"}, {"user", 0, "someone-else"}} {
		if _, err := s.RequestStop(r.ID, tc.reason, tc.seq, tc.actor); err == nil {
			t.Fatalf("conflicting stop request accepted: %+v", tc)
		}
	}
	if !reflect.DeepEqual(before, allEvents(t, s, r.ID)) {
		t.Fatal("conflict wrote an event")
	}
}

// Plan A7: stop_id derivation is a pure function of the execution ID.
func TestStopIDDeterminismFRRHZ076(t *testing.T) {
	s1, r1 := activeExecution(t, "stop-a7")
	s2, r2 := activeExecution(t, "stop-a7")
	a, err := s1.RequestStop(r1.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s2.RequestStop(r2.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if a.StopID == "" || a.StopID != b.StopID || a.StopID != stopIDFor(r1.ID) {
		t.Fatal(a.StopID, b.StopID)
	}
	s3, r3 := activeExecution(t, "stop-a7-other")
	c, err := s3.RequestStop(r3.ID, "user", 0, "operator")
	if err != nil || c.StopID == a.StopID {
		t.Fatal("distinct executions must derive distinct stop ids")
	}
}
