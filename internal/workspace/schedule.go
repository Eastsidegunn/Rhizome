package workspace

import (
	"encoding/json"
	"fmt"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/schedule"
)

// RHZ-136 (FR-RHZ-180): recurring schedules and mission timing on the
// workspace surface. Data and display only — nothing here fires a wake.

// Schedule is one replayed schedule projected to /v1/workspace.
type Schedule struct {
	ID, Name, GoalID, TZ, State string
	Rule                        schedule.Rule
	sched                       schedule.Schedule
}

var workKinds = map[string]bool{"processing": true, "understanding": true, "output": true}

// missionTiming validates the optional mission.create timing fields; a
// non-empty reason rejects the intent before any write.
func missionTiming(s events.Port, in Intent) (mission.Timing, string) {
	t := mission.Timing{DueAt: in.DueAt, Anchor: in.Anchor, WorkKind: in.WorkKind}
	if t.DueAt != "" {
		if _, err := time.Parse(time.RFC3339, t.DueAt); err != nil {
			return t, fmt.Sprintf("invalid dueAt %q: want RFC3339 with offset", t.DueAt)
		}
	}
	if t.Anchor != "" {
		sc, err := schedule.Replay(s.List("schedule", t.Anchor))
		if err != nil {
			return t, fmt.Sprintf("unknown anchor schedule %q", t.Anchor)
		}
		if sc.State != schedule.Active {
			return t, fmt.Sprintf("anchor schedule %q is cancelled", t.Anchor)
		}
	}
	if t.WorkKind != "" && !workKinds[t.WorkKind] {
		return t, fmt.Sprintf("invalid workKind %q: want processing|understanding|output", t.WorkKind)
	}
	return t, ""
}

// missionTimingOf reads the additive timing fields of a mission.created
// event (zero for every pre-RHZ-136 mission).
func missionTimingOf(log []events.Event) mission.Timing {
	var t mission.Timing
	for _, e := range log {
		if e.Type == "mission.created" {
			_ = json.Unmarshal(e.Payload, &t)
			break
		}
	}
	return t
}

func replaySchedules(s events.Port, ids []string) ([]Schedule, error) {
	var out []Schedule
	for _, id := range ids {
		sc, err := schedule.Replay(s.List("schedule", id))
		if err != nil {
			return nil, err
		}
		out = append(out, Schedule{ID: sc.ID, Name: sc.Name, GoalID: sc.GoalID, TZ: sc.TZ, State: sc.State, Rule: sc.Rule, sched: sc})
	}
	return out, nil
}

type scheduleDTO struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	GoalID string        `json:"goalId,omitempty"`
	Rule   schedule.Rule `json:"rule"`
	TZ     string        `json:"tz"`
	State  string        `json:"state"`
	// NextAt is the first occurrence after the server clock; absent once
	// cancelled.
	NextAt string `json:"nextAt,omitempty"`
}

// timingDTO fills schedules[] and each task's timing keys at now.
func timingDTO(d *dto, p Projection, now time.Time) {
	next := map[string]time.Time{}
	for _, sc := range p.Schedules {
		x := scheduleDTO{ID: sc.ID, Name: sc.Name, GoalID: sc.GoalID, Rule: sc.Rule, TZ: sc.TZ, State: sc.State}
		if at, ok := sc.sched.NextAt(now); ok {
			next[sc.ID] = at
			x.NextAt = at.Format(time.RFC3339)
		}
		d.Schedules = append(d.Schedules, x)
	}
	for i, t := range p.Tasks {
		if t.Timing == (mission.Timing{}) {
			continue
		}
		x := &d.Tasks[i]
		x.DueAt, x.Anchor, x.WorkKind = t.Timing.DueAt, t.Timing.Anchor, t.Timing.WorkKind
		var eff time.Time
		if due, err := time.Parse(time.RFC3339, t.Timing.DueAt); err == nil {
			eff = due
		}
		if at, ok := next[t.Timing.Anchor]; ok && (eff.IsZero() || at.Before(eff)) {
			eff = at
		}
		if !eff.IsZero() {
			x.EffectiveDueAt = eff.Format(time.RFC3339)
		}
	}
}
