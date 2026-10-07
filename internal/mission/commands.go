package mission

import (
	"encoding/json"
	"fmt"
	"strings"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

type Service struct{ Store events.Port }

func (s Service) CreateGoal(id, description, success, policy string) (domain.Goal, error) { // FR-RHZ-006
	if s.Store == nil {
		return domain.Goal{}, fmt.Errorf("nil event store")
	}
	if _, err := domain.NewGoal(id, description, success, policy); err != nil {
		return domain.Goal{}, err
	}
	p, _ := json.Marshal(struct{ ID, Description, Success, PolicyRef string }{id, description, success, policy})
	if err := s.Store.Append(0, events.Event{AggregateType: "goal", AggregateID: id, Revision: 1, Type: "goal.created", Payload: p}); err != nil {
		return domain.Goal{}, err
	}
	return projector.ReplayGoal(s.Store.List("goal", id))
}

func (s Service) TransitionGoal(id string, expected uint64, to domain.GoalState) (domain.Goal, error) { // FR-RHZ-006
	if s.Store == nil {
		return domain.Goal{}, fmt.Errorf("nil event store")
	}
	g, err := projector.ReplayGoal(s.Store.List("goal", id))
	if err != nil {
		return domain.Goal{}, err
	}
	if !goalTransitionValid(g.State, to) {
		return domain.Goal{}, domain.ErrInvalidState
	}
	if g.Revision != expected {
		return domain.Goal{}, events.ErrRevisionConflict
	}
	if to == domain.GoalAchieved || to == domain.GoalFailed {
		return domain.Goal{}, domain.ErrInvalidState
	}
	if !goalTransitionValid(g.State, to) {
		return domain.Goal{}, domain.ErrInvalidState
	}
	p, _ := json.Marshal(struct{ To domain.GoalState }{to})
	if err = s.Store.Append(expected, events.Event{AggregateType: "goal", AggregateID: id, Revision: expected + 1, Type: "goal.transitioned", Payload: p}); err != nil {
		return domain.Goal{}, err
	}
	return projector.ReplayGoal(s.Store.List("goal", id))
}

// UpdateGoal edits a goal's description and/or success criterion (RHZ-087,
// FR-RHZ-117) as one append-only goal.updated event on the goal stream. The
// ID is immutable (goal-<name> stays; a "rename" is a description change) and
// the state is not touched. At least one of description/success must be
// non-blank; values are stored trimmed. Every guard runs before the append so
// a rejected update never lands an event replay would refuse: both fields
// blank, a terminal goal (achieved/failed/cancelled → ErrInvalidState) and a
// revision mismatch (ErrRevisionConflict) all leave the journal unchanged.
// Identical values (nothing would change) are accepted with zero writes, so a
// resubmit is idempotent and deterministic. Only the fields that actually
// change ride the payload (omitempty): a description-only update carries no
// Success key and vice versa. goal.updated is a NEW event type introduced by
// RHZ-087: legacy journals are unaffected, a
// binary older than RHZ-087 cannot replay a journal containing it (rollback
// needs this binary), and the first record lands only after a serve restart.
func (s Service) UpdateGoal(id string, expected uint64, description, success, reason string) (domain.Goal, error) {
	if s.Store == nil {
		return domain.Goal{}, fmt.Errorf("nil event store")
	}
	description, success = strings.TrimSpace(description), strings.TrimSpace(success)
	if description == "" && success == "" {
		return domain.Goal{}, fmt.Errorf("description or success required")
	}
	g, err := projector.ReplayGoal(s.Store.List("goal", id))
	if err != nil {
		return domain.Goal{}, err
	}
	if g.State == domain.GoalAchieved || g.State == domain.GoalFailed || g.State == domain.GoalCancelled {
		return domain.Goal{}, domain.ErrInvalidState
	}
	if g.Revision != expected {
		return domain.Goal{}, events.ErrRevisionConflict
	}
	if description == g.Description {
		description = ""
	}
	if success == g.Success {
		success = ""
	}
	if description == "" && success == "" {
		return g, nil // idempotent: nothing changes, nothing written
	}
	p, _ := json.Marshal(struct {
		Description string `json:"Description,omitempty"`
		Success     string `json:"Success,omitempty"`
		Reason      string
	}{description, success, reason})
	if err := s.Store.Append(expected, events.Event{AggregateType: "goal", AggregateID: id, Revision: expected + 1, Type: "goal.updated", Payload: p}); err != nil {
		return domain.Goal{}, err
	}
	return projector.ReplayGoal(s.Store.List("goal", id))
}

func (s Service) ApplyGoalDecision(goalID, decisionID, correlation string, to domain.GoalState) (domain.Goal, error) {
	if s.Store == nil {
		return domain.Goal{}, fmt.Errorf("nil store")
	}
	if decisionID == "" || correlation == "" || (to != domain.GoalAchieved && to != domain.GoalFailed) {
		return domain.Goal{}, fmt.Errorf("invalid terminal decision")
	}
	g, err := projector.ReplayGoal(s.Store.List("goal", goalID))
	if err == nil && g.TerminalDecisionID == decisionID {
		return g, nil
	}
	if err != nil {
		return domain.Goal{}, err
	}
	if g.State == domain.GoalAchieved || g.State == domain.GoalFailed {
		return domain.Goal{}, fmt.Errorf("decision conflict")
	}
	// RHZ-061 (FR-RHZ-090): a command guard must never append what replay
	// rejects — without this, a decision on a cancelled goal appends an event
	// validGoalTransition refuses on replay, poisoning the stream (and the
	// whole workspace snapshot) permanently.
	if !goalTransitionValid(g.State, to) {
		return domain.Goal{}, domain.ErrInvalidState
	}
	p, _ := json.Marshal(struct {
		To         domain.GoalState
		DecisionID string
	}{to, decisionID})
	if e := s.Store.Append(g.Revision, events.Event{AggregateType: "goal", AggregateID: goalID, Revision: g.Revision + 1, Type: "goal.transitioned", Payload: p, CorrelationID: correlation}); e != nil {
		return domain.Goal{}, e
	}
	return projector.ReplayGoal(s.Store.List("goal", goalID))
}
func goalTransitionValid(a, b domain.GoalState) bool {
	if a == domain.GoalAchieved || a == domain.GoalFailed || a == domain.GoalCancelled {
		return false
	}
	if a == domain.GoalActive {
		return b == domain.GoalPaused || b == domain.GoalAchieved || b == domain.GoalFailed || b == domain.GoalCancelled
	}
	return a == domain.GoalPaused && (b == domain.GoalActive || b == domain.GoalCancelled)
}

func (s Service) Create(id, goalID, description, success string) (domain.Mission, error) { // FR-RHZ-003
	if s.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil event store")
	}
	if _, err := domain.NewMission(id, goalID, description, success); err != nil {
		return domain.Mission{}, err
	}
	if _, err := projector.ReplayGoal(s.Store.List("goal", goalID)); err != nil {
		return domain.Mission{}, fmt.Errorf("goal reference: %w", err)
	}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{id, goalID, description, success})
	e := events.Event{AggregateType: "mission", AggregateID: id, Revision: 1, Type: "mission.created", Payload: p}
	if err := s.Store.Append(0, e); err != nil {
		return domain.Mission{}, err
	}
	return projector.ReplayMission(s.Store.List("mission", id))
}

