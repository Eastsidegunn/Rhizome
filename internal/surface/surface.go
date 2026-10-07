package surface

import (
	"encoding/json"
	"fmt"
	"math"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"sort"
	"strings"
	"time"
)

type State struct {
	MissionID, CurrentAction, Instruction, InstructionActor, InstructionCorrelation, ProgressSource string
	Progress                                                                                        float64
	HasProgress                                                                                     bool
	// BlockedReason is the display-only reason recorded by mission.progress
	// (RHZ-082, FR-RHZ-113). It never moves the mission to blocked; the
	// lifecycle BlockedReason stays on domain.Mission.
	BlockedReason string
	Revision      uint64
	UpdatedAt     time.Time
}
type Service struct{ Store events.Port }
type progress struct {
	MissionID, CurrentAction, Source string
	Progress                         *float64
}
type instruction struct{ MissionID, Instruction, Actor, Correlation string }

// progressed is the surface.progressed payload (RHZ-082, FR-RHZ-113): every
// field but MissionID/Actor is optional and only present fields merge on
// replay. Progress is a pointer so 0 is distinguishable from absent.
type progressed struct {
	MissionID, Actor, CurrentAction, BlockedReason string
	Progress                                       *float64
}

// MaxProgressText caps currentAction and blockedReason (RHZ-082, new rule:
// no earlier short-string cap existed; note content uses 16KiB).
const MaxProgressText = 1024

func validProgress(p *float64) bool {
	return p == nil || (*p >= 0 && *p <= 1 && !math.IsNaN(*p) && !math.IsInf(*p, 0))
}

func (s Service) ensure(id string) error {
	if s.Store == nil {
		return fmt.Errorf("nil event store")
	}
	m, e := projector.ReplayMission(s.Store.List("mission", id))
	if e != nil {
		return e
	}
	if m.State == "succeeded" || m.State == "failed" || m.State == "cancelled" {
		return fmt.Errorf("terminal mission")
	}
	return nil
}
func (s Service) append(id, typ string, v any, rev uint64) (State, error) {
	if e := s.ensure(id); e != nil {
		return State{}, e
	}
	b, _ := json.Marshal(v)
	ev := events.Event{AggregateType: "surface", AggregateID: "surface-" + id, Revision: rev + 1, Type: typ, Payload: b, CreatedAt: time.Now().UTC()}
	if e := s.Store.Append(rev, ev); e != nil {
		return State{}, e
	}
	return s.ByMission(id)
}
func (s Service) ReportProgress(id, action string, p *float64, source string) (State, error) {
	if strings.TrimSpace(action) == "" || strings.TrimSpace(source) == "" || (p != nil && (*p < 0 || *p > 1 || math.IsNaN(*p) || math.IsInf(*p, 0))) {
		return State{}, fmt.Errorf("invalid progress")
	}
	cur, e := s.ByMission(id)
	if e != nil && s.Store != nil && len(s.Store.List("surface", "surface-"+id)) > 0 {
		return State{}, e
	}
	return s.append(id, "surface.progress_reported", progress{id, action, source, p}, cur.Revision)
}

