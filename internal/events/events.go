package events

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type Event struct {
	Sequence      uint64          `json:"sequence"`
	ID            string          `json:"id"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Revision      uint64          `json:"revision"`
	Type          string          `json:"type"`
	Payload       json.RawMessage `json:"payload"`
	CausationID   string          `json:"causation_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	CreatedAt     time.Time       `json:"created_at"`
}

var (
	ErrRevisionConflict = errors.New("aggregate revision conflict")
	ErrPoisoned         = errors.New("journal poisoned: restart required")
)

// PoisonLatch is optionally implemented by a durable Port. In-memory stores
// do not implement it and are therefore treated as never poisoned.
type PoisonLatch interface {
	Poisoned() bool
}

// Port is the minimal append-only event boundary consumed by domain services.
type Port interface {
	Append(expected uint64, event Event) error
	List(aggregateType, aggregateID string) []Event
	All() []Event
}

// aggKey identifies an aggregate stream (type, id) for index lookups.
type aggKey struct {
	t  string
	id string
}

type Store struct {
	mu     sync.Mutex
	events []Event
	// Derived indices (RHZ-054, FR-RHZ-084). Rebuilt from s.events only;
	// never a source of truth. Mutated under mu, and only AFTER the append to
	// s.events has succeeded, so a rejected append leaves them untouched (I5).
	lastRev   map[aggKey]uint64 // aggregate key -> last appended revision
	positions map[aggKey][]int  // aggregate key -> indices into s.events, in append order
}

var _ Port = (*Store)(nil)

func (s *Store) ensureIndex() {
	if s.lastRev == nil {
		s.lastRev = make(map[aggKey]uint64)
	}
	if s.positions == nil {
		s.positions = make(map[aggKey][]int)
	}
}

func (s *Store) Append(expected uint64, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureIndex()
	key := aggKey{event.AggregateType, event.AggregateID}
	// Revisions are appended strictly as expected+1, so the last appended
	// revision for a key is also its maximum — identical to scanning s.events.
	current := s.lastRev[key]
	if current != expected {
		return ErrRevisionConflict
	}
	if event.Revision != expected+1 {
		return ErrRevisionConflict
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	s.events = append(s.events, event)
	pos := len(s.events) - 1
	s.events[pos].Sequence = uint64(len(s.events))
	// Index update happens only here, after validation and the slice append
	// have both succeeded.
	s.lastRev[key] = event.Revision
	s.positions[key] = append(s.positions[key], pos)
	return nil
}

// All returns the append order, including every aggregate.
func (s *Store) All() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	for i := range out {
		out[i].Payload = append(json.RawMessage(nil), out[i].Payload...)
	}
	return out
}

func (s *Store) List(aggregateType, aggregateID string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	idxs := s.positions[aggKey{aggregateType, aggregateID}]
	out := make([]Event, 0, len(idxs))
	for _, i := range idxs {
		e := s.events[i]
		e.Payload = append(json.RawMessage(nil), e.Payload...)
		out = append(out, e)
	}
	return out
}

// Len reports the total number of appended events. O(1).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// Revision reports the last appended revision for an aggregate key, 0 if none. O(1).
func (s *Store) Revision(aggregateType, aggregateID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRev[aggKey{aggregateType, aggregateID}]
}

// AppendRevision restores a validated event while preserving its sequence.
func (s *Store) AppendRevision(event Event) error {
	if event.AggregateType == "" || event.AggregateID == "" || event.Type == "" {
		return ErrRevisionConflict
	}
	if event.Sequence == 0 {
		return ErrRevisionConflict
	}
	if event.Sequence != uint64(s.Len()+1) {
		return ErrRevisionConflict
	}
	return s.Append(s.Revision(event.AggregateType, event.AggregateID), event)
}
