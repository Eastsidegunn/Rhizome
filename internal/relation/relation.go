package relation

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
)

type Type string

const (
	PrerequisiteOf Type = "prerequisite_of"
	DerivedFrom    Type = "derived_from"
	Supports       Type = "supports"
	Contradicts    Type = "contradicts"
	AppliesWhen    Type = "applies_when"
	FailsWhen      Type = "fails_when"
)

type Relation struct {
	ID              string
	From            string
	Type            Type
	To              string
	SourceMemoryIDs []string
	Confidence      float64
	Revision        uint64
}

type Service struct{ Store events.Port }

type payload struct {
	ID              string   `json:"relation_id"`
	From            string   `json:"from"`
	Type            Type     `json:"type"`
	To              string   `json:"to"`
	SourceMemoryIDs []string `json:"source_memory_ids"`
	Confidence      float64  `json:"confidence"`
}

func validType(t Type) bool {
	switch t {
	case PrerequisiteOf, DerivedFrom, Supports, Contradicts, AppliesWhen, FailsWhen:
		return true
	default:
		return false
	}
}

func validate(r Relation) error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.From) == "" || strings.TrimSpace(r.To) == "" {
		return fmt.Errorf("relation id and endpoints are required")
	}
	if r.From == r.To {
		return fmt.Errorf("relation cannot reference itself")
	}
	if !validType(r.Type) {
		return fmt.Errorf("unknown relation type %q", r.Type)
	}
	if len(r.SourceMemoryIDs) == 0 {
		return fmt.Errorf("source memory is required")
	}
	if math.IsNaN(r.Confidence) || math.IsInf(r.Confidence, 0) || r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("confidence must be finite and in [0,1]")
	}
	return nil
}

func cloneStrings(in []string) []string { return append([]string(nil), in...) }

func (s Service) validateReferences(r Relation) error {
	if s.Store == nil {
		return fmt.Errorf("nil event store")
	}
	if _, err := knowledge.Replay(s.Store.List("knowledge", r.From)); err != nil {
		return fmt.Errorf("from knowledge: %w", err)
	}
	if _, err := knowledge.Replay(s.Store.List("knowledge", r.To)); err != nil {
		return fmt.Errorf("to knowledge: %w", err)
	}
	for _, id := range r.SourceMemoryIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("source memory id is required")
		}
		if _, err := memory.Replay(s.Store.List("memory", id)); err != nil {
			return fmt.Errorf("source memory %q: %w", id, err)
		}
	}
	return nil
}

func (s Service) Create(r Relation) (Relation, error) {
	if s.Store == nil {
		return Relation{}, fmt.Errorf("nil event store")
	}
	if r.Revision != 0 {
		return Relation{}, fmt.Errorf("invalid relation input")
	}
	if err := validate(r); err != nil {
		return Relation{}, err
	}
	if err := s.validateReferences(r); err != nil {
		return Relation{}, err
	}
	p := payload{ID: r.ID, From: r.From, Type: r.Type, To: r.To, SourceMemoryIDs: cloneStrings(r.SourceMemoryIDs), Confidence: r.Confidence}
	raw, err := json.Marshal(p)
	if err != nil {
		return Relation{}, fmt.Errorf("encode relation: %w", err)
	}
	e := events.Event{AggregateType: "relation", AggregateID: r.ID, Revision: 1, Type: "relation.created", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return Relation{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return Relation{}, err
	}
	return Replay(s.Store.List("relation", r.ID))
}

func Replay(log []events.Event) (Relation, error) {
	if len(log) == 0 {
		return Relation{}, fmt.Errorf("relation event stream is empty")
	}
	if len(log) != 1 {
		return Relation{}, fmt.Errorf("duplicate or unknown relation event")
	}
	e := log[0]
	if e.AggregateType != "relation" || e.AggregateID == "" || e.Revision != 1 || e.Type != "relation.created" {
		return Relation{}, fmt.Errorf("invalid relation event")
	}
	var p payload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Relation{}, fmt.Errorf("decode relation: %w", err)
	}
	r := Relation{ID: p.ID, From: p.From, Type: p.Type, To: p.To, SourceMemoryIDs: cloneStrings(p.SourceMemoryIDs), Confidence: p.Confidence, Revision: e.Revision}
	if p.ID != e.AggregateID {
		return Relation{}, fmt.Errorf("relation id does not match aggregate")
	}
	if err := validate(r); err != nil {
		return Relation{}, err
	}
	return r, nil
}

func (s Service) query(endpoint string, from bool, filters []Type) ([]Relation, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	allowed := make(map[Type]bool)
	for _, f := range filters {
		if !validType(f) {
			return nil, fmt.Errorf("unknown relation type %q", f)
		}
		allowed[f] = true
	}
	streams := make(map[string][]events.Event)
	for _, e := range s.Store.All() {
		if e.AggregateType == "relation" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Relation, 0)
	for _, id := range ids {
		r, err := Replay(streams[id])
		if err != nil {
			return nil, fmt.Errorf("relation %q: %w", id, err)
		}
		matches := (from && r.From == endpoint) || (!from && r.To == endpoint)
		if !matches || len(allowed) > 0 && !allowed[r.Type] {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// From returns relations whose from endpoint is itemID, sorted by relation ID.
func (s Service) From(itemID string, typeFilter ...Type) ([]Relation, error) {
	if strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("item id is required")
	}
	return s.query(itemID, true, typeFilter)
}

// To returns relations whose to endpoint is itemID, sorted by relation ID.
func (s Service) To(itemID string, typeFilter ...Type) ([]Relation, error) {
	if strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("item id is required")
	}
	return s.query(itemID, false, typeFilter)
}

func (s Service) Contradicts(itemID string) ([]Relation, error) {
	return s.To(itemID, Contradicts)
}
