package trace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rhizome/internal/decision"
	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/projector"
	"rhizome/internal/relation"
	"rhizome/internal/source"
)

type Trace struct {
	ID                   string
	Query                string
	RetrievedItemIDs     []string
	UsedItemIDs          []string
	RejectedItemIDs      []string
	TraversedRelationIDs []string
	OpenedSourceIDs      []string
	Outcome              string
	GoalID               string
	MissionID            string
	DecisionID           string
	Revision             uint64
}

type Service struct{ Store events.Port }

type payload struct {
	ID                   string   `json:"trace_id"`
	Query                string   `json:"query"`
	RetrievedItemIDs     []string `json:"retrieved_item_ids"`
	UsedItemIDs          []string `json:"used_item_ids"`
	RejectedItemIDs      []string `json:"rejected_item_ids"`
	TraversedRelationIDs []string `json:"traversed_relation_ids"`
	OpenedSourceIDs      []string `json:"opened_source_ids"`
	Outcome              string   `json:"outcome,omitempty"`
	GoalID               string   `json:"goal_id,omitempty"`
	MissionID            string   `json:"mission_id,omitempty"`
	DecisionID           string   `json:"decision_id,omitempty"`
}

func clone(in []string) []string { return append([]string(nil), in...) }

func validateList(name string, values []string, required bool) error {
	if required && len(values) == 0 {
		return fmt.Errorf("%s is required", name)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s contains blank id", name)
		}
		if seen[value] {
			return fmt.Errorf("%s contains duplicate id %q", name, value)
		}
		seen[value] = true
	}
	return nil
}

func membership(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func validateShape(t Trace) error {
	if strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.Query) == "" {
		return fmt.Errorf("trace id and query are required")
	}
	if err := validateList("retrieved items", t.RetrievedItemIDs, true); err != nil {
		return err
	}
	for name, values := range map[string][]string{
		"used items":          t.UsedItemIDs,
		"rejected items":      t.RejectedItemIDs,
		"traversed relations": t.TraversedRelationIDs,
		"opened sources":      t.OpenedSourceIDs,
	} {
		if err := validateList(name, values, false); err != nil {
			return err
		}
	}
	retrieved := membership(t.RetrievedItemIDs)
	used := membership(t.UsedItemIDs)
	rejected := membership(t.RejectedItemIDs)
	for id := range used {
		if !retrieved[id] {
			return fmt.Errorf("used item %q was not retrieved", id)
		}
		if rejected[id] {
			return fmt.Errorf("item %q is both used and rejected", id)
		}
	}
	for id := range rejected {
		if !retrieved[id] {
			return fmt.Errorf("rejected item %q was not retrieved", id)
		}
	}
	return nil
}

func (s Service) validateReferences(t Trace) error {
	if s.Store == nil {
		return fmt.Errorf("nil event store")
	}
	for _, id := range t.RetrievedItemIDs {
		if _, err := knowledge.Replay(s.Store.List("knowledge", id)); err != nil {
			return fmt.Errorf("knowledge %q: %w", id, err)
		}
	}
	for _, id := range t.TraversedRelationIDs {
		if _, err := relation.Replay(s.Store.List("relation", id)); err != nil {
			return fmt.Errorf("relation %q: %w", id, err)
		}
	}
	for _, id := range t.OpenedSourceIDs {
		if _, err := source.Replay(s.Store.List("source", id)); err != nil {
			return fmt.Errorf("source %q: %w", id, err)
		}
	}
	if t.GoalID != "" {
		if _, err := projector.ReplayGoal(s.Store.List("goal", t.GoalID)); err != nil {
			return fmt.Errorf("goal %q: %w", t.GoalID, err)
		}
	}
	if t.MissionID != "" {
		if _, err := projector.ReplayMission(s.Store.List("mission", t.MissionID)); err != nil {
			return fmt.Errorf("mission %q: %w", t.MissionID, err)
		}
	}
	if t.DecisionID != "" {
		d, err := decision.Replay(s.Store.List("decision", t.DecisionID))
		if err != nil {
			return fmt.Errorf("decision %q: %w", t.DecisionID, err)
		}
		for _, evidence := range d.Evidence {
			if evidence.SourceType != "memory" {
				continue
			}
			found := false
			for _, itemID := range t.UsedItemIDs {
				item, replayErr := knowledge.Replay(s.Store.List("knowledge", itemID))
				if replayErr != nil {
					return fmt.Errorf("used knowledge %q: %w", itemID, replayErr)
				}
				if item.SourceMemoryID == evidence.SourceID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("decision memory evidence %q is not represented by used knowledge", evidence.SourceID)
			}
		}
	}
	return nil
}

