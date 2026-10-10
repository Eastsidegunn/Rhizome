package secretplane

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/secretwire"
)

func rej(seq uint64, reason string) *secretwire.Rejected {
	return &secretwire.Rejected{Seq: seq, Reason: reason}
}

func seqs(es []secretwire.Entry) []uint64 {
	var out []uint64
	for _, e := range es {
		out = append(out, e.Seq)
	}
	return out
}

func TestOutboxSeqBatchAndAckFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 70)
	b := hs.batch()
	if len(b) != 64 || b[0].Seq != 1 || b[63].Seq != 64 {
		t.Fatalf("batch %v", seqs(b))
	}
	for i := 1; i < len(b); i++ {
		if b[i].Seq != b[i-1].Seq+1 {
			t.Fatal("contiguous")
		}
	}
	if small, _ := hs.pl.NextBatch(hs.planeID(), 5); len(small) != 5 {
		t.Fatal("max")
	}
	hs.mustPoll(64, nil)
	if got := seqs(hs.batch()); !reflect.DeepEqual(got, []uint64{65, 66, 67, 68, 69, 70}) {
		t.Fatalf("after ack %v", got)
	}
	// Equal ack is not recorded again (P2: only larger).
	before := hs.file(ledgerName)
	hs.mustPoll(64, nil)
	if !bytes.Equal(before, hs.file(ledgerName)) {
		t.Fatal("equal ack wrote")
	}
	hs.reopen()
	if hs.status().RecordedAck != 64 {
		t.Fatal("ack must be durable")
	}
	// P2 (a): acked entries leave the outbox view.
	ob, _ := hs.pl.Outbox()
	if len(ob) != 6 || ob[0].Entry.Seq != 65 {
		t.Fatalf("outbox view %d", len(ob))
	}
}

func TestPollSanityHaltsFRRHZ138(t *testing.T) {
	cases := []struct {
		name string
		do   func(hs *harness) error
	}{
		{"ack-below-recorded", func(hs *harness) error { return hs.poll(3, nil) }},
		{"ack-above-outbox", func(hs *harness) error { return hs.poll(13, nil) }},
		{"wrong-plane", func(hs *harness) error {
			p := hs.planeID()
			return hs.pl.OnPoll(p, secretwire.Ack{PlaneID: "pl-" + strings.Repeat("0", 32), PlaneEntrySeq: 5}, nil)
		}},
		{"conflict-above-ack", func(hs *harness) error { return hs.poll(5, rej(6, secretwire.EntrySeqConflict)) }},
		{"gap-not-beyond", func(hs *harness) error { return hs.poll(5, rej(6, secretwire.EntrySeqGap)) }},
		{"other-not-next", func(hs *harness) error { return hs.poll(5, rej(8, secretwire.EntryVersionMismatch)) }},
		{"rejected-beyond-outbox", func(hs *harness) error { return hs.poll(5, rej(13, secretwire.EntrySeqGap)) }},
		{"bad-reason", func(hs *harness) error { return hs.poll(5, rej(6, "bogus")) }},
		{"notice-ack-below", func(hs *harness) error {
			p := hs.planeID()
			return hs.pl.OnEntryRejected(p, secretwire.EntryState, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 4}, 5)
		}},
		{"notice-relation", func(hs *harness) error {
			p := hs.planeID()
			return hs.pl.OnEntryRejected(p, secretwire.EntryState, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 5}, 9)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seed([]byte("v"), 12)
			hs.mustPoll(5, nil)
			before := hs.file(ledgerName)
			if err := c.do(hs); !errors.Is(err, ErrHalted) {
				t.Fatalf("got %v", err)
			}
			if !bytes.Equal(before, hs.file(ledgerName)) {
				t.Fatal("halt must write nothing")
			}
			if _, err := hs.pl.NextBatch(hs.planeID(), 64); !errors.Is(err, ErrHalted) {
				t.Fatal("halted plane sends no batch")
			}
			if !hs.status().Halted {
				t.Fatal("status")
			}
		})
	}
}

func TestRejectionRecordingAndStallFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 12)
	hs.mustPoll(10, nil)
	p := hs.planeID()
	// (i) shape: batch [11,12], 11 accepted, 12 rejected → notice ack 11.
	if err := hs.pl.OnEntryRejected(p, secretwire.EntryVersionMismatch, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 11, JournalSeq: 40}, 12); err != nil {
		t.Fatal(err)
	}
	st := hs.status()
	if st.RecordedAck != 11 || st.LastRejected == nil || st.LastRejected.Seq != 12 || !st.Stalled {
		t.Fatalf("status %+v", st)
	}
	// Spec-conformance correction (review): W11 keeps the channel polling
	// and the same entry is delivered again (Rhizome repeats the rejection
	// with zero writes); only frozen mandates entries:[].
	if got := seqs(hs.batch()); !reflect.DeepEqual(got, []uint64{12}) {
		t.Fatalf("stalled plane keeps delivering the unacked entry: %v", got)
	}
	// The same lastRejected on the next poll writes nothing (lost notice
	// frame gives the same information).
	before := hs.file(ledgerName)
	hs.mustPoll(11, rej(12, secretwire.EntryVersionMismatch))
	if !bytes.Equal(before, hs.file(ledgerName)) {
		t.Fatal("duplicate rejection wrote")
	}
	hs.reopen()
	st = hs.status()
	if st.RecordedAck != 11 || st.LastRejected == nil || st.LastRejected.Reason != secretwire.EntryVersionMismatch {
		t.Fatal("rejection must be durable")
	}
}

// W11 stall is durable until epoch replacement: ack 10, seq 11 rejected
// `state`; the next batch still starts at 11; an ack claiming 11 is a
// sanity violation (halt, zero writes), never progress.
func TestStallDurableUntilEpochFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 12)
	hs.mustPoll(10, nil)
	p := hs.planeID()
	if err := hs.pl.OnEntryRejected(p, secretwire.EntryState, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 10}, 11); err != nil {
		t.Fatal(err)
	}
	if got := seqs(hs.batch()); !reflect.DeepEqual(got, []uint64{11, 12}) {
		t.Fatalf("batch %v", got)
	}
	// Same rejection again: zero writes.
	before := hs.file(ledgerName)
	if err := hs.pl.OnEntryRejected(p, secretwire.EntryState, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 10}, 11); err != nil {
		t.Fatal(err)
	}
	hs.mustPoll(10, rej(11, secretwire.EntryState))
	if !bytes.Equal(before, hs.file(ledgerName)) {
		t.Fatal("repeated rejection wrote")
	}
	hs.reopen() // the stall survives restart
	if !hs.status().Stalled {
		t.Fatal("stall must be durable")
	}
	if err := hs.poll(11, nil); !errors.Is(err, ErrHalted) {
		t.Fatalf("ack beyond the rejection must halt: %v", err)
	}
	if !bytes.Equal(before, hs.file(ledgerName)) || hs.status().RecordedAck != 10 {
		t.Fatal("halt must write nothing")
	}
	// A notice carrying a larger ack halts too.
	hs.reopen()
	if err := hs.pl.OnEntryRejected(p, secretwire.EntryState, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 11}, 12); !errors.Is(err, ErrHalted) {
		t.Fatalf("notice beyond the rejection must halt: %v", err)
	}
	// Epoch replacement is the recovery path.
	hs.reopen()
	hs.runEpoch(10, rej(11, secretwire.EntryState))
	hs.mustPoll(0, nil)
	if hs.status().Stalled {
		t.Fatal("P′ starts unstalled")
	}
}

