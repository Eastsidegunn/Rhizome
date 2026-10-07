package workspace

// RHZ-046 part 2: the /v1/execution/{taskId} surface (contract ① revision
// "execution 표면 v1"). Sessions carry only the
// fields derivable from ExecutionRef — id, taskId, state, label. The surface is
// a pure function of the journal projection.
//
// T25 multi-turn (FR-RHZ-083): the
// events array is no longer forced empty. When a SessionEventSource is injected
// (the JANUS-configured serve), each bound session's events are a READ-ONLY
// projection of the append-only JANUS session log — contiguous seq, no
// synthesis, no gap inference, no second writer and no separate state store.
// With no source injected (adapter disabled, and every existing test fixture),
// events stays the empty array exactly as before — backward compatible.

import (
	"errors"
	"fmt"
	"sort"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/projector"
)

// ErrUnknownTask marks a taskId with no mission aggregate (HTTP 404 — RHZ-046
// D10: distinct from an existing mission with no sessions, which is 200 + []).
var ErrUnknownTask = errors.New("unknown task")

type ExecutionSession struct {
	ID, TaskID, State, Label string
	// Status is the additive JANUS session status (FR-RHZ-119). nil when no
	// SessionEventSource is injected or the
	// session has no binding — the wire shape is then byte-identical to v1.
	Status *SessionStatus
	// Limits is the run's policy limits (FR-RHZ-124), derived
	// from the intent's journaled provenance. nil for legacy executions (no
	// provenance) — the wire shape is then byte-identical to v1.
	Limits *SessionLimits
}

// LimitAxes are the numeric budget axes of one policy.
type LimitAxes struct{ Tokens, TimeMs, MaxDepth int64 }

// SessionLimits are the configured ceiling and the effective (ceiling ∩ request)
// limits of one execution. Only numbers cross the wire: profile identity,
// hashes and the ledger digest stay in the journal (minimal projection).
type SessionLimits struct{ Ceiling, Effective LimitAxes }

func limitsOf(r execution.Ref) *SessionLimits {
	if r.Provenance == nil {
		return nil
	}
	c, e := r.Provenance.Ceiling, r.Provenance.Effective
	return &SessionLimits{Ceiling: LimitAxes{c.Budget, c.Timeout, c.MaxDepth}, Effective: LimitAxes{e.Budget, e.Timeout, e.MaxDepth}}
}

// SessionStatus is derived at request time, purely from the projected JANUS
// session log (the same read-only projection that fills events[]), with the
// rule JANUS itself applies: pending until subagent/spawn, running after it,
// exited on subagent/done or session/end. Usage totals are checked sums of
// the rows' usage_in/usage_out; LastActivityTS is the last row's ts (JANUS
// ts, Unix ms). Nothing here is stored by Rhizome. doneStatus is NOT derived:
// it lives in the done payload, which the projection never carries.
type SessionStatus struct {
	JanusState                  string
	UsageInTotal, UsageOutTotal int64
	LastActivityTS              int64
}

// deriveSessionStatus folds the projected events into a SessionStatus.
func deriveSessionStatus(evs []SessionEvent) (SessionStatus, error) {
	st := SessionStatus{JanusState: "pending"}
	for _, e := range evs {
		var err error
		if st.UsageInTotal, err = addChecked(st.UsageInTotal, e.UsageIn); err != nil {
			return SessionStatus{}, fmt.Errorf("seq %d usage_in: %w", e.Seq, err)
		}
		if st.UsageOutTotal, err = addChecked(st.UsageOutTotal, e.UsageOut); err != nil {
			return SessionStatus{}, fmt.Errorf("seq %d usage_out: %w", e.Seq, err)
		}
		st.LastActivityTS = e.TS
		switch e.Kind {
		case "subagent/spawn":
			if st.JanusState == "pending" {
				st.JanusState = "running"
			}
		case "subagent/done", "session/end":
			st.JanusState = "exited"
		}
	}
	return st, nil
}

