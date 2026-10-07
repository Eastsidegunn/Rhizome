package domain

import "errors"

var (
	ErrInvalidID      = errors.New("invalid id")
	ErrInvalidState   = errors.New("invalid state transition")
	ErrMissingGoal    = errors.New("goal is required")
	ErrMissingMission = errors.New("mission is required")
)

type GoalState string

const (
	GoalActive    GoalState = "active"
	GoalPaused    GoalState = "paused"
	GoalAchieved  GoalState = "achieved"
	GoalFailed    GoalState = "failed"
	GoalCancelled GoalState = "cancelled"
)

type MissionState string

const (
	MissionPlanned       MissionState = "planned"
	MissionReady         MissionState = "ready"
	MissionRunning       MissionState = "running"
	MissionPaused        MissionState = "paused"
	MissionWaitingResult MissionState = "waiting_for_result"
	MissionWaitingHuman  MissionState = "waiting_for_human"
	MissionBlocked       MissionState = "blocked"
	MissionSucceeded     MissionState = "succeeded"
	MissionFailed        MissionState = "failed"
	MissionCancelled     MissionState = "cancelled"
)

type Goal struct {
	ID                 string
	Description        string
	Success            string
	PolicyRef          string
	State              GoalState
	Revision           uint64
	TerminalDecisionID string
}

type Mission struct {
	ID                 string
	GoalID             string
	Description        string
	Success            string
	State              MissionState
	Revision           uint64
	TerminalDecisionID string
	BlockedReason      string
	// Assignee is who the mission is currently handed to (RHZ-080,
	// FR-RHZ-111): a free-text label written by mission.assigned, last wins.
	// Empty = unassigned (every pre-RHZ-080 journal replays to "").
	Assignee string
}

func NewGoal(id, description, success, policyRef string) (Goal, error) {
	if id == "" || description == "" || success == "" {
		return Goal{}, ErrMissingGoal
	}
	return Goal{ID: id, Description: description, Success: success, PolicyRef: policyRef, State: GoalActive}, nil
}

func NewMission(id, goalID, description, success string) (Mission, error) {
	if id == "" || goalID == "" || description == "" || success == "" {
		return Mission{}, ErrMissingMission
	}
	return Mission{ID: id, GoalID: goalID, Description: description, Success: success, State: MissionPlanned}, nil
}

func (m Mission) Transition(to MissionState) (Mission, error) {
	if !validMissionTransition(m.State, to) {
		return Mission{}, ErrInvalidState
	}
	m.State, m.Revision = to, m.Revision+1
	return m, nil
}

// validMissionTransition is the mission state table. RHZ-079 (FR-RHZ-110)
// extended it only: waiting_for_human and blocked may now close directly to
// succeeded/failed (operator terminal via mission.complete/mission.fail);
// every pre-existing row is unchanged, so any journal valid before replays.
func validMissionTransition(from, to MissionState) bool {
	if from == MissionCancelled || from == MissionSucceeded || from == MissionFailed {
		return false
	}
	switch from {
	case MissionPlanned:
		return to == MissionReady || to == MissionCancelled
	case MissionReady:
		return to == MissionRunning || to == MissionBlocked || to == MissionCancelled
	case MissionRunning:
		return to == MissionPaused || to == MissionWaitingResult || to == MissionWaitingHuman || to == MissionSucceeded || to == MissionFailed || to == MissionBlocked
	case MissionWaitingResult:
		return to == MissionRunning || to == MissionSucceeded || to == MissionFailed || to == MissionBlocked
	case MissionPaused:
		return to == MissionRunning || to == MissionCancelled
	case MissionWaitingHuman:
		return to == MissionRunning || to == MissionCancelled || to == MissionSucceeded || to == MissionFailed
	case MissionBlocked:
		return to == MissionReady || to == MissionCancelled || to == MissionSucceeded || to == MissionFailed
	default:
		return false
	}
}
