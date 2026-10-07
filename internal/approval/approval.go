package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"sort"
	"strings"
	"time"
)

type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

type State string

const (
	InputRecorded State = "input_recorded"
	Dispatched    State = "dispatched"
	Observed      State = "observed"
)

type RequestKey struct{ TraceID, SpanID, RequestID string }
type Request struct {
	Target, Destination, Scope, RequestedBy, RequestedAt, Reason, Impact string
	ExternalEffect                                                       bool
	ExpiresAt                                                            string
}
type GateFields struct {
	GateName, GateType, RequestedAction string
	Urgency, RiskTier                   string
	ReasonRequired                      bool
	Request                             Request
	Supersedes                          string
}
type Ref struct {
	ID            string
	Key           RequestKey
	HumanDecision Decision
	JanusDecision Decision
	Reason        string
	JanusReason   string
	ResponseID    string
	RequestDigest string
	ActorRef      string
	ActorVerified bool
	CorrelationID string
	State         State
	ResponseSeq   int64
	Revision      uint64
	GateFields
	InputAt, ObservedAt time.Time
	DecisionID          string
}
type Service struct{ Store events.Port }
type input struct {
	RequestKey
	Decision                                                               Decision `json:"decision"`
	Reason, ResponseID, RequestDigest, ActorRef, CorrelationID, DecisionID string
	ActorVerified                                                          bool
	GateFields
}
type dispatched struct {
	ResponseID            string
	Decision              Decision
	Reason, RequestDigest string
}
type observed struct {
	ResponseSeq   int64
	JanusDecision Decision
	JanusReason   string
}

var tr = regexp.MustCompile(`^[0-9a-f]{32}$`)
var sp = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (k RequestKey) valid() error {
	if !tr.MatchString(k.TraceID) || !sp.MatchString(k.SpanID) || k.RequestID == "" {
		return fmt.Errorf("invalid request key")
	}
	return nil
}
func id(k RequestKey) string {
	h := sha256.Sum256([]byte(k.TraceID + "\x00" + k.SpanID + "\x00" + k.RequestID))
	return "appr-" + hex.EncodeToString(h[:])[:24]
}

// IDFor derives the approval aggregate ID for a request key (RHZ-047 D17):
// the pending gate surfaced from an observed request and the eventual human
// input share one identity.
func IDFor(k RequestKey) string { return id(k) }

// ResponseIDFor derives the deterministic response_id for a gate (RHZ-047
// D20): a restarted Rhizome resubmits the same idempotency token (계약 §4).
func ResponseIDFor(aggregateID string) string {
	h := sha256.Sum256([]byte("response\x00" + aggregateID))
	return "resp-" + hex.EncodeToString(h[:])[:24]
}

// ErrDigestMismatch and VerifyDigest live here (moved from janusadapter in
// RHZ-047, which keeps a delegating alias) so the workspace relay can compare
// digests without importing janusadapter — that import would cycle with
// janusadapter's internal tests, and the digest is approval-domain material.
var ErrDigestMismatch = errors.New("DIGEST_MISMATCH")

// VerifyDigest compares the digest a human decided on with the durable
// request digest — opaque byte equality under the contract prefix, never a
// local recomputation (FR-RHZ-078).
func VerifyDigest(local, remote string) error {
	const p = "hx-args-digest-v1:"
	if !strings.HasPrefix(local, p) || !strings.HasPrefix(remote, p) {
		return ErrDigestMismatch
	}
	if local != remote {
		return ErrDigestMismatch
	}
	return nil
}
func validDec(d Decision) bool { return d == Allow || d == Deny }
func validGate(g GateFields) error {
	if g.GateName == "" && g.GateType == "" && g.RequestedAction == "" && g.RiskTier == "" && g.Request.Target == "" && !g.ReasonRequired && g.Urgency == "" && g.Request.ExpiresAt == "" {
		return nil
	}
	if strings.TrimSpace(g.GateName) == "" || strings.TrimSpace(g.GateType) == "" || strings.TrimSpace(g.RequestedAction) == "" || (g.RiskTier != "logged" && g.RiskTier != "privileged") || (g.Urgency != "" && g.Urgency != "low" && g.Urgency != "normal" && g.Urgency != "high") || strings.TrimSpace(g.Request.Target) == "" || strings.TrimSpace(g.Request.RequestedBy) == "" || strings.TrimSpace(g.Request.RequestedAt) == "" {
		return fmt.Errorf("invalid gate fields")
	}
	if _, e := time.Parse(time.RFC3339, g.Request.RequestedAt); e != nil {
		return e
	}
	if g.Request.ExpiresAt != "" {
		if _, e := time.Parse(time.RFC3339, g.Request.ExpiresAt); e != nil {
			return e
		}
	}
	return nil
}

