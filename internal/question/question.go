package question

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"strings"
	"time"
)

const (
	digestPrefixV1 = "rhz-question-v1:"
	digestPrefixV2 = "rhz-question-v2:"
)

var ErrDigestMismatch = errors.New("question digest mismatch")

type Decision string

const (
	Approve Decision = "approve"
	Reject  Decision = "reject"
	// RequestChanges (RHZ-078, FR-RHZ-109) is the non-terminal third decision:
	// the human sends the request back with a reason. It is recorded as a
	// question.answered event (existing type, new decision value); a later
	// approve/reject on the same question is allowed without re-asking.
	RequestChanges Decision = "requestChanges"
)

// Terminal reports whether a decision closes the question.
func (d Decision) Terminal() bool { return d == Approve || d == Reject }

type Ref struct {
	ID, Title, Body, Recommendation, MissionID, RequestedBy, CorrelationID, Digest string
	// GoalID binds a gate to a goal instead of a mission (RHZ-075,
	// FR-RHZ-108). Additive: legacy question.asked payloads without the key
	// replay with GoalID == "".
	GoalID string
	// Decision/Reason/ActorRef describe the LATEST answer: after
	// requestChanges→approve, Reason/ActorRef are those of the approve
	// (RHZ-078, FR-RHZ-109); the earlier request stays in the journal only.
	Decision         Decision
	Reason, ActorRef string
	Verification     *Verification
	Revision         uint64
}
type Service struct{ Store events.Port }

type asked struct {
	Title, Body, Recommendation, MissionID, RequestedBy, CorrelationID, Digest string
	GoalID                                                                     string `json:",omitempty"` // RHZ-075 (FR-RHZ-108), additive; omitted when empty so mission-bound payloads stay byte-identical to pre-075 journals.
}
type answered struct {
	Decision, Reason, ActorRef, Digest string
	Verification                       *Verification `json:",omitempty"`
}

// Verification is either a provenance claim or a signed decision envelope.
// Display status is deliberately not stored; projections derive it on read.
type Verification struct {
	ClaimKind     string     `json:",omitempty"`
	OriginClaim   string     `json:",omitempty"`
	OriginChannel string     `json:",omitempty"`
	RelayChain    []string   `json:",omitempty"`
	SessionRef    string     `json:",omitempty"`
	ObservedAt    string     `json:",omitempty"`
	Signature     *Signature `json:",omitempty"`
}

// Signature is the stored Go-name-key envelope for a signed gate decision.
type Signature struct{ KeyID, SignedAt, Nonce, Sig string }

var errInvalidVerification = errors.New("invalid verification")

var payloadVerificationKeys = map[string]bool{
	"ClaimKind": true, "OriginClaim": true, "OriginChannel": true,
	"RelayChain": true, "SessionRef": true, "ObservedAt": true,
	"Signature": true,
}

var intentVerificationKeys = map[string]bool{
	"claimKind": true, "originClaim": true, "originChannel": true,
	"relayChain": true, "sessionRef": true, "observedAt": true,
	"signature": true,
}

type intentVerification struct {
	ClaimKind     string   `json:"claimKind"`
	OriginClaim   string   `json:"originClaim"`
	OriginChannel string   `json:"originChannel"`
	RelayChain    []string `json:"relayChain"`
	SessionRef    string   `json:"sessionRef"`
	ObservedAt    string   `json:"observedAt"`
	Signature     *struct {
		KeyID    string `json:"keyId"`
		SignedAt string `json:"signedAt"`
		Nonce    string `json:"nonce"`
		Sig      string `json:"sig"`
	} `json:"signature"`
}

