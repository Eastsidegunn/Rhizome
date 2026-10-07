package procedure

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
)

// Step is one node of the procedure's step DAG (RHZ-063, FR-RHZ-092): After
// names prerequisite step IDs, NeedsGate marks a human gate (recorded in
// stage 1, acted on later). The DAG replaced the old []string steps while the
// aggregate was dormant (0 events) — no legacy payloads exist.
//
// Recommendation (RHZ-083, FR-RHZ-114) is the template-provided
// recommendation for the gate question assembly.Run asks on a NeedsGate step.
// Additive: it is omitted from the payload when empty, so procedure.created
// events written before RHZ-083 replay (and re-encode) byte-identically.
type Step struct {
	ID             string
	Action         string
	After          []string
	NeedsGate      bool
	Recommendation string
}

type Procedure struct {
	ID                string
	SourceKnowledgeID string
	Trigger           string
	Preconditions     []string
	Steps             []Step
	SuccessConditions []string
	FailureModes      []string
	RecoverySteps     []string
	Revision          uint64
}

type Service struct{ Store events.Port }

type stepPayload struct {
	ID             string   `json:"id"`
	Action         string   `json:"action"`
	After          []string `json:"after"`
	NeedsGate      bool     `json:"needs_gate"`
	Recommendation string   `json:"recommendation,omitempty"` // RHZ-083 (FR-RHZ-114), additive
}

type payload struct {
	ID                string        `json:"id"`
	SourceKnowledgeID string        `json:"source_knowledge_id"`
	Trigger           string        `json:"trigger"`
	Preconditions     []string      `json:"preconditions"`
	Steps             []stepPayload `json:"steps"`
	SuccessConditions []string      `json:"success_conditions"`
	FailureModes      []string      `json:"failure_modes"`
	RecoverySteps     []string      `json:"recovery_steps"`
}

func cloneStrings(in []string) []string { return append([]string(nil), in...) }

func cloneSteps(in []Step) []Step {
	out := make([]Step, 0, len(in))
	for _, s := range in {
		out = append(out, Step{ID: s.ID, Action: s.Action, After: cloneStrings(s.After), NeedsGate: s.NeedsGate, Recommendation: s.Recommendation})
	}
	return out
}

func stepsToPayload(in []Step) []stepPayload {
	out := make([]stepPayload, 0, len(in))
	for _, s := range in {
		out = append(out, stepPayload{ID: s.ID, Action: s.Action, After: cloneStrings(s.After), NeedsGate: s.NeedsGate, Recommendation: s.Recommendation})
	}
	return out
}

func stepsFromPayload(in []stepPayload) []Step {
	out := make([]Step, 0, len(in))
	for _, s := range in {
		out = append(out, Step{ID: s.ID, Action: s.Action, After: cloneStrings(s.After), NeedsGate: s.NeedsGate, Recommendation: s.Recommendation})
	}
	return out
}

func validateList(name string, values []string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s contains a blank item", name)
		}
	}
	return nil
}

func validate(p Procedure) error {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.SourceKnowledgeID) == "" {
		return fmt.Errorf("procedure id and source knowledge are required")
	}
	if strings.TrimSpace(p.Trigger) == "" {
		return fmt.Errorf("trigger is required")
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}
	ids := map[string]bool{}
	for _, step := range p.Steps {
		if strings.TrimSpace(step.ID) == "" || strings.TrimSpace(step.Action) == "" {
			return fmt.Errorf("step id and action are required")
		}
		if ids[step.ID] {
			return fmt.Errorf("duplicate step id %q", step.ID)
		}
		ids[step.ID] = true
	}
	for _, step := range p.Steps {
		for _, after := range step.After {
			if after == step.ID {
				return fmt.Errorf("step %q depends on itself", step.ID)
			}
			if !ids[after] {
				return fmt.Errorf("step %q depends on unknown step %q", step.ID, after)
			}
		}
	}
	// 사이클은 생성 시점에 거부한다 — runner가 영원히 위상 정렬 가능하도록
	// (command는 소비자가 거부할 구조를 append하지 않는다).
	if _, err := TopoOrder(p.Steps); err != nil {
		return err
	}
	for name, values := range map[string][]string{
		"preconditions":      p.Preconditions,
		"success_conditions": p.SuccessConditions,
		"failure_modes":      p.FailureModes,
		"recovery_steps":     p.RecoverySteps,
	} {
		if err := validateList(name, values); err != nil {
			return err
		}
	}
	return nil
}

