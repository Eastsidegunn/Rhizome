package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"rhizome/internal/events"
)

type Kind string

const (
	Fact        Kind = "fact"
	Decision    Kind = "decision"
	Preference  Kind = "preference"
	Observation Kind = "observation"
	Hypothesis  Kind = "hypothesis"
	Reference   Kind = "reference"
)

type Memory struct {
	ID                   string
	Kind                 Kind
	Content              string
	SourceType, SourceID string
	Confidence           float64
	GoalID, MissionID    string
	Tags                 []string
	Revision             uint64
	// Supersedes links this replacement to its original; it does not choose a current winner.
	Supersedes string
}
type Service struct{ Store events.Port }
type payload struct {
	ID         string   `json:"id"`
	Kind       Kind     `json:"kind"`
	Content    string   `json:"content"`
	SourceType string   `json:"source_type"`
	SourceID   string   `json:"source_id"`
	Confidence float64  `json:"confidence"`
	GoalID     string   `json:"goal_id"`
	MissionID  string   `json:"mission_id"`
	Tags       []string `json:"tags"`
	Supersedes string   `json:"supersedes,omitempty"`
}

func validate(m Memory) error {
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Content) == "" || strings.TrimSpace(m.SourceType) == "" || strings.TrimSpace(m.SourceID) == "" {
		return fmt.Errorf("memory id, content and source are required")
	}
	switch m.Kind {
	case Fact, Decision, Preference, Observation, Hypothesis, Reference:
	default:
		return fmt.Errorf("unknown memory kind %q", m.Kind)
	}
	if math.IsNaN(m.Confidence) || math.IsInf(m.Confidence, 0) || m.Confidence < 0 || m.Confidence > 1 {
		return fmt.Errorf("confidence must be finite and in [0,1]")
	}
	return nil
}

// Create records FR-RHZ-009 provenance in one append-only event.
func (s Service) Create(m Memory) (Memory, error) {
	if s.Store == nil {
		return Memory{}, fmt.Errorf("nil event store")
	}
	if m.Supersedes != "" {
		return Memory{}, fmt.Errorf("use Supersede to create a replacement")
	}
	return s.appendNew(m, "memory.created")
}

// Supersede implements FR-RHZ-010. Only old.ID is used: persisted history is authoritative.
// Multiple replacements of an original are permitted; the original stream is untouched.
func (s Service) Supersede(old Memory, next Memory) (Memory, error) {
	if s.Store == nil {
		return Memory{}, fmt.Errorf("nil event store")
	}
	if next.ID == old.ID {
		return Memory{}, fmt.Errorf("replacement must have a new id")
	}
	if _, err := Replay(s.Store.List("memory", old.ID)); err != nil {
		return Memory{}, fmt.Errorf("original memory: %w", err)
	}
	if next.Supersedes != "" && next.Supersedes != old.ID {
		return Memory{}, fmt.Errorf("replacement source mismatch")
	}
	next.Supersedes = old.ID
	return s.appendNew(next, "memory.superseded")
}

func (s Service) appendNew(m Memory, typ string) (Memory, error) {
	if err := validate(m); err != nil {
		return Memory{}, err
	}
	p := payload{m.ID, m.Kind, m.Content, m.SourceType, m.SourceID, m.Confidence, m.GoalID, m.MissionID, m.Tags, m.Supersedes}
	raw, err := json.Marshal(p)
	if err != nil {
		return Memory{}, fmt.Errorf("encode memory: %w", err)
	}
	e := events.Event{AggregateType: "memory", AggregateID: m.ID, Revision: 1, Type: typ, Payload: raw}
	// Validate exactly the persisted representation before crossing the append boundary.
	result, err := Replay([]events.Event{e})
	if err != nil {
		return Memory{}, err
	}
	if err = s.Store.Append(0, e); err != nil {
		return Memory{}, err
	}
	return result, nil
}

// Replay requires an already filtered single-aggregate stream. Each supported event
// creates a fresh memory: updates to an existing memory are deliberately unsupported.
func Replay(log []events.Event) (Memory, error) {
	if len(log) == 0 {
		return Memory{}, fmt.Errorf("memory event stream is empty")
	}
	var m Memory
	for i, e := range log {
		if e.AggregateType != "memory" {
			return Memory{}, fmt.Errorf("unexpected aggregate type %q", e.AggregateType)
		}
		if e.Revision != uint64(i)+1 {
			return Memory{}, events.ErrRevisionConflict
		}
		if i > 0 && e.AggregateID != m.ID {
			return Memory{}, fmt.Errorf("mixed memory aggregates")
		}
		if e.Type != "memory.created" && e.Type != "memory.superseded" {
			return Memory{}, fmt.Errorf("unknown memory event %q", e.Type)
		}
		if i > 0 {
			return Memory{}, fmt.Errorf("duplicate memory creation")
		}
		var p payload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return Memory{}, fmt.Errorf("decode memory: %w", err)
		}
		m = Memory{ID: p.ID, Kind: p.Kind, Content: p.Content, SourceType: p.SourceType, SourceID: p.SourceID, Confidence: p.Confidence, GoalID: p.GoalID, MissionID: p.MissionID, Tags: p.Tags, Revision: e.Revision, Supersedes: p.Supersedes}
		if err := validate(m); err != nil {
			return Memory{}, err
		}
		if m.ID != e.AggregateID {
			return Memory{}, fmt.Errorf("memory id does not match aggregate")
		}
		if e.Type == "memory.created" && m.Supersedes != "" {
			return Memory{}, fmt.Errorf("creation has unexpected supersedes link")
		}
		if e.Type == "memory.superseded" && (strings.TrimSpace(m.Supersedes) == "" || m.Supersedes == m.ID) {
			return Memory{}, fmt.Errorf("invalid supersedes link")
		}
	}
	return m, nil
}

// Search reads one snapshot and orders matching memories by ID. Filters are conjunctive.
func (s Service) Search(kind Kind, tag, sourceID string) ([]Memory, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	streams := make(map[string][]events.Event)
	for _, e := range s.Store.All() {
		if e.AggregateType == "memory" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Memory, 0)
	for _, id := range ids {
		m, err := Replay(streams[id])
		if err != nil {
			return nil, fmt.Errorf("memory %q: %w", id, err)
		}
		if kind != "" && m.Kind != kind || sourceID != "" && m.SourceID != sourceID || tag != "" && !contains(m.Tags, tag) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}
func contains(a []string, x string) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}
