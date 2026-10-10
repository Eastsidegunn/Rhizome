package secretplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"

	"rhizome/internal/secretwire"
)

// Ledger record types (ledger.ndjson, one JSON object per line). The format
// is plane-internal (FR-RHZ-138: "내부 형식은 SCP 밖"); it never has a value
// field.
const (
	recInit        = "init"         // first line: the plane's planeId (Q3)
	recBegin       = "begin"        // operation in-progress, written before any effect (P3 ②)
	recEnd         = "end"          // terminal result + its outbox entry in one line (P3 ④)
	recFact        = "fact"         // effect-free outbox fact (observed, attested reconciled)
	recAck         = "ack"          // ack larger than the recorded one (P2)
	recRej         = "rej"          // entry-rejected notice or poll.lastRejected (P2)
	recEpochFrozen = "epoch-frozen" // P5 ①
	recEpochCutoff = "epoch-cutoff" // P5 ②
	recEpochSplit  = "epoch-split"  // P5 ④
	recEpochActive = "epoch-active" // P5 ⑤
)

// Origins of a plane operation / ref. originRequest is plane-internal (a ref
// introduced by Rhizome's register request); the other two are G9.
const (
	originRequest     = "request"
	originPlaneDirect = secretwire.OriginPlaneDirect
	originAttested    = secretwire.OriginAttested
)

// rec is one ledger line. Unused fields are omitted.
type rec struct {
	T          string            `json:"t"`
	PlaneID    string            `json:"planeId,omitempty"`
	NewPlaneID string            `json:"newPlaneId,omitempty"`
	OpID       string            `json:"opId,omitempty"`
	Op         string            `json:"op,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Origin     string            `json:"origin,omitempty"`
	Attested   bool              `json:"attested,omitempty"` // begin: target ref is attested (record-only op)
	Name       string            `json:"name,omitempty"`
	RefKind    string            `json:"refKind,omitempty"`
	ScopeTags  []string          `json:"scopeTags,omitempty"`
	ReasonCode string            `json:"reasonCode,omitempty"`
	Result     string            `json:"result,omitempty"`
	Failure    string            `json:"failureCode,omitempty"`
	Entry      *secretwire.Entry `json:"entry,omitempty"`
	Seq        uint64            `json:"seq,omitempty"`
	JournalSeq uint64            `json:"journalSeq,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	AckSeq     uint64            `json:"ackSeq,omitempty"`
	Cutoff     *uint64           `json:"cutoff,omitempty"`
	InProgress []string          `json:"inProgress,omitempty"`
}

// decodeLine strictly decodes one ledger or quarantine line.
func decodeLine(line []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	_, err := dec.Token()
	return err != nil // only EOF may follow
}

type phase int

const (
	phaseNormal phase = iota // initial epoch, no epoch record yet
	phaseFrozen
	phaseCutoff
	phaseSplit
	phaseActive // new-epoch-active: normal operation of P′
)

// String returns the P5 stage name.
func (p phase) String() string {
	switch p {
	case phaseFrozen:
		return "frozen"
	case phaseCutoff:
		return "cutoff-recorded"
	case phaseSplit:
		return "split-recorded"
	case phaseActive:
		return "new-epoch-active"
	}
	return "normal"
}

// outboxOpen reports whether the plane may append outbox facts or finish
// operations (P5 ①: not between frozen and split).
func (p phase) outboxOpen() bool { return p != phaseFrozen && p != phaseCutoff }

type opRec struct {
	OpID, Op, Ref, PlaneID, Origin string
	Attested                       bool
	Name, RefKind                  string
	ScopeTags                      []string
	ReasonCode                     string

	Done            bool
	Result, Failure string
	Entry           *secretwire.Entry
	EntryPlane      string
}

type refRec struct {
	Origin  string
	Version uint64
	Revoked bool
	Name    string
	Kind    string
	Tags    []string
	Busy    string // opId of the in-progress rotate/revoke
}

