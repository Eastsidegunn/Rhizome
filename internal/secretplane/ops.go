package secretplane

import (
	"errors"

	"rhizome/internal/secretwire"
)

// PendingStatus classifies a pending request received from Rhizome.
type PendingStatus int

const (
	// PendingNew can be started with Begin.
	PendingNew PendingStatus = iota
	// PendingInProgress was begun and awaits Commit.
	PendingInProgress
	// PendingDone already has its terminal outcome (P1 dedupe: no new
	// outcome; Apply/Commit return the recorded one).
	PendingDone
	// PendingDisplayOnly is abandoned or has a quarantined outcome: shown to
	// [H] only, never processed (P5).
	PendingDisplayOnly
	// PendingFrozen arrived while the epoch is frozen: ignored (P5 ①).
	PendingFrozen
	// PendingInvalid does not match the plane state (unknown or busy ref,
	// ref not derived from opId, same opId with different content, …).
	PendingInvalid
)

// Decision is the confirmed human action for an operation, collected by the
// interactive front end.
type Decision struct {
	// Reject records the human's rejection (outcome rejected, no effect).
	Reject bool
	// Value is the new value for register and for rotate of a stored ref.
	// It must be nil for revoke and for any operation on an attested ref
	// (record-only: the plane never touches values of attested refs, Q2).
	// The plane copies it; the caller should wipe its slice afterwards.
	Value []byte
}

// Result is the terminal outcome of an operation.
type Result struct {
	OpID string
	Ref  string
	// Result and FailureCode are the G7/G8 terminal values.
	Result      string
	FailureCode string
	// Entry is the outbox entry recorded with the terminal line (nil for a
	// failed plane-direct registration, which has no fact).
	Entry *secretwire.Entry
	// Duplicate is true when the operation was already terminal: nothing was
	// written (P1).
	Duplicate bool
}

func (op *opRec) result(dup bool) Result {
	r := Result{OpID: op.OpID, Ref: op.Ref, Result: op.Result, FailureCode: op.Failure, Duplicate: dup}
	if op.Entry != nil {
		e := cloneEntry(*op.Entry)
		r.Entry = &e
	}
	return r
}

func (pl *Plane) displayOnly(opID string) bool {
	return pl.st.abandoned[opID] || pl.q.abandoned[opID] || pl.q.outcomeOps[opID]
}

func (pl *Plane) abandonedOp(opID string) bool { return pl.st.abandoned[opID] || pl.q.abandoned[opID] }

func beginRec(p secretwire.Pending, planeID string, attested bool) *rec {
	return &rec{T: recBegin, PlaneID: planeID, OpID: p.OpID, Op: p.Op, Ref: p.Ref, Origin: originRequest,
		Attested: attested, Name: p.Name, RefKind: p.RefKind, ScopeTags: cloneTags(p.ScopeTags), ReasonCode: p.ReasonCode}
}

// sameRequest compares the canonical pending content with the stored
// operation (opId dedupe, P1): register name/refKind/scopeTags, rotate ref,
// revoke ref and reasonCode.
func sameRequest(op *opRec, p secretwire.Pending) bool {
	if op.Origin != originRequest || op.Op != p.Op || op.Ref != p.Ref {
		return false
	}
	switch p.Op {
	case secretwire.OpRegister:
		return op.Name == p.Name && op.RefKind == p.RefKind && sameTags(op.ScopeTags, p.ScopeTags) && p.ReasonCode == ""
	case secretwire.OpRotate:
		return len(p.ScopeTags) == 0 && p.ReasonCode == ""
	case secretwire.OpRevoke:
		return op.ReasonCode == p.ReasonCode && len(p.ScopeTags) == 0
	}
	return false
}

