package secretref

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/secretwire"
)

func countType(st events.Port, typ string) int {
	n := 0
	for _, e := range st.All() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// assertAckFromEvents checks the returned ack against a fresh projection of
// the store (W8: ack is a pure function of durable events).
func assertAckFromEvents(t *testing.T, st events.Port, plane string, got secretwire.Ack) {
	t.Helper()
	p, err := Project(st.All())
	if err != nil {
		t.Fatal(err)
	}
	if want := p.Ack(plane); got != want {
		t.Fatalf("ack %+v, events say %+v", got, want)
	}
}

// setupHW builds plane 1 with hw = n: op1 registered and active, then
// observed facts up to seq n.
func setupHW(t *testing.T, s *Service, n uint64) {
	t.Helper()
	v := verified(t)
	acc(t)(s.RequestRegister(reg(opN(1), "alpha")))
	entries := []secretwire.Entry{outcomeEntry(1, opN(1), ref(opN(1)), 1, rOK)}
	for seq := uint64(2); seq <= n; seq++ {
		entries = append(entries, observedEntry(seq, ref(opN(1)), 1))
	}
	deliver(t, s, v, entries...)
}

func TestProcessBatchLifecycleFRRHZ134(t *testing.T) {
	path := journalPath(t)
	j := openJournal(t, path)
	s := New(j, WithClock(fixedClock()))
	v := verified(t)
	P := planeN(1)
	r1 := ref(opN(1))

	ps, err := s.PollState(P)
	if err != nil || ps.Retired || ps.Ack != (secretwire.Ack{PlaneID: P}) || ps.LastRejected != nil || len(ps.Pending) != 0 {
		t.Fatalf("initial poll %+v %v", ps, err)
	}
	acc(t)(s.RequestRegister(reg(opN(1), "alpha")))
	res := deliver(t, s, v, outcomeEntry(1, opN(1), r1, 1, rOK))
	assertAckFromEvents(t, j, P, res.Ack)
	acc(t)(s.RequestRotate(RotateRequest{OpID: opN(2), Ref: r1, Actor: actor}))
	deliver(t, s, v, outcomeEntry(2, opN(2), r1, 2, rOK), observedEntry(3, r1, 2))
	acc(t)(s.RequestRevoke(RevokeRequest{OpID: opN(3), Ref: r1, ReasonCode: "unused", Actor: actor}))
	res = deliver(t, s, v, outcomeEntry(4, opN(3), r1, 2, rOK))
	if res.Appended != 1 || res.Ack.PlaneEntrySeq != 4 {
		t.Fatalf("res = %+v", res)
	}
	assertAckFromEvents(t, j, P, res.Ack)
	p, _, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := p.Ref(r1); r.State != StateRevoked || r.Version != 2 || r.Observed == nil || r.Observed.Version != 2 {
		t.Fatalf("ref = %+v", r)
	}
	// Restart: the reopened journal yields the same projection and poll.
	before, _ := s.PollState(P)
	j.Close()
	j2 := openJournal(t, path)
	s2 := New(j2, WithClock(fixedClock()))
	after, err := s2.PollState(P)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("poll differs after reopen: %+v vs %+v", before, after)
	}
	p2, _, _ := s2.Snapshot()
	if !reflect.DeepEqual(p.Refs(), p2.Refs()) {
		t.Fatal("refs differ after reopen")
	}
}