// DecodeIntentVerification performs the exact-case, pre-decode key check for
// the lowerCamel intent surface, then applies the common V1-V7 rules.
func DecodeIntentVerification(raw json.RawMessage, actor string) (*Verification, error) {
	keys, err := exactVerificationKeys(raw, intentVerificationKeys)
	if err != nil {
		return nil, err
	}
	var wire intentVerification
	if json.Unmarshal(raw, &wire) != nil {
		return nil, errInvalidVerification
	}
	v := &Verification{
		ClaimKind: wire.ClaimKind, OriginClaim: wire.OriginClaim,
		OriginChannel: wire.OriginChannel, RelayChain: wire.RelayChain,
		SessionRef: wire.SessionRef, ObservedAt: wire.ObservedAt,
	}
	if wire.Signature != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(keysRaw(raw, "signature"), &object) != nil || len(object) != 4 {
			return nil, errInvalidVerification
		}
		for _, key := range []string{"keyId", "signedAt", "nonce", "sig"} {
			if _, ok := object[key]; !ok {
				return nil, errInvalidVerification
			}
		}
		v.Signature = &Signature{KeyID: wire.Signature.KeyID, SignedAt: wire.Signature.SignedAt, Nonce: wire.Signature.Nonce, Sig: wire.Signature.Sig}
	}
	return validateVerification(v, actor, keys, false)
}

func keysRaw(raw json.RawMessage, key string) json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	return object[key]
}

// DecodePayloadVerification performs the exact-case pre-decode check for the
// Go-field-name journal surface, then applies the common V1-V7 rules.
func DecodePayloadVerification(raw json.RawMessage, actor string) (*Verification, error) {
	keys, err := exactVerificationKeys(raw, payloadVerificationKeys)
	if err != nil {
		return nil, err
	}
	var v Verification
	if json.Unmarshal(raw, &v) != nil {
		return nil, errInvalidVerification
	}
	if v.Signature != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(keysRaw(raw, "Signature"), &object) != nil || len(object) != 4 {
			return nil, errInvalidVerification
		}
		for _, key := range []string{"KeyID", "SignedAt", "Nonce", "Sig"} {
			if _, ok := object[key]; !ok {
				return nil, errInvalidVerification
			}
		}
	}
	return validateVerification(&v, actor, keys, true)
}

func exactVerificationKeys(raw json.RawMessage, allowed map[string]bool) (map[string]bool, error) {
	var object map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, errInvalidVerification
	}
	keys := make(map[string]bool, len(object))
	for key := range object {
		if !allowed[key] {
			return nil, errInvalidVerification
		}
		keys[key] = true
	}
	return keys, nil
}

// ValidateVerification applies the common write/replay shape rules to a
// programmatically supplied claim and returns a normalized copy.
func ValidateVerification(v *Verification, actor string) (*Verification, error) {
	if v == nil {
		return nil, nil
	}
	keys := map[string]bool{}
	if v.OriginChannel != "" {
		keys["OriginChannel"] = true
	}
	if v.RelayChain != nil {
		keys["RelayChain"] = true
	}
	if v.SessionRef != "" {
		keys["SessionRef"] = true
	}
	if v.ObservedAt != "" {
		keys["ObservedAt"] = true
	}
	if v.Signature != nil {
		keys["Signature"] = true
	}
	return validateVerification(v, actor, keys, true)
}