// The ack and the rejection become durable in one line, so a crash between
// any two steps never lets the ack drop seq 9 (P2 (a)) before the conflict
// is recorded and quarantined behind the P5a barrier.
func TestAckRejectionAtomicCrashFRRHZ138(t *testing.T) {
	points := []string{"ledger-mid-line:rej", "ledger-appended:rej", "quarantine-created",
		"quarantine-mid-line", "barrier-start", "barrier-before-sync", "barrier-done"}
	for _, pt := range points {
		t.Run(pt, func(t *testing.T) {
			hs := newHarness(t)
			hs.seed([]byte("v"), 12)
			hs.mustPoll(8, nil)
			hs.h.trace = nil
			hs.h.crashOn(pt, 1)
			hs.crash(func() { hs.poll(10, rej(9, secretwire.EntrySeqConflict)) })
			for _, x := range hs.h.trace {
				if x == "ledger-appended:ack" {
					t.Fatal("ack must not be written ahead of the rejection")
				}
			}
			st := newState()
			for _, line := range bytes.Split(bytes.TrimSpace(hs.file(ledgerName)), []byte("\n")) {
				var r rec
				if len(line) == 0 {
					continue
				}
				if !decodeLine(line, &r) {
					continue // torn tail: dropped at startup
				}
				if st.apply(&r) != nil {
					t.Fatal("replay")
				}
			}
			view, _ := (&Plane{st: st, q: newQStore()}).Outbox()
			has9 := false
			for _, e := range view {
				has9 = has9 || e.Entry.Seq == 9
			}
			if !has9 {
				t.Fatalf("seq 9 dropped before quarantine (ack %d)", st.ackSeq(st.planeID))
			}
			hs.reopen()
			k := hs.quarantineKeys()
			ack := hs.status().RecordedAck
			switch {
			case ack == 10 && reflect.DeepEqual(k, []string{"seq-conflict:9"}):
			case ack == 8 && len(k) == 0:
				ob, _ := hs.pl.Outbox()
				if len(ob) != 4 || ob[0].Entry.Seq != 9 {
					t.Fatal("9 must stay retained")
				}
			default:
				t.Fatalf("ack %d quarantine %v", ack, k)
			}
		})
	}
}

// P2: journal-poisoned (or any non entry-rejected error frame) is not an ack.
func TestJournalPoisonedIsNotAckFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 5)
	p := hs.planeID()
	before := hs.file(ledgerName)
	for _, code := range []string{secretwire.CodeJournalPoisoned, secretwire.CodeRetiredPlane, secretwire.CodeBusy} {
		f := secretwire.ErrorFrame{Code: code, Ack: &secretwire.Ack{PlaneID: p, PlaneEntrySeq: 5}}
		if err := hs.pl.OnErrorFrame(p, f); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(before, hs.file(ledgerName)) || hs.status().RecordedAck != 0 {
		t.Fatal("non-ack frame changed state")
	}
	if ob, _ := hs.pl.Outbox(); len(ob) != 5 {
		t.Fatal("nothing may be cleaned up")
	}
	// entry-rejected is dispatched.
	f := secretwire.ErrorFrame{Code: secretwire.CodeEntryRejected, Reason: secretwire.EntryState,
		Ack: &secretwire.Ack{PlaneID: p, PlaneEntrySeq: 2}, RejectedSeq: 3}
	if err := hs.pl.OnErrorFrame(p, f); err != nil || hs.status().RecordedAck != 2 {
		t.Fatal("entry-rejected must be recorded")
	}
}