type rejRec struct {
	Seq    uint64
	Reason string
	AckSeq uint64
}

// state is the plane state derived from the ledger alone. apply is the only
// mutator; it validates every record against the current state, both on
// replay (refuse to start on violation) and before a runtime append.
type state struct {
	inited  bool
	planeID string
	planes  map[string]bool
	order   []string // planeIds in first-seen order
	phase   phase

	ops       map[string]*opRec
	opOrder   []string
	refs      map[string]*refRec
	regRefs   map[string]bool // refs reserved by an in-progress register
	outbox    map[string][]secretwire.Entry
	acks      map[string]secretwire.Ack
	rejs      map[string]*rejRec
	conflicts map[string]map[uint64]bool

	frozenOps []string
	abandoned map[string]bool // opIds in-progress at some epoch-frozen
	cutoff    uint64
	cutoffRej *secretwire.Rejected
}

func newState() *state {
	return &state{
		planes:    map[string]bool{},
		ops:       map[string]*opRec{},
		refs:      map[string]*refRec{},
		regRefs:   map[string]bool{},
		outbox:    map[string][]secretwire.Entry{},
		acks:      map[string]secretwire.Ack{},
		rejs:      map[string]*rejRec{},
		conflicts: map[string]map[uint64]bool{},
		abandoned: map[string]bool{},
	}
}

var errBad = errors.New("bad record")

func (s *state) maxSeq(p string) uint64 { return uint64(len(s.outbox[p])) }

func (s *state) ackSeq(p string) uint64 { return s.acks[p].PlaneEntrySeq }

func (s *state) addPlane(p string) {
	s.planes[p] = true
	s.order = append(s.order, p)
}

// rejRelation is R7b's relation between a rejected seq, its reason and the
// ack it was reported with.
func rejRelation(seq uint64, reason string, ack uint64) bool {
	switch reason {
	case secretwire.EntrySeqConflict:
		return seq <= ack
	case secretwire.EntrySeqGap:
		return seq > ack+1
	}
	return seq == ack+1
}

func validTags(tags []string) bool { return tags == nil || secretwire.ValidScopeTags(tags) }

func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneTags(t []string) []string {
	if len(t) == 0 {
		return nil
	}
	return append([]string(nil), t...)
}

func cloneEntry(e secretwire.Entry) secretwire.Entry {
	e.ScopeTags = cloneTags(e.ScopeTags)
	return e
}

func entryEqual(a, b secretwire.Entry) bool {
	return a.Seq == b.Seq && a.Fact == b.Fact && a.OpID == b.OpID && a.Ref == b.Ref && a.Version == b.Version &&
		a.Result == b.Result && a.FailureCode == b.FailureCode && a.Present == b.Present &&
		a.PlaneObservedAt == b.PlaneObservedAt && a.Name == b.Name && a.RefKind == b.RefKind &&
		a.Origin == b.Origin && sameTags(a.ScopeTags, b.ScopeTags)
}

// checkBegin validates an operation start against the state (shared by
// replay, Classify and runtime appends).
func (s *state) checkBegin(r *rec) error {
	if !s.phase.outboxOpen() || r.PlaneID != s.planeID {
		return errBad
	}
	if !secretwire.ValidOpID(r.OpID) || s.ops[r.OpID] != nil || !secretwire.ValidOp(r.Op) || !secretwire.ValidRef(r.Ref) {
		return errBad
	}
	switch r.Origin {
	case originRequest:
	case originPlaneDirect:
		if r.Op != secretwire.OpRegister {
			return errBad
		}
	default:
		return errBad
	}
	if !secretwire.ValidName(r.Name) || !secretwire.ValidRefKind(r.RefKind) || !validTags(r.ScopeTags) {
		return errBad
	}
	switch r.Op {
	case secretwire.OpRegister:
		if r.Ref != secretwire.DeriveRef(r.OpID) || s.refs[r.Ref] != nil || s.regRefs[r.Ref] || r.Attested || r.ReasonCode != "" {
			return errBad
		}
	case secretwire.OpRotate, secretwire.OpRevoke:
		ref := s.refs[r.Ref]
		if ref == nil || ref.Revoked || ref.Busy != "" || r.Attested != (ref.Origin == originAttested) {
			return errBad
		}
		if r.Name != ref.Name || r.RefKind != ref.Kind || r.ScopeTags != nil {
			return errBad
		}
		if (r.Op == secretwire.OpRevoke) != (r.ReasonCode != "") {
			return errBad
		}
		if r.ReasonCode != "" && !secretwire.ValidReasonCode(r.ReasonCode) {
			return errBad
		}
	}
	return nil
}