// Progress records "what is happening now" on a mission without any state
// transition (RHZ-082, FR-RHZ-113): one append-only surface.progressed event
// through the single writer. Validation runs before append; an intent whose
// given fields all equal the current state is accepted with zero writes.
// The caller (relay) owns the lifecycle allow-list; terminal missions are
// refused here too (ensure).
func (s Service) Progress(id, actor, action string, p *float64, blockedReason string) (State, error) {
	if s.Store == nil {
		return State{}, fmt.Errorf("nil event store")
	}
	action, blockedReason = strings.TrimSpace(action), strings.TrimSpace(blockedReason)
	if action == "" && p == nil && blockedReason == "" {
		return State{}, fmt.Errorf("at least one of currentAction, progress, blockedReason required")
	}
	if !validProgress(p) {
		return State{}, fmt.Errorf("progress must be within [0,1]")
	}
	if len(action) > MaxProgressText || len(blockedReason) > MaxProgressText {
		return State{}, fmt.Errorf("currentAction/blockedReason exceed 1KiB limit")
	}
	if strings.TrimSpace(actor) == "" {
		return State{}, fmt.Errorf("actor required")
	}
	cur, e := s.ByMission(id)
	if e != nil {
		return State{}, e
	}
	same := (action == "" || action == cur.CurrentAction) && (p == nil || (cur.HasProgress && *p == cur.Progress)) && (blockedReason == "" || blockedReason == cur.BlockedReason)
	if same {
		return cur, nil
	}
	return s.append(id, "surface.progressed", progressed{id, actor, action, blockedReason, p}, cur.Revision)
}
func (s Service) Instruct(id, text, actor string, verified bool, correlation string) (State, error) {
	if s.Store == nil {
		return State{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(text) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(correlation) == "" {
		return State{}, fmt.Errorf("invalid instruction")
	}
	if !verified && !strings.HasPrefix(actor, "unverified-local-operator:") {
		actor = "unverified-local-operator:" + actor
	}
	cur, e := s.ByMission(id)
	if e != nil && len(s.Store.List("surface", "surface-"+id)) > 0 {
		return State{}, e
	}
	return s.append(id, "surface.instructed", instruction{id, text, actor, correlation}, cur.Revision)
}
func (s Service) ByMission(id string) (State, error) {
	if s.Store == nil {
		return State{}, fmt.Errorf("nil event store")
	}
	log := s.Store.List("surface", "surface-"+id)
	if len(log) == 0 {
		if e := s.ensure(id); e != nil {
			return State{}, e
		}
		return State{MissionID: id}, nil
	}
	return Replay(log)
}
func Replay(log []events.Event) (State, error) {
	if len(log) == 0 {
		return State{}, fmt.Errorf("empty surface")
	}
	var st State
	for i, e := range log {
		if e.AggregateType != "surface" || e.Revision != uint64(i+1) {
			return State{}, events.ErrRevisionConflict
		}
		if i == 0 {
			st.MissionID = strings.TrimPrefix(e.AggregateID, "surface-")
		}
		switch e.Type {
		case "surface.progress_reported":
			var p progress
			if json.Unmarshal(e.Payload, &p) != nil || p.MissionID != st.MissionID || strings.TrimSpace(p.CurrentAction) == "" || strings.TrimSpace(p.Source) == "" || (p.Progress != nil && (*p.Progress < 0 || *p.Progress > 1 || math.IsNaN(*p.Progress) || math.IsInf(*p.Progress, 0))) {
				return State{}, fmt.Errorf("invalid progress")
			}
			st.CurrentAction, st.ProgressSource, st.Progress, st.HasProgress = p.CurrentAction, p.Source, 0, p.Progress != nil
			if p.Progress != nil {
				st.Progress = *p.Progress
			}
		case "surface.progressed":
			// RHZ-082 (FR-RHZ-113): merge present fields only; never a
			// lifecycle change. Same bounds as the write side.
			var p progressed
			if json.Unmarshal(e.Payload, &p) != nil || p.MissionID != st.MissionID || strings.TrimSpace(p.Actor) == "" || !validProgress(p.Progress) || (strings.TrimSpace(p.CurrentAction) == "" && p.Progress == nil && strings.TrimSpace(p.BlockedReason) == "") || len(p.CurrentAction) > MaxProgressText || len(p.BlockedReason) > MaxProgressText {
				return State{}, fmt.Errorf("invalid progressed")
			}
			if a := strings.TrimSpace(p.CurrentAction); a != "" {
				st.CurrentAction, st.ProgressSource = a, "relay:"+p.Actor
			}
			if p.Progress != nil {
				st.Progress, st.HasProgress, st.ProgressSource = *p.Progress, true, "relay:"+p.Actor
			}
			if r := strings.TrimSpace(p.BlockedReason); r != "" {
				st.BlockedReason = r
			}
		case "surface.instructed":
			var p instruction
			if json.Unmarshal(e.Payload, &p) != nil || p.MissionID != st.MissionID || strings.TrimSpace(p.Instruction) == "" || strings.TrimSpace(p.Actor) == "" || strings.TrimSpace(p.Correlation) == "" {
				return State{}, fmt.Errorf("invalid instruction")
			}
			st.Instruction, st.InstructionActor, st.InstructionCorrelation = p.Instruction, p.Actor, p.Correlation
		default:
			return State{}, fmt.Errorf("unknown surface event")
		}
		st.Revision = e.Revision
		st.UpdatedAt = e.CreatedAt
	}
	return st, nil
}
func Sort(states []State) {
	sort.Slice(states, func(i, j int) bool { return states[i].MissionID < states[j].MissionID })
}