func addChecked(a, b int64) (int64, error) {
	if b < 0 {
		return 0, errors.New("negative usage")
	}
	if a > 0 && b > 0 && a > (1<<63-1)-b {
		return 0, errors.New("usage total overflow")
	}
	return a + b, nil
}

// SessionEvent is one projected JANUS session-log row (FR-RHZ-083). It mirrors
// the janusadapter projection but is defined here so the surface layer never
// imports the seam — main.go adapts one into the other. Only envelope facts
// cross the boundary; raw payload/args never do (비복제 헌장).
type SessionEvent struct {
	Seq               int64
	Kind, Actor       string
	TS                int64
	UsageIn, UsageOut int64
}

// SessionEventSource reads one bound JANUS session's log and returns its
// contiguous, read-only event projection. It is injected (nil when the adapter
// is disabled) and is always transient — the surface persists nothing.
type SessionEventSource func(sessionDB, traceID string) ([]SessionEvent, error)

// ExecutionEvent is a projected session event tagged with the owning Rhizome
// execution (session) id for the flat outbound events array.
type ExecutionEvent struct {
	SessionID         string
	Seq               int64
	Kind, Actor       string
	TS                int64
	UsageIn, UsageOut int64
}

type ExecutionProjection struct {
	Revision uint64
	TaskID   string
	Sessions []ExecutionSession
	Events   []ExecutionEvent
}

// executionSessionState maps ExecutionRef states to the contract vocabulary.
// States outside the ratified mapping (intent, dispatch_claimed, unknown) are
// not emitted: none of running|killed|ended can be claimed honestly for them.
func executionSessionState(st execution.State) (string, bool) {
	switch st {
	case execution.Accepted, execution.Observing:
		return "running", true
	case execution.Cancelled:
		return "killed", true
	case execution.Succeeded, execution.Failed:
		return "ended", true
	}
	return "", false
}

// ExecutionSnapshot derives the sessions of one task (= mission) from the
// journal. Revision is the global event sequence (§2), never a surface-local
// counter. Sessions are sorted by id for determinism.
func ExecutionSnapshot(s events.Port, taskID string, src SessionEventSource) (ExecutionProjection, error) {
	p := ExecutionProjection{TaskID: taskID, Sessions: []ExecutionSession{}, Events: []ExecutionEvent{}}
	if s == nil {
		return p, fmt.Errorf("nil store")
	}
	if taskID == "" {
		return p, ErrUnknownTask
	}
	missionLog := s.List("mission", taskID)
	if len(missionLog) == 0 {
		return p, ErrUnknownTask
	}
	if _, err := projector.ReplayMission(missionLog); err != nil {
		return p, err
	}
	execIDs := map[string]bool{}
	for _, e := range s.All() {
		if e.Sequence > p.Revision {
			p.Revision = e.Sequence
		}
		if e.AggregateType == "execution" {
			execIDs[e.AggregateID] = true
		}
	}
	ids := make([]string, 0, len(execIDs))
	for id := range execIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r, err := execution.Replay(s.List("execution", id))
		if err != nil {
			return p, err
		}
		if r.MissionID != taskID {
			continue
		}
		st, ok := executionSessionState(r.State)
		if !ok {
			continue
		}
		p.Sessions = append(p.Sessions, ExecutionSession{ID: r.ID, TaskID: r.MissionID, State: st, Label: r.Summary, Limits: limitsOf(r)})
		// FR-RHZ-083: project the bound session's JANUS log. Only sessions with a
		// durable binding have a session-log path; without an injected source the
		// events stay empty (adapter disabled / all existing fixtures). Any
		// projection fault surfaces as an error — the surface never fabricates or
		// gap-fills events it cannot derive.
		if src == nil || r.Binding == nil {
			continue
		}
		evs, err := src(r.Binding.SessionDB, r.Binding.TraceID)
		if err != nil {
			return p, err
		}
		for _, e := range evs {
			p.Events = append(p.Events, ExecutionEvent{SessionID: r.ID, Seq: e.Seq, Kind: e.Kind, Actor: e.Actor, TS: e.TS, UsageIn: e.UsageIn, UsageOut: e.UsageOut})
		}
		// FR-RHZ-119: session status, derived from the same projection only.
		status, err := deriveSessionStatus(evs)
		if err != nil {
			return p, err
		}
		p.Sessions[len(p.Sessions)-1].Status = &status
	}
	return p, nil
}

