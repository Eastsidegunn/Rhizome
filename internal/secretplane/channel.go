package secretplane

import (
	"sort"

	"rhizome/internal/secretwire"
)

// HelloPlaneID returns the planeId to announce in the plane hello: P until
// the epoch split, then P′ (P5 ④).
func (pl *Plane) HelloPlaneID() (string, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return "", err
	}
	return pl.st.planeID, nil
}

// halt is the R7b fail-closed stop: an ack or rejection from Rhizome that
// contradicts the plane's records stops batch sending until restart; the
// front end reports it to the operator. Nothing is written.
func (pl *Plane) halt() error {
	pl.halted = true
	return ErrHalted
}

// checkAck: ack belongs to the hello planeId, is not below the recorded ack
// and not above the outbox maximum (W11 plane-side checks). While a
// rejection is recorded for P the stall is durable until epoch replacement:
// an ack beyond the rejection's ack is a violation, not progress.
func (pl *Plane) checkAck(p string, ack secretwire.Ack) bool {
	if l := pl.st.rejs[p]; l != nil && ack.PlaneEntrySeq > l.AckSeq {
		return false
	}
	return ack.PlaneID == p && ack.PlaneEntrySeq >= pl.st.ackSeq(p) && ack.PlaneEntrySeq <= pl.st.maxSeq(p) &&
		secretwire.ValidJournalSeq(ack.JournalSeq)
}

func (pl *Plane) checkRej(p string, seq uint64, reason string, ack uint64) bool {
	return secretwire.ValidSeq(seq) && secretwire.ValidEntryReason(reason) && rejRelation(seq, reason, ack) &&
		seq <= pl.st.maxSeq(p)
}

func (pl *Plane) channelEntry(helloPlaneID string) error {
	if err := pl.usable(); err != nil {
		return err
	}
	if pl.halted {
		return ErrHalted
	}
	if helloPlaneID != pl.st.planeID {
		return ErrStalePlane
	}
	return nil
}

// recordAck appends an ack line only when larger than the recorded one
// (P2, within one epoch).
func (pl *Plane) recordAck(ack secretwire.Ack) error {
	if ack.PlaneEntrySeq <= pl.st.ackSeq(ack.PlaneID) {
		return nil
	}
	return pl.appendRec(&rec{T: recAck, PlaneID: ack.PlaneID, Seq: ack.PlaneEntrySeq, JournalSeq: ack.JournalSeq})
}

// recordRej appends one line holding the rejection together with the ack it
// came with (unless equal to the latest recorded rejection); a seq-conflict
// is then quarantined at once, barrier included (P2). The ack is never
// written ahead of the rejection.
func (pl *Plane) recordRej(ack secretwire.Ack, seq uint64, reason string) error {
	p := ack.PlaneID
	if l := pl.st.rejs[p]; l == nil || *l != (rejRec{seq, reason, ack.PlaneEntrySeq}) {
		r := &rec{T: recRej, PlaneID: p, Seq: seq, Reason: reason, AckSeq: ack.PlaneEntrySeq}
		if ack.PlaneEntrySeq > pl.st.ackSeq(p) {
			r.JournalSeq = ack.JournalSeq
		}
		if err := pl.appendRec(r); err != nil {
			return err
		}
	}
	if reason == secretwire.EntrySeqConflict {
		return pl.quarantineConflict(p, seq)
	}
	return nil
}

