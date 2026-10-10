package secretref

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/peercred"
	"rhizome/internal/secretwire"
)

func reg(op, name string, tags ...string) RegisterRequest {
	r := RegisterRequest{OpID: op, Name: name, RefKind: secretwire.RefKindOther, Actor: actor}
	if len(tags) > 0 {
		r.ScopeTags = tags
	}
	return r
}

// acc asserts an Accepted intent result.
func acc(t *testing.T) func(IntentResult, error) IntentResult {
	return func(res IntentResult, err error) IntentResult {
		t.Helper()
		if err != nil || !res.Accepted || res.Reason != "" || res.OpID == "" || res.Ref == "" {
			t.Fatalf("not accepted: %+v %v", res, err)
		}
		return res
	}
}

func mustRefuse(t *testing.T, st events.Port, want string, res IntentResult, err error) {
	t.Helper()
	if err != nil || res.Accepted || res.Reason != want || res.OpID != "" || res.Ref != "" {
		t.Fatalf("got %+v %v, want refusal %q", res, err, want)
	}
}

// deliver feeds outcome/observed entries on plane 1 in sequence.
func deliver(t *testing.T, s *Service, v peercred.Verified, entries ...secretwire.Entry) BatchResult {
	t.Helper()
	res, err := s.ProcessBatch(v, planeN(1), entries)
	if err != nil || res.Rejection != nil {
		t.Fatalf("batch: %+v %v", res, err)
	}
	return res
}

func TestIntentGrammarRefusalsFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	r1 := ref(opN(1))
	cases := []struct {
		name string
		do   func() (IntentResult, error)
		want string
	}{
		{"X3 opId", func() (IntentResult, error) { return s.RequestRegister(reg("op-1", "alpha")) }, ReasonInvalidOpID},
		{"X4 name", func() (IntentResult, error) { return s.RequestRegister(reg(opN(1), "Alpha")) }, ReasonInvalidName},
		{"X5 refKind", func() (IntentResult, error) {
			r := reg(opN(1), "alpha")
			r.RefKind = "password"
			return s.RequestRegister(r)
		}, ReasonInvalidRefKind},
		{"X6 scopeTags unsorted", func() (IntentResult, error) { return s.RequestRegister(reg(opN(1), "alpha", "b", "a")) }, ReasonInvalidScopeTags},
		{"X6 scopeTags empty non-nil", func() (IntentResult, error) {
			r := reg(opN(1), "alpha")
			r.ScopeTags = []string{}
			return s.RequestRegister(r)
		}, ReasonInvalidScopeTags},
		{"X7 ref", func() (IntentResult, error) {
			return s.RequestRotate(RotateRequest{OpID: opN(1), Ref: "sref-1", Actor: actor})
		}, ReasonInvalidRef},
		{"X8 reasonCode", func() (IntentResult, error) {
			return s.RequestRevoke(RevokeRequest{OpID: opN(1), Ref: r1, ReasonCode: "leaked", Actor: actor})
		}, ReasonInvalidReasonCode},
		{"X9 actor", func() (IntentResult, error) {
			r := reg(opN(1), "alpha")
			r.Actor = ""
			return s.RequestRegister(r)
		}, ReasonInvalidActor},
		{"X3 before X15", func() (IntentResult, error) { return (*Service)(nil).RequestRegister(reg("bad", "alpha")) }, ReasonInvalidOpID},
		{"X15 nil service", func() (IntentResult, error) { return (*Service)(nil).RequestRegister(reg(opN(1), "alpha")) }, ReasonUnavailable},
		{"X15 zero service", func() (IntentResult, error) { return (&Service{}).RequestRegister(reg(opN(1), "alpha")) }, ReasonUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := c.do()
			mustRefuse(t, st, c.want, res, err)
			if st.Len() != 0 {
				t.Fatal("refusal wrote")
			}
		})
	}
}

func TestIntentStateRulesFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	v := verified(t)
	r1 := ref(opN(1))

	// I5: register, then an identical retry (different actor) is Accepted
	// with zero writes; different content is X10.
	res := acc(t)(s.RequestRegister(reg(opN(1), "alpha", "ci", "prod")))
	if res.OpID != opN(1) || res.Ref != r1 {
		t.Fatalf("result = %+v", res)
	}
	n := st.Len()
	again := reg(opN(1), "alpha", "ci", "prod")
	again.Actor = "someone-else"
	if got := acc(t)(s.RequestRegister(again)); got != res || st.Len() != n {
		t.Fatal("idempotent retry wrote or differed")
	}
	// refuse captures the journal length before invoking call, so a write
	// by the refused call itself would be caught.
	refuse := func(want string, call func() (IntentResult, error)) {
		t.Helper()
		before := st.Len()
		res, err := call()
		mustRefuse(t, st, want, res, err)
		if st.Len() != before {
			t.Fatal("refusal wrote")
		}
	}
	refuse(ReasonOpIDConflict, func() (IntentResult, error) { return s.RequestRegister(reg(opN(1), "alpha", "ci")) })
	refuse(ReasonOpIDConflict, func() (IntentResult, error) {
		return s.RequestRotate(RotateRequest{OpID: opN(1), Ref: r1, Actor: actor})
	})
	// I7: name held by a pending-registration ref.
	refuse(ReasonNameInUse, func() (IntentResult, error) { return s.RequestRegister(reg(opN(2), "alpha")) })
	// I6: pending-registration is not active; unknown refs are X11.
	refuse(ReasonRefNotActive, func() (IntentResult, error) {
		return s.RequestRotate(RotateRequest{OpID: opN(3), Ref: r1, Actor: actor})
	})
	refuse(ReasonUnknownRef, func() (IntentResult, error) {
		return s.RequestRevoke(RevokeRequest{OpID: opN(3), Ref: ref(opN(99)), ReasonCode: "unused", Actor: actor})
	})

	deliver(t, s, v, outcomeEntry(1, opN(1), r1, 1, rOK))
	refuse(ReasonNameInUse, func() (IntentResult, error) { return s.RequestRegister(reg(opN(2), "alpha")) })
	acc(t)(s.RequestRotate(RotateRequest{OpID: opN(3), Ref: r1, Actor: actor}))
	n = st.Len()
	acc(t)(s.RequestRotate(RotateRequest{OpID: opN(3), Ref: r1, Actor: "other"}))
	if st.Len() != n {
		t.Fatal("idempotent rotate wrote")
	}
	// I6: in flight is X13.
	refuse(ReasonRefBusy, func() (IntentResult, error) {
		return s.RequestRevoke(RevokeRequest{OpID: opN(4), Ref: r1, ReasonCode: "unused", Actor: actor})
	})
	refuse(ReasonRefBusy, func() (IntentResult, error) {
		return s.RequestRotate(RotateRequest{OpID: opN(4), Ref: r1, Actor: actor})
	})
	deliver(t, s, v, outcomeEntry(2, opN(3), r1, 2, rOK))
	acc(t)(s.RequestRevoke(RevokeRequest{OpID: opN(4), Ref: r1, ReasonCode: "exposed", Actor: actor}))
	refuse(ReasonOpIDConflict, func() (IntentResult, error) {
		return s.RequestRevoke(RevokeRequest{OpID: opN(4), Ref: r1, ReasonCode: "unused", Actor: actor})
	})
	deliver(t, s, v, outcomeEntry(3, opN(4), r1, 2, rOK))
	// Revoked: X12, and the name is free again.
	refuse(ReasonRefNotActive, func() (IntentResult, error) {
		return s.RequestRotate(RotateRequest{OpID: opN(5), Ref: r1, Actor: actor})
	})
	acc(t)(s.RequestRegister(reg(opN(6), "alpha")))
	// Registration failed: X12, name free.
	deliver(t, s, v, outcomeEntry(4, opN(6), ref(opN(6)), 0, rRejected))
	refuse(ReasonRefNotActive, func() (IntentResult, error) {
		return s.RequestRevoke(RevokeRequest{OpID: opN(7), Ref: ref(opN(6)), ReasonCode: "unused", Actor: actor})
	})
	acc(t)(s.RequestRegister(reg(opN(8), "alpha")))
	// I5: a register whose derived ref was introduced by a reconciled entry is X10.
	deliver(t, s, v, reconciledEntry(5, opN(9), "zeta"))
	refuse(ReasonOpIDConflict, func() (IntentResult, error) { return s.RequestRegister(reg(opN(9), "other-name")) })
	// Q2: an attested ref may be rotated (no X16).
	att := reconciledEntry(6, opN(10), "broker")
	att.Origin = secretwire.OriginAttested
	deliver(t, s, v, att)
	acc(t)(s.RequestRotate(RotateRequest{OpID: opN(11), Ref: ref(opN(10)), Actor: actor}))
	// Q2: an attested ref may also be revoked (no X16).
	deliver(t, s, v, outcomeEntry(7, opN(11), ref(opN(10)), 2, rOK))
	acc(t)(s.RequestRevoke(RevokeRequest{OpID: opN(12), Ref: ref(opN(10)), ReasonCode: "rotated-out", Actor: actor}))
	deliver(t, s, v, outcomeEntry(8, opN(12), ref(opN(10)), 2, rOK))
	if p, _, _ := s.Snapshot(); func() bool {
		r, _ := p.Ref(ref(opN(10)))
		return r.State != StateRevoked || r.Origin != secretwire.OriginAttested
	}() {
		t.Fatal("attested revoke not applied")
	}

	// Every write is replayable and reconstructs the same state.
	if _, err := Project(st.All()); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRequiresVerifiedFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	acc(t)(s.RequestRegister(reg(opN(1), "alpha")))
	n := st.Len()
	var zero peercred.Verified
	e := outcomeEntry(1, opN(1), ref(opN(1)), 1, rOK)
	checks := []error{}
	_, err := s.ProcessBatch(zero, planeN(1), []secretwire.Entry{e})
	checks = append(checks, err)
	_, err = s.RecordOutcome(zero, planeN(1), e)
	checks = append(checks, err)
	_, err = s.RecordReconciled(zero, planeN(1), reconciledEntry(1, opN(2), "beta"))
	checks = append(checks, err)
	_, err = s.RecordObserved(zero, planeN(1), observedEntry(1, ref(opN(1)), 1))
	checks = append(checks, err)
	_, err = s.RecordRejection(zero, planeN(1), 1, secretwire.EntryState)
	checks = append(checks, err)
	_, err = (*Service)(nil).ProcessBatch(zero, planeN(1), nil)
	checks = append(checks, err)
	for i, err := range checks {
		if err != ErrPeerRejected {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	if st.Len() != n {
		t.Fatal("unverified call wrote")
	}
	// The same calls with a verified peer do write.
	if ok, err := s.RecordOutcome(verified(t), planeN(1), e); err != nil || !ok || st.Len() != n+1 {
		t.Fatalf("verified record: %v %v", ok, err)
	}
}

func TestAppendFailureFailsWriterFRRHZ134(t *testing.T) {
	for _, inject := range []error{errors.New("disk"), events.ErrPoisoned} {
		st := &faultStore{}
		s := New(st, WithClock(fixedClock()))
		v := verified(t)
		acc(t)(s.RequestRegister(reg(opN(1), "alpha")))
		st.mu.Lock()
		st.failWith = inject
		st.mu.Unlock()
		res, err := s.RequestRegister(reg(opN(2), "beta"))
		if res.Accepted || res.Reason != ReasonUnavailable {
			t.Fatalf("result = %+v", res)
		}
		if wantPoison := inject == events.ErrPoisoned; (err == events.ErrPoisoned) != wantPoison {
			t.Fatalf("err = %v", err)
		}
		calls := st.appendCount()
		// Failed for good: later writes refuse without touching the store.
		st.mu.Lock()
		st.failWith = nil
		st.mu.Unlock()
		res, _ = s.RequestRegister(reg(opN(3), "gamma"))
		if res.Reason != ReasonUnavailable {
			t.Fatalf("writer not failed: %+v", res)
		}
		if _, err := s.ProcessBatch(v, planeN(1), []secretwire.Entry{outcomeEntry(1, opN(1), ref(opN(1)), 1, rOK)}); err != ErrRegistryUnavailable {
			t.Fatalf("batch err = %v", err)
		}
		if _, err := s.PollState(planeN(1)); err != ErrRegistryUnavailable {
			t.Fatalf("poll err = %v", err)
		}
		if st.appendCount() != calls || st.Len() != 1 {
			t.Fatal("writer retried or wrote after failure")
		}
	}
}

func TestAppendFailureInBatchFRRHZ134(t *testing.T) {
	st := &faultStore{}
	s := New(st, WithClock(fixedClock()))
	v := verified(t)
	acc(t)(s.RequestRegister(reg(opN(1), "alpha")))
	acc(t)(s.RequestRegister(reg(opN(2), "beta")))
	// The first entry appends, the second hits an injected failure.
	ok1 := outcomeEntry(1, opN(1), ref(opN(1)), 1, rOK)
	if _, err := s.ProcessBatch(v, planeN(1), []secretwire.Entry{ok1}); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.failWith = events.ErrPoisoned
	st.mu.Unlock()
	res, err := s.ProcessBatch(v, planeN(1), []secretwire.Entry{ok1, outcomeEntry(2, opN(2), ref(opN(2)), 1, rOK)})
	if err != events.ErrPoisoned || res.Ack != (secretwire.Ack{}) || res.Appended != 0 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	calls := st.appendCount()
	if _, err := s.ProcessBatch(v, planeN(1), []secretwire.Entry{outcomeEntry(2, opN(2), ref(opN(2)), 1, rOK)}); err != ErrRegistryUnavailable {
		t.Fatalf("err = %v", err)
	}
	if st.appendCount() != calls {
		t.Fatal("retried")
	}
}

func TestPoisonLatchRefusesFRRHZ134(t *testing.T) {
	st := &faultStore{poison: true}
	s := New(st, WithClock(fixedClock()))
	res, err := s.RequestRegister(reg(opN(1), "alpha"))
	if res.Reason != ReasonUnavailable || err != events.ErrPoisoned {
		t.Fatalf("intent: %+v %v", res, err)
	}
	if _, err := s.PollState(planeN(1)); err != events.ErrPoisoned {
		t.Fatalf("poll: %v", err)
	}
	if _, err := s.ProcessBatch(verified(t), planeN(1), nil); err != events.ErrPoisoned {
		t.Fatalf("batch: %v", err)
	}
	if st.appendCount() != 0 {
		t.Fatal("append attempted under poison")
	}
	// Reads keep working (§1.8).
	if _, _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentRegisterVsReconcileFRRHZ134: E1 and E5 aiming at the same ref
// race through the single writer; exactly one introduces it.
func TestConcurrentRegisterVsReconcileFRRHZ134(t *testing.T) {
	v := verified(t)
	for i := 0; i < 50; i++ {
		st := &events.Store{}
		s := New(st, WithClock(fixedClock()))
		var wg sync.WaitGroup
		var res IntentResult
		var batch BatchResult
		var berr error
		wg.Add(2)
		go func() { defer wg.Done(); res, _ = s.RequestRegister(reg(opN(1), "alpha")) }()
		go func() {
			defer wg.Done()
			batch, berr = s.ProcessBatch(v, planeN(1), []secretwire.Entry{reconciledEntry(1, opN(1), "beta")})
		}()
		wg.Wait()
		if berr != nil {
			t.Fatal(berr)
		}
		introductions := 0
		for _, e := range st.All() {
			if e.Type == TypeRegisterRequested || e.Type == TypeReconciled {
				introductions++
			}
		}
		if introductions != 1 {
			t.Fatalf("introductions = %d", introductions)
		}
		switch {
		case res.Accepted && batch.Rejection != nil && batch.Rejection.Reason == secretwire.EntryRefExists:
		case !res.Accepted && res.Reason == ReasonOpIDConflict && batch.Appended == 1:
		default:
			t.Fatalf("unexpected outcome: %+v %+v", res, batch)
		}
		if _, err := Project(st.All()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoAliasingFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	v := verified(t)
	tags := []string{"ci", "prod"}
	acc(t)(s.RequestRegister(reg(opN(1), "alpha", tags...)))
	tags[0] = "zz"
	ps, err := s.PollState(planeN(1))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ps.Pending[0].ScopeTags, []string{"ci", "prod"}) {
		t.Fatal("input slice aliased")
	}
	ps.Pending[0].ScopeTags[0] = "zz"
	entries := []secretwire.Entry{reconciledEntry(1, opN(2), "beta")}
	entries[0].ScopeTags = []string{"a", "b"}
	deliver(t, s, v, entries...)
	entries[0].ScopeTags[0] = "zz"
	p, _, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	refs := p.Refs()
	if !reflect.DeepEqual(refs[0].ScopeTags, []string{"ci", "prod"}) || !reflect.DeepEqual(refs[1].ScopeTags, []string{"a", "b"}) ||
		!reflect.DeepEqual(refs[0].Pending.ScopeTags, []string{"ci", "prod"}) {
		t.Fatal("projection aliased an input or a poll result")
	}
	refs[0].ScopeTags[0] = "zz"
	refs[0].Pending.ScopeTags[0] = "zz"
	refs[1].Observed = &ObservedView{}
	pend := p.Pending(0)
	pend[0].ScopeTags[0] = "zz"
	again := p.Refs()
	if again[0].ScopeTags[0] != "ci" || again[0].Pending.ScopeTags[0] != "ci" || again[1].Observed != nil || p.Pending(0)[0].ScopeTags[0] != "ci" {
		t.Fatal("projection aliased its output")
	}
	if r, ok := p.Ref(refs[1].Ref); !ok || r.ScopeTags[0] != "a" {
		t.Fatal("Ref accessor wrong")
	}
}
