// Package session is the session registry (RHZ-139, FR-RHZ-183): every
// agent session Rhizome spawns — a background claude -p of the launcher or an
// interactive terminal tab opened through an external driver — is recorded
// on the session/<sessionId> stream (session.opened, session.closed).
// Events are append-only, this Service is the single writer of the stream
// and Replay rebuilds the state. A stream may cycle opened → closed → opened
// (a gate resume relaunches the same claude session id).
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"rhizome/internal/events"
)

const (
	AggregateType = "session"
	OpenedType    = "session.opened"
	ClosedType    = "session.closed"

	Background  = "background"
	Interactive = "interactive"

	Open   = "open"
	Closed = "closed"
)

// Opened is the session.opened payload. ExecutionID links a launcher session
// to its execution stream (the run record is not duplicated here).
type Opened struct {
	SessionID   string `json:"sessionId"`
	Name        string `json:"name"`
	MissionID   string `json:"missionId,omitempty"`
	Mode        string `json:"mode"`
	Cwd         string `json:"cwd"`
	DriverRef   string `json:"driverRef,omitempty"`
	ExecutionID string `json:"executionId,omitempty"`
}

type closedPayload struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
}

// Session is one replayed stream.
type Session struct {
	Opened
	State    string
	Reason   string // close reason of a closed session
	OpenedAt int64  // envelope CreatedAt (Unix ms) of the latest opened, 0 if unknown
	Revision uint64
}

type Service struct{ Store events.Port }

func validOpened(p Opened, agg string) error {
	if strings.TrimSpace(p.SessionID) == "" || p.SessionID != agg {
		return errors.New("invalid session id")
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("session name required")
	}
	if p.Mode != Background && p.Mode != Interactive {
		return fmt.Errorf("invalid session mode %q: want background|interactive", p.Mode)
	}
	if !filepath.IsAbs(p.Cwd) {
		return fmt.Errorf("session cwd %q must be absolute", p.Cwd)
	}
	return nil
}

// Open appends session.opened; an open session is rejected with zero writes,
// a closed one is reopened.
func (s Service) Open(p Opened) (Session, error) {
	if s.Store == nil {
		return Session{}, errors.New("nil store")
	}
	if err := validOpened(p, p.SessionID); err != nil {
		return Session{}, err
	}
	var rev uint64
	if log := s.Store.List(AggregateType, p.SessionID); len(log) > 0 {
		cur, err := Replay(log)
		if err != nil {
			return Session{}, err
		}
		if cur.State == Open {
			return Session{}, errors.New("session already open")
		}
		rev = cur.Revision
	}
	b, _ := json.Marshal(p)
	if err := s.Store.Append(rev, events.Event{AggregateType: AggregateType, AggregateID: p.SessionID, Revision: rev + 1, Type: OpenedType, Payload: b}); err != nil {
		return Session{}, err
	}
	return Replay(s.Store.List(AggregateType, p.SessionID))
}

// Close appends session.closed; an unknown or already closed session is
// rejected with zero writes.
func (s Service) Close(id, reason string) (Session, error) {
	if s.Store == nil {
		return Session{}, errors.New("nil store")
	}
	if strings.TrimSpace(id) == "" {
		return Session{}, errors.New("sessionId required")
	}
	if strings.TrimSpace(reason) == "" {
		return Session{}, errors.New("close reason required")
	}
	log := s.Store.List(AggregateType, id)
	if len(log) == 0 {
		return Session{}, errors.New("session not found")
	}
	cur, err := Replay(log)
	if err != nil {
		return Session{}, err
	}
	if cur.State != Open {
		return Session{}, errors.New("session already closed")
	}
	b, _ := json.Marshal(closedPayload{SessionID: id, Reason: reason})
	if err := s.Store.Append(cur.Revision, events.Event{AggregateType: AggregateType, AggregateID: id, Revision: cur.Revision + 1, Type: ClosedType, Payload: b}); err != nil {
		return Session{}, err
	}
	return Replay(s.Store.List(AggregateType, id))
}

// Get replays one session.
func (s Service) Get(id string) (Session, error) { return Replay(s.Store.List(AggregateType, id)) }

// Replay rebuilds one session stream: opened (closed opened)* closed?.
func Replay(log []events.Event) (Session, error) {
	var out Session
	if len(log) == 0 {
		return out, errors.New("session not found")
	}
	for i, e := range log {
		if e.AggregateType != AggregateType || e.Revision != uint64(i+1) {
			return Session{}, errors.New("invalid session stream")
		}
		switch {
		case e.Type == OpenedType && (i == 0 || out.State == Closed):
			var p Opened
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return Session{}, err
			}
			if err := validOpened(p, e.AggregateID); err != nil {
				return Session{}, err
			}
			out = Session{Opened: p, State: Open}
			if !e.CreatedAt.IsZero() {
				out.OpenedAt = e.CreatedAt.UnixMilli()
			}
		case e.Type == ClosedType && out.State == Open:
			var p closedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return Session{}, err
			}
			out.State, out.Reason = Closed, p.Reason
		default:
			return Session{}, errors.New("invalid session event")
		}
		out.Revision = e.Revision
	}
	return out, nil
}
