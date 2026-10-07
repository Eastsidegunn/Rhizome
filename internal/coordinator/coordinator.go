package coordinator

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/decision"
	"rhizome/internal/deliverable"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/memory"
	"rhizome/internal/projector"
	"rhizome/internal/source"
	"rhizome/internal/wake"
	"sync"
)

type Input struct {
	Mission   domain.Mission
	Decisions []decision.Decision
}

func (c *Coordinator) HandleWake(wakeID string, p Planner) (decision.Decision, error) {
	if c == nil || c.Store == nil {
		return decision.Decision{}, fmt.Errorf("nil store")
	}
	if p == nil {
		return decision.Decision{}, fmt.Errorf("nil planner")
	}
	ws, err := wake.Replay(c.Store.List("wake", wakeID))
	if err != nil {
		return decision.Decision{}, err
	}
	if ws.TargetType != "mission" {
		return decision.Decision{}, fmt.Errorf("goal wake unsupported")
	}
	if ws.TickKey == "" || ws.CorrelationID == "" {
		return decision.Decision{}, fmt.Errorf("wake correlation/tick required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		c.done = map[string]bool{}
	}
	if c.done[ws.TickKey] {
		return decision.Decision{}, events.ErrRevisionConflict
	}
	if tl, e := (decision.Service{Store: c.Store}).Timeline(); e == nil {
		for _, x := range tl {
			if x.TickKey == ws.TickKey {
				return decision.Decision{}, events.ErrRevisionConflict
			}
		}
	}
	m, err := projector.ReplayMission(c.Store.List("mission", ws.TargetID))
	if err != nil {
		return decision.Decision{}, err
	}
	timeline, err := (decision.Service{Store: c.Store}).Timeline()
	if err != nil {
		return decision.Decision{}, err
	}
	d, err := p.Plan(Input{m, timeline})
	if err != nil {
		return decision.Decision{}, err
	}
	got, err := (decision.Service{Store: c.Store}).CreateWithTickCorrelation(d, ws.TickKey, ws.CorrelationID)
	if err != nil {
		return decision.Decision{}, err
	}
	c.done[ws.TickKey] = true
	return got, nil
}

// DeriveDeliverable computes the deterministic completion record for a
// Complete decision (FR-RHZ-079, RHZ-048): ID "deliv-"+MissionID (D31), Kind
// "completion" (D29), Summary = Decision.Reason verbatim (D30), SourceRef =
// the first execution evidence in document order (D27). When no execution
// evidence exists the second value is true: the caller registers the Reason
// bytes as a source blob and uses that content-addressed sha256: id (D28) —
// still fully deterministic. Pure: no clock, no randomness, no store.
func DeriveDeliverable(d decision.Decision) (deliverable.Deliverable, bool) {
	out := deliverable.Deliverable{ID: "deliv-" + d.MissionID, Kind: "completion", MissionID: d.MissionID, Summary: d.Reason}
	for _, ev := range d.Evidence {
		if ev.SourceType == "execution" {
			out.SourceRef = ev.SourceID
			return out, false
		}
	}
	return out, true
}

// ensureDeliverable makes the completion record exist for a Complete
// decision. Skip rule (D31/D33): the mission already has any deliverable —
// this protects the legacy hand-recorded entries whose IDs predate the
// derived scheme. Registration and creation are idempotent or revision-
// guarded, so crash retries converge; an identity collision (the derived ID
// taken by another mission's record) surfaces as a revision conflict (D34).
func (c *Coordinator) ensureDeliverable(d decision.Decision) error {
	existing, err := (deliverable.Service{Store: c.Store}).ByMission(d.MissionID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	out, register := DeriveDeliverable(d)
	if register {
		sr, err := (source.Service{Store: c.Store}).Register([]byte(d.Reason), "text/markdown", "decision://"+d.ID)
		if err != nil {
			return err
		}
		out.SourceRef = sr.BlobID
	}
	_, err = (deliverable.Service{Store: c.Store}).Create(out)
	return err
}

func (c *Coordinator) ApplyDecision(id string) (domain.Mission, error) {
	if c == nil || c.Store == nil {
		return domain.Mission{}, fmt.Errorf("nil store")
	}
	d, err := decision.Replay(c.Store.List("decision", id))
	if err != nil {
		return domain.Mission{}, err
	}
	if d.Kind != decision.Complete && d.Kind != decision.Fail {
		return domain.Mission{}, fmt.Errorf("decision not terminal")
	}
	m, err := projector.ReplayMission(c.Store.List("mission", d.MissionID))
	if err != nil {
		return domain.Mission{}, err
	}
	if (d.Kind == decision.Complete && m.State == domain.MissionSucceeded) || (d.Kind == decision.Fail && m.State == domain.MissionFailed) {
		if m.TerminalDecisionID == d.ID {
			// D33: a terminal re-apply backfills a missing completion record
			// (self-healing); the ByMission skip protects legacy entries.
			if d.Kind == decision.Complete {
				if err := c.ensureDeliverable(d); err != nil {
					return domain.Mission{}, err
				}
			}
			return m, nil
		}
	}
	for _, ev := range d.Evidence {
		if ev.SourceType != "mission" && ev.SourceType != "goal" && ev.SourceType != "memory" && ev.SourceType != "execution" {
			return domain.Mission{}, fmt.Errorf("unsupported evidence")
		}
		found := false
		var seq uint64
		for _, x := range c.Store.All() {
			if x.AggregateType == ev.SourceType && x.AggregateID == ev.SourceID {
				found = true
				seq = x.Sequence
			}
		}
		if !found || seq >= c.Store.List("decision", id)[0].Sequence {
			return domain.Mission{}, fmt.Errorf("evidence missing")
		}
		if ev.SourceType == "mission" && ev.SourceID != d.MissionID {
			return domain.Mission{}, fmt.Errorf("evidence mission mismatch")
		}
		if ev.SourceType == "goal" {
			g, e := projector.ReplayGoal(c.Store.List("goal", ev.SourceID))
			if e != nil || g.ID == "" || g.ID != m.GoalID {
				return domain.Mission{}, fmt.Errorf("goal evidence mismatch")
			}
		}
		if ev.SourceType == "memory" {
			x, e := memory.Replay(c.Store.List("memory", ev.SourceID))
			if e != nil || x.MissionID != d.MissionID {
				return domain.Mission{}, fmt.Errorf("memory evidence mismatch")
			}
		}
		if ev.SourceType == "execution" {
			x, e := execution.Replay(c.Store.List("execution", ev.SourceID))
			if e != nil || x.MissionID != d.MissionID {
				return domain.Mission{}, fmt.Errorf("execution evidence mismatch")
			}
		}
	}
	if d.Kind == decision.Complete && m.State == domain.MissionSucceeded || d.Kind == decision.Fail && m.State == domain.MissionFailed {
		if m.TerminalDecisionID != d.ID {
			return domain.Mission{}, fmt.Errorf("different terminal decision")
		}
		return m, nil
	}
	if m.State == domain.MissionSucceeded || m.State == domain.MissionFailed || m.State == domain.MissionCancelled {
		return domain.Mission{}, fmt.Errorf("terminal mismatch")
	}
	to := domain.MissionSucceeded
	if d.Kind == decision.Fail {
		to = domain.MissionFailed
	}
	if _, err := m.Transition(to); err != nil {
		return domain.Mission{}, err
	}
	// FR-RHZ-079 (D32): the completion record is durable BEFORE the mission
	// transition. The reverse order would let a crash strand a succeeded
	// mission whose re-apply hits the idempotent return with no record ever
	// derived. A derivation failure therefore surfaces here with the mission
	// untouched, and a retry converges. Fail decisions derive nothing (D35):
	// the failure is already durable as decision + mission.failed.
	if d.Kind == decision.Complete {
		if err := c.ensureDeliverable(d); err != nil {
			return domain.Mission{}, err
		}
	}
	p, _ := json.Marshal(struct {
		To         domain.MissionState
		DecisionID string
	}{to, id})
	e := events.Event{AggregateType: "mission", AggregateID: m.ID, Revision: m.Revision + 1, Type: "mission.transitioned", Payload: p, CorrelationID: d.CorrelationID}
	if err := c.Store.Append(m.Revision, e); err != nil {
		return domain.Mission{}, err
	}
	return projector.ReplayMission(c.Store.List("mission", m.ID))
}

type Planner interface {
	Plan(Input) (decision.Decision, error)
}
type Coordinator struct {
	Store events.Port
	mu    sync.Mutex
	done  map[string]bool
}

func (c *Coordinator) Tick(key, missionID string, p Planner) (decision.Decision, error) {
	if c == nil || c.Store == nil {
		return decision.Decision{}, fmt.Errorf("nil store")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		c.done = map[string]bool{}
	}
	if key == "" || missionID == "" || p == nil {
		return decision.Decision{}, fmt.Errorf("invalid tick")
	}
	if c.done[key] {
		return decision.Decision{}, events.ErrRevisionConflict
	}
	m, err := projector.ReplayMission(c.Store.List("mission", missionID))
	if err != nil {
		return decision.Decision{}, err
	}
	timeline, err := (decision.Service{Store: c.Store}).Timeline()
	if err != nil {
		return decision.Decision{}, err
	}
	for _, x := range timeline {
		if x.TickKey == key {
			return decision.Decision{}, events.ErrRevisionConflict
		}
	}
	d, err := p.Plan(Input{m, timeline})
	if err != nil {
		return decision.Decision{}, err
	}
	if d.ID == "" {
		return decision.Decision{}, fmt.Errorf("invalid planner output")
	}
	got, err := (decision.Service{Store: c.Store}).CreateWithTick(d, key)
	if err != nil {
		return decision.Decision{}, err
	}
	c.done[key] = true
	return got, nil
}
