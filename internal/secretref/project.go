package secretref

import (
	"reflect"
	"sort"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/secretwire"
)

// Ref states (§1.4, §4.2).
const (
	StatePendingRegistration = "pending-registration"
	StateRegistrationFailed  = "registration-failed"
	StateActive              = "active"
	StateRotating            = "rotating"
	StateRevoking            = "revoking"
	StateRevoked             = "revoked"
)

// OriginRequest marks a ref introduced by E1 (§4.2). The other origins are
// secretwire.OriginPlaneDirect and secretwire.OriginAttested (E5).
const OriginRequest = "request"

// OpView is a pending (outcome-less) secretop.
type OpView struct {
	OpID        string
	Op          string // secretwire.OpRegister | OpRotate | OpRevoke
	Ref         string
	Name        string // the ref's projected name (register: the requested name)
	RefKind     string // the ref's projected kind
	ScopeTags   []string
	ReasonCode  string // revoke only
	JournalSeq  uint64 // journal Sequence of the request event
	RequestedAt time.Time
}

// OutcomeView is a ref's last E4.
type OutcomeView struct {
	OpID, Op, Result, FailureCode string
}

// ObservedView is a ref's latest E6 (R5).
type ObservedView struct {
	Present         bool
	Version         int64
	PlaneObservedAt string
}

// RefView is one ref of the projection, the data behind the §4.2 DTO. Zero
// times mean absent.
type RefView struct {
	Ref, Name, Kind, Origin, State string
	Version                        int64
	ScopeTags                      []string
	Pending                        *OpView
	LastOutcome                    *OutcomeView
	// RegisteredAt is the register-ok E4 time (origin request) or the E5 time.
	RegisteredAt, RotatedAt, RevokedAt time.Time
	Observed                           *ObservedView
	// IntroducedSeq is the journal Sequence of the introducing E1 or E5 (R9
	// order).
	IntroducedSeq uint64
}

type opState struct {
	opID, op, ref string
	name, kind    string // register
	scopeTags     []string
	reasonCode    string // revoke
	seq           uint64
	at            time.Time
	done          bool
}

type refState struct {
	view    RefView // Pending is always nil here; derived from pendingOp
	pending string  // opID of the in-flight op, "" if none
}

type fact struct {
	typ     string
	payload any // canonical decoded payload, compared on re-delivery (W10a)
}

type planeState struct {
	first      uint64 // Sequence of the first appearance (R7a)
	hw         uint64
	hwJournal  uint64          // Sequence of the fact with seq == hw
	facts      map[uint64]fact // seq -> fact
	last       *entryRejected  // most recent E7
	lastRejSeq uint64          // its journal Sequence
}

type aggKey struct{ t, id string }

// Projection is the fold of the three secret aggregates (R1–R9). Its
// accessors return copies; it is never mutated after Project returns except
// by the Service, which owns its own instance.
type Projection struct {
	ops        map[string]*opState
	opOrder    []string // request Sequence ascending
	refs       map[string]*refState
	refOrder   []string // introduction Sequence ascending (R9)
	planes     map[string]*planeState
	planeOrder []string // first appearance ascending; the last one is current
	revs       map[aggKey]uint64
}

func newProjection() *Projection {
	return &Projection{
		ops:    map[string]*opState{},
		refs:   map[string]*refState{},
		planes: map[string]*planeState{},
		revs:   map[aggKey]uint64{},
	}
}

func isSecretAggregate(t string) bool {
	return t == AggregateOp || t == AggregateRef || t == AggregateChan
}

