package deliverable

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/projector"
	"rhizome/internal/source"
	"sort"
	"strings"
)

type Deliverable struct {
	ID, Kind, MissionID, SourceRef, Summary string
	// GoalID binds a deliverable to a goal instead of a mission (RHZ-081,
	// FR-RHZ-112): exactly one of MissionID/GoalID is set. Omitted from the
	// payload when empty, so pre-081 streams replay byte-for-byte unchanged.
	GoalID   string
	Revision uint64
}
type Service struct{ Store events.Port }
type payload struct {
	ID, Kind, MissionID, SourceRef, Summary string
	GoalID                                  string `json:",omitempty"`
}

// bound reports whether exactly one of MissionID/GoalID is set (FR-RHZ-112).
func bound(missionID, goalID string) bool { return (missionID == "") != (goalID == "") }

func valid(p payload, agg string, s events.Port) error {
	if p.ID == "" || p.ID != agg || strings.TrimSpace(p.Kind) == "" || !bound(p.MissionID, p.GoalID) || strings.TrimSpace(p.SourceRef) == "" || strings.TrimSpace(p.Summary) == "" {
		return fmt.Errorf("invalid deliverable")
	}
	if p.GoalID != "" {
		if _, e := projector.ReplayGoal(s.List("goal", p.GoalID)); e != nil {
			return fmt.Errorf("goal: %w", e)
		}
	} else if _, e := projector.ReplayMission(s.List("mission", p.MissionID)); e != nil {
		return fmt.Errorf("mission: %w", e)
	}
	if strings.HasPrefix(p.SourceRef, "sha256:") {
		if _, e := (source.Service{Store: s}).Get(p.SourceRef); e != nil {
			return fmt.Errorf("source: %w", e)
		}
	} else if strings.HasPrefix(p.SourceRef, "exec-") {
		x, e := execution.Replay(s.List("execution", p.SourceRef))
		if e != nil {
			return fmt.Errorf("execution: %w", e)
		}
		// RHZ-048 (L-k): an execution source must belong to the deliverable's
		// mission. Cross-aggregate, so it lives here and in Get — the pure
		// Replay cannot reach the store (mission-existence has the same shape).
		// A goal-bound deliverable has no mission, so an exec- source can
		// never belong to it (MissionID "" never equals an execution's).
		if x.MissionID != p.MissionID {
			return fmt.Errorf("execution mission mismatch")
		}
	} else {
		return fmt.Errorf("invalid source ref")
	}
	return nil
}
func (s Service) Create(d Deliverable) (Deliverable, error) {
	if s.Store == nil {
		return Deliverable{}, fmt.Errorf("nil store")
	}
	p := payload{ID: d.ID, Kind: d.Kind, MissionID: d.MissionID, SourceRef: d.SourceRef, Summary: d.Summary, GoalID: d.GoalID}
	if e := valid(p, d.ID, s.Store); e != nil {
		return Deliverable{}, e
	}
	b, _ := json.Marshal(p)
	e := events.Event{AggregateType: "deliverable", AggregateID: d.ID, Revision: 1, Type: "deliverable.declared", Payload: b}
	if _, e2 := Replay([]events.Event{e}); e2 != nil {
		return Deliverable{}, e2
	}
	if err := s.Store.Append(0, e); err != nil {
		return Deliverable{}, err
	}
	return Replay(s.Store.List("deliverable", d.ID))
}
func Replay(log []events.Event) (Deliverable, error) {
	if len(log) != 1 {
		return Deliverable{}, fmt.Errorf("invalid deliverable stream")
	}
	e := log[0]
	if e.AggregateType != "deliverable" || e.Revision != 1 || e.Type != "deliverable.declared" {
		return Deliverable{}, events.ErrRevisionConflict
	}
	var p payload
	if json.Unmarshal(e.Payload, &p) != nil {
		return Deliverable{}, fmt.Errorf("malformed payload")
	}
	d := Deliverable{ID: p.ID, Kind: p.Kind, MissionID: p.MissionID, SourceRef: p.SourceRef, Summary: p.Summary, GoalID: p.GoalID, Revision: 1}
	if p.ID != e.AggregateID || strings.TrimSpace(p.Kind) == "" || !bound(p.MissionID, p.GoalID) || strings.TrimSpace(p.SourceRef) == "" || strings.TrimSpace(p.Summary) == "" || (!strings.HasPrefix(p.SourceRef, "sha256:") && !strings.HasPrefix(p.SourceRef, "exec-")) {
		return Deliverable{}, fmt.Errorf("invalid deliverable")
	}
	return d, nil
}
func (s Service) Get(id string) (Deliverable, error) {
	if s.Store == nil {
		return Deliverable{}, fmt.Errorf("nil store")
	}
	d, e := Replay(s.Store.List("deliverable", id))
	if e != nil {
		return Deliverable{}, e
	}
	if d.GoalID != "" {
		if _, e = projector.ReplayGoal(s.Store.List("goal", d.GoalID)); e != nil {
			return Deliverable{}, e
		}
	} else if _, e = projector.ReplayMission(s.Store.List("mission", d.MissionID)); e != nil {
		return Deliverable{}, e
	}
	// RHZ-048 (L-k): read-side re-verification of the exec-/mission bond, so
	// a tampered stream is rejected on consumption too (service/read 대칭).
	if strings.HasPrefix(d.SourceRef, "exec-") {
		x, e := execution.Replay(s.Store.List("execution", d.SourceRef))
		if e != nil || x.MissionID != d.MissionID {
			return Deliverable{}, fmt.Errorf("execution mission mismatch")
		}
	}
	return d, nil
}
func (s Service) ByMission(mid string) ([]Deliverable, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil store")
	}
	ids := map[string]bool{}
	for _, e := range s.Store.All() {
		if e.AggregateType == "deliverable" {
			ids[e.AggregateID] = true
		}
	}
	out := []Deliverable{}
	for id := range ids {
		d, e := s.Get(id)
		if e != nil {
			return nil, e
		}
		if d.MissionID == mid {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
