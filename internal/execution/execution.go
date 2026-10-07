package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"rhizome/internal/events"
	"rhizome/internal/policy"
	"rhizome/internal/projector"
	"strings"
	"time"
)

type State string

const (
	Intent          State = "intent"
	DispatchClaimed State = "dispatch_claimed"
	Accepted        State = "accepted"
	Observing       State = "observing"
	Succeeded       State = "succeeded"
	Failed          State = "failed"
	Unknown         State = "unknown"
	Cancelled       State = "cancelled"
)

type UnknownClass string

const (
	ExternalEffectPossible UnknownClass = "external_effect_possible"
	ObservationDelay       UnknownClass = "observation_delay"
	DeadlineNear           UnknownClass = "deadline_near"
	Repeated               UnknownClass = "repeated"
	NeedsHuman             UnknownClass = "needs_human"
)

type Binding struct {
	SessionDB          string `json:"session_db"`
	TraceID            string `json:"trace_id"`
	PolicyHash         string `json:"policy_hash"`
	RequestFingerprint string `json:"request_fingerprint"`
}
type ReconciliationEvidence struct {
	Binding                                   Binding `json:"binding"`
	Target, IdempotencyKey, Cursor, SourceRef string
}
type Ref struct {
	ID, MissionID, IdempotencyKey, ExternalID, Cursor, Summary, SourceRef string
	State                                                                 State
	Revision                                                              uint64
	Policy                                                                *policy.Policy
	Claimed                                                               bool
	Target, ClaimCorrelation, ScopedKey                                   string
	Binding                                                               *Binding
	UnknownClass                                                          UnknownClass
	IncidentRef, HumanDecisionID                                          string
	StopRequested                                                         bool
	StopID, StopReason, StopActor                                         string
	StopEvidenceSeq                                                       int64
	// Provenance is the intent's policy provenance (FR-RHZ-124); nil for
	// legacy intents and for IntentWithPolicy callers.
	Provenance *Provenance
}

// Provenance is the plain-text policy provenance of one execution intent
// (RHZ-096, FR-RHZ-124; contract ② §3.2): the policy ceiling, the
// request, the effective policy (= policy.Merge(requested, ceiling)), the
// profile identity and a digest of the operating ledger, plus who asked.
// Digests and numbers only — never a file path or a key. It rides on the
// existing execution.intent payload as an additive optional field (no new
// event type); omitempty keeps legacy and IntentWithPolicy bytes identical.
// Consistency (Effective == the stored snapshot) is enforced at write time
// only; Replay accepts what was written so a historical record can never
// poison the aggregate.
type Provenance struct {
	Ceiling          policy.Policy `json:"ceiling"`
	Requested        policy.Policy `json:"requested"`
	Effective        policy.Policy `json:"effective"`
	ProfileID        string        `json:"profileId"`
	ProfileHash      string        `json:"profileHash"`
	ExecConfigDigest string        `json:"execConfigDigest"`
	Actor            string        `json:"actor"`
}
type Service struct{ Store events.Port }
type payload struct {
	MissionID, IdempotencyKey, ExternalID, Cursor, Summary, SourceRef string
	State                                                             State
	Policy                                                            *policy.Policy
	Provenance                                                        *Provenance `json:"provenance,omitempty"`
}
type claimPayload struct{ MissionID, IdempotencyKey, Target, CorrelationID, ScopedKey string }
type bindingPayload struct {
	MissionID, IdempotencyKey, ExternalID string
	Binding
}
type unknownPayload struct {
	MissionID, IdempotencyKey, ExternalID, SourceRef, Summary string
	Class                                                     UnknownClass
	IncidentRef                                               string
}
type reconcilePayload struct {
	MissionID, IdempotencyKey, ExternalID, SourceRef, Cursor string
	Binding                                                  Binding
	Target                                                   string
}
type resolvePayload struct {
	MissionID, IdempotencyKey string
	To                        State
	HumanDecisionID           string
}
type stopPayload struct {
	MissionID, IdempotencyKey, ExternalID string
	StopID, Reason, Actor                 string
	EvidenceSeq                           int64
}

var traceRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validBinding(b Binding) error {
	if b.SessionDB == "" || !strings.HasPrefix(b.SessionDB, "/") || b.TraceID == "" || !traceRE.MatchString(b.TraceID) || b.PolicyHash == "" || b.RequestFingerprint == "" {
		return fmt.Errorf("invalid binding")
	}
	return nil
}
func scoped(target, key string) string {
	h := sha256.Sum256([]byte(target + "\x00" + key))
	return "sha256:" + hex.EncodeToString(h[:])
}
func cursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	if len(c) != 19 {
		return 0, fmt.Errorf("cursor migration required")
	}
	for _, x := range c {
		if x < '0' || x > '9' {
			return 0, fmt.Errorf("cursor migration required")
		}
	}
	var n int64
	for _, x := range c {
		if n > (math.MaxInt64-int64(x-'0'))/10 {
			return 0, fmt.Errorf("cursor overflow")
		}
		n = n*10 + int64(x-'0')
	}
	return n, nil
}
func checkCursor(prev, next string) error {
	if next == "" {
		return nil
	}
	n, e := cursor(next)
	if e != nil {
		return e
	}
	if prev != "" {
		p, e := cursor(prev)
		if e != nil {
			return e
		}
		if n <= p {
			return fmt.Errorf("cursor not increasing")
		}
	}
	return nil
}
func (s Service) Intent(missionID, key string) (Ref, error) {
	return Ref{}, fmt.Errorf("policy snapshot required; use IntentWithPolicy")
}
func (s Service) IntentWithPolicy(missionID, key string, requested, ceiling policy.Policy) (Ref, error) {
	return s.intent(missionID, key, requested, ceiling, nil)
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func samePolicy(a, b policy.Policy) bool {
	x, e1 := json.Marshal(a)
	y, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(x) == string(y)
}

// IntentWithProvenance is IntentWithPolicy plus the policy provenance
// (FR-RHZ-124). The provenance must describe exactly this intent: Ceiling
// and Requested equal the arguments and Effective equals the merged snapshot
// that is stored as Policy. The idempotency key and execution id are the
// caller's key, untouched by provenance.
func (s Service) IntentWithProvenance(missionID, key string, requested, ceiling policy.Policy, prov Provenance) (Ref, error) {
	if strings.TrimSpace(prov.ProfileID) == "" || strings.TrimSpace(prov.ProfileHash) == "" || !digestRE.MatchString(prov.ExecConfigDigest) {
		return Ref{}, fmt.Errorf("invalid provenance")
	}
	merged := policy.Merge(requested, ceiling)
	if merged.Invalid || merged.Empty {
		return Ref{}, fmt.Errorf("invalid or empty policy")
	}
	if !samePolicy(prov.Ceiling, ceiling) || !samePolicy(prov.Requested, requested) || !samePolicy(prov.Effective, merged.Policy) {
		return Ref{}, fmt.Errorf("provenance does not match intent policy")
	}
	return s.intent(missionID, key, requested, ceiling, &prov)
}

func (s Service) intent(missionID, key string, requested, ceiling policy.Policy, prov *Provenance) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil event store")
	}
	if missionID == "" || key == "" {
		return Ref{}, fmt.Errorf("mission and idempotency key required")
	}
	m, e := projector.ReplayMission(s.Store.List("mission", missionID))
	if e != nil || m.State == "succeeded" || m.State == "failed" || m.State == "cancelled" {
		return Ref{}, fmt.Errorf("mission reference invalid")
	}
	for _, x := range s.Store.All() {
		if x.AggregateType == "execution" {
			var p payload
			if json.Unmarshal(x.Payload, &p) == nil && p.IdempotencyKey == key {
				return Ref{}, fmt.Errorf("duplicate idempotency key")
			}
		}
	}
	merged := policy.Merge(requested, ceiling)
	if merged.Invalid || merged.Empty {
		return Ref{}, fmt.Errorf("invalid or empty policy")
	}
	snap := merged.Policy
	return s.append(idFor(key), "execution.intent", payload{MissionID: missionID, IdempotencyKey: key, State: Intent, Policy: &snap, Provenance: prov}, 0)
}
func idFor(k string) string { return "exec-" + k }
func (s Service) append(id, typ string, v any, rev uint64) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil event store")
	}
	b, e := json.Marshal(v)
	if e != nil {
		return Ref{}, e
	}
	log := s.Store.List("execution", id)
	if rev != uint64(len(log)) {
		return Ref{}, events.ErrRevisionConflict
	}
	ev := events.Event{AggregateType: "execution", AggregateID: id, Revision: rev + 1, Type: typ, Payload: b, CreatedAt: time.Now().UTC()}
	if _, e = Replay(append(append([]events.Event(nil), log...), ev)); e != nil {
		return Ref{}, e
	}
	if e = s.Store.Append(rev, ev); e != nil {
		return Ref{}, e
	}
	return Replay(s.Store.List("execution", id))
}
func (s Service) ClaimDispatch(id, target, corr string) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != Intent || r.Claimed || target == "" || corr == "" {
		return Ref{}, fmt.Errorf("invalid dispatch claim")
	}
	return s.append(id, "execution.dispatch_claimed", claimPayload{r.MissionID, r.IdempotencyKey, target, corr, scoped(target, r.IdempotencyKey)}, r.Revision)
}
func (s Service) Accept(id, external string) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != DispatchClaimed || !r.Claimed || external == "" {
		return Ref{}, fmt.Errorf("claim required")
	}
	return s.append(id, "execution.accepted", payload{MissionID: r.MissionID, IdempotencyKey: r.IdempotencyKey, ExternalID: external, State: Accepted}, r.Revision)
}
func (s Service) Bind(id string, b Binding) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != Accepted && r.State != Observing {
		return Ref{}, fmt.Errorf("not accepted")
	}
	if r.Binding != nil {
		return Ref{}, fmt.Errorf("binding already set")
	}
	if e = validBinding(b); e != nil {
		return Ref{}, e
	}
	if b.TraceID != r.ExternalID {
		return Ref{}, fmt.Errorf("trace/external mismatch")
	}
	return s.append(id, "execution.binding", bindingPayload{r.MissionID, r.IdempotencyKey, r.ExternalID, b}, r.Revision)
}
func (s Service) Observe(id, cursorV, summary, source string, failed bool) (Ref, error) {
	st := Succeeded
	if failed {
		st = Failed
	}
	return s.ObserveState(id, cursorV, summary, source, st)
}
func (s Service) ObserveState(id, c, summary, source string, st State) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != Accepted && r.State != Observing {
		return Ref{}, fmt.Errorf("not observable")
	}
	if e = checkCursor(r.Cursor, c); e != nil {
		return Ref{}, e
	}
	if st != Observing && st != Succeeded && st != Failed && st != Cancelled {
		return Ref{}, fmt.Errorf("invalid state")
	}
	return s.append(id, "execution.observed", payload{MissionID: r.MissionID, IdempotencyKey: r.IdempotencyKey, ExternalID: r.ExternalID, Cursor: c, Summary: summary, SourceRef: source, State: st}, r.Revision)
}
func (s Service) MarkUnknown(id string, class UnknownClass, incident, source, summary string) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != DispatchClaimed && r.State != Accepted && r.State != Observing {
		return Ref{}, fmt.Errorf("unknown not allowed")
	}
	if !validUnknown(class, incident) {
		return Ref{}, fmt.Errorf("invalid unknown class")
	}
	return s.append(id, "execution.unknown", unknownPayload{r.MissionID, r.IdempotencyKey, r.ExternalID, source, summary, class, incident}, r.Revision)
}
func validUnknown(c UnknownClass, i string) bool {
	switch c {
	case ExternalEffectPossible, ObservationDelay, DeadlineNear, NeedsHuman:
		return true
	case Repeated:
		return i != ""
	}
	return false
}
func validStopReason(r string) bool {
	return r == "user" || r == "budget_exceeded" || r == "policy" || r == "parent_done"
}