// (viii) conflict: 9 is not removable by P2 (a) and is quarantined at once;
// a crash before the barrier keeps it in the outbox and restart finishes.
func TestConflictImmediateQuarantineFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 12)
	hs.mustPoll(10, nil)
	hs.h.crashOn("quarantine-mid-line", 1)
	hs.crash(func() { hs.poll(10, rej(9, secretwire.EntrySeqConflict)) })

	// Replay alone (no recovery): 9 is retained, not quarantined.
	st := newState()
	data := hs.file(ledgerName)
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r rec
		if !decodeLine(line, &r) || st.apply(&r) != nil {
			t.Fatal("replay")
		}
	}
	p := st.planeID
	if st.ackSeq(p) != 10 || !st.conflicts[p][9] {
		t.Fatal("conflict must be durable before quarantine")
	}
	view, _ := (&Plane{st: st, q: newQStore()}).Outbox()
	if len(view) != 3 || view[0].Entry.Seq != 9 {
		t.Fatalf("9 must stay in the outbox until quarantined (%d)", len(view))
	}

	hs.reopen()
	keys := hs.quarantineKeys()
	if !reflect.DeepEqual(keys, []string{"seq-conflict:9"}) {
		t.Fatalf("quarantine %v", keys)
	}
	ob, _ := hs.pl.Outbox()
	if got := func() []uint64 {
		var s []uint64
		for _, e := range ob {
			s = append(s, e.Entry.Seq)
		}
		return s
	}(); !reflect.DeepEqual(got, []uint64{11, 12}) {
		t.Fatalf("outbox %v (9 removable only after quarantine)", got)
	}
	// Re-recording the same conflict is a no-op (keyed rewrite).
	before, qb := hs.file(ledgerName), hs.file(quarantineName)
	hs.mustPoll(10, rej(9, secretwire.EntrySeqConflict))
	if !bytes.Equal(before, hs.file(ledgerName)) || !bytes.Equal(qb, hs.file(quarantineName)) {
		t.Fatal("idempotent")
	}
	if m := modeOf(t, filepath.Join(hs.dir, quarantineName)); m.Perm() != 0o600 {
		t.Fatal("quarantine mode")
	}
}

// runEpoch freezes, then answers the first poll after the freeze.
func (hs *harness) runEpoch(ack uint64, lr *secretwire.Rejected) {
	hs.t.Helper()
	if err := hs.pl.Freeze(); err != nil {
		hs.t.Fatal(err)
	}
	if b := hs.batch(); len(b) != 0 {
		hs.t.Fatal("frozen P sends entries:[] only")
	}
	hs.mustPoll(ack, lr)
}

func (hs *harness) assertNewEpoch(old string) string {
	hs.t.Helper()
	np := hs.planeID()
	st := hs.status()
	if np == old || st.Phase != "split-recorded" || st.OutboxMax != 0 {
		hs.t.Fatalf("after split %+v", st)
	}
	if _, err := hs.pl.NextBatch(old, 64); !errors.Is(err, ErrStalePlane) {
		hs.t.Fatal("no batch for P after split")
	}
	if b := hs.batch(); len(b) != 0 {
		hs.t.Fatal("P′ outbox is empty")
	}
	return np
}