// OnPoll handles a poll received on a connection whose plane hello carried
// helloPlaneID: R7b sanity checks (else halt), epoch-active on the first
// poll of P′ (P5 ⑤), ack and lastRejected recording (P2), and, while
// frozen, the cutoff → quarantine → barrier → split sequence (P5 ②–④).
func (pl *Plane) OnPoll(helloPlaneID string, ack secretwire.Ack, lastRejected *secretwire.Rejected) error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.channelEntry(helloPlaneID); err != nil {
		return err
	}
	p := helloPlaneID
	if pl.st.phase == phaseCutoff {
		return ErrEpochPhase
	}
	if !pl.checkAck(p, ack) {
		return pl.halt()
	}
	if lastRejected != nil && !pl.checkRej(p, lastRejected.Seq, lastRejected.Reason, ack.PlaneEntrySeq) {
		return pl.halt()
	}
	if pl.st.phase == phaseSplit {
		if err := pl.appendRec(&rec{T: recEpochActive, PlaneID: p}); err != nil {
			return err
		}
	}
	if lastRejected != nil {
		if err := pl.recordRej(ack, lastRejected.Seq, lastRejected.Reason); err != nil {
			return err
		}
	} else if err := pl.recordAck(ack); err != nil {
		return err
	}
	if pl.st.phase != phaseFrozen {
		return nil
	}
	r := &rec{T: recEpochCutoff, PlaneID: p, Cutoff: &ack.PlaneEntrySeq}
	if lastRejected != nil {
		r.Seq, r.Reason = lastRejected.Seq, lastRejected.Reason
	}
	if err := pl.appendRec(r); err != nil {
		return err
	}
	return pl.finishSplit()
}

// OnEntryRejected handles an entry-rejected error frame (W11): the same
// sanity checks as OnPoll, then ack and rejection recording.
func (pl *Plane) OnEntryRejected(helloPlaneID, reason string, ack secretwire.Ack, rejectedSeq uint64) error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.channelEntry(helloPlaneID); err != nil {
		return err
	}
	p := helloPlaneID
	if !pl.checkAck(p, ack) || !pl.checkRej(p, rejectedSeq, reason, ack.PlaneEntrySeq) {
		return pl.halt()
	}
	if pl.st.phase == phaseCutoff {
		return ErrEpochPhase
	}
	return pl.recordRej(ack, rejectedSeq, reason)
}

// OnErrorFrame dispatches an error frame from Rhizome. Only entry-rejected
// carries an ack; every other code (journal-poisoned in particular) is not
// an ack and changes nothing (P2).
func (pl *Plane) OnErrorFrame(helloPlaneID string, f secretwire.ErrorFrame) error {
	if f.Code != secretwire.CodeEntryRejected || f.Ack == nil {
		return nil
	}
	return pl.OnEntryRejected(helloPlaneID, f.Reason, *f.Ack, f.RejectedSeq)
}

// stalled: a rejection is recorded for p. The stall lasts until epoch
// replacement (W11: the only recovery path); batches keep being delivered so
// Rhizome repeats the same rejection with zero writes.
func (pl *Plane) stalled(p string) bool { return pl.st.rejs[p] != nil }

// NextBatch returns the unacked outbox entries of helloPlaneID in seq order
// (at most max, clamped to 1..64). It is empty while frozen or
// cutoff-recorded (P5 ①: P sends only entries:[]). A stalled plane keeps
// delivering its unacked entries (W11).
func (pl *Plane) NextBatch(helloPlaneID string, max int) ([]secretwire.Entry, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.channelEntry(helloPlaneID); err != nil {
		return nil, err
	}
	p := helloPlaneID
	if !pl.st.phase.outboxOpen() {
		return []secretwire.Entry{}, nil
	}
	if max < 1 || max > secretwire.MaxEntries {
		max = secretwire.MaxEntries
	}
	out := []secretwire.Entry{}
	for _, e := range pl.st.outbox[p][pl.st.ackSeq(p):] {
		if len(out) == max || pl.q.hasSeq(p, e.Seq) {
			break
		}
		out = append(out, cloneEntry(e))
	}
	return out, nil
}

// OutboxEntry is a retained outbox entry.
type OutboxEntry struct {
	PlaneID string
	Entry   secretwire.Entry
}