func (s Service) RecordInput(k RequestKey, d Decision, reason, response, digest, actor, correlation, decisionID string, verified bool) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	if e := k.valid(); e != nil {
		return Ref{}, e
	}
	if !validDec(d) || digest == "" || response == "" || (d == Deny && reason == "") {
		return Ref{}, fmt.Errorf("invalid input")
	}
	if strings.TrimSpace(actor) == "" {
		return Ref{}, fmt.Errorf("actor required")
	}
	if !verified && !strings.HasPrefix(actor, "unverified-local-operator:") {
		actor = "unverified-local-operator:" + actor
	}
	if strings.TrimSpace(strings.TrimPrefix(actor, "unverified-local-operator:")) == "" {
		return Ref{}, fmt.Errorf("actor required")
	}
	if decisionID != "" {
		x, e := decision.Replay(s.Store.List("decision", decisionID))
		if e != nil || x.Kind != decision.WaitHuman {
			return Ref{}, fmt.Errorf("invalid wait_human decision")
		}
	}
	return s.recordInput(k, d, reason, response, digest, actor, correlation, decisionID, verified, GateFields{})
}
func (s Service) recordInput(k RequestKey, d Decision, reason, response, digest, actor, correlation, decisionID string, verified bool, g GateFields) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	if e := k.valid(); e != nil {
		return Ref{}, e
	}
	if !validDec(d) || digest == "" || response == "" || (d == Deny && reason == "") || strings.TrimSpace(actor) == "" {
		return Ref{}, fmt.Errorf("invalid input")
	}
	if !verified && !strings.HasPrefix(actor, "unverified-local-operator:") {
		actor = "unverified-local-operator:" + actor
	}
	if strings.TrimSpace(strings.TrimPrefix(actor, "unverified-local-operator:")) == "" {
		return Ref{}, fmt.Errorf("actor required")
	}
	if e := validGate(g); e != nil {
		return Ref{}, e
	}
	if g.ReasonRequired && reason == "" {
		return Ref{}, fmt.Errorf("reason required")
	}
	if decisionID != "" {
		x, e := decision.Replay(s.Store.List("decision", decisionID))
		if e != nil || x.Kind != decision.WaitHuman {
			return Ref{}, fmt.Errorf("invalid wait_human decision")
		}
	}
	p, _ := json.Marshal(input{RequestKey: k, Decision: d, Reason: reason, ResponseID: response, RequestDigest: digest, ActorRef: actor, ActorVerified: verified, CorrelationID: correlation, DecisionID: decisionID, GateFields: g})
	return s.append(id(k), "approval.input_recorded", p, 0)
}
func (s Service) RecordInputWithGate(k RequestKey, d Decision, reason, response, digest, actor, correlation, decisionID string, verified bool, g GateFields) (Ref, error) {
	return s.recordInput(k, d, reason, response, digest, actor, correlation, decisionID, verified, g)
}