func TestEpochCounterexamplesFRRHZ138(t *testing.T) {
	t.Run("i-accept-11-reject-12", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 14)
		hs.mustPoll(10, nil)
		p := hs.planeID()
		if err := hs.pl.OnEntryRejected(p, secretwire.EntryRefMismatch, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 11}, 12); err != nil {
			t.Fatal(err)
		}
		hs.runEpoch(11, rej(12, secretwire.EntryRefMismatch))
		if k := hs.quarantineKeys(); !reflect.DeepEqual(k, []string{"after-cutoff:12", "after-cutoff:13", "after-cutoff:14"}) {
			t.Fatalf("quarantine %v", k)
		}
		hs.assertNewEpoch(p)
	})
	t.Run("ii-seq-gap", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 13)
		hs.mustPoll(10, nil)
		p := hs.planeID()
		if err := hs.pl.OnEntryRejected(p, secretwire.EntrySeqGap, secretwire.Ack{PlaneID: p, PlaneEntrySeq: 10}, 12); err != nil {
			t.Fatal(err)
		}
		hs.runEpoch(10, rej(12, secretwire.EntrySeqGap))
		if k := hs.quarantineKeys(); !reflect.DeepEqual(k, []string{"after-cutoff:11", "after-cutoff:12", "after-cutoff:13"}) {
			t.Fatalf("quarantine %v", k)
		}
		hs.assertNewEpoch(p)
	})
	t.Run("iii-seq-conflict", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 12)
		hs.mustPoll(10, rej(9, secretwire.EntrySeqConflict))
		p := hs.planeID()
		hs.runEpoch(10, rej(9, secretwire.EntrySeqConflict))
		if k := hs.quarantineKeys(); !reflect.DeepEqual(k, []string{"seq-conflict:9", "after-cutoff:11", "after-cutoff:12"}) {
			t.Fatalf("quarantine %v", k)
		}
		hs.assertNewEpoch(p)
	})
	t.Run("iv-no-rejection", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 12)
		hs.mustPoll(10, nil)
		p := hs.planeID()
		hs.runEpoch(10, nil)
		if k := hs.quarantineKeys(); !reflect.DeepEqual(k, []string{"after-cutoff:11", "after-cutoff:12"}) {
			t.Fatalf("quarantine %v", k)
		}
		hs.assertNewEpoch(p)
		// Nothing is renumbered: P′ starts at seq 1 with new facts only.
		ref := hs.seed([]byte("w"), 1)
		hs.observeN(ref, 1)
		if got := seqs(hs.batch()); !reflect.DeepEqual(got, []uint64{1, 2}) {
			t.Fatalf("P′ seqs %v", got)
		}
	})
	t.Run("v-x-frozen-op-abandoned", func(t *testing.T) {
		hs := newHarness(t)
		ref := hs.seed([]byte("v"), 3)
		hs.mustPoll(3, nil)
		p := hs.planeID()
		rot := rotPending(newOpIDT(t), ref)
		if err := hs.pl.Begin(rot); err != nil {
			t.Fatal(err)
		}
		// The effect finishes, then [H] freezes before the terminal line.
		hs.h.fn = func(pt string) error {
			if pt == "after-effect" {
				hs.h.fn = nil
				if err := hs.pl.Freeze(); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
		before := hs.status().OutboxMax
		if _, err := hs.pl.Commit(rot.OpID, Decision{Value: []byte("new")}); !errors.Is(err, ErrFrozen) {
			t.Fatalf("commit after freeze: %v", err)
		}
		if hs.status().OutboxMax != before {
			t.Fatal("frozen: no outbox append")
		}
		if st, _ := hs.pl.Classify(regPending(newOpIDT(t))); st != PendingFrozen {
			t.Fatal("frozen ignores pending")
		}
		if _, err := hs.pl.Observe(ref); !errors.Is(err, ErrFrozen) {
			t.Fatal("frozen makes no facts")
		}
		hs.mustPoll(3, nil)
		if k := hs.quarantineKeys(); !reflect.DeepEqual(k, []string{"abandoned:" + rot.OpID}) {
			t.Fatalf("quarantine %v", k)
		}
		hs.assertNewEpoch(p)
		// (x) no outcome anywhere; listed; P′ never processes it.
		if len(hs.outcomes(rot.OpID)) != 0 {
			t.Fatal("abandoned op has no outcome")
		}
		if st, _ := hs.pl.Classify(rot); st != PendingDisplayOnly {
			t.Fatalf("status %v", st)
		}
		if _, err := hs.pl.Apply(rot, Decision{Value: []byte("again")}); !errors.Is(err, ErrDisplayOnly) {
			t.Fatal("display only")
		}
		ids, _ := hs.pl.DisplayOnlyOpIDs()
		if !reflect.DeepEqual(ids, []string{rot.OpID}) {
			t.Fatal("display list")
		}
		// Restart in new-epoch-active keeps it abandoned (not resolved).
		hs.mustPoll(0, nil)
		hs.reopen()
		if len(hs.outcomes(rot.OpID)) != 0 || hs.status().Phase != "new-epoch-active" {
			t.Fatal("abandoned op must stay without outcome")
		}
	})
	t.Run("vii-first-poll-activates", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 2)
		hs.runEpoch(0, nil)
		np := hs.planeID()
		hs.mustPoll(0, nil)
		if st := hs.status(); st.Phase != "new-epoch-active" || st.PlaneID != np || st.RecordedAck != 0 {
			t.Fatalf("status %+v", st)
		}
		lines := bytes.Split(bytes.TrimSpace(hs.file(ledgerName)), []byte("\n"))
		if !bytes.Contains(lines[len(lines)-1], []byte(`"t":"epoch-active"`)) {
			t.Fatal("epoch-active must be recorded")
		}
	})
	t.Run("outcome-quarantined-opid-display-only", func(t *testing.T) {
		hs := newHarness(t)
		ref := hs.seed([]byte("v"), 1)
		hs.mustPoll(1, nil)
		rot := rotPending(newOpIDT(t), ref)
		hs.apply(rot, Decision{Value: []byte("w")})
		hs.runEpoch(1, nil)
		if st, _ := hs.pl.Classify(rot); st != PendingDisplayOnly {
			t.Fatal("quarantined outcome → display only in P′")
		}
		items, _ := hs.pl.QuarantineList()
		if len(items) != 1 || items[0].Fact != secretwire.FactOutcome || items[0].OpID != rot.OpID || items[0].Ref != ref {
			t.Fatalf("items %+v", items)
		}
	})
	t.Run("freeze-phase-rules", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 1)
		if err := hs.pl.Freeze(); err != nil {
			t.Fatal(err)
		}
		if err := hs.pl.Freeze(); !errors.Is(err, ErrEpochPhase) {
			t.Fatal("no double freeze")
		}
		hs.mustPoll(0, nil)
		if err := hs.pl.Freeze(); !errors.Is(err, ErrEpochPhase) {
			t.Fatal("split-recorded cannot freeze before activation")
		}
		hs.mustPoll(0, nil)
		if err := hs.pl.Freeze(); err != nil {
			t.Fatal("re-rejection in P′ runs the same procedure")
		}
	})
}

