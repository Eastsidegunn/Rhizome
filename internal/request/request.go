// Package request owns durable requests for work that must be performed by a
// human outside Rhizome. A request has exactly two revisions: created, then
// closed. Both write and replay paths validate the same durable invariants.
package request

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"rhizome/internal/question"
)

type Decision string

const (
	Done      Decision = "done"
	Unable    Decision = "unable"
	Cancelled Decision = "cancelled"
)

type Ref struct {
	ID, Name, MissionID, GoalID, Why, Where, After, Rollback, RequestedBy string
	Commands                                                              []string
	Decision                                                              Decision
	Memo, Reason, ActorRef                                                string
	CreatedAt, ClosedAt                                                   time.Time
	Revision                                                              uint64
}

type Create struct {
	Name, MissionID, GoalID, Why, Where, After, Rollback, RequestedBy string
	Commands                                                          []string
	CorrelationID                                                     string
}

type Service struct{ Store events.Port }

type created struct {
	Name        string
	MissionID   string `json:",omitempty"`
	GoalID      string `json:",omitempty"`
	Why         string
	Where       string
	Commands    []string
	After       string
	Rollback    string `json:",omitempty"`
	RequestedBy string
}

type closed struct {
	Decision string
	Memo     string `json:",omitempty"`
	Reason   string `json:",omitempty"`
	ActorRef string
}

var createdKeys = map[string]bool{
	"Name": true, "MissionID": true, "GoalID": true, "Why": true,
	"Where": true, "Commands": true, "After": true, "Rollback": true,
	"RequestedBy": true,
}

var closedKeys = map[string]bool{
	"Decision": true, "Memo": true, "Reason": true, "ActorRef": true,
}

func writeLength(h interface{ Write([]byte) (int, error) }, n int) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	_, _ = h.Write(b[:])
}

func writeString(h interface{ Write([]byte) (int, error) }, value string) {
	writeLength(h, len(value))
	_, _ = h.Write([]byte(value))
}

// IDFor derives the stable request id from every durable creation field.
func IDFor(in Create) string {
	h := sha256.New()
	_, _ = h.Write([]byte("rhz-request-v1\x00"))
	for _, value := range []string{in.Name, in.MissionID, in.GoalID, in.Why, in.Where} {
		writeString(h, value)
	}
	writeLength(h, len(in.Commands))
	for _, command := range in.Commands {
		writeString(h, command)
	}
	for _, value := range []string{in.After, in.Rollback, in.RequestedBy} {
		writeString(h, value)
	}
	return "r-" + hex.EncodeToString(h.Sum(nil))[:24]
}

func exactKeys(raw json.RawMessage, allowed map[string]bool) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return fmt.Errorf("invalid request payload")
	}
	for key := range object {
		if !allowed[key] {
			return fmt.Errorf("invalid request payload")
		}
	}
	return nil
}

func invalidControl(value string, allowNewline bool) bool {
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		if r == '\t' || (allowNewline && r == '\n') {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateActor(actor string) error {
	const prefix = "unverified-local-operator:"
	if strings.TrimSpace(actor) == "" || !strings.HasPrefix(actor, prefix) || strings.TrimSpace(strings.TrimPrefix(actor, prefix)) == "" {
		return fmt.Errorf("actor required")
	}
	return nil
}

func validateCreatedQ1(p created, write bool) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("request name required")
	}
	if write && (len(p.Name) > 200 || strings.ContainsAny(p.Name, "\n\r")) {
		return fmt.Errorf("request name invalid")
	}
	return nil
}

func validateCreatedQ2Shape(p created) error {
	if p.MissionID == "" && p.GoalID == "" {
		return fmt.Errorf("missionId or goalId required")
	}
	if p.MissionID != "" && p.GoalID != "" {
		return fmt.Errorf("both missionId and goalId given")
	}
	return nil
}

func validateCreatedQ3ToQ5(p created, write bool) error {
	for _, field := range []struct{ name, value string }{{"why", p.Why}, {"where", p.Where}, {"after", p.After}} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("request %s required", field.name)
		}
		if write && len(field.value) > 2000 {
			return fmt.Errorf("request %s too long", field.name)
		}
	}
	if write && len(p.Rollback) > 2000 {
		return fmt.Errorf("request rollback too long")
	}
	if write && len(p.Commands) > 32 {
		return fmt.Errorf("too many commands")
	}
	for _, command := range p.Commands {
		if strings.TrimSpace(command) == "" || (write && len(command) > 2000) || strings.Contains(command, "```") || strings.HasSuffix(command, "\n") || strings.Contains(command, "\r") {
			return fmt.Errorf("invalid command")
		}
	}
	for _, field := range []struct {
		name, value string
		newline     bool
	}{
		{"name", p.Name, false}, {"why", p.Why, true}, {"where", p.Where, true},
		{"after", p.After, true}, {"rollback", p.Rollback, true},
	} {
		if invalidControl(field.value, field.newline) {
			return fmt.Errorf("invalid control character in %s", field.name)
		}
	}
	for _, command := range p.Commands {
		if invalidControl(command, true) {
			return fmt.Errorf("invalid control character in command")
		}
	}
	return nil
}