type executionSessionDTO struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	State  string `json:"state"`
	Label  string `json:"label,omitempty"`
	// Additive (FR-RHZ-119): present only when the
	// session log projection is wired; absent = 숨김. lastActivityTs is JANUS
	// ts (Unix ms). doneStatus is not emitted (not derivable, see
	// SessionStatus).
	JanusState     string `json:"janusState,omitempty"`
	UsageInTotal   *int64 `json:"usageInTotal,omitempty"`
	UsageOutTotal  *int64 `json:"usageOutTotal,omitempty"`
	LastActivityTS *int64 `json:"lastActivityTs,omitempty"`
	// Additive (FR-RHZ-124): present only for executions whose
	// intent journaled provenance (mission.start); absent for legacy.
	Limits *executionLimitsDTO `json:"limits,omitempty"`
}

type limitAxesDTO struct {
	Tokens   int64 `json:"tokens"`
	TimeMs   int64 `json:"timeMs"`
	MaxDepth int64 `json:"maxDepth"`
}
type executionLimitsDTO struct {
	Ceiling   limitAxesDTO `json:"ceiling"`
	Effective limitAxesDTO `json:"effective"`
}

// executionEventDTO is the wire shape of one projected JANUS session event
// (FR-RHZ-083). usageIn/usageOut are always present (0 when the row carries no
// usage) so the consumer can sum per-session totals; ts drives idle/last-
// activity. Raw payload/args are never emitted.
type executionEventDTO struct {
	SessionID string `json:"sessionId"`
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	Actor     string `json:"actor"`
	TS        int64  `json:"ts"`
	UsageIn   int64  `json:"usageIn"`
	UsageOut  int64  `json:"usageOut"`
}
type executionBodyDTO struct {
	TaskID   string                `json:"taskId"`
	Sessions []executionSessionDTO `json:"sessions"`
	// Events is the read-only projection of the bound sessions' JANUS logs
	// (FR-RHZ-083); the empty array when no source is injected.
	Events []executionEventDTO `json:"events"`
}
type executionEnvelope struct {
	Revision uint64           `json:"revision"`
	Body     executionBodyDTO `json:"body"`
}

func toExecutionDTO(p ExecutionProjection) executionBodyDTO {
	d := executionBodyDTO{TaskID: p.TaskID, Sessions: []executionSessionDTO{}, Events: []executionEventDTO{}}
	for _, s := range p.Sessions {
		dto := executionSessionDTO{ID: s.ID, TaskID: s.TaskID, State: s.State, Label: s.Label}
		if s.Status != nil {
			in, out, ts := s.Status.UsageInTotal, s.Status.UsageOutTotal, s.Status.LastActivityTS
			dto.JanusState, dto.UsageInTotal, dto.UsageOutTotal, dto.LastActivityTS = s.Status.JanusState, &in, &out, &ts
		}
		if l := s.Limits; l != nil {
			dto.Limits = &executionLimitsDTO{Ceiling: limitAxesDTO(l.Ceiling), Effective: limitAxesDTO(l.Effective)}
		}
		d.Sessions = append(d.Sessions, dto)
	}
	for _, e := range p.Events {
		d.Events = append(d.Events, executionEventDTO{SessionID: e.SessionID, Seq: e.Seq, Kind: e.Kind, Actor: e.Actor, TS: e.TS, UsageIn: e.UsageIn, UsageOut: e.UsageOut})
	}
	return d
}