func validateVerification(v *Verification, actor string, keys map[string]bool, payloadKeys bool) (*Verification, error) {
	key := func(payload, intent string) bool {
		if payloadKeys {
			return keys[payload]
		}
		return keys[intent]
	}
	if v == nil {
		return nil, errInvalidVerification
	}
	actor, err := normalizeActor(actor)
	if err != nil {
		return nil, errInvalidVerification
	}
	out := *v
	out.RelayChain = append([]string(nil), v.RelayChain...)
	if v.Signature != nil {
		if len(keys) != 1 || !key("Signature", "signature") || !validSignature(v.Signature) {
			return nil, errInvalidVerification
		}
		sig := *v.Signature
		out.Signature = &sig
		return &out, nil
	}
	if (v.ClaimKind != "relayed" && v.ClaimKind != "session-direct") || v.OriginClaim != "H" {
		return nil, errInvalidVerification
	}
	if key("ObservedAt", "observedAt") {
		if _, err := time.Parse(time.RFC3339, v.ObservedAt); err != nil {
			return nil, errInvalidVerification
		}
	}
	switch v.ClaimKind {
	case "relayed":
		if len(out.RelayChain) == 0 || key("SessionRef", "sessionRef") {
			return nil, errInvalidVerification
		}
		for i := range out.RelayChain {
			out.RelayChain[i], err = normalizeActor(out.RelayChain[i])
			if err != nil {
				return nil, errInvalidVerification
			}
		}
		if out.RelayChain[len(out.RelayChain)-1] != actor {
			return nil, errInvalidVerification
		}
		if key("OriginChannel", "originChannel") && out.OriginChannel != "ops-session" && out.OriginChannel != "board" {
			return nil, errInvalidVerification
		}
	case "session-direct":
		if strings.TrimSpace(out.SessionRef) == "" || key("RelayChain", "relayChain") {
			return nil, errInvalidVerification
		}
		if key("OriginChannel", "originChannel") && out.OriginChannel != "dev-session" {
			return nil, errInvalidVerification
		}
	}
	return &out, nil
}

var signatureKeyID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var signatureNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validSignature(sig *Signature) bool {
	if sig == nil || !signatureKeyID.MatchString(sig.KeyID) || !signatureNonce.MatchString(sig.Nonce) {
		return false
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", sig.SignedAt)
	if err != nil || t.UTC().Format("2006-01-02T15:04:05Z") != sig.SignedAt {
		return false
	}
	_, err = base64.StdEncoding.Strict().DecodeString(sig.Sig)
	return err == nil
}

func Digest(title, body, recommendation string) string {
	return digestV2(title, body, recommendation)
}

func digestV1(title, body, recommendation string) string {
	h := sha256.Sum256([]byte(title + "\x00" + body + "\x00" + recommendation))
	return digestPrefixV1 + hex.EncodeToString(h[:])
}

func digestV2(title, body, recommendation string) string {
	h := sha256.New()
	for _, field := range []string{"rhz-question-v2", title, body, recommendation} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(field))
	}
	return digestPrefixV2 + hex.EncodeToString(h.Sum(nil))
}
func VerifyDigest(got, expected string) error {
	known := func(d string) bool {
		return strings.HasPrefix(d, digestPrefixV1) || strings.HasPrefix(d, digestPrefixV2)
	}
	if !known(got) || !known(expected) || got != expected {
		return ErrDigestMismatch
	}
	return nil
}
func IDForDigest(d string) (string, error) {
	var hexDigest string
	switch {
	case strings.HasPrefix(d, digestPrefixV1):
		hexDigest = strings.TrimPrefix(d, digestPrefixV1)
	case strings.HasPrefix(d, digestPrefixV2):
		hexDigest = strings.TrimPrefix(d, digestPrefixV2)
	default:
		return "", ErrDigestMismatch
	}
	if len(hexDigest) < 24 {
		return "", ErrDigestMismatch
	}
	return "q-" + hexDigest[:24], nil
}
func IDFor(title, body, recommendation string) (string, error) {
	return IDForDigest(Digest(title, body, recommendation))
}

// NormalizeActor is the kernel's requestedBy/actor rule, exported (RHZ-083,
// FR-RHZ-114) so a caller that pre-validates before any append (assembly.Run)
// applies exactly the rule Ask/Answer will apply: non-blank, prefixed with
// unverified-local-operator:, and non-blank after the prefix.
func NormalizeActor(actor string) (string, error) { return normalizeActor(actor) }

func normalizeActor(actor string) (string, error) {
	if strings.TrimSpace(actor) == "" {
		return "", fmt.Errorf("actor required")
	}
	if !strings.HasPrefix(actor, "unverified-local-operator:") {
		actor = "unverified-local-operator:" + actor
	}
	if validateActor(actor) != nil {
		return "", fmt.Errorf("actor required")
	}
	return actor, nil
}