// stopIDFor derives the contract stop_id deterministically from the execution
// aggregate ID: a restarted Rhizome never mints a fresh stop identity (§7).
func stopIDFor(id string) string {
	h := sha256.Sum256([]byte("stop\x00" + id))
	return "stop-" + hex.EncodeToString(h[:])
}
func validStopEvidence(reason string, seq int64) bool {
	if reason == "policy" {
		return seq > 0
	}
	return seq == 0
}

// RequestStop records the durable stop intent (execution.stop_requested,
// contract v1.4 ratification condition). It never transitions state: cancelled
// is only ever concluded from replay observation of the JANUS session log.
func (s Service) RequestStop(id, reason string, evidenceSeq int64, actor string) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != Accepted && r.State != Observing {
		return Ref{}, fmt.Errorf("stop request requires active execution")
	}
	if !validStopReason(reason) || strings.TrimSpace(actor) == "" || !validStopEvidence(reason, evidenceSeq) {
		return Ref{}, fmt.Errorf("invalid stop request")
	}
	if r.StopRequested {
		if r.StopReason == reason && r.StopEvidenceSeq == evidenceSeq && r.StopActor == actor {
			return r, nil // Idempotent: same content appends no second event.
		}
		return Ref{}, fmt.Errorf("stop request conflict")
	}
	return s.append(id, "execution.stop_requested", stopPayload{r.MissionID, r.IdempotencyKey, r.ExternalID, stopIDFor(id), reason, actor, evidenceSeq}, r.Revision)
}
func (s Service) Reconcile(id string, x ReconciliationEvidence) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != Unknown || x.Target == "" || x.IdempotencyKey != r.IdempotencyKey || x.SourceRef == "" {
		return Ref{}, fmt.Errorf("invalid evidence")
	}
	if e = validBinding(x.Binding); e != nil {
		return Ref{}, fmt.Errorf("invalid evidence")
	}
	if r.Claimed && x.Target != r.Target {
		return Ref{}, fmt.Errorf("target mismatch")
	}
	if x.Binding.TraceID != r.ExternalID && r.ExternalID != "" {
		return Ref{}, fmt.Errorf("trace mismatch")
	}
	if r.Binding != nil && *r.Binding != x.Binding {
		return Ref{}, fmt.Errorf("binding mismatch")
	}
	if r.ExternalID == "" {
		// The recovered binding supplies the external identity.
		r.ExternalID = x.Binding.TraceID
	}
	if e = checkCursor(r.Cursor, x.Cursor); e != nil {
		return Ref{}, e
	}
	return s.append(id, "execution.reconciled", reconcilePayload{r.MissionID, r.IdempotencyKey, r.ExternalID, x.SourceRef, x.Cursor, x.Binding, x.Target}, r.Revision)
}
func (s Service) ResolveUnknown(id string, to State, human string) (Ref, error) {
	r, e := s.get(id)
	if e != nil {
		return Ref{}, e
	}
	if r.State != Unknown || (to != Failed && to != Cancelled) || human == "" {
		return Ref{}, fmt.Errorf("human decision required")
	}
	return s.append(id, "execution.human_resolved", resolvePayload{r.MissionID, r.IdempotencyKey, to, human}, r.Revision)
}
func (s Service) get(id string) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil event store")
	}
	return Replay(s.Store.List("execution", id))
}
func Replay(log []events.Event) (Ref, error) {
	if len(log) == 0 {
		return Ref{}, fmt.Errorf("empty execution")
	}
	var r Ref
	for i, e := range log {
		if e.AggregateType != "execution" || e.Revision != uint64(i+1) || (i > 0 && e.AggregateID != r.ID) {
			return Ref{}, events.ErrRevisionConflict
		}
		if i == 0 {
			r.ID = e.AggregateID
		}
		var raw struct {
			MissionID, IdempotencyKey, ExternalID, Cursor, Summary, SourceRef string
			State                                                             State
			Policy                                                            *policy.Policy
			Provenance                                                        *Provenance `json:"provenance"`
		}
		if json.Unmarshal(e.Payload, &raw) != nil {
			return Ref{}, fmt.Errorf("malformed payload")
		}
		if i == 0 {
			if e.Type != "execution.intent" || raw.State != Intent || raw.Policy == nil || !policy.Valid(*raw.Policy) || len(raw.Policy.Capabilities) == 0 {
				return Ref{}, fmt.Errorf("invalid intent")
			}
			r.MissionID, r.IdempotencyKey, r.Policy, r.Provenance = raw.MissionID, raw.IdempotencyKey, raw.Policy, raw.Provenance
			r.State = Intent
			r.Revision = 1
			continue
		}
		if raw.Policy != nil || raw.Provenance != nil {
			return Ref{}, fmt.Errorf("policy injection")
		}
		if raw.MissionID != "" && raw.MissionID != r.MissionID || raw.IdempotencyKey != "" && raw.IdempotencyKey != r.IdempotencyKey {
			return Ref{}, fmt.Errorf("identity changed")
		}
		switch e.Type {
		case "execution.dispatch_claimed":
			var p claimPayload
			if json.Unmarshal(e.Payload, &p) != nil || p.Target == "" || p.CorrelationID == "" || p.ScopedKey != scoped(p.Target, r.IdempotencyKey) || r.State != Intent || r.Claimed {
				return Ref{}, fmt.Errorf("invalid claim")
			}
			r.Claimed = true
			r.Target, r.ClaimCorrelation, r.ScopedKey = p.Target, p.CorrelationID, p.ScopedKey
			r.State = DispatchClaimed
		case "execution.accepted":
			if raw.State != Accepted || raw.ExternalID == "" || (!r.Claimed && i != 1) || r.State != DispatchClaimed && !(i == 1 && r.State == Intent) {
				return Ref{}, fmt.Errorf("invalid acceptance")
			}
			r.ExternalID = raw.ExternalID
			r.State = Accepted
		case "execution.binding":
			var p bindingPayload
			if json.Unmarshal(e.Payload, &p) != nil || r.State != Accepted && r.State != Observing || r.Binding != nil || p.ExternalID != r.ExternalID || validBinding(p.Binding) != nil || p.TraceID != r.ExternalID {
				return Ref{}, fmt.Errorf("invalid binding")
			}
			b := p.Binding
			r.Binding = &b
		case "execution.observed":
			if r.State != Accepted && r.State != Observing {
				return Ref{}, fmt.Errorf("invalid observation")
			}
			if checkCursor(r.Cursor, raw.Cursor) != nil {
				return Ref{}, fmt.Errorf("invalid cursor")
			}
			if raw.State != Observing && raw.State != Succeeded && raw.State != Failed && raw.State != Unknown && raw.State != Cancelled {
				return Ref{}, fmt.Errorf("invalid observation state")
			}
			r.State = raw.State
			r.Cursor, r.Summary, r.SourceRef = raw.Cursor, raw.Summary, raw.SourceRef
		case "execution.unknown":
			var p unknownPayload
			if json.Unmarshal(e.Payload, &p) != nil || !validUnknown(p.Class, p.IncidentRef) || (r.State != DispatchClaimed && r.State != Accepted && r.State != Observing) {
				return Ref{}, fmt.Errorf("invalid unknown")
			}
			r.State = Unknown
			r.UnknownClass, r.IncidentRef = p.Class, p.IncidentRef
			r.SourceRef, r.Summary = p.SourceRef, p.Summary
		case "execution.reconciled":
			var p reconcilePayload
			if json.Unmarshal(e.Payload, &p) != nil || r.State != Unknown || validBinding(p.Binding) != nil || p.IdempotencyKey != r.IdempotencyKey || p.SourceRef == "" || checkCursor(r.Cursor, p.Cursor) != nil {
				return Ref{}, fmt.Errorf("invalid reconciliation")
			}
			if r.Claimed && p.Target != r.Target {
				return Ref{}, fmt.Errorf("target mismatch")
			}
			if p.ExternalID != "" && p.ExternalID != p.Binding.TraceID {
				return Ref{}, fmt.Errorf("reconciled external id mismatch")
			}
			if r.ExternalID != "" && p.Binding.TraceID != r.ExternalID {
				return Ref{}, fmt.Errorf("trace mismatch")
			}
			if r.ExternalID == "" {
				r.ExternalID = p.Binding.TraceID
			}
			r.State = Observing
			r.Binding = &p.Binding
			r.Cursor, r.SourceRef = p.Cursor, p.SourceRef
		case "execution.stop_requested":
			var p stopPayload
			if json.Unmarshal(e.Payload, &p) != nil || (r.State != Accepted && r.State != Observing) || r.StopRequested || !validStopReason(p.Reason) || strings.TrimSpace(p.Actor) == "" || !validStopEvidence(p.Reason, p.EvidenceSeq) || p.StopID != stopIDFor(r.ID) || p.ExternalID != r.ExternalID {
				return Ref{}, fmt.Errorf("invalid stop request")
			}
			r.StopRequested = true
			r.StopID, r.StopReason, r.StopActor, r.StopEvidenceSeq = p.StopID, p.Reason, p.Actor, p.EvidenceSeq
		case "execution.human_resolved":
			var p resolvePayload
			if json.Unmarshal(e.Payload, &p) != nil || r.State != Unknown || (p.To != Failed && p.To != Cancelled) || p.HumanDecisionID == "" {
				return Ref{}, fmt.Errorf("invalid human resolution")
			}
			r.State, r.HumanDecisionID = p.To, p.HumanDecisionID
		default:
			return Ref{}, fmt.Errorf("duplicate or unknown event")
		}
		if raw.ExternalID != "" && r.ExternalID == "" {
			r.ExternalID = raw.ExternalID
		}
		r.Revision = e.Revision
	}
	return r, nil
}