// (vi)/(ix): crash at every epoch phase and quarantine write/barrier point;
// restart resumes the same stage, quarantine is idempotent, and epoch-split
// is written only once every expected key is in the quarantine file.
func TestEpochCrashEveryPointFRRHZ138(t *testing.T) {
	points := []string{
		"ledger-mid-line:epoch-frozen", "ledger-appended:epoch-frozen",
		"ledger-mid-line:epoch-cutoff", "ledger-appended:epoch-cutoff",
		"quarantine-created", "quarantine-mid-line", "barrier-start", "barrier-before-sync", "barrier-done",
		"before-split", "ledger-mid-line:epoch-split", "ledger-appended:epoch-split",
		"ledger-mid-line:epoch-active", "ledger-appended:epoch-active",
	}
	want := []string{"after-cutoff:11", "after-cutoff:12"}
	for _, pt := range points {
		t.Run(pt, func(t *testing.T) {
			hs := newHarness(t)
			hs.seed([]byte("v"), 12)
			hs.mustPoll(10, nil)
			p := hs.planeID()
			hs.h.crashOn(pt, 1)
			hs.crash(func() {
				if err := hs.pl.Freeze(); err != nil {
					t.Fatal(err)
				}
				hs.poll(10, nil)
				hs.poll(0, nil) // first P′ poll
			})
			splitWritten := bytes.Contains(hs.file(ledgerName), []byte(`"t":"epoch-split"`))
			if splitWritten {
				// (ix) split only after every expected key is durable.
				recs, ok := parseQLines(hs.file(quarantineName))
				if !ok || len(recs) != 2 {
					t.Fatal("split before quarantine complete")
				}
			}
			hs.reopen()
			st := hs.status()
			switch st.Phase {
			case "normal":
				// Torn frozen record: freeze again.
				if err := hs.pl.Freeze(); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "frozen":
				if b, _ := hs.pl.NextBatch(p, 64); len(b) != 0 {
					t.Fatal("P never re-sends a non-empty batch")
				}
				hs.mustPoll(10, nil)
			}
			if st := hs.status(); st.Phase != "split-recorded" && st.Phase != "new-epoch-active" {
				t.Fatalf("phase %s", st.Phase)
			}
			if hs.planeID() == p {
				t.Fatal("must be on P′")
			}
			if k := hs.quarantineKeys(); !reflect.DeepEqual(k, want) {
				t.Fatalf("quarantine %v", k)
			}
			if recs, ok := parseQLines(hs.file(quarantineName)); !ok || len(recs) != 2 {
				t.Fatal("quarantine file must hold each key once")
			}
			hs.mustPoll(0, nil)
			if hs.status().Phase != "new-epoch-active" {
				t.Fatal("activation")
			}
		})
	}
}