func (pl *Plane) classify(p secretwire.Pending) PendingStatus {
	s := pl.st
	if op := s.ops[p.OpID]; op != nil {
		if !sameRequest(op, p) {
			return PendingInvalid
		}
		switch {
		case pl.displayOnly(p.OpID):
			return PendingDisplayOnly
		case !s.phase.outboxOpen():
			return PendingFrozen
		case op.Done:
			return PendingDone
		}
		return PendingInProgress
	}
	if !s.phase.outboxOpen() {
		return PendingFrozen
	}
	attested := false
	if ref := s.refs[p.Ref]; ref != nil && p.Op != secretwire.OpRegister {
		attested = ref.Origin == originAttested
	}
	if !secretwire.ValidJournalSeq(p.JournalSeq) || s.checkBegin(beginRec(p, s.planeID, attested)) != nil {
		return PendingInvalid
	}
	return PendingNew
}

// Classify reports how the plane treats a pending request (P1, P5).
func (pl *Plane) Classify(p secretwire.Pending) (PendingStatus, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return 0, err
	}
	return pl.classify(p), nil
}

func statusErr(st PendingStatus) error {
	switch st {
	case PendingDone:
		return ErrDone
	case PendingDisplayOnly:
		return ErrDisplayOnly
	case PendingFrozen:
		return ErrFrozen
	case PendingInvalid:
		return ErrInvalidPending
	}
	return nil
}

// Begin records the in-progress line for a new pending request (P3 ②)
// before the front end collects the human's decision. Begin of an operation
// already in progress is a no-op. It returns ErrDone, ErrDisplayOnly,
// ErrFrozen or ErrInvalidPending per Classify, writing nothing.
func (pl *Plane) Begin(p secretwire.Pending) error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return err
	}
	st := pl.classify(p)
	if st == PendingInProgress {
		return nil
	}
	if st != PendingNew {
		return statusErr(st)
	}
	attested := false
	if p.Op != secretwire.OpRegister {
		attested = pl.st.refs[p.Ref].Origin == originAttested
	}
	return pl.appendRec(beginRec(p, pl.st.planeID, attested))
}

// Commit executes the human decision for an in-progress operation and writes
// its terminal line together with its outbox entry (P3 ③ ④). For a stored
// ref the effect on the values file happens between the two lines; for an
// attested ref (record-only, Q2) and for a rejection there is no effect.
// Versions follow R4: register ok 1, rotate ok current+1, revoke ok current,
// failed/rejected current (register 0). A values-file failure gives
// failed/backend-error. Commit of a terminal operation returns the recorded
// Result with Duplicate set and writes nothing. If the epoch was frozen
// before the terminal line, the operation stays in progress (to be
// abandoned at ③) and ErrFrozen is returned.
func (pl *Plane) Commit(opID string, d Decision) (Result, error) {
	pl.mu.Lock()
	locked := true
	defer func() {
		if locked {
			pl.mu.Unlock()
		}
	}()
	if err := pl.usable(); err != nil {
		return Result{}, err
	}
	op := pl.st.ops[opID]
	if op == nil {
		return Result{}, ErrUnknownOp
	}
	if pl.displayOnly(opID) {
		return Result{}, ErrDisplayOnly
	}
	if op.Done {
		return op.result(true), nil
	}
	if !pl.st.phase.outboxOpen() {
		return Result{}, ErrFrozen
	}
	if pl.committing[opID] {
		return Result{}, ErrOpBusy
	}
	if !decisionOK(op.Op, op.Attested, op.Origin == originPlaneDirect, d) {
		return Result{}, ErrInvalidDecision
	}
	if op.Op == secretwire.OpRotate && !d.Reject && pl.st.refs[op.Ref].Version >= secretwire.MaxVersion {
		// G12: version+1 would leave the grammar. Refused before any
		// effect; the outcome keeps the current version.
		pl.at("before-terminal")
		if err := pl.appendResult(op, secretwire.ResultFailed, secretwire.FailureInvalidState); err != nil {
			return Result{}, err
		}
		return op.result(false), nil
	}
	effect := !d.Reject && !op.Attested
	if !effect {
		result := secretwire.ResultOK
		if d.Reject {
			result = secretwire.ResultRejected
		}
		pl.at("before-terminal")
		if err := pl.appendResult(op, result, ""); err != nil {
			return Result{}, err
		}
		return op.result(false), nil
	}

	var version uint64 = 1
	if op.Op == secretwire.OpRotate {
		version = pl.st.refs[op.Ref].Version + 1
	}
	pl.committing[opID] = true
	pl.mu.Unlock()
	locked = false

	pl.at("before-effect")
	effErr := pl.applyValue(op.Op, op.Ref, opID, version, d.Value)
	if effErr == nil {
		pl.at("after-effect")
	}

	pl.mu.Lock()
	locked = true
	delete(pl.committing, opID)
	if err := pl.usable(); err != nil {
		return Result{}, err
	}
	if effErr != nil && !errors.Is(effErr, errBackend) {
		return Result{}, effErr
	}
	if !pl.st.phase.outboxOpen() || pl.abandonedOp(opID) {
		// Frozen during the effect: no terminal, no outbox (P5 ①).
		return Result{}, ErrFrozen
	}
	pl.at("before-terminal")
	if effErr != nil {
		err := pl.appendResult(op, secretwire.ResultFailed, secretwire.FailureBackendError)
		if err != nil {
			return Result{}, err
		}
	} else if err := pl.appendResult(op, secretwire.ResultOK, ""); err != nil {
		return Result{}, err
	}
	return op.result(false), nil
}