// Project folds the secret events of log in journal Sequence order. Other
// aggregates are ignored. It is pure and deterministic (R9): no clock, no map
// iteration order reaches the result. Any V or R violation is a
// *ReplayError (R8).
func Project(log []events.Event) (*Projection, error) {
	idx := make([]int, 0, len(log))
	for i := range log {
		if isSecretAggregate(log[i].AggregateType) {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return log[idx[a]].Sequence < log[idx[b]].Sequence })
	p := newProjection()
	for _, i := range idx {
		if err := p.apply(log[i]); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Projection) apply(e events.Event) error {
	commit, err := p.prepare(e)
	if err != nil {
		return err
	}
	commit()
	return nil
}

// prepare runs every V and R check of e against the current projection
// without mutating it, and returns the mutation. The Service calls it on a
// candidate before Append (so a write can never produce a journal that fails
// replay) and applies the read-back event afterwards.
func (p *Projection) prepare(e events.Event) (func(), error) {
	payload, err := validate(e)
	if err != nil {
		return nil, err
	}
	key := aggKey{e.AggregateType, e.AggregateID}
	if e.Revision != p.revs[key]+1 {
		if e.AggregateType == AggregateOp {
			return nil, replayErr(codeR1) // one request, at most one outcome per opId
		}
		return nil, replayErr(codeVT)
	}
	var mutate func()
	switch v := payload.(type) {
	case registerRequested:
		mutate, err = p.prepareRegister(e, v)
	case rotateRequested:
		mutate, err = p.prepareChange(e, v.OpID, v.Ref, secretwire.OpRotate, "")
	case revokeRequested:
		mutate, err = p.prepareChange(e, v.OpID, v.Ref, secretwire.OpRevoke, v.ReasonCode)
	case opOutcome:
		mutate, err = p.prepareOutcome(e, v)
	case reconciled:
		mutate, err = p.prepareReconciled(e, v)
	case observed:
		mutate, err = p.prepareObserved(e, v)
	case entryRejected:
		mutate, err = p.prepareRejected(e, v)
	}
	if err != nil {
		return nil, err
	}
	return func() {
		p.revs[key] = e.Revision
		mutate()
	}, nil
}

func (p *Projection) prepareRegister(e events.Event, v registerRequested) (func(), error) {
	ref := secretwire.DeriveRef(v.OpID)
	if p.refs[ref] != nil {
		return nil, replayErr(codeR2) // a ref is introduced once
	}
	return func() {
		p.addOp(&opState{opID: v.OpID, op: secretwire.OpRegister, ref: ref, name: v.Name, kind: v.Kind,
			scopeTags: clone(v.ScopeTags), seq: e.Sequence, at: e.CreatedAt})
		p.refs[ref] = &refState{
			view: RefView{Ref: ref, Name: v.Name, Kind: v.Kind, Origin: OriginRequest,
				State: StatePendingRegistration, ScopeTags: clone(v.ScopeTags), IntroducedSeq: e.Sequence},
			pending: v.OpID,
		}
		p.refOrder = append(p.refOrder, ref)
	}, nil
}

func (p *Projection) prepareChange(e events.Event, opID, ref, op, reason string) (func(), error) {
	r := p.refs[ref]
	if r == nil {
		return nil, replayErr(codeVA) // E2/E3 target must be introduced
	}
	// R3: only an active ref accepts a change, so at most one op is in
	// flight. The attested clause of R3 is dropped by approval Q2: attested
	// refs may be rotated and revoked.
	if r.view.State != StateActive || r.pending != "" {
		return nil, replayErr(codeR3)
	}
	next := StateRotating
	if op == secretwire.OpRevoke {
		next = StateRevoking
	}
	return func() {
		p.addOp(&opState{opID: opID, op: op, ref: ref, reasonCode: reason, seq: e.Sequence, at: e.CreatedAt})
		r.view.State = next
		r.pending = opID
	}, nil
}

// expectedVersion is R4 for an outcome of op on a ref currently at cur.
func expectedVersion(op, result string, cur int64) int64 {
	if result == secretwire.ResultOK {
		switch op {
		case secretwire.OpRegister:
			return 1
		case secretwire.OpRotate:
			return cur + 1
		}
	}
	return cur // revoke ok, failed, rejected (register: cur is 0)
}

func (p *Projection) prepareOutcome(e events.Event, v opOutcome) (func(), error) {
	op := p.ops[v.OpID]
	if op == nil || op.done {
		return nil, replayErr(codeR1) // unreachable past revision continuity
	}
	if v.Ref != op.ref {
		return nil, replayErr(codeVA) // E4 Ref equals the op's (derived) ref
	}
	r := p.refs[op.ref]
	if r == nil || r.pending != op.opID {
		return nil, replayErr(codeR1)
	}
	commitPlane, err := p.prepareFact(e, v.PlaneID, v.PlaneEntrySeq, v)
	if err != nil {
		return nil, err
	}
	if v.Version != expectedVersion(op.op, v.Result, r.view.Version) {
		return nil, replayErr(codeR4)
	}
	return func() {
		commitPlane()
		op.done = true
		r.pending = ""
		r.view.LastOutcome = &OutcomeView{OpID: op.opID, Op: op.op, Result: v.Result, FailureCode: v.FailureCode}
		ok := v.Result == secretwire.ResultOK
		switch op.op {
		case secretwire.OpRegister:
			if ok {
				r.view.State, r.view.Version, r.view.RegisteredAt = StateActive, 1, e.CreatedAt
			} else {
				r.view.State = StateRegistrationFailed
			}
		case secretwire.OpRotate:
			r.view.State = StateActive
			if ok {
				r.view.Version, r.view.RotatedAt = v.Version, e.CreatedAt
			}
		case secretwire.OpRevoke:
			if ok {
				r.view.State, r.view.RevokedAt = StateRevoked, e.CreatedAt
			} else {
				r.view.State = StateActive
			}
		}
	}, nil
}

func (p *Projection) prepareReconciled(e events.Event, v reconciled) (func(), error) {
	if p.refs[v.Ref] != nil || p.ops[v.OpID] != nil {
		return nil, replayErr(codeR2) // re-introduction, or OpID of an existing secretop
	}
	if v.Version != 1 {
		return nil, replayErr(codeR6)
	}
	commitPlane, err := p.prepareFact(e, v.PlaneID, v.PlaneEntrySeq, v)
	if err != nil {
		return nil, err
	}
	return func() {
		commitPlane()
		p.refs[v.Ref] = &refState{view: RefView{Ref: v.Ref, Name: v.Name, Kind: v.Kind, Origin: v.Origin,
			State: StateActive, Version: 1, ScopeTags: clone(v.ScopeTags), RegisteredAt: e.CreatedAt,
			IntroducedSeq: e.Sequence}}
		p.refOrder = append(p.refOrder, v.Ref)
	}, nil
}

func (p *Projection) prepareObserved(e events.Event, v observed) (func(), error) {
	r := p.refs[v.Ref]
	if r == nil {
		return nil, replayErr(codeVA) // E6 target must be introduced
	}
	switch r.view.State {
	case StateActive, StateRotating, StateRevoking, StateRevoked:
	default:
		return nil, replayErr(codeR5)
	}
	if v.Version < 1 || v.Version > r.view.Version {
		return nil, replayErr(codeR5)
	}
	commitPlane, err := p.prepareFact(e, v.PlaneID, v.PlaneEntrySeq, v)
	if err != nil {
		return nil, err
	}
	return func() {
		commitPlane()
		r.view.Observed = &ObservedView{Present: v.Present, Version: v.Version, PlaneObservedAt: v.PlaneObservedAt}
	}, nil
}

// preparePlane is R7a for any channel event (E4–E7) of plane id.
func (p *Projection) preparePlane(e events.Event, id string) (*planeState, func(), error) {
	if ps := p.planes[id]; ps != nil {
		if p.planeOrder[len(p.planeOrder)-1] != id {
			return nil, nil, replayErr(codeR7a) // retired planeId
		}
		return ps, func() {}, nil
	}
	ps := &planeState{first: e.Sequence, facts: map[uint64]fact{}}
	return ps, func() {
		p.planes[id] = ps
		p.planeOrder = append(p.planeOrder, id)
	}, nil
}

// prepareFact is R7a and R7 for a channel fact (E4–E6).
func (p *Projection) prepareFact(e events.Event, id string, seq uint64, payload any) (func(), error) {
	ps, commitPlane, err := p.preparePlane(e, id)
	if err != nil {
		return nil, err
	}
	if seq != ps.hw+1 {
		return nil, replayErr(codeR7)
	}
	return func() {
		commitPlane()
		ps.hw, ps.hwJournal = seq, e.Sequence
		ps.facts[seq] = fact{typ: e.Type, payload: payload}
	}, nil
}

// rejectionRelation is the R7b RejectedSeq/Reason/AckSeq relation.
func rejectionRelation(rejected uint64, reason string, ack uint64) bool {
	switch reason {
	case secretwire.EntrySeqConflict:
		return rejected <= ack
	case secretwire.EntrySeqGap:
		return rejected > ack+1
	}
	return rejected == ack+1
}

func (p *Projection) prepareRejected(e events.Event, v entryRejected) (func(), error) {
	ps, commitPlane, err := p.preparePlane(e, v.PlaneID)
	if err != nil {
		return nil, err
	}
	if v.AckSeq != ps.hw || !rejectionRelation(v.RejectedSeq, v.Reason, v.AckSeq) {
		return nil, replayErr(codeR7b)
	}
	if ps.last != nil && *ps.last == v {
		return nil, replayErr(codeR7b) // duplicate of the previous E7
	}
	return func() {
		commitPlane()
		rec := v
		ps.last, ps.lastRejSeq = &rec, e.Sequence
	}, nil
}

func (p *Projection) addOp(o *opState) {
	p.ops[o.opID] = o
	p.opOrder = append(p.opOrder, o.opID)
}

func clone(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

// sameFact reports W10(a) equality: same fact type and canonical content.
func (p *Projection) sameFact(plane string, seq uint64, typ string, payload any) bool {
	ps := p.planes[plane]
	if ps == nil {
		return false
	}
	f, ok := ps.facts[seq]
	return ok && f.typ == typ && reflect.DeepEqual(f.payload, payload)
}

// --- read-only accessors (copies) ---

func (p *Projection) refView(r *refState) RefView {
	v := r.view
	v.ScopeTags = clone(v.ScopeTags)
	if v.LastOutcome != nil {
		o := *v.LastOutcome
		v.LastOutcome = &o
	}
	if v.Observed != nil {
		o := *v.Observed
		v.Observed = &o
	}
	if r.pending != "" {
		o := p.opView(p.ops[r.pending])
		v.Pending = &o
	}
	return v
}

func (p *Projection) opView(o *opState) OpView {
	r := p.refs[o.ref].view
	v := OpView{OpID: o.opID, Op: o.op, Ref: o.ref, Name: r.Name, RefKind: r.Kind,
		ReasonCode: o.reasonCode, JournalSeq: o.seq, RequestedAt: o.at}
	if o.op == secretwire.OpRegister {
		v.Name, v.RefKind, v.ScopeTags = o.name, o.kind, clone(o.scopeTags)
	}
	return v
}

// Refs returns every ref in introduction order (R9).
func (p *Projection) Refs() []RefView {
	out := make([]RefView, 0, len(p.refOrder))
	for _, id := range p.refOrder {
		out = append(out, p.refView(p.refs[id]))
	}
	return out
}

// Ref returns one ref.
func (p *Projection) Ref(ref string) (RefView, bool) {
	r := p.refs[ref]
	if r == nil {
		return RefView{}, false
	}
	return p.refView(r), true
}

// Pending returns the outcome-less secretops in request Sequence order, at
// most limit of them, oldest first (W7). limit <= 0 means all.
func (p *Projection) Pending(limit int) []OpView {
	out := []OpView{}
	for _, id := range p.opOrder {
		if limit > 0 && len(out) == limit {
			break
		}
		if o := p.ops[id]; !o.done {
			out = append(out, p.opView(o))
		}
	}
	return out
}

// HighWater is hw(P) (R7): the largest PlaneEntrySeq of P's facts, 0 if none.
func (p *Projection) HighWater(planeID string) uint64 {
	if ps := p.planes[planeID]; ps != nil {
		return ps.hw
	}
	return 0
}

// Ack is the W8 ack for P, computed from the folded events only.
func (p *Projection) Ack(planeID string) secretwire.Ack {
	a := secretwire.Ack{PlaneID: planeID}
	if ps := p.planes[planeID]; ps != nil {
		a.PlaneEntrySeq, a.JournalSeq = ps.hw, ps.hwJournal
	}
	return a
}

// LastRejected is lastRejected(P) (R7b): P's most recent E7 if its AckSeq
// equals hw(P), else nil.
func (p *Projection) LastRejected(planeID string) *secretwire.Rejected {
	ps := p.planes[planeID]
	if ps == nil || ps.last == nil || ps.last.AckSeq != ps.hw {
		return nil
	}
	return &secretwire.Rejected{Seq: ps.last.RejectedSeq, Reason: ps.last.Reason}
}

// Retired reports R7a: P has appeared and a later-appearing planeId exists.
func (p *Projection) Retired(planeID string) bool {
	return p.planes[planeID] != nil && p.planeOrder[len(p.planeOrder)-1] != planeID
}

// CurrentPlane is the planeId with the latest first appearance, "" if none.
func (p *Projection) CurrentPlane() string {
	if len(p.planeOrder) == 0 {
		return ""
	}
	return p.planeOrder[len(p.planeOrder)-1]
}

// RetiredPlanes returns the retired planeIds in first-appearance order.
func (p *Projection) RetiredPlanes() []string {
	if len(p.planeOrder) < 2 {
		return []string{}
	}
	return append([]string(nil), p.planeOrder[:len(p.planeOrder)-1]...)
}