// P4 exception: in frozen/cutoff/split an in-progress op is not resolved but
// abandoned; in new-epoch-active the normal recovery gives an outcome.
func TestEpochRecoveryExceptionFRRHZ138(t *testing.T) {
	t.Run("frozen-restart", func(t *testing.T) {
		hs := newHarness(t)
		ref := hs.seed([]byte("v"), 2)
		hs.mustPoll(2, nil)
		rot := rotPending(newOpIDT(t), ref)
		hs.pl.Begin(rot)
		hs.pl.Freeze()
		hs.reopen()
		if len(hs.outcomes(rot.OpID)) != 0 || hs.status().Phase != "frozen" {
			t.Fatal("frozen: no resolution")
		}
		hs.h.crashOn("before-split", 1)
		hs.crash(func() { hs.poll(2, nil) })
		hs.reopen() // cutoff-recorded → ③ + barrier + ④
		if hs.status().Phase != "split-recorded" || len(hs.outcomes(rot.OpID)) != 0 {
			t.Fatal("cutoff restart")
		}
		if k := hs.quarantineKeys(); !reflect.DeepEqual(k, []string{"abandoned:" + rot.OpID}) {
			t.Fatalf("quarantine %v", k)
		}
	})
	t.Run("split-restart-abandons-new-op", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 1)
		hs.runEpoch(0, nil)
		ref := hs.seed([]byte("w"), 1) // P′ work in split-recorded
		rot := rotPending(newOpIDT(t), ref)
		hs.pl.Begin(rot)
		hs.reopen()
		if len(hs.outcomes(rot.OpID)) != 0 {
			t.Fatal("split-recorded: no resolution")
		}
		k := hs.quarantineKeys()
		if !reflect.DeepEqual(k, []string{"after-cutoff:1", "abandoned:" + rot.OpID}) {
			t.Fatalf("quarantine %v", k)
		}
	})
	t.Run("active-restart-resolves", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 1)
		hs.runEpoch(0, nil)
		hs.mustPoll(0, nil)
		ref := hs.seed([]byte("w"), 1)
		rot := rotPending(newOpIDT(t), ref)
		hs.h.crashOn("after-effect", 1)
		hs.crash(func() { hs.pl.Apply(rot, Decision{Value: []byte("x")}) })
		hs.reopen()
		got := hs.outcomes(rot.OpID)
		if len(got) != 1 || got[0].Result != "ok" || got[0].Version != 2 {
			t.Fatalf("active recovery %+v", got)
		}
	})
}

// P5a: the barrier reopens the file and re-adds a key that is missing on
// disk before epoch-split is written; mid-file corruption refuses start.
func TestQuarantineBarrierAndCorruptionFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 12)
	hs.mustPoll(10, nil)
	path := filepath.Join(hs.dir, quarantineName)
	hs.h.fn = func(pt string) error {
		if pt == "barrier-start" {
			hs.h.fn = nil
			data, _ := os.ReadFile(path)
			lines := bytes.SplitAfter(data, []byte("\n"))
			os.WriteFile(path, lines[0], 0o600) // lose the second key
		}
		return nil
	}
	hs.runEpoch(10, nil)
	recs, ok := parseQLines(hs.file(quarantineName))
	if !ok || len(recs) != 2 {
		t.Fatalf("barrier must restore the missing key (%d)", len(recs))
	}
	i := hs.h.index(t, "barrier-start")
	j := hs.h.index(t, "ledger-appended:epoch-split")
	k := hs.h.index(t, "sync:quarantine-barrier")
	if !(i < k && k < j) {
		t.Fatal("barrier fsync before split")
	}

	// Torn last line: truncated at startup.
	hs.pl.Close()
	good := hs.file(quarantineName)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte(`{"k":"entry","plane`))
	f.Close()
	hs.reopen()
	if !bytes.Equal(good, hs.file(quarantineName)) {
		t.Fatal("torn tail")
	}
	// Mid-file corruption: refuse.
	hs.pl.Close()
	lines := bytes.SplitAfter(good, []byte("\n"))
	os.WriteFile(path, append([]byte("{bad}\n"), lines[1]...), 0o600)
	if _, err := hs.open(); !errors.Is(err, ErrQuarantineCorrupt) {
		t.Fatalf("got %v", err)
	}
}