func (s Service) append(a, t string, p []byte, rev uint64) (Ref, error) {
	log := s.Store.List("approval", a)
	if rev != uint64(len(log)) {
		return Ref{}, events.ErrRevisionConflict
	}
	e := events.Event{AggregateType: "approval", AggregateID: a, Revision: rev + 1, Type: t, Payload: p, CreatedAt: time.Now().UTC()}
	if _, x := Replay(append(append([]events.Event(nil), log...), e)); x != nil {
		return Ref{}, x
	}
	if x := s.Store.Append(rev, e); x != nil {
		return Ref{}, x
	}
	return Replay(s.Store.List("approval", a))
}
func (s Service) Dispatch(a, response string) (Ref, error) {
	r, e := s.Get(a)
	if e != nil {
		return Ref{}, e
	}
	if r.State == Observed || r.State == State("") {
		return Ref{}, fmt.Errorf("invalid state")
	}
	if response != r.ResponseID {
		return Ref{}, fmt.Errorf("response mismatch")
	}
	p, _ := json.Marshal(dispatched{r.ResponseID, r.HumanDecision, r.Reason, r.RequestDigest})
	return s.append(a, "approval.response_dispatched", p, r.Revision)
}
func (s Service) Observe(a string, seq int64, d Decision, reason string) (Ref, error) {
	r, e := s.Get(a)
	if e != nil {
		return Ref{}, e
	}
	if r.State != InputRecorded && r.State != Dispatched || seq <= 0 || !validDec(d) || (d == Deny && reason == "") {
		return Ref{}, fmt.Errorf("invalid observation")
	}
	p, _ := json.Marshal(observed{seq, d, reason})
	return s.append(a, "approval.response_observed", p, r.Revision)
}
func Replay(log []events.Event) (Ref, error) {
	if len(log) == 0 {
		return Ref{}, fmt.Errorf("empty approval")
	}
	var r Ref
	for i, e := range log {
		if e.AggregateType != "approval" || e.Revision != uint64(i+1) || (i > 0 && e.AggregateID != r.ID) {
			return Ref{}, events.ErrRevisionConflict
		}
		if i == 0 {
			var p input
			if e.Type != "approval.input_recorded" || json.Unmarshal(e.Payload, &p) != nil {
				return Ref{}, fmt.Errorf("invalid input")
			}
			if p.RequestKey.valid() != nil || validGate(p.GateFields) != nil || (p.GateFields.ReasonRequired && p.Reason == "") || p.GateFields.Supersedes == e.AggregateID || strings.TrimSpace(p.ActorRef) == "" || (!p.ActorVerified && !strings.HasPrefix(p.ActorRef, "unverified-local-operator:")) || !validDec(p.Decision) || p.RequestDigest == "" || p.ResponseID == "" || (p.Decision == Deny && p.Reason == "") || e.AggregateID != id(p.RequestKey) {
				return Ref{}, fmt.Errorf("invalid input")
			}
			r = Ref{ID: e.AggregateID, Key: p.RequestKey, HumanDecision: p.Decision, Reason: p.Reason, ResponseID: p.ResponseID, RequestDigest: p.RequestDigest, ActorRef: p.ActorRef, ActorVerified: p.ActorVerified, CorrelationID: p.CorrelationID, State: InputRecorded, Revision: 1, GateFields: p.GateFields, InputAt: e.CreatedAt, DecisionID: p.DecisionID}
			continue
		}
		if r.State == Observed {
			return Ref{}, fmt.Errorf("terminal approval")
		}
		switch e.Type {
		case "approval.response_dispatched":
			var p dispatched
			if json.Unmarshal(e.Payload, &p) != nil || p.ResponseID != r.ResponseID || p.Decision != r.HumanDecision || p.Reason != r.Reason || p.RequestDigest != r.RequestDigest {
				return Ref{}, fmt.Errorf("dispatch mismatch")
			}
			r.State = Dispatched
		case "approval.response_observed":
			var p observed
			if r.State != InputRecorded && r.State != Dispatched || json.Unmarshal(e.Payload, &p) != nil || p.ResponseSeq <= 0 || !validDec(p.JanusDecision) || (p.JanusDecision == Deny && p.JanusReason == "") {
				return Ref{}, fmt.Errorf("invalid observed")
			}
			r.JanusDecision, r.JanusReason, r.ResponseSeq, r.State, r.ObservedAt = p.JanusDecision, p.JanusReason, p.ResponseSeq, Observed, e.CreatedAt
		default:
			return Ref{}, fmt.Errorf("unknown approval event")
		}
		r.Revision = e.Revision
	}
	return r, nil
}
func (s Service) Get(a string) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	return Replay(s.Store.List("approval", a))
}
func (s Service) ByTrace(t string) ([]Ref, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil store")
	}
	m := map[string]bool{}
	for _, e := range s.Store.All() {
		if e.AggregateType == "approval" {
			m[e.AggregateID] = true
		}
	}
	ids := make([]string, 0, len(m))
	for x := range m {
		ids = append(ids, x)
	}
	sort.Strings(ids)
	out := make([]Ref, 0)
	for _, x := range ids {
		r, e := s.Get(x)
		if e != nil {
			return nil, e
		}
		if r.Key.TraceID == t {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s Service) Supersede(old string, k RequestKey, d Decision, reason, response, digest, actor, correlation, decisionID string, verified bool, g GateFields) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	if _, e := s.Get(old); e != nil {
		return Ref{}, e
	}
	if id(k) == old {
		return Ref{}, fmt.Errorf("self supersede")
	}
	g.Supersedes = old
	return s.recordInput(k, d, reason, response, digest, actor, correlation, decisionID, verified, g)
}
func (s Service) IsSuperseded(aggregateID string) (bool, error) {
	if s.Store == nil {
		return false, fmt.Errorf("nil store")
	}
	for _, e := range s.Store.All() {
		if e.AggregateType != "approval" || e.Type != "approval.input_recorded" {
			continue
		}
		var p input
		if json.Unmarshal(e.Payload, &p) != nil {
			return false, fmt.Errorf("malformed approval")
		}
		if p.Supersedes == aggregateID {
			return true, nil
		}
	}
	return false, nil
}