// CreateFromProcedure records mission.created with the spawning procedure's
// identity, pinned revision and input params (RHZ-063, FR-RHZ-092). The
// fields are additive: missionCreated decode ignores them, so replay is
// untouched while the journal keeps the full instantiation record — a run is
// recomputable from its own event without consulting the procedure stream.
func (s Service) CreateFromProcedure(id, goalID, description, success, procedureID string, procedureRevision uint64, params map[string]string) (domain.Mission, error) {
	if s.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(procedureID) == "" || procedureRevision == 0 {
		return domain.Mission{}, fmt.Errorf("procedure id and revision are required")
	}
	if _, err := domain.NewMission(id, goalID, description, success); err != nil {
		return domain.Mission{}, err
	}
	if _, err := projector.ReplayGoal(s.Store.List("goal", goalID)); err != nil {
		return domain.Mission{}, fmt.Errorf("goal reference: %w", err)
	}
	p, _ := json.Marshal(struct {
		ID, GoalID, Description, Success string
		ProcedureID                      string            `json:"procedure_id"`
		ProcedureRevision                uint64            `json:"procedure_revision"`
		Params                           map[string]string `json:"params"`
	}{id, goalID, description, success, procedureID, procedureRevision, params})
	e := events.Event{AggregateType: "mission", AggregateID: id, Revision: 1, Type: "mission.created", Payload: p}
	if err := s.Store.Append(0, e); err != nil {
		return domain.Mission{}, err
	}
	return projector.ReplayMission(s.Store.List("mission", id))
}

func (s Service) Transition(id string, expected uint64, to domain.MissionState) (domain.Mission, error) { // FR-RHZ-003
	if s.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil event store")
	}
	log := s.Store.List("mission", id)
	m, err := projector.ReplayMission(log)
	if err != nil {
		return domain.Mission{}, err
	}
	if m.Revision != expected {
		return domain.Mission{}, events.ErrRevisionConflict
	}
	if to == domain.MissionSucceeded || to == domain.MissionFailed {
		return domain.Mission{}, domain.ErrInvalidState
	}
	if _, err := m.Transition(to); err != nil {
		return domain.Mission{}, err
	} // validate before append
	p, _ := json.Marshal(struct {
		To     domain.MissionState
		Reason string
	}{To: to})
	e := events.Event{AggregateType: "mission", AggregateID: id, Revision: expected + 1, Type: "mission.transitioned", Payload: p}
	if err := s.Store.Append(expected, e); err != nil {
		return domain.Mission{}, err
	}
	return projector.ReplayMission(s.Store.List("mission", id))
}