func validateCreated(p created, write bool) error {
	if err := validateCreatedQ1(p, write); err != nil {
		return err
	}
	if err := validateCreatedQ2Shape(p); err != nil {
		return err
	}
	if err := validateCreatedQ3ToQ5(p, write); err != nil {
		return err
	}
	return validateActor(p.RequestedBy)
}

func validateClosed(p closed, write bool) error {
	// Q5.
	for _, field := range []struct{ name, value string }{{"memo", p.Memo}, {"reason", p.Reason}} {
		if invalidControl(field.value, true) {
			return fmt.Errorf("invalid control character in %s", field.name)
		}
	}
	// Q6.
	if err := validateActor(p.ActorRef); err != nil {
		return err
	}
	// Q8.
	switch Decision(p.Decision) {
	case Done:
		if p.Reason != "" {
			return fmt.Errorf("unknown intent field")
		}
		if write && len(p.Memo) > 2000 {
			return fmt.Errorf("request memo too long")
		}
	case Unable, Cancelled:
		if strings.TrimSpace(p.Reason) == "" {
			return fmt.Errorf("reason required")
		}
		if p.Memo != "" {
			return fmt.Errorf("unknown intent field")
		}
		if write && len(p.Reason) > 2000 {
			return fmt.Errorf("request reason too long")
		}
	default:
		return fmt.Errorf("unknown intent field")
	}
	return nil
}

// CreateRequest validates and appends request.created at expected revision 0.
func (s Service) CreateRequest(in Create, unknownIntentField ...bool) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	p := created{Name: in.Name, MissionID: in.MissionID, GoalID: in.GoalID, Why: in.Why, Where: in.Where, Commands: append([]string{}, in.Commands...), After: in.After, Rollback: in.Rollback, RequestedBy: in.RequestedBy}
	if err := validateCreatedQ1(p, true); err != nil {
		return Ref{}, err
	}
	in.MissionID, in.GoalID = strings.TrimSpace(in.MissionID), strings.TrimSpace(in.GoalID)
	p.MissionID, p.GoalID = in.MissionID, in.GoalID
	if err := validateCreatedQ2Shape(p); err != nil {
		return Ref{}, err
	}
	if in.MissionID != "" {
		m, err := projector.ReplayMission(s.Store.List("mission", in.MissionID))
		if err != nil {
			return Ref{}, fmt.Errorf("mission not found")
		}
		switch m.State {
		case domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled:
			return Ref{}, fmt.Errorf("mission is terminal")
		}
	} else {
		g, err := projector.ReplayGoal(s.Store.List("goal", in.GoalID))
		if err != nil {
			return Ref{}, fmt.Errorf("goal not found")
		}
		switch g.State {
		case domain.GoalAchieved, domain.GoalFailed, domain.GoalCancelled:
			return Ref{}, fmt.Errorf("goal is terminal")
		}
	}
	if err := validateCreatedQ3ToQ5(p, true); err != nil {
		return Ref{}, err
	}
	actor, err := question.NormalizeActor(in.RequestedBy)
	if err != nil {
		return Ref{}, err
	}
	in.RequestedBy, p.RequestedBy = actor, actor
	id := IDFor(in)
	if len(s.Store.List("request", id)) > 0 {
		return Ref{}, fmt.Errorf("request already exists: %s", id)
	}
	// Q11 is an HTTP-intent concern supplied by the caller, but it is checked
	// here after Q1-Q7 and before the append so numerical rule order and the
	// zero-write invariant both hold.
	if len(unknownIntentField) > 0 && unknownIntentField[0] {
		return Ref{}, fmt.Errorf("unknown intent field")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Ref{}, err
	}
	e := events.Event{AggregateType: "request", AggregateID: id, Revision: 1, Type: "request.created", Payload: b, CorrelationID: in.CorrelationID, CreatedAt: time.Now().UTC()}
	if err := s.Store.Append(0, e); err != nil {
		if errors.Is(err, events.ErrRevisionConflict) {
			if _, replayErr := Replay(s.Store.List("request", id)); replayErr != nil {
				return Ref{}, replayErr
			}
			return Ref{}, fmt.Errorf("request already exists: %s", id)
		}
		return Ref{}, err
	}
	return Replay(s.Store.List("request", id))
}