func (s Service) Create(t Trace) (Trace, error) {
	if s.Store == nil {
		return Trace{}, fmt.Errorf("nil event store")
	}
	if t.Revision != 0 {
		return Trace{}, fmt.Errorf("invalid trace input")
	}
	if err := validateShape(t); err != nil {
		return Trace{}, err
	}
	if err := s.validateReferences(t); err != nil {
		return Trace{}, err
	}
	p := payload{ID: t.ID, Query: t.Query, RetrievedItemIDs: clone(t.RetrievedItemIDs), UsedItemIDs: clone(t.UsedItemIDs), RejectedItemIDs: clone(t.RejectedItemIDs), TraversedRelationIDs: clone(t.TraversedRelationIDs), OpenedSourceIDs: clone(t.OpenedSourceIDs), Outcome: t.Outcome, GoalID: t.GoalID, MissionID: t.MissionID, DecisionID: t.DecisionID}
	raw, err := json.Marshal(p)
	if err != nil {
		return Trace{}, fmt.Errorf("encode trace: %w", err)
	}
	e := events.Event{AggregateType: "trace", AggregateID: t.ID, Revision: 1, Type: "trace.recorded", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return Trace{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return Trace{}, err
	}
	return Replay(s.Store.List("trace", t.ID))
}

func Replay(log []events.Event) (Trace, error) {
	if len(log) == 0 {
		return Trace{}, fmt.Errorf("trace event stream is empty")
	}
	if len(log) != 1 {
		return Trace{}, fmt.Errorf("duplicate or unknown trace event")
	}
	e := log[0]
	if e.AggregateType != "trace" || e.AggregateID == "" || e.Revision != 1 || e.Type != "trace.recorded" {
		return Trace{}, fmt.Errorf("invalid trace event")
	}
	var p payload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Trace{}, fmt.Errorf("decode trace: %w", err)
	}
	t := Trace{ID: p.ID, Query: p.Query, RetrievedItemIDs: clone(p.RetrievedItemIDs), UsedItemIDs: clone(p.UsedItemIDs), RejectedItemIDs: clone(p.RejectedItemIDs), TraversedRelationIDs: clone(p.TraversedRelationIDs), OpenedSourceIDs: clone(p.OpenedSourceIDs), Outcome: p.Outcome, GoalID: p.GoalID, MissionID: p.MissionID, DecisionID: p.DecisionID, Revision: e.Revision}
	if t.ID != e.AggregateID {
		return Trace{}, fmt.Errorf("trace id does not match aggregate")
	}
	if err := validateShape(t); err != nil {
		return Trace{}, err
	}
	return t, nil
}

func (s Service) query(field, id string) ([]Trace, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("query id is required")
	}
	streams := make(map[string][]events.Event)
	for _, e := range s.Store.All() {
		if e.AggregateType == "trace" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for traceID := range streams {
		ids = append(ids, traceID)
	}
	sort.Strings(ids)
	out := make([]Trace, 0)
	for _, traceID := range ids {
		t, err := Replay(streams[traceID])
		if err != nil {
			return nil, fmt.Errorf("trace %q: %w", traceID, err)
		}
		if (field == "mission" && t.MissionID == id) || (field == "decision" && t.DecisionID == id) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s Service) ByMission(id string) ([]Trace, error)  { return s.query("mission", id) }
func (s Service) ByDecision(id string) ([]Trace, error) { return s.query("decision", id) }