// Assign records who a mission is handed to (RHZ-080, FR-RHZ-111) as one
// append-only mission.assigned event {Assignee, Reason} on the mission
// stream. Reassignment is allowed (the journal keeps the history, replay
// takes the last value). Every guard runs before the append so a rejected
// assign never lands an event replay would refuse: empty/whitespace assignee,
// a terminal mission (succeeded/failed/cancelled → ErrInvalidState) and a
// revision mismatch (ErrRevisionConflict) all leave the journal unchanged.
// The mission state is not touched.
func (s Service) Assign(id string, expected uint64, assignee, reason string) (domain.Mission, error) {
	if s.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(assignee) == "" {
		return domain.Mission{}, fmt.Errorf("assignee required")
	}
	m, err := projector.ReplayMission(s.Store.List("mission", id))
	if err != nil {
		return domain.Mission{}, err
	}
	if m.State == domain.MissionSucceeded || m.State == domain.MissionFailed || m.State == domain.MissionCancelled {
		return domain.Mission{}, domain.ErrInvalidState
	}
	if m.Revision != expected {
		return domain.Mission{}, events.ErrRevisionConflict
	}
	// Stored trimmed so the ?assignee= exact-match query finds it.
	p, _ := json.Marshal(struct{ Assignee, Reason string }{strings.TrimSpace(assignee), reason})
	if err := s.Store.Append(expected, events.Event{AggregateType: "mission", AggregateID: id, Revision: expected + 1, Type: "mission.assigned", Payload: p}); err != nil {
		return domain.Mission{}, err
	}
	return projector.ReplayMission(s.Store.List("mission", id))
}

// ApplyMissionDecision is the operator-terminal writer for a mission
// (RHZ-069, FR-RHZ-098): mission.complete / mission.fail. It is the mission
// counterpart of ApplyGoalDecision (goal.resolve/fail) — one decision event,
// append-only, no idempotent branch (resubmit on a terminal mission is a
// rejection, like mission.cancel). The transition is validated before the
// append so that a rejected decision never lands an event replay would refuse
// (projector requires a DecisionID on every Succeeded/Failed transition).
// Transition deliberately refuses Succeeded/Failed; this is the only kernel
// path that writes them besides the execution-decision coordinator.
func (s Service) ApplyMissionDecision(id string, expected uint64, to domain.MissionState, decisionID, correlation, reason string) (domain.Mission, error) {
	if s.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil event store")
	}
	if decisionID == "" || correlation == "" || (to != domain.MissionSucceeded && to != domain.MissionFailed) {
		return domain.Mission{}, fmt.Errorf("invalid terminal decision")
	}
	m, err := projector.ReplayMission(s.Store.List("mission", id))
	if err != nil {
		return domain.Mission{}, err
	}
	if m.Revision != expected {
		return domain.Mission{}, events.ErrRevisionConflict
	}
	if _, err := m.Transition(to); err != nil {
		return domain.Mission{}, err
	} // validate before append (no poison path)
	p, _ := json.Marshal(struct {
		To         domain.MissionState
		DecisionID string
		Reason     string
	}{to, decisionID, reason})
	if err := s.Store.Append(expected, events.Event{AggregateType: "mission", AggregateID: id, Revision: expected + 1, Type: "mission.transitioned", Payload: p, CorrelationID: correlation}); err != nil {
		return domain.Mission{}, err
	}
	return projector.ReplayMission(s.Store.List("mission", id))
}

func (s Service) TransitionWithReason(id string, expected uint64, to domain.MissionState, reason string) (domain.Mission, error) {
	if to == domain.MissionBlocked && reason == "" {
		return domain.Mission{}, fmt.Errorf("blocked reason required")
	}
	if to != domain.MissionBlocked {
		return s.Transition(id, expected, to)
	}
	if s.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil event store")
	}
	m, e := projector.ReplayMission(s.Store.List("mission", id))
	if e != nil {
		return domain.Mission{}, e
	}
	if m.Revision != expected {
		return domain.Mission{}, events.ErrRevisionConflict
	}
	if _, e = m.Transition(to); e != nil {
		return domain.Mission{}, e
	}
	p, _ := json.Marshal(struct {
		To     domain.MissionState
		Reason string
	}{to, reason})
	if e = s.Store.Append(expected, events.Event{AggregateType: "mission", AggregateID: id, Revision: expected + 1, Type: "mission.transitioned", Payload: p}); e != nil {
		return domain.Mission{}, e
	}
	return projector.ReplayMission(s.Store.List("mission", id))
}
