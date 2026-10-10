package secretref

import (
	"errors"

	"rhizome/internal/peercred"
	"rhizome/internal/secretwire"
)

// Channel side of the single writer (S6, W6–W11). Every method that can
// append first checks v.SameUser(): a zero peercred.Verified is refused with
// ErrPeerRejected and zero writes. Ack and lastRejected are always computed
// from the folded events, never from counters.
//
// Entry rejection mapping (W10(b), §3.6). Checks run in this order and the
// first violation names the code:
//
//	rule                                  fact        entry code
//	R7a  planeId retired                  any         (whole batch: ErrRetiredPlane, W8)
//	R7   seq <= hw(P), content differs    any         seq-conflict      (W10a)
//	R7   seq >  hw(P)+1                   any         seq-gap           (W10c)
//	W6/R1 opId has no secretop            outcome     unknown-op
//	R1   opId already has an E4           outcome     redundant-outcome (§1.6)
//	V-A  ref != the op's (derived) ref    outcome     ref-mismatch
//	R1/R3 op is not the ref's pending op  outcome     state             (unreachable)
//	R4   version != expected              outcome     version-mismatch
//	V-A  ref != DeriveRef(opId)           reconciled  ref-mismatch
//	R2   ref introduced / opId is an op   reconciled  ref-exists
//	R6   version != 1                     reconciled  version-mismatch
//	V-A/R2 ref not introduced             observed    unknown-ref
//	R5   state not active|rotating|
//	     revoking|revoked                 observed    state
//	R5   version outside 1..current       observed    version-mismatch
//	R3   attested target                  outcome     attested-op       (unreachable: approval Q2)
//
// Any remaining fold rejection of the candidate (defence in depth) maps to
// state. An entry whose fields break the §0.1 grammar never passed frame
// decoding; it is refused with secretwire.ErrFrameMalformed and no E7.

// EntryRejectedError is a single Record* refusal; Reason is a §3.6 entry code.
type EntryRejectedError struct{ Reason string }

func (e *EntryRejectedError) Error() string { return secretwire.EntryRejectedPrefix + e.Reason }

// PollState is what one poll carries (W7, W8, R7b).
type PollState struct {
	// Retired is R7a: the channel must send error retired-plane instead of
	// a poll; the other fields are then empty.
	Retired      bool
	Ack          secretwire.Ack
	LastRejected *secretwire.Rejected
	Pending      []secretwire.Pending
}

// BatchResult is the outcome of ProcessBatch.
type BatchResult struct {
	Appended          int                  // facts appended (E4–E6)
	Rejection         *secretwire.Rejected // first rejected entry {seq, reason}, nil if none
	RejectionRecorded bool                 // an E7 was appended (W11①)
	Ack               secretwire.Ack       // post-append durable ack (W8, W11②)
}

func (s *Service) channelBegin(v peercred.Verified, planeID string) (*Projection, error) {
	if !v.SameUser() {
		return nil, ErrPeerRejected
	}
	if s == nil {
		return nil, ErrRegistryUnavailable
	}
	if !secretwire.ValidPlaneID(planeID) {
		return nil, secretwire.ErrFrameMalformed
	}
	p, err := s.begin()
	if err != nil {
		return nil, err
	}
	if p.Retired(planeID) {
		return nil, ErrRetiredPlane
	}
	return p, nil
}