// ZV (§8.1 ⑦): the sentinel used as a secret value appears in no ledger,
// quarantine, outbox/batch encoding, error string or quarantine list output;
// it is present (base64, the values encoding) only in the values file.
func TestZeroValueSentinelFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	s := sentinel(t)
	v := []byte(s)
	reg := regPending(newOpIDT(t))
	hs.apply(reg, Decision{Value: v})
	hs.apply(rotPending(newOpIDT(t), reg.Ref), Decision{Value: v})
	if _, err := hs.pl.RegisterDirect("direct-key", secretwire.RefKindOther, nil, v); err != nil {
		t.Fatal(err)
	}
	// Crash paths with the sentinel in flight.
	reg2 := regPending(newOpIDT(t))
	hs.h.crashOn("after-effect", 1)
	hs.crash(func() { hs.pl.Apply(reg2, Decision{Value: v}) })
	hs.reopen()
	reg3 := regPending(newOpIDT(t))
	hs.h.crashOn("values-before-rename", 1)
	hs.crash(func() { hs.pl.Apply(reg3, Decision{Value: v}) })
	hs.reopen()
	// Error paths.
	_, err := hs.pl.Commit(reg.OpID, Decision{Value: v})
	hs.note(err)
	hs.note(hs.pl.Begin(secretwire.Pending{OpID: s, Op: s, Ref: s, Name: s}))
	_, err = hs.pl.RegisterDirect(s, s, []string{s}, v)
	hs.note(err)
	_, err = hs.pl.Observe(s)
	hs.note(err)
	_, err = hs.pl.Commit(s, Decision{Value: v})
	hs.note(err)
	_, err = hs.pl.Apply(regPending(newOpIDT(t)), Decision{Reject: true, Value: v})
	hs.note(err)
	// Epoch with an abandoned op carrying the sentinel effect.
	rot := rotPending(newOpIDT(t), reg.Ref)
	hs.pl.Begin(rot)
	hs.mustPoll(3, rej(2, secretwire.EntrySeqConflict))
	hs.runEpoch(3, rej(2, secretwire.EntrySeqConflict))
	hs.mustPoll(0, nil)

	var frames bytes.Buffer
	b, err := secretwire.EncodeBatch(secretwire.Batch{V: 1, PlaneID: hs.planeID(), Entries: hs.batch()})
	if err != nil {
		t.Fatal(err)
	}
	frames.Write(b)
	for _, e := range hs.allEntries() {
		eb, _ := e.Entry.MarshalJSON()
		frames.Write(eb)
	}
	items, _ := hs.pl.QuarantineList()
	if len(items) == 0 {
		t.Fatal("quarantine list must be exercised")
	}
	list := FormatQuarantineList(items)

	assertNoForms(t, "ledger", hs.file(ledgerName), s)
	assertNoForms(t, "quarantine", hs.file(quarantineName), s)
	assertNoForms(t, "outbox", frames.Bytes(), s)
	assertNoForms(t, "quarantine list", []byte(list), s)
	if len(hs.errs) < 5 {
		t.Fatal("error paths must be exercised")
	}
	assertNoForms(t, "errors", joinErrs(hs.errs), s)
	st := hs.status()
	assertNoForms(t, "status", []byte(st.PlaneID+st.Phase), s)
	assertNoForms(t, "banner", []byte(Banner), s)
	vals := hs.file(valuesName)
	if !bytes.Contains(vals, []byte(zvForms(s)[2])) {
		t.Fatal("values file must hold the value")
	}
	if m := modeOf(t, filepath.Join(hs.dir, valuesName)); m.Perm() != 0o600 {
		t.Fatal("values mode 0600")
	}
	// Every other file in the state directory is value-free.
	ents, _ := os.ReadDir(hs.dir)
	for _, e := range ents {
		if e.Name() != valuesName {
			assertNoForms(t, "file", hs.file(e.Name()), s)
		}
	}
}
