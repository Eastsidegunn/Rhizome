package question

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"strings"
	"time"
)

const digestPrefix = "rhz-question-v1:"

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
	Revision         uint64
}
type Service struct{ Store events.Port }

type asked struct {
	Title, Body, Recommendation, MissionID, RequestedBy, CorrelationID, Digest string
	GoalID                                                                     string `json:",omitempty"` // RHZ-075 (FR-RHZ-108), additive; omitted when empty so mission-bound payloads stay byte-identical to pre-075 journals.
}
type answered struct{ Decision, Reason, ActorRef, Digest string }

func Digest(title, body, recommendation string) string {
	h := sha256.Sum256([]byte(title + "\x00" + body + "\x00" + recommendation))
	return digestPrefix + hex.EncodeToString(h[:])
}
func VerifyDigest(got, expected string) error {
	if !strings.HasPrefix(got, digestPrefix) || !strings.HasPrefix(expected, digestPrefix) || got != expected {
		return ErrDigestMismatch
	}
	return nil
}
func IDForDigest(d string) string { return "q-" + strings.TrimPrefix(d, digestPrefix)[:24] }
func IDFor(title, body, recommendation string) string {
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
	id := IDForDigest(d)
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
func (s Service) Answer(id string, d Decision, reason, actor, digest string) (Ref, error) {
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
	p, _ := json.Marshal(answered{string(d), reason, actor, digest})
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
			if p.Digest != Digest(p.Title, p.Body, p.Recommendation) {
				return Ref{}, ErrDigestMismatch
			}
			if IDForDigest(p.Digest) != e.AggregateID {
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
			r.Revision = uint64(i + 1)
		}
	}
	return r, nil
}
