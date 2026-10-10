package secretref

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/secretwire"
)

// Intent kinds (§2.1).
const (
	KindRegisterRequest = "secret.register.request"
	KindRotateRequest   = "secret.rotate.request"
	KindRevokeRequest   = "secret.revoke.request"
)

// Intent refusal Reasons (§0.2), the only strings a refusal carries.
const (
	ReasonChannelOnly       = "secret facts are channel-only"         // X1
	ReasonMalformed         = "secret intent: malformed"              // X2
	ReasonInvalidOpID       = "secret intent: invalid opId"           // X3
	ReasonInvalidName       = "secret intent: invalid name"           // X4
	ReasonInvalidRefKind    = "secret intent: invalid refKind"        // X5
	ReasonInvalidScopeTags  = "secret intent: invalid scopeTags"      // X6
	ReasonInvalidRef        = "secret intent: invalid ref"            // X7
	ReasonInvalidReasonCode = "secret intent: invalid reasonCode"     // X8
	ReasonInvalidActor      = "secret intent: invalid actor"          // X9
	ReasonOpIDConflict      = "secret intent: opId conflict"          // X10
	ReasonUnknownRef        = "secret intent: unknown ref"            // X11
	ReasonRefNotActive      = "secret intent: ref not active"         // X12
	ReasonRefBusy           = "secret intent: ref busy"               // X13
	ReasonNameInUse         = "secret intent: name in use"            // X14
	ReasonUnavailable       = "secret registry unavailable"           // X15
	ReasonExternallyManaged = "secret intent: ref externally managed" // X16: unreachable, approval Q2
)

// Channel-side errors (§3.6 codes). Poison is reported as events.ErrPoisoned
// itself so callers can errors.Is it.
var (
	ErrPeerRejected        = errors.New(secretwire.ReportPeerRejected)
	ErrRegistryUnavailable = errors.New(secretwire.ReportRegistryUnavailable)
	ErrAppendFailed        = errors.New(secretwire.ReportAppendFailed)
	ErrRetiredPlane        = errors.New(secretwire.ReportRetiredPlane)
	// ErrStalled refuses single-entry writes for a plane whose channel is
	// stalled by a rejection (W11); only an epoch replacement (P5) clears it.
	ErrStalled = errors.New("stalled")
)

// Service is the single secret writer (§1.5). Exactly one instance is shared
// by the intent relay and the channel loop. Every write runs, under one
// mutex: project the journal → V/R/I/W checks → one Append → return. A zero
// Service, a nil *Service or a nil store refuses every write (X15). After
// any Append error the writer is failed for good: no retry, X15 thereafter.
type Service struct {
	mu     sync.Mutex
	store  events.Port
	now    func() time.Time
	failed bool
}

// Option configures New.
type Option func(*Service)

// WithClock injects the envelope CreatedAt clock (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New returns the writer over store.
func New(store events.Port, opts ...Option) *Service {
	s := &Service{store: store, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Service) poisoned() bool {
	pl, ok := s.store.(events.PoisonLatch)
	return ok && pl.Poisoned()
}

// begin is critical-section step ①. Caller holds s.mu.
func (s *Service) begin() (*Projection, error) {
	if s.store == nil || s.failed {
		return nil, ErrRegistryUnavailable
	}
	if s.poisoned() {
		return nil, events.ErrPoisoned
	}
	p, err := Project(s.store.All())
	if err != nil {
		return nil, ErrRegistryUnavailable
	}
	return p, nil
}

// write is critical-section steps ②(V/R guard)–③ for one event: validate
// and fold-check the candidate, append once, read the stored event back and
// fold it into p. Caller holds s.mu. Any Append error fails the writer.
func (s *Service) write(p *Projection, typ, aggType, aggID, corrID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ErrRegistryUnavailable
	}
	key := aggKey{aggType, aggID}
	rev := p.revs[key] + 1
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	e := events.Event{AggregateType: aggType, AggregateID: aggID, Revision: rev, Type: typ,
		Payload: raw, CorrelationID: corrID, CreatedAt: now().UTC()}
	if _, err := p.prepare(e); err != nil {
		return err // guard: never append what replay would reject
	}
	if err := s.store.Append(rev-1, e); err != nil {
		s.failed = true
		if errors.Is(err, events.ErrPoisoned) {
			return events.ErrPoisoned
		}
		return ErrAppendFailed
	}
	stored := s.store.List(aggType, aggID)
	if len(stored) == 0 || stored[len(stored)-1].Revision != rev {
		s.failed = true
		return ErrAppendFailed
	}
	if err := p.apply(stored[len(stored)-1]); err != nil {
		s.failed = true
		return ErrRegistryUnavailable
	}
	return nil
}

