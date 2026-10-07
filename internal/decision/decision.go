package decision

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"sort"
)

type Kind string

const (
	StartExecution Kind = "start_execution"
	AwaitResult    Kind = "await_result"
	Complete       Kind = "complete"
	Fail           Kind = "fail"
	WaitHuman      Kind = "wait_human"
	Retry          Kind = "retry"
)

type Evidence struct{ SourceType, SourceID string }
type Decision struct {
	ID, MissionID, Reason string
	Kind                  Kind
	Evidence              []Evidence
	NextAction            string
	Sequence              uint64
	TickKey               string
	CorrelationID         string
}
type Service struct{ Store events.Port }
type payload struct {
	ID, MissionID, Reason string
	Kind                  Kind
	Evidence              []Evidence
	NextAction            string
	TickKey               string
	CorrelationID         string
}

func valid(k Kind) bool {
	switch k {
	case StartExecution, AwaitResult, Complete, Fail, WaitHuman, Retry:
		return true
	}
	return false
}
func validatePayload(p payload, aggregate string) error {
	if p.ID == "" || p.ID != aggregate || p.MissionID == "" || p.Reason == "" || !valid(p.Kind) {
		return fmt.Errorf("invalid decision")
	}
	if (p.Kind == Complete || p.Kind == Fail) && len(p.Evidence) == 0 {
		return fmt.Errorf("evidence required")
	}
	for _, e := range p.Evidence {
		if e.SourceType == "" || e.SourceID == "" {
			return fmt.Errorf("invalid evidence")
		}
	}
	return nil
}
func (s Service) Create(d Decision) (Decision, error) {
	return s.create(d)
}
func (s Service) CreateWithTick(d Decision, tick string) (Decision, error) {
	if tick == "" {
		return Decision{}, fmt.Errorf("tick key required")
	}
	d.TickKey = tick
	return s.create(d)
}
func (s Service) CreateWithTickCorrelation(d Decision, tick, correlation string) (Decision, error) {
	d.CorrelationID = correlation
	return s.CreateWithTick(d, tick)
}
func (s Service) create(d Decision) (Decision, error) {
	if s.Store == nil {
		return Decision{}, fmt.Errorf("nil store")
	}
	if d.ID == "" || d.MissionID == "" || !valid(d.Kind) || d.Reason == "" {
		return Decision{}, fmt.Errorf("invalid decision")
	}
	m, e := projector.ReplayMission(s.Store.List("mission", d.MissionID))
	if e != nil || m.State == "succeeded" || m.State == "failed" || m.State == "cancelled" {
		return Decision{}, fmt.Errorf("invalid mission")
	}
	if (d.Kind == Complete || d.Kind == Fail) && (len(d.Evidence) == 0) {
		return Decision{}, fmt.Errorf("evidence required")
	}
	if d.Reason == "" {
		return Decision{}, fmt.Errorf("reason required")
	}
	for _, ev := range d.Evidence {
		if ev.SourceType == "" || ev.SourceID == "" {
			return Decision{}, fmt.Errorf("invalid evidence")
		}
	}
	for _, e := range s.Store.All() {
		if e.AggregateType == "decision" && e.AggregateID == d.ID {
			return Decision{}, fmt.Errorf("duplicate decision")
		}
	}
	p, _ := json.Marshal(payload{d.ID, d.MissionID, d.Reason, d.Kind, d.Evidence, d.NextAction, d.TickKey, d.CorrelationID})
	e = s.Store.Append(0, events.Event{AggregateType: "decision", AggregateID: d.ID, Revision: 1, Type: "decision.created", Payload: p})
	if e != nil {
		return Decision{}, e
	}
	return Replay(s.Store.List("decision", d.ID))
}
func Replay(log []events.Event) (Decision, error) {
	if len(log) != 1 {
		return Decision{}, fmt.Errorf("invalid decision stream")
	}
	e := log[0]
	var p payload
	if e.AggregateType != "decision" || e.Revision != 1 || e.Type != "decision.created" {
		return Decision{}, fmt.Errorf("invalid decision event")
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Decision{}, err
	}
	if e.Sequence == 0 {
		return Decision{}, fmt.Errorf("missing sequence")
	}
	if err := validatePayload(p, e.AggregateID); err != nil {
		return Decision{}, err
	}
	return Decision{p.ID, p.MissionID, p.Reason, p.Kind, p.Evidence, p.NextAction, e.Sequence, p.TickKey, p.CorrelationID}, nil
}
func (s Service) Timeline() ([]Decision, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil store")
	}
	var out []Decision
	for _, e := range s.Store.All() {
		if e.AggregateType != "decision" {
			continue
		}
		d, err := Replay(s.Store.List("decision", e.AggregateID))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}