// terminalEntry is the exact outbox entry an operation's terminal record must
// carry (R4 versions for outcomes, R6 for reconciled), or nil when the
// terminal has no fact (a failed plane-direct registration).
func (s *state) terminalEntry(op *opRec, result, failure string) *secretwire.Entry {
	seq := s.maxSeq(s.planeID) + 1
	if op.Origin == originPlaneDirect {
		if result != secretwire.ResultOK {
			return nil
		}
		return &secretwire.Entry{Seq: seq, Fact: secretwire.FactReconciled, OpID: op.OpID, Ref: op.Ref,
			Name: op.Name, RefKind: op.RefKind, Version: 1, Origin: originPlaneDirect, ScopeTags: cloneTags(op.ScopeTags)}
	}
	var v uint64
	switch op.Op {
	case secretwire.OpRegister:
		if result == secretwire.ResultOK {
			v = 1
		}
	case secretwire.OpRotate:
		v = s.refs[op.Ref].Version
		if result == secretwire.ResultOK {
			v++
		}
	case secretwire.OpRevoke:
		v = s.refs[op.Ref].Version
	}
	return &secretwire.Entry{Seq: seq, Fact: secretwire.FactOutcome, OpID: op.OpID, Ref: op.Ref,
		Version: v, Result: result, FailureCode: failure}
}

func (s *state) pushOutbox(e secretwire.Entry) {
	s.outbox[s.planeID] = append(s.outbox[s.planeID], cloneEntry(e))
}

// inProgress lists started, unfinished, not abandoned operations.
func (s *state) inProgress() []string {
	var out []string
	for _, id := range s.opOrder {
		if op := s.ops[id]; !op.Done && !s.abandoned[id] {
			out = append(out, id)
		}
	}
	return out
}