// decisionOK checks the decision shape: a rejection carries no value (and a
// plane-direct registration cannot be rejected); register and rotate of a
// stored ref need a value; revoke and every attested operation take none.
func decisionOK(op string, attested, direct bool, d Decision) bool {
	if d.Reject {
		return d.Value == nil && !direct
	}
	if attested || op == secretwire.OpRevoke {
		return d.Value == nil
	}
	return len(d.Value) > 0
}

// Apply is Begin followed by Commit, idempotent by opId: a terminal
// operation returns its recorded Result (Duplicate) and writes nothing. The
// decision shape is checked before anything is written.
func (pl *Plane) Apply(p secretwire.Pending, d Decision) (Result, error) {
	pl.mu.Lock()
	attested := false
	if ref := pl.st.refs[p.Ref]; ref != nil && p.Op != secretwire.OpRegister {
		attested = ref.Origin == originAttested
	}
	pl.mu.Unlock()
	if !decisionOK(p.Op, attested, false, d) {
		return Result{}, ErrInvalidDecision
	}
	if err := pl.Begin(p); err != nil && !errors.Is(err, ErrDone) {
		return Result{}, err
	}
	return pl.Commit(p.OpID, d)
}

// RegisterDirect registers a value directly at the plane (reconciled fact,
// origin plane-direct). The plane generates the opId; ref = DeriveRef(opId).
// The same in-progress → effect → terminal sequence as Commit applies; a
// failed effect leaves a terminal failure with no fact.
func (pl *Plane) RegisterDirect(name, refKind string, scopeTags []string, value []byte) (Result, error) {
	if len(value) == 0 {
		return Result{}, ErrInvalidInput
	}
	pl.mu.Lock()
	if err := pl.usable(); err != nil {
		pl.mu.Unlock()
		return Result{}, err
	}
	if !pl.st.phase.outboxOpen() {
		pl.mu.Unlock()
		return Result{}, ErrFrozen
	}
	if !secretwire.ValidName(name) || !secretwire.ValidRefKind(refKind) || !validTags(scopeTags) {
		pl.mu.Unlock()
		return Result{}, ErrInvalidInput
	}
	opID, err := pl.newOpID()
	if err != nil {
		pl.mu.Unlock()
		return Result{}, err
	}
	r := &rec{T: recBegin, PlaneID: pl.st.planeID, OpID: opID, Op: secretwire.OpRegister, Ref: secretwire.DeriveRef(opID),
		Origin: originPlaneDirect, Name: name, RefKind: refKind, ScopeTags: cloneTags(scopeTags)}
	if err := pl.appendRec(r); err != nil {
		pl.mu.Unlock()
		return Result{}, err
	}
	pl.mu.Unlock()
	return pl.Commit(opID, Decision{Value: value})
}