// PollState computes, inside the critical section, the poll contents for
// plane P: ack, lastRejected(P) and up to 64 oldest pending requests.
func (s *Service) PollState(planeID string) (PollState, error) {
	if s == nil {
		return PollState{}, ErrRegistryUnavailable
	}
	if !secretwire.ValidPlaneID(planeID) {
		return PollState{}, secretwire.ErrFrameMalformed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.begin()
	if err != nil {
		return PollState{}, err
	}
	if p.Retired(planeID) {
		return PollState{Retired: true}, nil
	}
	ps := PollState{Ack: p.Ack(planeID), LastRejected: p.LastRejected(planeID), Pending: []secretwire.Pending{}}
	for _, o := range p.Pending(secretwire.MaxPending) {
		ps.Pending = append(ps.Pending, secretwire.Pending{OpID: o.OpID, Op: o.Op, Ref: o.Ref, Name: o.Name,
			RefKind: o.RefKind, ScopeTags: o.ScopeTags, ReasonCode: o.ReasonCode, JournalSeq: o.JournalSeq})
	}
	return ps, nil
}

// ProcessBatch is W10 + W11① for one batch of plane P under the single
// writer: entries are handled in order; the first rejection stops the batch
// and records E7 unless lastRejected(P) already equals (seq, reason).
// Entries appended before a rejection stay.
//
// Stall (W11): once lastRejected(P) is set (P's latest E7 has AckSeq ==
// hw(P)) the channel for P is stalled and the only recovery is an epoch
// replacement (P5, a new planeId that retires P). A stalled P appends
// nothing: every batch, whatever its content and including an empty one,
// returns Rejection = lastRejected(P), RejectionRecorded = false and the
// unchanged ack, with zero writes, so the channel can repeat the same
// entry-rejected notice. Because nothing appends, hw(P) never moves and the
// stall is durable across restarts; PollState keeps reporting ack and
// lastRejected. On an error the returned result
// still counts the appended facts but carries no ack: a channel must not
// send frames after its own append failed (§1.8).
func (s *Service) ProcessBatch(v peercred.Verified, planeID string, entries []secretwire.Entry) (BatchResult, error) {
	if !v.SameUser() {
		return BatchResult{}, ErrPeerRejected
	}
	if s == nil {
		return BatchResult{}, ErrRegistryUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.channelBegin(v, planeID)
	if err != nil {
		return BatchResult{}, err
	}
	if lr := p.LastRejected(planeID); lr != nil {
		return BatchResult{Rejection: lr, Ack: p.Ack(planeID)}, nil // stalled
	}
	var res BatchResult
	for _, e := range entries {
		appended, reason, err := s.processEntry(p, planeID, e)
		if err != nil {
			return BatchResult{Appended: res.Appended}, err
		}
		if appended {
			res.Appended++
		}
		if reason != "" {
			res.Rejection = &secretwire.Rejected{Seq: e.Seq, Reason: reason}
			break
		}
	}
	if res.Rejection != nil {
		recorded, err := s.recordRejection(p, planeID, res.Rejection.Seq, res.Rejection.Reason)
		if err != nil {
			return BatchResult{Appended: res.Appended}, err
		}
		res.RejectionRecorded = recorded
	}
	res.Ack = p.Ack(planeID)
	return res, nil
}

// RecordOutcome records one outcome entry (E4) under W10 without E7. The
// single-entry Record* methods refuse with ErrStalled while P is stalled.
func (s *Service) RecordOutcome(v peercred.Verified, planeID string, e secretwire.Entry) (bool, error) {
	return s.recordOne(v, planeID, e, secretwire.FactOutcome)
}

// RecordReconciled records one reconciled entry (E5) under W10 without E7.
func (s *Service) RecordReconciled(v peercred.Verified, planeID string, e secretwire.Entry) (bool, error) {
	return s.recordOne(v, planeID, e, secretwire.FactReconciled)
}

// RecordObserved records one observed entry (E6) under W10 without E7.
func (s *Service) RecordObserved(v peercred.Verified, planeID string, e secretwire.Entry) (bool, error) {
	return s.recordOne(v, planeID, e, secretwire.FactObserved)
}

// RecordRejection records E7 {P, rejectedSeq, reason, AckSeq: hw(P)}. It
// writes nothing (false, nil) when lastRejected(P) already equals (seq,
// reason), refuses any other pair while P is stalled (ErrStalled), and
// refuses a pair that breaks the R7b relation.
func (s *Service) RecordRejection(v peercred.Verified, planeID string, rejectedSeq uint64, reason string) (bool, error) {
	if !v.SameUser() {
		return false, ErrPeerRejected
	}
	if s == nil {
		return false, ErrRegistryUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.channelBegin(v, planeID)
	if err != nil {
		return false, err
	}
	if !secretwire.ValidSeq(rejectedSeq) || !secretwire.ValidEntryReason(reason) ||
		!rejectionRelation(rejectedSeq, reason, p.HighWater(planeID)) {
		return false, secretwire.ErrFrameMalformed
	}
	if lr := p.LastRejected(planeID); lr != nil {
		if lr.Seq == rejectedSeq && lr.Reason == reason {
			return false, nil
		}
		return false, ErrStalled
	}
	return s.recordRejection(p, planeID, rejectedSeq, reason)
}

func (s *Service) recordOne(v peercred.Verified, planeID string, e secretwire.Entry, want string) (bool, error) {
	if !v.SameUser() {
		return false, ErrPeerRejected
	}
	if s == nil {
		return false, ErrRegistryUnavailable
	}
	if e.Fact != want {
		return false, secretwire.ErrFrameMalformed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.channelBegin(v, planeID)
	if err != nil {
		return false, err
	}
	if p.LastRejected(planeID) != nil {
		return false, ErrStalled // W11: stalled until epoch replacement
	}
	appended, reason, err := s.processEntry(p, planeID, e)
	if err != nil {
		return false, err
	}
	if reason != "" {
		return false, &EntryRejectedError{Reason: reason}
	}
	return appended, nil
}

// recordRejection is W11①. Caller holds s.mu.
func (s *Service) recordRejection(p *Projection, planeID string, seq uint64, reason string) (bool, error) {
	if lr := p.LastRejected(planeID); lr != nil && lr.Seq == seq && lr.Reason == reason {
		return false, nil
	}
	err := s.write(p, TypeEntryRejected, AggregateChan, planeID, planeID, entryRejected{
		PlaneID: planeID, RejectedSeq: seq, Reason: reason, AckSeq: p.HighWater(planeID)})
	if err != nil {
		var re *ReplayError
		if errors.As(err, &re) {
			return false, secretwire.ErrFrameMalformed
		}
		return false, err
	}
	return true, nil
}

// candidate maps an entry to its event type, aggregate, correlation and
// canonical payload (the same value W10(a) compares).
func candidate(planeID string, e secretwire.Entry) (typ, aggType, aggID, corrID string, payload any, ok bool) {
	switch e.Fact {
	case secretwire.FactOutcome:
		return TypeOpOutcome, AggregateOp, e.OpID, e.OpID, opOutcome{OpID: e.OpID, Ref: e.Ref,
			Version: int64(e.Version), Result: e.Result, FailureCode: e.FailureCode,
			PlaneID: planeID, PlaneEntrySeq: e.Seq}, true
	case secretwire.FactReconciled:
		return TypeReconciled, AggregateRef, e.Ref, e.OpID, reconciled{Ref: e.Ref, OpID: e.OpID, Name: e.Name,
			Kind: e.RefKind, ScopeTags: clone(e.ScopeTags), Version: int64(e.Version), Origin: e.Origin,
			PlaneID: planeID, PlaneEntrySeq: e.Seq}, true
	case secretwire.FactObserved:
		return TypeObserved, AggregateRef, e.Ref, planeID, observed{Ref: e.Ref, Version: int64(e.Version),
			Present: e.Present, PlaneObservedAt: e.PlaneObservedAt, PlaneID: planeID, PlaneEntrySeq: e.Seq}, true
	}
	return "", "", "", "", nil, false
}

// processEntry is W10 for one entry. It returns whether a fact was appended,
// the entry code of a rejection, or an error that stops the channel.
func (s *Service) processEntry(p *Projection, planeID string, e secretwire.Entry) (bool, string, error) {
	if !secretwire.ValidSeq(e.Seq) || e.Version > secretwire.MaxVersion {
		return false, "", secretwire.ErrFrameMalformed
	}
	typ, aggType, aggID, corrID, payload, ok := candidate(planeID, e)
	hw := p.HighWater(planeID)
	switch {
	case e.Seq <= hw: // W10(a)
		if ok && p.sameFact(planeID, e.Seq, typ, payload) {
			return false, "", nil
		}
		return false, secretwire.EntrySeqConflict, nil
	case e.Seq > hw+1: // W10(c)
		return false, secretwire.EntrySeqGap, nil
	}
	if !ok {
		return false, "", secretwire.ErrFrameMalformed
	}
	if reason := p.entryCheck(e); reason != "" {
		return false, reason, nil
	}
	if err := s.write(p, typ, aggType, aggID, corrID, payload); err != nil {
		var re *ReplayError
		if errors.As(err, &re) {
			if re.Code == codeVJ || re.Code == codeVK || re.Code == codeVZ {
				return false, "", secretwire.ErrFrameMalformed
			}
			return false, secretwire.EntryState, nil
		}
		return false, "", err
	}
	return true, "", nil
}

// entryCheck is W10(b): R2–R6 against the current projection, mapped to
// §3.6 entry codes (table above). R7/R7a are handled by the caller.
func (p *Projection) entryCheck(e secretwire.Entry) string {
	switch e.Fact {
	case secretwire.FactOutcome:
		op := p.ops[e.OpID]
		switch {
		case op == nil:
			return secretwire.EntryUnknownOp
		case op.done:
			return secretwire.EntryRedundantOutcome
		case e.Ref != op.ref:
			return secretwire.EntryRefMismatch
		}
		r := p.refs[op.ref]
		if r == nil || r.pending != op.opID {
			return secretwire.EntryState
		}
		if int64(e.Version) != expectedVersion(op.op, e.Result, r.view.Version) {
			return secretwire.EntryVersionMismatch
		}
	case secretwire.FactReconciled:
		switch {
		case e.Ref != secretwire.DeriveRef(e.OpID):
			return secretwire.EntryRefMismatch
		case p.refs[e.Ref] != nil || p.ops[e.OpID] != nil:
			return secretwire.EntryRefExists
		case e.Version != 1:
			return secretwire.EntryVersionMismatch
		}
	case secretwire.FactObserved:
		r := p.refs[e.Ref]
		if r == nil {
			return secretwire.EntryUnknownRef
		}
		switch r.view.State {
		case StateActive, StateRotating, StateRevoking, StateRevoked:
		default:
			return secretwire.EntryState
		}
		if e.Version < 1 || int64(e.Version) > r.view.Version {
			return secretwire.EntryVersionMismatch
		}
	}
	return ""
}
