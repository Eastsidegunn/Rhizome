// Package projector rebuilds domain state exclusively from append-only events.
package projector

import (
	"encoding/json"
	"fmt"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

type missionCreated struct{ ID, GoalID, Description, Success string }
type missionTransitioned struct {
	To         domain.MissionState
	DecisionID string
	Reason     string
}

// missionAssigned is the mission.assigned payload (RHZ-080, FR-RHZ-111).
type missionAssigned struct{ Assignee, Reason string }

// ReplayMission implements FR-RHZ-001. It does not mutate or persist events.
func ReplayMission(log []events.Event) (domain.Mission, error) {
	var m domain.Mission
	var expected uint64
	var aggregateID string
	for _, e := range log {
		if e.AggregateType != "mission" {
			continue
		}
		if aggregateID != "" && e.AggregateID != aggregateID {
			return domain.Mission{}, events.ErrRevisionConflict
		}
		if e.Revision != expected+1 {
			return domain.Mission{}, events.ErrRevisionConflict
		}
		expected = e.Revision
		switch e.Type {
		case "mission.created":
			if aggregateID != "" {
				return domain.Mission{}, fmt.Errorf("duplicate mission.created")
			}
			var p missionCreated
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return domain.Mission{}, fmt.Errorf("decode creation: %w", err)
			}
			var err error
			m, err = domain.NewMission(p.ID, p.GoalID, p.Description, p.Success)
			if err != nil {
				return domain.Mission{}, err
			}
			if e.AggregateID == "" || p.ID != e.AggregateID {
				return domain.Mission{}, fmt.Errorf("mission id does not match aggregate")
			}
			aggregateID = e.AggregateID
			m.Revision = e.Revision
		case "mission.transitioned":
			var p missionTransitioned
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return domain.Mission{}, fmt.Errorf("decode transition: %w", err)
			}
			if (p.To == domain.MissionSucceeded || p.To == domain.MissionFailed) && p.DecisionID == "" {
				return domain.Mission{}, fmt.Errorf("terminal decision missing")
			}
			var err error
			m, err = m.Transition(p.To)
			if err != nil {
				return domain.Mission{}, err
			}
			if p.To == domain.MissionBlocked {
				m.BlockedReason = p.Reason
			} else {
				m.BlockedReason = ""
			}
			if p.To == domain.MissionSucceeded || p.To == domain.MissionFailed {
				m.TerminalDecisionID = p.DecisionID
			}
		case "mission.assigned":
			// RHZ-080 (FR-RHZ-111): last assignment wins; state untouched.
			// An empty assignee is refused on replay, symmetric with the
			// command guard (mission.Service.Assign), like the other cases.
			var p missionAssigned
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return domain.Mission{}, fmt.Errorf("decode assignment: %w", err)
			}
			if p.Assignee == "" {
				return domain.Mission{}, fmt.Errorf("assignee missing")
			}
			// Replay guards symmetric with the command: an
			// assignment cannot precede creation, nor land on a terminal mission.
			if aggregateID == "" {
				return domain.Mission{}, fmt.Errorf("assignment before creation")
			}
			if m.State == domain.MissionSucceeded || m.State == domain.MissionFailed || m.State == domain.MissionCancelled {
				return domain.Mission{}, fmt.Errorf("assignment on terminal mission")
			}
			m.Assignee = p.Assignee
			m.Revision = e.Revision
		default:
			return domain.Mission{}, fmt.Errorf("unknown mission event %q", e.Type)
		}
	}
	if expected == 0 {
		return domain.Mission{}, fmt.Errorf("mission event stream is empty")
	}
	return m, nil
}