// TestProcessBatchRejectionsFRRHZ134 covers W10(a)/(b)/(c), W11① and the
// entry-code mapping.
func TestProcessBatchRejectionsFRRHZ134(t *testing.T) {
	P := planeN(1)
	r1 := ref(opN(1))
	type step struct {
		entries    []secretwire.Entry
		appended   int
		rejection  *secretwire.Rejected
		recorded   bool
		ackSeq     uint64
		e7Total    int
		lastReject *secretwire.Rejected
	}
	rej := func(seq uint64, reason string) *secretwire.Rejected {
		return &secretwire.Rejected{Seq: seq, Reason: reason}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, s *Service)
		steps []step
	}{
		{"partial accept then reject, repeat writes nothing", nil, []step{
			{[]secretwire.Entry{observedEntry(4, r1, 1), observedEntry(5, r1, 9)}, 1, rej(5, secretwire.EntryVersionMismatch), true, 4, 1, rej(5, secretwire.EntryVersionMismatch)},
			{[]secretwire.Entry{observedEntry(4, r1, 1), observedEntry(5, r1, 9)}, 0, rej(5, secretwire.EntryVersionMismatch), false, 4, 1, rej(5, secretwire.EntryVersionMismatch)},
		}},
		{"seq-gap", nil, []step{
			{[]secretwire.Entry{observedEntry(5, r1, 1)}, 0, rej(5, secretwire.EntrySeqGap), true, 3, 1, rej(5, secretwire.EntrySeqGap)},
			{[]secretwire.Entry{observedEntry(5, r1, 1)}, 0, rej(5, secretwire.EntrySeqGap), false, 3, 1, rej(5, secretwire.EntrySeqGap)},
		}},
		{"seq-conflict and identical no-op", nil, []step{
			{[]secretwire.Entry{observedEntry(2, r1, 1)}, 0, nil, false, 3, 0, nil},
			{[]secretwire.Entry{func() secretwire.Entry { e := observedEntry(2, r1, 1); e.Present = false; return e }()}, 0, rej(2, secretwire.EntrySeqConflict), true, 3, 1, rej(2, secretwire.EntrySeqConflict)},
		}},
		// W11: after a rejection the channel is stalled; neither a different
		// rejection nor a now-valid entry is recorded. Only an epoch
		// replacement (new planeId, P5) recovers. (Earlier revisions of this
		// case expected recovery without an epoch, which contradicts W11.)
		{"stalled: no new rejection, no recovery without epoch", nil, []step{
			{[]secretwire.Entry{observedEntry(5, r1, 1)}, 0, rej(5, secretwire.EntrySeqGap), true, 3, 1, rej(5, secretwire.EntrySeqGap)},
			{[]secretwire.Entry{observedEntry(4, ref(opN(99)), 1)}, 0, rej(5, secretwire.EntrySeqGap), false, 3, 1, rej(5, secretwire.EntrySeqGap)},
			{[]secretwire.Entry{observedEntry(4, r1, 1)}, 0, rej(5, secretwire.EntrySeqGap), false, 3, 1, rej(5, secretwire.EntrySeqGap)},
			{nil, 0, rej(5, secretwire.EntrySeqGap), false, 3, 1, rej(5, secretwire.EntrySeqGap)},
		}},
		{"redundant-outcome", nil, []step{
			{[]secretwire.Entry{outcomeEntry(4, opN(1), r1, 1, rOK)}, 0, rej(4, secretwire.EntryRedundantOutcome), true, 3, 1, rej(4, secretwire.EntryRedundantOutcome)},
		}},
		{"unknown-op", nil, []step{
			{[]secretwire.Entry{outcomeEntry(4, opN(50), r1, 1, rOK)}, 0, rej(4, secretwire.EntryUnknownOp), true, 3, 1, rej(4, secretwire.EntryUnknownOp)},
		}},
		{"ref-mismatch outcome", func(t *testing.T, s *Service) {
			acc(t)(s.RequestRotate(RotateRequest{OpID: opN(2), Ref: r1, Actor: actor}))
		}, []step{
			{[]secretwire.Entry{outcomeEntry(4, opN(2), ref(opN(2)), 2, rOK)}, 0, rej(4, secretwire.EntryRefMismatch), true, 3, 1, nil},
		}},
		{"version-mismatch outcome", func(t *testing.T, s *Service) {
			acc(t)(s.RequestRotate(RotateRequest{OpID: opN(2), Ref: r1, Actor: actor}))
		}, []step{
			{[]secretwire.Entry{outcomeEntry(4, opN(2), r1, 1, rOK)}, 0, rej(4, secretwire.EntryVersionMismatch), true, 3, 1, nil},
			// W11 stall: the corrected outcome is not accepted on the same
			// planeId (previously expected to append, contradicting W11).
			{[]secretwire.Entry{outcomeEntry(4, opN(2), r1, 1, rFailed)}, 0, rej(4, secretwire.EntryVersionMismatch), false, 3, 1, rej(4, secretwire.EntryVersionMismatch)},
		}},
		{"ref-mismatch reconciled", nil, []step{
			{[]secretwire.Entry{func() secretwire.Entry { e := reconciledEntry(4, opN(7), "x"); e.Ref = ref(opN(8)); return e }()}, 0, rej(4, secretwire.EntryRefMismatch), true, 3, 1, nil},
		}},
		{"ref-exists", nil, []step{
			{[]secretwire.Entry{reconciledEntry(4, opN(1), "x")}, 0, rej(4, secretwire.EntryRefExists), true, 3, 1, nil},
		}},
		{"version-mismatch reconciled", nil, []step{
			{[]secretwire.Entry{func() secretwire.Entry { e := reconciledEntry(4, opN(7), "x"); e.Version = 2; return e }()}, 0, rej(4, secretwire.EntryVersionMismatch), true, 3, 1, nil},
		}},
		{"state observed", func(t *testing.T, s *Service) {
			acc(t)(s.RequestRegister(reg(opN(7), "pending")))
		}, []step{
			{[]secretwire.Entry{observedEntry(4, ref(opN(7)), 1)}, 0, rej(4, secretwire.EntryState), true, 3, 1, nil},
		}},
		{"version-mismatch observed zero", nil, []step{
			{[]secretwire.Entry{observedEntry(4, r1, 0)}, 0, rej(4, secretwire.EntryVersionMismatch), true, 3, 1, nil},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &events.Store{}
			s := New(st, WithClock(fixedClock()))
			v := verified(t)
			setupHW(t, s, 3)
			if c.setup != nil {
				c.setup(t, s)
			}
			for i, sp := range c.steps {
				before := st.Len()
				res, err := s.ProcessBatch(v, P, sp.entries)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if res.Appended != sp.appended || !reflect.DeepEqual(res.Rejection, sp.rejection) || res.RejectionRecorded != sp.recorded || res.Ack.PlaneEntrySeq != sp.ackSeq {
					t.Fatalf("step %d: res = %+v", i, res)
				}
				wantWrites := sp.appended
				if sp.recorded {
					wantWrites++
				}
				if st.Len()-before != wantWrites {
					t.Fatalf("step %d: wrote %d, want %d", i, st.Len()-before, wantWrites)
				}
				if got := countType(st, TypeEntryRejected); got != sp.e7Total {
					t.Fatalf("step %d: E7 count %d", i, got)
				}
				assertAckFromEvents(t, st, P, res.Ack)
				ps, err := s.PollState(P)
				if err != nil || ps.Ack != res.Ack {
					t.Fatalf("step %d: poll ack %+v", i, ps.Ack)
				}
				if sp.lastReject != nil && !reflect.DeepEqual(ps.LastRejected, sp.lastReject) {
					t.Fatalf("step %d: lastRejected %+v", i, ps.LastRejected)
				}
				if sp.rejection == nil && ps.LastRejected != nil && res.Appended > 0 {
					t.Fatalf("step %d: stale lastRejected", i)
				}
			}
			if _, err := Project(st.All()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetiredPlaneFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	v := verified(t)
	setupHW(t, s, 2)
	P1, P2 := planeN(1), planeN(2)
	// An E7-only first appearance of P2 (hw 0, seq-gap) retires P1 (R7a).
	res, err := s.ProcessBatch(v, P2, []secretwire.Entry{observedEntry(3, ref(opN(1)), 1)})
	if err != nil || !res.RejectionRecorded || res.Ack != (secretwire.Ack{PlaneID: P2}) {
		t.Fatalf("res = %+v %v", res, err)
	}
	n := st.Len()
	if ps, err := s.PollState(P1); err != nil || !ps.Retired {
		t.Fatalf("P1 poll = %+v %v", ps, err)
	}
	if _, err := s.ProcessBatch(v, P1, []secretwire.Entry{observedEntry(3, ref(opN(1)), 1)}); err != ErrRetiredPlane {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.RecordObserved(v, P1, observedEntry(3, ref(opN(1)), 1)); err != ErrRetiredPlane {
		t.Fatalf("err = %v", err)
	}
	if st.Len() != n {
		t.Fatal("retired plane wrote")
	}
	// P2 reports ack 0 and its lastRejected; it is stalled (W11), so a
	// valid entry is not appended (previously expected to append, which
	// contradicts W11). A further planeId P3 recovers and retires P2.
	ps, err := s.PollState(P2)
	if err != nil || ps.Ack.PlaneEntrySeq != 0 || ps.LastRejected == nil || ps.LastRejected.Reason != secretwire.EntrySeqGap {
		t.Fatalf("P2 poll = %+v %v", ps, err)
	}
	if res, err := s.ProcessBatch(v, P2, []secretwire.Entry{observedEntry(1, ref(opN(1)), 1)}); err != nil || res.Appended != 0 ||
		res.Rejection == nil || res.Rejection.Seq != 3 || st.Len() != n {
		t.Fatalf("stalled P2 res = %+v %v", res, err)
	}
	P3 := planeN(3)
	if res, err := s.ProcessBatch(v, P3, []secretwire.Entry{observedEntry(1, ref(opN(1)), 1)}); err != nil || res.Appended != 1 {
		t.Fatalf("res = %+v %v", res, err)
	}
	if ps, err := s.PollState(P2); err != nil || !ps.Retired {
		t.Fatalf("P2 not retired: %+v %v", ps, err)
	}
}

func TestRecordSingleFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	v := verified(t)
	setupHW(t, s, 1)
	P := planeN(1)
	if _, err := s.RecordOutcome(v, P, observedEntry(2, ref(opN(1)), 1)); err != secretwire.ErrFrameMalformed {
		t.Fatalf("fact mismatch err = %v", err)
	}
	_, err := s.RecordObserved(v, P, observedEntry(2, ref(opN(1)), 5))
	var er *EntryRejectedError
	if !errors.As(err, &er) || er.Reason != secretwire.EntryVersionMismatch || err.Error() != "entry-rejected:version-mismatch" {
		t.Fatalf("err = %v", err)
	}
	if countType(st, TypeEntryRejected) != 0 {
		t.Fatal("single record wrote E7")
	}
	if ok, err := s.RecordReconciled(v, P, reconciledEntry(2, opN(5), "beta")); !ok || err != nil {
		t.Fatalf("reconciled %v %v", ok, err)
	}
	if ok, err := s.RecordReconciled(v, P, reconciledEntry(2, opN(5), "beta")); ok || err != nil {
		t.Fatalf("identical redelivery %v %v", ok, err)
	}
	// A malformed entry is a frame error, never an E7.
	bad := observedEntry(3, ref(opN(1)), 1)
	bad.PlaneObservedAt = "yesterday"
	if _, err := s.ProcessBatch(v, P, []secretwire.Entry{bad}); err != secretwire.ErrFrameMalformed {
		t.Fatalf("malformed err = %v", err)
	}
	// RecordRejection enforces R7b and skips a duplicate.
	if _, err := s.RecordRejection(v, P, 9, secretwire.EntryState); err != secretwire.ErrFrameMalformed {
		t.Fatalf("relation err = %v", err)
	}
	if ok, err := s.RecordRejection(v, P, 3, secretwire.EntryState); !ok || err != nil {
		t.Fatalf("rejection %v %v", ok, err)
	}
	if ok, err := s.RecordRejection(v, P, 3, secretwire.EntryState); ok || err != nil {
		t.Fatalf("duplicate rejection %v %v", ok, err)
	}
	// Stalled (W11): single records and other rejections are refused.
	n := st.Len()
	if _, err := s.RecordObserved(v, P, observedEntry(3, ref(opN(1)), 1)); err != ErrStalled {
		t.Fatalf("stalled record err = %v", err)
	}
	if _, err := s.RecordRejection(v, P, 3, secretwire.EntryUnknownRef); err != ErrStalled {
		t.Fatalf("stalled rejection err = %v", err)
	}
	if st.Len() != n || countType(st, TypeEntryRejected) != 1 {
		t.Fatal("stalled plane wrote")
	}
}

// TestStallFRRHZ134 is the W11 stall: hw 3, batch [5] → seq-gap E7; the
// same planeId then sends a valid [4]: nothing is appended, the rejection
// repeats, the stall survives a restart, and only a new planeId clears it
// (retiring the old one, R7a / P5).
func TestStallFRRHZ134(t *testing.T) {
	path := journalPath(t)
	j := openJournal(t, path)
	s := New(j, WithClock(fixedClock()))
	v := verified(t)
	setupHW(t, s, 3)
	P, P2 := planeN(1), planeN(2)
	r1 := ref(opN(1))
	gap := &secretwire.Rejected{Seq: 5, Reason: secretwire.EntrySeqGap}
	res, err := s.ProcessBatch(v, P, []secretwire.Entry{observedEntry(5, r1, 1)})
	if err != nil || !reflect.DeepEqual(res.Rejection, gap) || !res.RejectionRecorded || res.Ack.PlaneEntrySeq != 3 {
		t.Fatalf("res = %+v %v", res, err)
	}
	ack := res.Ack
	n := len(j.All())
	stalled := func(s *Service, label string) {
		t.Helper()
		for _, batch := range [][]secretwire.Entry{{observedEntry(4, r1, 1)}, nil} {
			res, err := s.ProcessBatch(v, P, batch)
			if err != nil || res.Appended != 0 || res.RejectionRecorded || !reflect.DeepEqual(res.Rejection, gap) || res.Ack != ack {
				t.Fatalf("%s: res = %+v %v", label, res, err)
			}
		}
		ps, err := s.PollState(P)
		if err != nil || ps.Retired || ps.Ack != ack || !reflect.DeepEqual(ps.LastRejected, gap) {
			t.Fatalf("%s: poll = %+v %v", label, ps, err)
		}
		if _, err := s.RecordObserved(v, P, observedEntry(4, r1, 1)); err != ErrStalled {
			t.Fatalf("%s: record err = %v", label, err)
		}
	}
	stalled(s, "live")
	if len(j.All()) != n {
		t.Fatal("stalled plane wrote")
	}
	// Stall persists across a restart: it is derived from durable events.
	j.Close()
	j2 := openJournal(t, path)
	s2 := New(j2, WithClock(fixedClock()))
	stalled(s2, "reopened")
	if len(j2.All()) != n {
		t.Fatal("stalled plane wrote after reopen")
	}
	// Epoch replacement: P′ starts at ack 0, appends, and retires P.
	ps, err := s2.PollState(P2)
	if err != nil || ps.Ack != (secretwire.Ack{PlaneID: P2}) || ps.LastRejected != nil {
		t.Fatalf("P2 poll = %+v %v", ps, err)
	}
	res, err = s2.ProcessBatch(v, P2, []secretwire.Entry{observedEntry(1, r1, 1)})
	if err != nil || res.Appended != 1 || res.Rejection != nil || res.Ack.PlaneEntrySeq != 1 {
		t.Fatalf("P2 res = %+v %v", res, err)
	}
	if ps, err := s2.PollState(P); err != nil || !ps.Retired {
		t.Fatalf("P not retired: %+v %v", ps, err)
	}
	if _, err := s2.ProcessBatch(v, P, nil); err != ErrRetiredPlane {
		t.Fatalf("retired err = %v", err)
	}
}

func TestPollPendingFRRHZ134(t *testing.T) {
	st := &events.Store{}
	s := New(st, WithClock(fixedClock()))
	v := verified(t)
	acc(t)(s.RequestRegister(reg(opN(1), "alpha", "ci")))
	deliver(t, s, v, outcomeEntry(1, opN(1), ref(opN(1)), 1, rOK))
	acc(t)(s.RequestRevoke(RevokeRequest{OpID: opN(2), Ref: ref(opN(1)), ReasonCode: "exposed", Actor: actor}))
	for i := 0; i < 70; i++ {
		acc(t)(s.RequestRegister(reg(opN(100+i), "n"+itoa(i))))
	}
	ps, err := s.PollState(planeN(1))
	if err != nil || len(ps.Pending) != secretwire.MaxPending {
		t.Fatalf("pending = %d %v", len(ps.Pending), err)
	}
	first := ps.Pending[0]
	want := secretwire.Pending{OpID: opN(2), Op: secretwire.OpRevoke, Ref: ref(opN(1)), Name: "alpha",
		RefKind: secretwire.RefKindOther, ReasonCode: "exposed", JournalSeq: first.JournalSeq}
	if !reflect.DeepEqual(first, want) || first.JournalSeq == 0 {
		t.Fatalf("revoke pending = %+v", first)
	}
	for i := 1; i < len(ps.Pending); i++ {
		if ps.Pending[i].JournalSeq <= ps.Pending[i-1].JournalSeq {
			t.Fatal("pending not in request order")
		}
	}
	if last := ps.Pending[63]; last.OpID != opN(100+62) || last.Op != secretwire.OpRegister {
		t.Fatalf("not the oldest 64: %+v", last)
	}
	// Pending frames encode under the channel spec.
	if _, err := secretwire.EncodePoll(secretwire.Poll{V: 1, Pending: ps.Pending, Ack: ps.Ack}); err != nil {
		t.Fatal(err)
	}
}

// TestZVFRRHZ134: sentinels in rejected inputs never reach the journal or
// any error/reason string (§8.1 ①, plus refusal strings).
func TestZVFRRHZ134(t *testing.T) {
	s0 := sentinel(t)
	path := journalPath(t)
	j := openJournal(t, path)
	s := New(j, WithClock(fixedClock()))
	v := verified(t)
	setupHW(t, s, 1)
	var out bytes.Buffer
	note := func(res IntentResult, err error) {
		out.WriteString(res.Reason + res.OpID + res.Ref)
		if err != nil {
			out.WriteString(err.Error())
		}
		if res.Accepted {
			t.Fatal("sentinel input accepted")
		}
	}
	note(s.RequestRegister(reg(s0, "alpha2")))
	r := reg(opN(2), "beta")
	r.RefKind = s0
	note(s.RequestRegister(r))
	note(s.RequestRegister(reg(opN(2), "beta", s0, s0)))
	note(s.RequestRegister(RegisterRequest{OpID: opN(2), Name: "beta", RefKind: "other", Actor: "X" + s0}))
	note(s.RequestRotate(RotateRequest{OpID: opN(2), Ref: s0, Actor: actor}))
	note(s.RequestRevoke(RevokeRequest{OpID: opN(2), Ref: ref(opN(1)), ReasonCode: s0, Actor: actor}))
	for _, e := range []secretwire.Entry{
		{Seq: 2, Fact: secretwire.FactObserved, Ref: s0, Version: 1, PlaneObservedAt: "2026-10-10T00:00:00Z"},
		outcomeEntry(2, s0, ref(opN(1)), 1, rOK),
		outcomeEntry(2, opN(1), s0, 1, rOK),
		{Seq: 2, Fact: s0},
	} {
		res, err := s.ProcessBatch(v, planeN(1), []secretwire.Entry{e})
		if err != nil {
			out.WriteString(err.Error())
		}
		if res.Rejection != nil {
			out.WriteString(res.Rejection.Reason)
		}
	}
	if _, err := s.ProcessBatch(v, s0, nil); err != nil {
		out.WriteString(err.Error())
	}
	if _, err := s.PollState(s0); err != nil {
		out.WriteString(err.Error())
	}
	if _, err := s.RecordRejection(v, planeN(1), 2, s0); err != nil {
		out.WriteString(err.Error())
	}
	j.Close()
	assertNoSentinel(t, "journal", readFile(t, path), s0)
	assertNoSentinel(t, "reasons and errors", out.Bytes(), s0)
	if out.Len() == 0 {
		t.Fatal("no refusals collected")
	}
}