// RegisterRequest is intent secret.register.request (§2.1). A nil ScopeTags
// means the key was omitted; a non-nil empty list is invalid (G4).
type RegisterRequest struct {
	OpID, Name, RefKind string
	ScopeTags           []string
	Actor               string
}

// RotateRequest is intent secret.rotate.request.
type RotateRequest struct{ OpID, Ref, Actor string }

// RevokeRequest is intent secret.revoke.request.
type RevokeRequest struct{ OpID, Ref, ReasonCode, Actor string }

// IntentResult is the relay outcome. Accepted fills OpID and Ref; a refusal
// carries only Reason (a §0.2 string).
type IntentResult struct {
	Accepted bool
	OpID     string
	Ref      string
	Reason   string
}

func refused(reason string) IntentResult { return IntentResult{Reason: reason} }

// RequestRegister applies I2, I3, I4, I5, I7 and records E1. The error is
// non-nil only for events.ErrPoisoned (the result then carries X15), so the
// HTTP layer can answer 503 (§1.8 MJ-1a).
func (s *Service) RequestRegister(r RegisterRequest) (IntentResult, error) {
	switch {
	case !secretwire.ValidOpID(r.OpID):
		return refused(ReasonInvalidOpID), nil
	case !secretwire.ValidName(r.Name):
		return refused(ReasonInvalidName), nil
	case !secretwire.ValidRefKind(r.RefKind):
		return refused(ReasonInvalidRefKind), nil
	case r.ScopeTags != nil && !secretwire.ValidScopeTags(r.ScopeTags):
		return refused(ReasonInvalidScopeTags), nil
	case !secretwire.ValidActor(r.Actor):
		return refused(ReasonInvalidActor), nil
	}
	tags := clone(r.ScopeTags)
	return s.intent(func(p *Projection) (IntentResult, func() error) {
		ref := secretwire.DeriveRef(r.OpID)
		if op := p.ops[r.OpID]; op != nil { // I5
			if op.op == secretwire.OpRegister && op.name == r.Name && op.kind == r.RefKind && equalTags(op.scopeTags, tags) {
				return IntentResult{Accepted: true, OpID: op.opID, Ref: op.ref}, nil
			}
			return refused(ReasonOpIDConflict), nil
		}
		if p.refs[ref] != nil {
			return refused(ReasonOpIDConflict), nil // I5: derived ref already introduced (R2)
		}
		for _, id := range p.refOrder { // I7
			v := p.refs[id].view
			if v.Name == r.Name {
				switch v.State {
				case StatePendingRegistration, StateActive, StateRotating, StateRevoking:
					return refused(ReasonNameInUse), nil
				}
			}
		}
		return IntentResult{Accepted: true, OpID: r.OpID, Ref: ref}, func() error {
			return s.write(p, TypeRegisterRequested, AggregateOp, r.OpID, r.OpID, registerRequested{
				OpID: r.OpID, Name: r.Name, Kind: r.RefKind, ScopeTags: tags,
				ActorRef: secretwire.ActorRefPrefix + r.Actor})
		}
	})
}