func (s *state) apply(r *rec) error {
	if !s.inited {
		if r.T != recInit || !secretwire.ValidPlaneID(r.PlaneID) {
			return errBad
		}
		s.inited, s.planeID = true, r.PlaneID
		s.addPlane(r.PlaneID)
		return nil
	}
	switch r.T {
	case recBegin:
		if err := s.checkBegin(r); err != nil {
			return err
		}
		op := &opRec{OpID: r.OpID, Op: r.Op, Ref: r.Ref, PlaneID: r.PlaneID, Origin: r.Origin, Attested: r.Attested,
			Name: r.Name, RefKind: r.RefKind, ScopeTags: cloneTags(r.ScopeTags), ReasonCode: r.ReasonCode}
		s.ops[r.OpID] = op
		s.opOrder = append(s.opOrder, r.OpID)
		if r.Op == secretwire.OpRegister {
			s.regRefs[r.Ref] = true
		} else {
			s.refs[r.Ref].Busy = r.OpID
		}
		return nil
	case recEnd:
		return s.applyEnd(r)
	case recFact:
		return s.applyFact(r)
	case recAck:
		if r.PlaneID != s.planeID || r.Seq <= s.ackSeq(r.PlaneID) || r.Seq > s.maxSeq(r.PlaneID) ||
			!secretwire.ValidJournalSeq(r.JournalSeq) || s.rejs[r.PlaneID] != nil {
			// Once a rejection is recorded the ack cannot advance in this
			// epoch (durable stall, W11).
			return errBad
		}
		s.acks[r.PlaneID] = secretwire.Ack{PlaneID: r.PlaneID, PlaneEntrySeq: r.Seq, JournalSeq: r.JournalSeq}
		return nil
	case recRej:
		// One line carries the rejection and the ack it was reported with,
		// so the ack never becomes durable (and cleanup-authoritative, P2
		// (a)) ahead of the rejection — in particular a seq-conflict marker.
		p := r.PlaneID
		if p != s.planeID || !secretwire.ValidSeq(r.Seq) || !secretwire.ValidEntryReason(r.Reason) ||
			r.AckSeq < s.ackSeq(p) || r.AckSeq > s.maxSeq(p) || !rejRelation(r.Seq, r.Reason, r.AckSeq) ||
			r.Seq > s.maxSeq(p) || !secretwire.ValidJournalSeq(r.JournalSeq) {
			return errBad
		}
		if l := s.rejs[p]; l != nil && (*l == (rejRec{r.Seq, r.Reason, r.AckSeq}) || r.AckSeq > l.AckSeq) {
			return errBad
		}
		if r.AckSeq > s.ackSeq(p) {
			s.acks[p] = secretwire.Ack{PlaneID: p, PlaneEntrySeq: r.AckSeq, JournalSeq: r.JournalSeq}
		}
		s.rejs[p] = &rejRec{r.Seq, r.Reason, r.AckSeq}
		if r.Reason == secretwire.EntrySeqConflict {
			if s.conflicts[p] == nil {
				s.conflicts[p] = map[uint64]bool{}
			}
			s.conflicts[p][r.Seq] = true
		}
		return nil
	case recEpochFrozen:
		if (s.phase != phaseNormal && s.phase != phaseActive) || r.PlaneID != s.planeID {
			return errBad
		}
		seen := map[string]bool{}
		for _, id := range r.InProgress {
			op := s.ops[id]
			if op == nil || op.Done || seen[id] {
				return errBad
			}
			seen[id] = true
		}
		s.phase = phaseFrozen
		s.frozenOps = append([]string(nil), r.InProgress...)
		for _, id := range r.InProgress {
			s.abandoned[id] = true
		}
		return nil
	case recEpochCutoff:
		if s.phase != phaseFrozen || r.PlaneID != s.planeID || r.Cutoff == nil || *r.Cutoff != s.ackSeq(r.PlaneID) {
			return errBad
		}
		var lr *secretwire.Rejected
		if r.Reason != "" || r.Seq != 0 {
			if !secretwire.ValidSeq(r.Seq) || !secretwire.ValidEntryReason(r.Reason) || !rejRelation(r.Seq, r.Reason, *r.Cutoff) {
				return errBad
			}
			lr = &secretwire.Rejected{Seq: r.Seq, Reason: r.Reason}
		}
		s.phase, s.cutoff, s.cutoffRej = phaseCutoff, *r.Cutoff, lr
		return nil
	case recEpochSplit:
		if s.phase != phaseCutoff || r.PlaneID != s.planeID || !secretwire.ValidPlaneID(r.NewPlaneID) || s.planes[r.NewPlaneID] {
			return errBad
		}
		s.addPlane(r.NewPlaneID)
		s.planeID, s.phase = r.NewPlaneID, phaseSplit
		return nil
	case recEpochActive:
		if s.phase != phaseSplit || r.PlaneID != s.planeID {
			return errBad
		}
		s.phase = phaseActive
		return nil
	}
	return errBad
}

