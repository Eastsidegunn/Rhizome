package projector

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/domain"
	"rhizome/internal/events"
)

type goalCreated struct{ ID, Description, Success, PolicyRef string }
type goalTransitioned struct {
	To         domain.GoalState
	DecisionID string
}

// goalUpdated is the goal.updated payload (RHZ-087, FR-RHZ-117). The writer
// marshals Description/Success with omitempty, so an absent key and an empty
// string mean the same thing on replay: unchanged.
type goalUpdated struct{ Description, Success, Reason string }

func ReplayGoal(log []events.Event) (domain.Goal, error) { // FR-RHZ-006
	var g domain.Goal
	var rev uint64
	var id string
	for _, e := range log {
		if e.AggregateType != "goal" {
			continue
		}
		if id != "" && e.AggregateID != id || e.Revision != rev+1 {
			return domain.Goal{}, events.ErrRevisionConflict
		}
		rev = e.Revision
		switch e.Type {
		case "goal.created":
			if id != "" {
				return domain.Goal{}, fmt.Errorf("duplicate goal.created")
			}
			var p goalCreated
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return domain.Goal{}, err
			}
			var err error
			g, err = domain.NewGoal(p.ID, p.Description, p.Success, p.PolicyRef)
			if err != nil {
				return domain.Goal{}, err
			}
			if p.ID == "" || p.ID != e.AggregateID {
				return domain.Goal{}, fmt.Errorf("goal id mismatch")
			}
			id = e.AggregateID
			g.Revision = e.Revision
		case "goal.transitioned":
			var p goalTransitioned
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return domain.Goal{}, err
			}
			if (p.To == domain.GoalAchieved || p.To == domain.GoalFailed) && p.DecisionID == "" {
				return domain.Goal{}, fmt.Errorf("terminal decision missing")
			}
			if !validGoalTransition(g.State, p.To) {
				return domain.Goal{}, domain.ErrInvalidState
			}
			g.State = p.To
			g.Revision++
			if p.DecisionID != "" {
				g.TerminalDecisionID = p.DecisionID
			}
		case "goal.updated":
			// RHZ-087 (FR-RHZ-117): description/success edit, ID and state
			// untouched; only non-empty fields apply (absent = unchanged).
			// New event type introduced by RHZ-087:
			// legacy journals (no goal.updated) replay unchanged; a binary
			// older than RHZ-087 cannot replay a journal that contains one, so
			// a rollback must carry this binary; the first record lands only
			// after a serve restart. Guards are symmetric with the command
			// (mission.Service.UpdateGoal) so a rejected update is never
			// appended and a hand-written poison record is refused here.
			var p goalUpdated
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return domain.Goal{}, fmt.Errorf("decode goal update: %w", err)
			}
			if id == "" {
				return domain.Goal{}, fmt.Errorf("update before creation")
			}
			if p.Description == "" && p.Success == "" {
				return domain.Goal{}, fmt.Errorf("goal update without fields")
			}
			if g.State == domain.GoalAchieved || g.State == domain.GoalFailed || g.State == domain.GoalCancelled {
				return domain.Goal{}, fmt.Errorf("update on terminal goal")
			}
			if p.Description != "" {
				g.Description = p.Description
			}
			if p.Success != "" {
				g.Success = p.Success
			}
			g.Revision++
		default:
			return domain.Goal{}, fmt.Errorf("unknown goal event %q", e.Type)
		}
	}
	if rev == 0 {
		return domain.Goal{}, fmt.Errorf("goal event stream is empty")
	}
	return g, nil
}
func validGoalTransition(a, b domain.GoalState) bool {
	if a == domain.GoalAchieved || a == domain.GoalFailed || a == domain.GoalCancelled {
		return false
	}
	switch a {
	case domain.GoalActive:
		return b == domain.GoalPaused || b == domain.GoalAchieved || b == domain.GoalFailed || b == domain.GoalCancelled
	case domain.GoalPaused:
		return b == domain.GoalActive || b == domain.GoalCancelled
	}
	return false
}