// RequestRotate applies I2, I3, I4, I5, I6 and records E2.
func (s *Service) RequestRotate(r RotateRequest) (IntentResult, error) {
	switch {
	case !secretwire.ValidOpID(r.OpID):
		return refused(ReasonInvalidOpID), nil
	case !secretwire.ValidRef(r.Ref):
		return refused(ReasonInvalidRef), nil
	case !secretwire.ValidActor(r.Actor):
		return refused(ReasonInvalidActor), nil
	}
	return s.change(r.OpID, r.Ref, secretwire.OpRotate, "", r.Actor)
}

// RequestRevoke applies I2, I3, I4, I5, I6 and records E3.
func (s *Service) RequestRevoke(r RevokeRequest) (IntentResult, error) {
	switch {
	case !secretwire.ValidOpID(r.OpID):
		return refused(ReasonInvalidOpID), nil
	case !secretwire.ValidRef(r.Ref):
		return refused(ReasonInvalidRef), nil
	case !secretwire.ValidReasonCode(r.ReasonCode):
		return refused(ReasonInvalidReasonCode), nil
	case !secretwire.ValidActor(r.Actor):
		return refused(ReasonInvalidActor), nil
	}
	return s.change(r.OpID, r.Ref, secretwire.OpRevoke, r.ReasonCode, r.Actor)
}

func (s *Service) change(opID, ref, op, reason, actor string) (IntentResult, error) {
	return s.intent(func(p *Projection) (IntentResult, func() error) {
		if o := p.ops[opID]; o != nil { // I5
			if o.op == op && o.ref == ref && o.reasonCode == reason {
				return IntentResult{Accepted: true, OpID: o.opID, Ref: o.ref}, nil
			}
			return refused(ReasonOpIDConflict), nil
		}
		r := p.refs[ref]
		if r == nil { // I6
			return refused(ReasonUnknownRef), nil
		}
		// I6's attested clause (X16) is dropped by approval Q2: attested refs
		// may be rotated and revoked, so ReasonExternallyManaged is never
		// returned.
		switch r.view.State {
		case StateRotating, StateRevoking:
			return refused(ReasonRefBusy), nil
		case StateActive:
		default:
			return refused(ReasonRefNotActive), nil
		}
		actorRef := secretwire.ActorRefPrefix + actor
		return IntentResult{Accepted: true, OpID: opID, Ref: ref}, func() error {
			if op == secretwire.OpRotate {
				return s.write(p, TypeRotateRequested, AggregateOp, opID, opID,
					rotateRequested{OpID: opID, Ref: ref, ActorRef: actorRef})
			}
			return s.write(p, TypeRevokeRequested, AggregateOp, opID, opID,
				revokeRequested{OpID: opID, Ref: ref, ReasonCode: reason, ActorRef: actorRef})
		}
	})
}

// intent runs I4 (critical section, X15 on projection error) around decide,
// which returns the result and, when a write is due, the write.
func (s *Service) intent(decide func(*Projection) (IntentResult, func() error)) (IntentResult, error) {
	if s == nil {
		return refused(ReasonUnavailable), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.begin()
	if err != nil {
		return refused(ReasonUnavailable), poisonOnly(err)
	}
	res, write := decide(p)
	if write == nil {
		return res, nil
	}
	if err := write(); err != nil {
		return refused(ReasonUnavailable), poisonOnly(err)
	}
	return res, nil
}

func poisonOnly(err error) error {
	if errors.Is(err, events.ErrPoisoned) {
		return events.ErrPoisoned
	}
	return nil
}

func equalTags(a, b []string) bool {
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

// Snapshot projects the journal for readers (S3 GET /v1/secrets). Reads keep
// working after poison or a failed writer; only a projection error (R8) or a
// missing store refuses. Revision is the journal's largest Sequence.
func (s *Service) Snapshot() (*Projection, uint64, error) {
	if s == nil || s.store == nil {
		return nil, 0, ErrRegistryUnavailable
	}
	log := s.store.All()
	p, err := Project(log)
	if err != nil {
		return nil, 0, err
	}
	var rev uint64
	for _, e := range log {
		if e.Sequence > rev {
			rev = e.Sequence
		}
	}
	return p, rev, nil
}