func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" || !strings.HasPrefix(actor, "unverified-local-operator:") || strings.TrimSpace(strings.TrimPrefix(actor, "unverified-local-operator:")) == "" {
		return fmt.Errorf("actor required")
	}
	return nil
}

// Ask records a question. At most one of missionID/goalID may be given
// (RHZ-075, FR-RHZ-108); the "exactly one" rule is owned by the relay
// entrance so kernel callers keep today's permissiveness. Replay rules are
// unchanged — binding is validated only here, never on replay.
func (s Service) Ask(title, body, recommendation, missionID, goalID, requestedBy, correlationID string) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		return Ref{}, fmt.Errorf("title and body required")
	}
	requestedBy, err := normalizeActor(requestedBy)
	if err != nil {
		return Ref{}, err
	}
	d := Digest(title, body, recommendation)
	id, err := IDForDigest(d)
	if err != nil {
		return Ref{}, err
	}
	if log := s.Store.List("question", id); len(log) > 0 {
		return Replay(log)
	}
	if missionID != "" && goalID != "" {
		return Ref{}, fmt.Errorf("both missionId and goalId given")
	}
	if missionID != "" {
		m, err := projector.ReplayMission(s.Store.List("mission", missionID))
		if err != nil {
			return Ref{}, fmt.Errorf("mission reference: %w", err)
		}
		switch m.State {
		case domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled:
			return Ref{}, fmt.Errorf("mission is terminal")
		}
	}
	if goalID != "" {
		g, err := projector.ReplayGoal(s.Store.List("goal", goalID))
		if err != nil {
			return Ref{}, fmt.Errorf("goal reference: %w", err)
		}
		switch g.State {
		case domain.GoalAchieved, domain.GoalFailed, domain.GoalCancelled:
			return Ref{}, fmt.Errorf("goal is terminal")
		}
	}
	p, _ := json.Marshal(asked{Title: title, Body: body, Recommendation: recommendation, MissionID: missionID, GoalID: goalID, RequestedBy: requestedBy, CorrelationID: correlationID, Digest: d})
	e := events.Event{AggregateType: "question", AggregateID: id, Revision: 1, Type: "question.asked", Payload: p, CorrelationID: correlationID, CreatedAt: time.Now().UTC()}
	if err := s.Store.Append(0, e); err != nil {
		return Ref{}, err
	}
	return Replay(s.Store.List("question", id))
}
func (s Service) Answer(id string, d Decision, reason, actor, digest string, verification ...*Verification) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	r, err := Replay(s.Store.List("question", id))
	if err != nil {
		return Ref{}, err
	}
	if err := nextDecision(r.Decision, d); err != nil {
		return Ref{}, err
	}
	if d != Approve && strings.TrimSpace(reason) == "" {
		return Ref{}, fmt.Errorf("reason required")
	}
	if err := VerifyDigest(digest, r.Digest); err != nil {
		return Ref{}, err
	}
	actor, err = normalizeActor(actor)
	if err != nil {
		return Ref{}, err
	}
	if len(verification) > 1 {
		return Ref{}, errInvalidVerification
	}
	var claim *Verification
	if len(verification) == 1 {
		claim, err = ValidateVerification(verification[0], actor)
		if err != nil {
			return Ref{}, err
		}
	}
	p, _ := json.Marshal(answered{Decision: string(d), Reason: reason, ActorRef: actor, Digest: digest, Verification: claim})
	e := events.Event{AggregateType: "question", AggregateID: id, Revision: r.Revision + 1, Type: "question.answered", Payload: p, CreatedAt: time.Now().UTC()}
	if err := s.Store.Append(r.Revision, e); err != nil {
		return Ref{}, err
	}
	return Replay(s.Store.List("question", id))
}
func (s Service) Get(id string) (Ref, error) { return Replay(s.Store.List("question", id)) }

