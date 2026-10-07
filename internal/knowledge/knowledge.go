package knowledge

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"rhizome/internal/events"
	"rhizome/internal/memory"
)

// Kind is the intentionally small first vocabulary for structured knowledge.
type Kind string

const (
	Concept     Kind = "concept"
	Claim       Kind = "claim"
	Procedure   Kind = "procedure"
	Constraint  Kind = "constraint"
	Assumption  Kind = "assumption"
	FailureMode Kind = "failure_mode"
)

type Status string

const (
	Candidate Status = "candidate"
	Promoted  Status = "promoted"
)

// KnowledgeItem is a projection of one knowledge aggregate. Status is derived
// from the event stream; superseded items are reported by Service.IsSuperseded.
type KnowledgeItem struct {
	ID             string
	Kind           Kind
	Statement      string
	Status         Status
	Confidence     float64
	SourceMemoryID string
	SourceEventIDs []string
	Tags           []string
	Supersedes     string
	ValidFrom      string
	ValidUntil     string
	Revision       uint64
}

type Service struct{ Store events.Port }

type payload struct {
	ID             string   `json:"id"`
	Kind           Kind     `json:"kind"`
	Statement      string   `json:"statement"`
	SourceMemoryID string   `json:"source_memory_id"`
	Confidence     float64  `json:"confidence"`
	SourceEventIDs []string `json:"source_event_ids,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Supersedes     string   `json:"supersedes,omitempty"`
	ValidFrom      string   `json:"valid_from,omitempty"`
	ValidUntil     string   `json:"valid_until,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

func validKind(k Kind) bool {
	switch k {
	case Concept, Claim, Procedure, Constraint, Assumption, FailureMode:
		return true
	default:
		return false
	}
}

func validateItem(i KnowledgeItem) error {
	if strings.TrimSpace(i.ID) == "" || strings.TrimSpace(i.Statement) == "" || strings.TrimSpace(i.SourceMemoryID) == "" {
		return fmt.Errorf("knowledge id, statement and source memory are required")
	}
	if !validKind(i.Kind) {
		return fmt.Errorf("unknown knowledge kind %q", i.Kind)
	}
	if math.IsNaN(i.Confidence) || math.IsInf(i.Confidence, 0) || i.Confidence < 0 || i.Confidence > 1 {
		return fmt.Errorf("confidence must be finite and in [0,1]")
	}
	return nil
}

func cloneStrings(in []string) []string {
	return append([]string(nil), in...)
}

func (s Service) validateSource(id string) error {
	if s.Store == nil {
		return fmt.Errorf("nil event store")
	}
	if _, err := memory.Replay(s.Store.List("memory", id)); err != nil {
		return fmt.Errorf("source memory: %w", err)
	}
	return nil
}

// Create records a candidate. The persisted representation is replay-checked
// before the append boundary (FR-RHZ-039, FR-RHZ-041).
func (s Service) Create(i KnowledgeItem) (KnowledgeItem, error) {
	if s.Store == nil {
		return KnowledgeItem{}, fmt.Errorf("nil event store")
	}
	if i.Supersedes != "" || i.Status != "" || i.Revision != 0 {
		return KnowledgeItem{}, fmt.Errorf("invalid candidate input")
	}
	if err := validateItem(i); err != nil {
		return KnowledgeItem{}, err
	}
	if err := s.validateSource(i.SourceMemoryID); err != nil {
		return KnowledgeItem{}, err
	}
	p := payload{ID: i.ID, Kind: i.Kind, Statement: i.Statement, SourceMemoryID: i.SourceMemoryID, Confidence: i.Confidence, SourceEventIDs: cloneStrings(i.SourceEventIDs), Tags: cloneStrings(i.Tags), ValidFrom: i.ValidFrom, ValidUntil: i.ValidUntil}
	raw, err := json.Marshal(p)
	if err != nil {
		return KnowledgeItem{}, fmt.Errorf("encode knowledge: %w", err)
	}
	e := events.Event{AggregateType: "knowledge", AggregateID: i.ID, Revision: 1, Type: "knowledge.candidate", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return KnowledgeItem{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return KnowledgeItem{}, err
	}
	return Replay(s.Store.List("knowledge", i.ID))
}

// Promote appends exactly one promotion to a candidate stream (FR-RHZ-040).
func (s Service) Promote(id, reason string) (KnowledgeItem, error) {
	if s.Store == nil {
		return KnowledgeItem{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(reason) == "" {
		return KnowledgeItem{}, fmt.Errorf("promotion reason required")
	}
	item, err := Replay(s.Store.List("knowledge", id))
	if err != nil {
		return KnowledgeItem{}, err
	}
	if item.Status != Candidate {
		return KnowledgeItem{}, fmt.Errorf("knowledge is not promotable")
	}
	p := payload{ID: item.ID, Kind: item.Kind, Statement: item.Statement, SourceMemoryID: item.SourceMemoryID, Confidence: item.Confidence, Reason: reason}
	raw, err := json.Marshal(p)
	if err != nil {
		return KnowledgeItem{}, fmt.Errorf("encode promotion: %w", err)
	}
	e := events.Event{AggregateType: "knowledge", AggregateID: id, Revision: 2, Type: "knowledge.promoted", Payload: raw}
	if err := s.Store.Append(1, e); err != nil {
		return KnowledgeItem{}, err
	}
	return Replay(s.Store.List("knowledge", id))
}

// Supersede creates a new knowledge aggregate and leaves the original stream
// untouched, following the Memory aggregate's replacement rule.
func (s Service) Supersede(old KnowledgeItem, next KnowledgeItem) (KnowledgeItem, error) {
	if s.Store == nil {
		return KnowledgeItem{}, fmt.Errorf("nil event store")
	}
	if old.ID == "" || next.ID == "" || old.ID == next.ID {
		return KnowledgeItem{}, fmt.Errorf("replacement must have a new id")
	}
	if _, err := Replay(s.Store.List("knowledge", old.ID)); err != nil {
		return KnowledgeItem{}, fmt.Errorf("original knowledge: %w", err)
	}
	if next.Supersedes != "" && next.Supersedes != old.ID {
		return KnowledgeItem{}, fmt.Errorf("replacement source mismatch")
	}
	if next.Status != "" || next.Revision != 0 {
		return KnowledgeItem{}, fmt.Errorf("invalid replacement input")
	}
	next.Supersedes = old.ID
	if err := validateItem(next); err != nil {
		return KnowledgeItem{}, err
	}
	if err := s.validateSource(next.SourceMemoryID); err != nil {
		return KnowledgeItem{}, err
	}
	p := payload{ID: next.ID, Kind: next.Kind, Statement: next.Statement, SourceMemoryID: next.SourceMemoryID, Confidence: next.Confidence, SourceEventIDs: cloneStrings(next.SourceEventIDs), Tags: cloneStrings(next.Tags), Supersedes: old.ID, ValidFrom: next.ValidFrom, ValidUntil: next.ValidUntil}
	raw, err := json.Marshal(p)
	if err != nil {
		return KnowledgeItem{}, fmt.Errorf("encode replacement: %w", err)
	}
	e := events.Event{AggregateType: "knowledge", AggregateID: next.ID, Revision: 1, Type: "knowledge.superseded", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return KnowledgeItem{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return KnowledgeItem{}, err
	}
	return Replay(s.Store.List("knowledge", next.ID))
}

// Replay rebuilds one knowledge aggregate and rejects malformed streams.
func Replay(log []events.Event) (KnowledgeItem, error) {
	if len(log) == 0 {
		return KnowledgeItem{}, fmt.Errorf("knowledge event stream is empty")
	}
	var item KnowledgeItem
	var aggregateID string
	for n, e := range log {
		if e.AggregateType != "knowledge" {
			return KnowledgeItem{}, fmt.Errorf("unexpected aggregate type %q", e.AggregateType)
		}
		if aggregateID == "" {
			aggregateID = e.AggregateID
		} else if e.AggregateID != aggregateID {
			return KnowledgeItem{}, fmt.Errorf("mixed knowledge aggregates")
		}
		if e.AggregateID == "" || e.Revision != uint64(n)+1 {
			return KnowledgeItem{}, events.ErrRevisionConflict
		}
		var p payload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return KnowledgeItem{}, fmt.Errorf("decode knowledge: %w", err)
		}
		switch n {
		case 0:
			if e.Type != "knowledge.candidate" && e.Type != "knowledge.superseded" {
				return KnowledgeItem{}, fmt.Errorf("invalid first knowledge event %q", e.Type)
			}
			if p.ID != e.AggregateID {
				return KnowledgeItem{}, fmt.Errorf("knowledge id does not match aggregate")
			}
			item = KnowledgeItem{ID: p.ID, Kind: p.Kind, Statement: p.Statement, Status: Candidate, Confidence: p.Confidence, SourceMemoryID: p.SourceMemoryID, SourceEventIDs: cloneStrings(p.SourceEventIDs), Tags: cloneStrings(p.Tags), Supersedes: p.Supersedes, ValidFrom: p.ValidFrom, ValidUntil: p.ValidUntil, Revision: e.Revision}
			if e.Type == "knowledge.candidate" && item.Supersedes != "" {
				return KnowledgeItem{}, fmt.Errorf("candidate has unexpected supersedes link")
			}
			if e.Type == "knowledge.superseded" && (item.Supersedes == "" || item.Supersedes == item.ID) {
				return KnowledgeItem{}, fmt.Errorf("invalid supersedes link")
			}
			if err := validateItem(item); err != nil {
				return KnowledgeItem{}, err
			}
		case 1:
			if e.Type != "knowledge.promoted" || p.ID != item.ID || p.Kind != item.Kind || p.Statement != item.Statement || p.SourceMemoryID != item.SourceMemoryID || p.Confidence != item.Confidence || strings.TrimSpace(p.Reason) == "" {
				return KnowledgeItem{}, fmt.Errorf("invalid promotion")
			}
			item.Status = Promoted
			item.Revision = e.Revision
		default:
			return KnowledgeItem{}, fmt.Errorf("duplicate or unknown knowledge event %q", e.Type)
		}
	}
	return item, nil
}

// IsSuperseded reports whether another valid knowledge aggregate links to id.
// It does not mutate the original item's projection.
func (s Service) IsSuperseded(id string) (bool, error) {
	if s.Store == nil {
		return false, fmt.Errorf("nil event store")
	}
	if _, err := Replay(s.Store.List("knowledge", id)); err != nil {
		return false, err
	}
	for _, e := range s.Store.All() {
		if e.AggregateType != "knowledge" || e.AggregateID == id {
			continue
		}
		item, err := Replay(s.Store.List("knowledge", e.AggregateID))
		if err != nil {
			return false, fmt.Errorf("knowledge %q: %w", e.AggregateID, err)
		}
		if item.Supersedes == id {
			return true, nil
		}
	}
	return false, nil
}

// Search uses one All snapshot and returns stable ID order. A corrupt stream
// is surfaced instead of silently omitted.
func (s Service) Search(kind Kind, sourceMemoryID, tag string) ([]KnowledgeItem, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	streams := make(map[string][]events.Event)
	for _, e := range s.Store.All() {
		if e.AggregateType == "knowledge" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]KnowledgeItem, 0)
	for _, id := range ids {
		item, err := Replay(streams[id])
		if err != nil {
			return nil, fmt.Errorf("knowledge %q: %w", id, err)
		}
		if kind != "" && item.Kind != kind || sourceMemoryID != "" && item.SourceMemoryID != sourceMemoryID || tag != "" && !hasTag(item.Tags, tag) {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