// Outbox lists the entries that the P2 cleanup invariant still retains:
// an entry is removable only if (a) seq ≤ recorded ack and seq is not a
// recorded seq-conflict seq, or (b) it is durably quarantined (the barrier
// has passed for every key loaded or written by this process). The ledger
// itself is append-only; removal is logical.
func (pl *Plane) Outbox() ([]OutboxEntry, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return nil, err
	}
	var out []OutboxEntry
	for _, p := range pl.st.order {
		ack := pl.st.ackSeq(p)
		for _, e := range pl.st.outbox[p] {
			if (e.Seq <= ack && !pl.st.conflicts[p][e.Seq]) || pl.q.hasSeq(p, e.Seq) {
				continue
			}
			out = append(out, OutboxEntry{PlaneID: p, Entry: cloneEntry(e)})
		}
	}
	return out, nil
}

// Status is a value-free summary for the front end.
type Status struct {
	PlaneID      string
	Phase        string
	RecordedAck  uint64
	OutboxMax    uint64
	Stalled      bool
	Halted       bool
	LastRejected *secretwire.Rejected
}

// Status reports the current plane state.
func (pl *Plane) Status() (Status, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return Status{}, err
	}
	p := pl.st.planeID
	s := Status{PlaneID: p, Phase: pl.st.phase.String(), RecordedAck: pl.st.ackSeq(p), OutboxMax: pl.st.maxSeq(p),
		Stalled: pl.stalled(p), Halted: pl.halted}
	if l := pl.st.rejs[p]; l != nil {
		s.LastRejected = &secretwire.Rejected{Seq: l.Seq, Reason: l.Reason}
	}
	return s, nil
}

// Freeze starts an epoch replacement (P5 ①). It must be invoked only by
// [H]'s interactive command in the plane front end (D19(d)); there is no
// other trigger. The record lists the operations in progress, which become
// abandoned.
func (pl *Plane) Freeze() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return err
	}
	if pl.st.phase != phaseNormal && pl.st.phase != phaseActive {
		return ErrEpochPhase
	}
	var live []string
	for _, id := range pl.st.inProgress() {
		if !pl.q.abandoned[id] {
			live = append(live, id)
		}
	}
	return pl.appendRec(&rec{T: recEpochFrozen, PlaneID: pl.st.planeID, InProgress: live})
}

// expectedKeys is the P5 ③ key set: every outbox entry of P with seq > C,
// the plane's entry at each recorded seq-conflict R ≤ C (including the
// cutoff lastRejected), and every operation in progress at ①.
func (pl *Plane) expectedKeys() []qrec {
	s := pl.st
	p, c := s.planeID, s.cutoff
	var exp []qrec
	conf := map[uint64]bool{}
	for seq := range s.conflicts[p] {
		conf[seq] = true
	}
	if s.cutoffRej != nil && s.cutoffRej.Reason == secretwire.EntrySeqConflict {
		conf[s.cutoffRej.Seq] = true
	}
	var cs []uint64
	for seq := range conf {
		if seq <= c && seq <= s.maxSeq(p) {
			cs = append(cs, seq)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i] < cs[j] })
	for _, seq := range cs {
		exp = append(exp, entryQ(p, s.outbox[p][seq-1], QuarantineSeqConflict))
	}
	for _, e := range s.outbox[p] {
		if e.Seq > c {
			exp = append(exp, entryQ(p, e, QuarantineAfterCutoff))
		}
	}
	for _, id := range s.frozenOps {
		op := s.ops[id]
		exp = append(exp, qrec{K: qkAbandoned, PlaneID: p, OpID: id, Op: op.Op, Ref: op.Ref, Reason: QuarantineAbandoned})
	}
	return exp
}

// finishSplit runs P5 ③ and ④ from cutoff-recorded: quarantine the expected
// keys (idempotent), pass the P5a barrier, then record epoch-split with a
// fresh P′ whose outbox is empty.
func (pl *Plane) finishSplit() error {
	exp := pl.expectedKeys()
	for _, r := range exp {
		if err := pl.quarantine(r); err != nil {
			return err
		}
	}
	if err := pl.barrier(exp); err != nil {
		return err
	}
	pl.at("before-split")
	np, err := pl.newPlaneID()
	if err != nil {
		return pl.fatal()
	}
	return pl.appendRec(&rec{T: recEpochSplit, PlaneID: pl.st.planeID, NewPlaneID: np})
}