// nextDecision is the answer sequencing rule (RHZ-078, FR-RHZ-109), shared by
// Answer and Replay so the write path and the projection cannot drift:
// terminal (approve/reject) → nothing more; requestChanges → only
// approve/reject; pending → any known decision.
func nextDecision(current, next Decision) error {
	if next != Approve && next != Reject && next != RequestChanges {
		return fmt.Errorf("invalid decision")
	}
	switch {
	case current.Terminal():
		return fmt.Errorf("gate already has input")
	case current == RequestChanges && next == RequestChanges:
		return fmt.Errorf("changes already requested")
	}
	return nil
}
func Replay(log []events.Event) (Ref, error) {
	if len(log) == 0 {
		return Ref{}, fmt.Errorf("empty question")
	}
	var r Ref
	for i, e := range log {
		if e.AggregateType != "question" || e.Revision != uint64(i+1) || (i > 0 && e.AggregateID != r.ID) {
			return Ref{}, events.ErrRevisionConflict
		}
		if i == 0 {
			var p asked
			if e.Type != "question.asked" || json.Unmarshal(e.Payload, &p) != nil {
				return Ref{}, fmt.Errorf("invalid asked")
			}
			if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Body) == "" {
				return Ref{}, fmt.Errorf("title and body required")
			}
			if validateActor(p.RequestedBy) != nil {
				return Ref{}, fmt.Errorf("requested by required")
			}
			var recomputed string
			switch {
			case strings.HasPrefix(p.Digest, digestPrefixV1):
				recomputed = digestV1(p.Title, p.Body, p.Recommendation)
			case strings.HasPrefix(p.Digest, digestPrefixV2):
				recomputed = digestV2(p.Title, p.Body, p.Recommendation)
			default:
				return Ref{}, ErrDigestMismatch
			}
			if p.Digest != recomputed {
				return Ref{}, ErrDigestMismatch
			}
			id, err := IDForDigest(p.Digest)
			if err != nil || id != e.AggregateID {
				return Ref{}, ErrDigestMismatch
			}
			r = Ref{ID: e.AggregateID, Title: p.Title, Body: p.Body, Recommendation: p.Recommendation, MissionID: p.MissionID, GoalID: p.GoalID, RequestedBy: p.RequestedBy, CorrelationID: p.CorrelationID, Digest: p.Digest, Revision: 1}
		} else {
			if e.Type != "question.answered" {
				return Ref{}, fmt.Errorf("invalid answered")
			}
			var p answered
			if json.Unmarshal(e.Payload, &p) != nil {
				return Ref{}, fmt.Errorf("invalid answered")
			}
			p.Verification = nil
			var object map[string]json.RawMessage
			if json.Unmarshal(e.Payload, &object) != nil {
				return Ref{}, fmt.Errorf("invalid answered")
			}
			if raw, ok := object["Verification"]; ok {
				var err error
				p.Verification, err = DecodePayloadVerification(raw, p.ActorRef)
				if err != nil {
					return Ref{}, errInvalidVerification
				}
			}
			d := Decision(p.Decision)
			if err := nextDecision(r.Decision, d); err != nil {
				return Ref{}, err
			}
			if d != Approve && strings.TrimSpace(p.Reason) == "" {
				return Ref{}, fmt.Errorf("reason required")
			}
			if VerifyDigest(p.Digest, r.Digest) != nil {
				return Ref{}, ErrDigestMismatch
			}
			if validateActor(p.ActorRef) != nil {
				return Ref{}, fmt.Errorf("actor required")
			}
			r.Decision = d
			r.Reason = p.Reason
			r.ActorRef = p.ActorRef
			r.Verification = p.Verification
			r.Revision = uint64(i + 1)
		}
	}
	return r, nil
}