// TopoOrder returns the steps in dependency order (prerequisites first).
// Deterministic: ready steps are taken in lexicographic ID order. A cycle is
// an error — Create rejects it, so a stored procedure always orders.
func TopoOrder(steps []Step) ([]Step, error) {
	byID := map[string]Step{}
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for _, s := range steps {
		byID[s.ID] = s
		indegree[s.ID] = len(s.After)
		for _, after := range s.After {
			dependents[after] = append(dependents[after], s.ID)
		}
	}
	ready := []string{}
	for id, n := range indegree {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	out := make([]Step, 0, len(steps))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, byID[id])
		next := []string{}
		for _, d := range dependents[id] {
			indegree[d]--
			if indegree[d] == 0 {
				next = append(next, d)
			}
		}
		sort.Strings(next)
		ready = append(ready, next...)
		sort.Strings(ready)
	}
	if len(out) != len(steps) {
		return nil, fmt.Errorf("step dependencies contain a cycle")
	}
	return out, nil
}

func (s Service) validateSource(id string) error {
	if s.Store == nil {
		return fmt.Errorf("nil event store")
	}
	k, err := knowledge.Replay(s.Store.List("knowledge", id))
	if err != nil {
		return fmt.Errorf("source knowledge: %w", err)
	}
	if k.Kind != knowledge.Procedure {
		return fmt.Errorf("source knowledge is not a procedure")
	}
	return nil
}

// Create records one procedure.created event after validating the source
// KnowledgeItem and the exact persisted representation (FR-RHZ-044/045).
func (s Service) Create(p Procedure) (Procedure, error) {
	if s.Store == nil {
		return Procedure{}, fmt.Errorf("nil event store")
	}
	if p.Revision != 0 {
		return Procedure{}, fmt.Errorf("invalid procedure input")
	}
	if err := validate(p); err != nil {
		return Procedure{}, err
	}
	if err := s.validateSource(p.SourceKnowledgeID); err != nil {
		return Procedure{}, err
	}
	raw, err := json.Marshal(payload{ID: p.ID, SourceKnowledgeID: p.SourceKnowledgeID, Trigger: p.Trigger, Preconditions: cloneStrings(p.Preconditions), Steps: stepsToPayload(p.Steps), SuccessConditions: cloneStrings(p.SuccessConditions), FailureModes: cloneStrings(p.FailureModes), RecoverySteps: cloneStrings(p.RecoverySteps)})
	if err != nil {
		return Procedure{}, fmt.Errorf("encode procedure: %w", err)
	}
	e := events.Event{AggregateType: "procedure", AggregateID: p.ID, Revision: 1, Type: "procedure.created", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return Procedure{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return Procedure{}, err
	}
	return Replay(s.Store.List("procedure", p.ID))
}

func Replay(log []events.Event) (Procedure, error) {
	if len(log) == 0 {
		return Procedure{}, fmt.Errorf("procedure event stream is empty")
	}
	if len(log) != 1 {
		return Procedure{}, fmt.Errorf("duplicate or unknown procedure event")
	}
	e := log[0]
	if e.AggregateType != "procedure" || e.AggregateID == "" || e.Revision != 1 || e.Type != "procedure.created" {
		return Procedure{}, fmt.Errorf("invalid procedure event")
	}
	var p payload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Procedure{}, fmt.Errorf("decode procedure: %w", err)
	}
	if p.ID != e.AggregateID {
		return Procedure{}, fmt.Errorf("procedure id does not match aggregate")
	}
	result := Procedure{ID: p.ID, SourceKnowledgeID: p.SourceKnowledgeID, Trigger: p.Trigger, Preconditions: cloneStrings(p.Preconditions), Steps: stepsFromPayload(p.Steps), SuccessConditions: cloneStrings(p.SuccessConditions), FailureModes: cloneStrings(p.FailureModes), RecoverySteps: cloneStrings(p.RecoverySteps), Revision: e.Revision}
	if err := validate(result); err != nil {
		return Procedure{}, err
	}
	return result, nil
}

func (s Service) Get(id string) (Procedure, error) {
	if s.Store == nil {
		return Procedure{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(id) == "" {
		return Procedure{}, fmt.Errorf("procedure id is required")
	}
	return Replay(s.Store.List("procedure", id))
}

// BySourceKnowledge returns procedures in deterministic procedure ID order.
func (s Service) BySourceKnowledge(knowledgeID string) ([]Procedure, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(knowledgeID) == "" {
		return nil, fmt.Errorf("knowledge id is required")
	}
	streams := make(map[string][]events.Event)
	for _, e := range s.Store.All() {
		if e.AggregateType == "procedure" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Procedure, 0)
	for _, id := range ids {
		p, err := Replay(streams[id])
		if err != nil {
			return nil, fmt.Errorf("procedure %q: %w", id, err)
		}
		if p.SourceKnowledgeID == knowledgeID {
			out = append(out, p)
		}
	}
	return out, nil
}