func (s Service) Close(id string, decision Decision, memo, reason, actor, correlationID string, unknownIntentField ...bool) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	// Q5 precedes actor normalization (Q6) and decision-field pairing (Q8).
	for _, field := range []struct{ name, value string }{{"memo", memo}, {"reason", reason}} {
		if invalidControl(field.value, true) {
			return Ref{}, fmt.Errorf("invalid control character in %s", field.name)
		}
	}
	actor, err := question.NormalizeActor(actor)
	if err != nil {
		return Ref{}, err
	}
	p := closed{Decision: string(decision), Memo: memo, Reason: reason, ActorRef: actor}
	if err := validateClosed(p, true); err != nil {
		return Ref{}, err
	}
	log := s.Store.List("request", id)
	if len(log) == 0 {
		return Ref{}, fmt.Errorf("request not found")
	}
	r, err := Replay(log)
	if err != nil {
		return Ref{}, err
	}
	if r.Revision > 1 {
		return Ref{}, fmt.Errorf("request already closed")
	}
	// Q11 follows the close state checks (Q9/Q10) and still precedes append.
	if len(unknownIntentField) > 0 && unknownIntentField[0] {
		return Ref{}, fmt.Errorf("unknown intent field")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Ref{}, err
	}
	e := events.Event{AggregateType: "request", AggregateID: id, Revision: 2, Type: "request.closed", Payload: b, CorrelationID: correlationID, CreatedAt: time.Now().UTC()}
	if err := s.Store.Append(1, e); err != nil {
		if errors.Is(err, events.ErrRevisionConflict) {
			after, replayErr := Replay(s.Store.List("request", id))
			if replayErr != nil {
				return Ref{}, replayErr
			}
			if after.Revision > 1 {
				return Ref{}, fmt.Errorf("request already closed")
			}
		}
		return Ref{}, err
	}
	return Replay(s.Store.List("request", id))
}

func (s Service) Get(id string) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	if len(s.Store.List("request", id)) == 0 {
		return Ref{}, fmt.Errorf("request not found")
	}
	return Replay(s.Store.List("request", id))
}

func Replay(log []events.Event) (Ref, error) {
	if len(log) == 0 {
		return Ref{}, fmt.Errorf("empty request")
	}
	if len(log) > 2 {
		return Ref{}, fmt.Errorf("invalid request revision")
	}
	var out Ref
	for i, event := range log {
		if event.AggregateType != "request" || event.Revision != uint64(i+1) || (i > 0 && event.AggregateID != out.ID) {
			return Ref{}, events.ErrRevisionConflict
		}
		switch i {
		case 0:
			if event.Type != "request.created" || exactKeys(event.Payload, createdKeys) != nil {
				return Ref{}, fmt.Errorf("invalid request.created")
			}
			var p created
			if json.Unmarshal(event.Payload, &p) != nil || p.Commands == nil || validateCreated(p, false) != nil {
				return Ref{}, fmt.Errorf("invalid request.created")
			}
			in := Create{Name: p.Name, MissionID: p.MissionID, GoalID: p.GoalID, Why: p.Why, Where: p.Where, Commands: p.Commands, After: p.After, Rollback: p.Rollback, RequestedBy: p.RequestedBy}
			if IDFor(in) != event.AggregateID {
				return Ref{}, fmt.Errorf("request id mismatch")
			}
			out = Ref{ID: event.AggregateID, Name: p.Name, MissionID: p.MissionID, GoalID: p.GoalID, Why: p.Why, Where: p.Where, Commands: append([]string{}, p.Commands...), After: p.After, Rollback: p.Rollback, RequestedBy: p.RequestedBy, CreatedAt: event.CreatedAt, Revision: 1}
		case 1:
			if event.Type != "request.closed" || exactKeys(event.Payload, closedKeys) != nil {
				return Ref{}, fmt.Errorf("invalid request.closed")
			}
			var p closed
			if json.Unmarshal(event.Payload, &p) != nil || validateClosed(p, false) != nil {
				return Ref{}, fmt.Errorf("invalid request.closed")
			}
			out.Decision, out.Memo, out.Reason, out.ActorRef = Decision(p.Decision), p.Memo, p.Reason, p.ActorRef
			out.ClosedAt, out.Revision = event.CreatedAt, 2
		}
	}
	return out, nil
}