func (s *state) applyEnd(r *rec) error {
	op := s.ops[r.OpID]
	if op == nil || op.Done || s.abandoned[r.OpID] || !s.phase.outboxOpen() {
		return errBad
	}
	switch r.Result {
	case secretwire.ResultOK, secretwire.ResultRejected:
		if r.Failure != "" {
			return errBad
		}
	case secretwire.ResultFailed:
		if !secretwire.ValidFailureCode(r.Failure) {
			return errBad
		}
	default:
		return errBad
	}
	if op.Origin == originPlaneDirect && r.Result == secretwire.ResultRejected {
		return errBad
	}
	want := s.terminalEntry(op, r.Result, r.Failure)
	if (want == nil) != (r.Entry == nil) || (want != nil && (!entryEqual(*want, *r.Entry) || !validEntry(*r.Entry))) {
		return errBad
	}
	op.Done, op.Result, op.Failure = true, r.Result, r.Failure
	if want != nil {
		e := cloneEntry(*want)
		op.Entry, op.EntryPlane = &e, s.planeID
		s.pushOutbox(e)
	}
	delete(s.regRefs, op.Ref)
	if ref := s.refs[op.Ref]; ref != nil && ref.Busy == op.OpID {
		ref.Busy = ""
	}
	if r.Result != secretwire.ResultOK {
		return nil
	}
	switch op.Op {
	case secretwire.OpRegister:
		s.refs[op.Ref] = &refRec{Origin: op.Origin, Version: 1, Name: op.Name, Kind: op.RefKind, Tags: cloneTags(op.ScopeTags)}
	case secretwire.OpRotate:
		s.refs[op.Ref].Version++
	case secretwire.OpRevoke:
		s.refs[op.Ref].Revoked = true
	}
	return nil
}

func (s *state) applyFact(r *rec) error {
	if !s.phase.outboxOpen() || r.Entry == nil || r.Entry.Seq != s.maxSeq(s.planeID)+1 {
		return errBad
	}
	e := *r.Entry
	if !validEntry(e) {
		return errBad
	}
	switch e.Fact {
	case secretwire.FactObserved:
		ref := s.refs[e.Ref]
		if ref == nil || e.Version < 1 || e.Version > ref.Version || !secretwire.ValidObservedAt(e.PlaneObservedAt) {
			return errBad
		}
		if !entryEqual(e, secretwire.Entry{Seq: e.Seq, Fact: e.Fact, Ref: e.Ref, Version: e.Version, Present: e.Present, PlaneObservedAt: e.PlaneObservedAt}) {
			return errBad
		}
	case secretwire.FactReconciled:
		if e.Origin != originAttested || !secretwire.ValidOpID(e.OpID) || s.ops[e.OpID] != nil ||
			e.Ref != secretwire.DeriveRef(e.OpID) || s.refs[e.Ref] != nil || s.regRefs[e.Ref] || e.Version != 1 ||
			!secretwire.ValidName(e.Name) || !secretwire.ValidRefKind(e.RefKind) || !validTags(e.ScopeTags) {
			return errBad
		}
		want := secretwire.Entry{Seq: e.Seq, Fact: e.Fact, OpID: e.OpID, Ref: e.Ref, Name: e.Name, RefKind: e.RefKind,
			Version: 1, Origin: originAttested, ScopeTags: e.ScopeTags}
		if !entryEqual(e, want) {
			return errBad
		}
		ec := cloneEntry(e)
		s.ops[e.OpID] = &opRec{OpID: e.OpID, Op: secretwire.OpRegister, Ref: e.Ref, PlaneID: s.planeID, Origin: originAttested,
			Name: e.Name, RefKind: e.RefKind, ScopeTags: cloneTags(e.ScopeTags), Done: true, Result: secretwire.ResultOK,
			Entry: &ec, EntryPlane: s.planeID}
		s.opOrder = append(s.opOrder, e.OpID)
		s.refs[e.Ref] = &refRec{Origin: originAttested, Version: 1, Name: e.Name, Kind: e.RefKind, Tags: cloneTags(e.ScopeTags)}
	default:
		return errBad
	}
	s.pushOutbox(e)
	return nil
}

// validEntry checks the wire grammar of an outbox entry (G11, G12).
func validEntry(e secretwire.Entry) bool {
	return secretwire.ValidSeq(e.Seq) && secretwire.ValidVersion(e.Version)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