// DeclareAttested records an attested registration (reconciled fact, origin
// attested): the key lives outside the plane, so no value is stored and the
// values file is not touched. One ledger line.
func (pl *Plane) DeclareAttested(name, refKind string, scopeTags []string) (Result, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return Result{}, err
	}
	if !pl.st.phase.outboxOpen() {
		return Result{}, ErrFrozen
	}
	if !secretwire.ValidName(name) || !secretwire.ValidRefKind(refKind) || !validTags(scopeTags) {
		return Result{}, ErrInvalidInput
	}
	opID, err := pl.newOpID()
	if err != nil {
		return Result{}, err
	}
	e := &secretwire.Entry{Seq: pl.st.maxSeq(pl.st.planeID) + 1, Fact: secretwire.FactReconciled, OpID: opID,
		Ref: secretwire.DeriveRef(opID), Name: name, RefKind: refKind, Version: 1, Origin: originAttested,
		ScopeTags: cloneTags(scopeTags)}
	if err := pl.appendRec(&rec{T: recFact, Entry: e}); err != nil {
		return Result{}, err
	}
	return pl.st.ops[opID].result(false), nil
}

// Observe records an observed fact for a stored ref: present is whether the
// values file holds the ref, version the plane's current version,
// planeObservedAt the clock in second-precision UTC (G13). Attested refs
// have no stored value and are refused (ErrUnknownRef).
func (pl *Plane) Observe(ref string) (secretwire.Entry, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return secretwire.Entry{}, err
	}
	if !pl.st.phase.outboxOpen() {
		return secretwire.Entry{}, ErrFrozen
	}
	rr := pl.st.refs[ref]
	if rr == nil || rr.Origin == originAttested {
		return secretwire.Entry{}, ErrUnknownRef
	}
	pl.valuesMu.Lock()
	tags, err := pl.readTags()
	pl.valuesMu.Unlock()
	if err != nil {
		return secretwire.Entry{}, err
	}
	_, present := tags[ref]
	e := &secretwire.Entry{Seq: pl.st.maxSeq(pl.st.planeID) + 1, Fact: secretwire.FactObserved, Ref: ref,
		Version: rr.Version, Present: present,
		PlaneObservedAt: pl.opts.Now().UTC().Truncate(1e9).Format("2006-01-02T15:04:05Z")}
	if err := pl.appendRec(&rec{T: recFact, Entry: e}); err != nil {
		return secretwire.Entry{}, err
	}
	return cloneEntry(*e), nil
}

// DisplayOnlyOpIDs lists abandoned operations and operations whose outcome
// is quarantined: their pending requests are shown to [H] but never
// processed (P5).
func (pl *Plane) DisplayOnlyOpIDs() ([]string, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return nil, err
	}
	var out []string
	for _, id := range pl.st.opOrder {
		if pl.displayOnly(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// UnreconciledValueRefs lists refs present in the values file that the
// ledger does not know (e.g. manual edits): candidates for a reconciled
// registration by [H] (split plan M5a). Nothing is recorded automatically.
func (pl *Plane) UnreconciledValueRefs() ([]string, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return nil, err
	}
	pl.valuesMu.Lock()
	tags, err := pl.readTags()
	pl.valuesMu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []string
	for ref, t := range tags {
		if pl.st.refs[ref] == nil && pl.st.ops[t.OpID] == nil {
			out = append(out, ref)
		}
	}
	return sortedCopy(out), nil
}
