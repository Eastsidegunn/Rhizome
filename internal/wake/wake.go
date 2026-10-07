package wake

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"time"
)

type Source string

const (
	Cron    Source = "cron"
	Webhook Source = "webhook"
	Manual  Source = "manual"
	Event   Source = "event"
)

type Wake struct {
	ID                                           string
	Source                                       Source
	RequestedAt                                  time.Time
	Payload                                      []byte
	TargetType, TargetID, CorrelationID, TickKey string
	Sequence                                     uint64
}
type Service struct{ Store events.Port }
type payload struct {
	ID                                           string
	Source                                       Source
	RequestedAt                                  time.Time
	Payload                                      []byte
	TargetType, TargetID, CorrelationID, TickKey string
}

func validPayload(p payload, agg string) error {
	if p.ID == "" || p.ID != agg || p.TargetID == "" || p.CorrelationID == "" {
		return fmt.Errorf("invalid wake")
	}
	if p.Source != Cron && p.Source != Webhook && p.Source != Manual && p.Source != Event {
		return fmt.Errorf("unknown source")
	}
	if p.TargetType != "mission" && p.TargetType != "goal" {
		return fmt.Errorf("unknown target")
	}
	if p.RequestedAt.IsZero() {
		return fmt.Errorf("invalid time")
	}
	return nil
}

func (s Service) Create(w Wake) (Wake, error) {
	if s.Store == nil {
		return Wake{}, fmt.Errorf("nil store")
	}
	if w.ID == "" || w.TargetID == "" || w.RequestedAt.IsZero() {
		return Wake{}, fmt.Errorf("invalid wake")
	}
	if w.Source != Cron && w.Source != Webhook && w.Source != Manual && w.Source != Event {
		return Wake{}, fmt.Errorf("unknown source")
	}
	if w.TargetType == "mission" {
		if _, e := projector.ReplayMission(s.Store.List("mission", w.TargetID)); e != nil {
			return Wake{}, e
		}
	} else if w.TargetType == "goal" {
		if _, e := projector.ReplayGoal(s.Store.List("goal", w.TargetID)); e != nil {
			return Wake{}, e
		}
	} else {
		return Wake{}, fmt.Errorf("invalid target")
	}
	for _, e := range s.Store.All() {
		if e.AggregateType == "wake" && e.AggregateID == w.ID {
			return Wake{}, fmt.Errorf("duplicate wake")
		}
	}
	p, _ := json.Marshal(payload{w.ID, w.Source, w.RequestedAt, append([]byte(nil), w.Payload...), w.TargetType, w.TargetID, w.CorrelationID, w.TickKey})
	var check payload
	_ = json.Unmarshal(p, &check)
	if err := validPayload(check, w.ID); err != nil {
		return Wake{}, err
	}
	e := events.Event{AggregateType: "wake", AggregateID: w.ID, Revision: 1, Type: "wake.created", Payload: p}
	if err := s.Store.Append(0, e); err != nil {
		return Wake{}, err
	}
	return Replay(s.Store.List("wake", w.ID))
}
func Replay(log []events.Event) (Wake, error) {
	if len(log) != 1 {
		return Wake{}, fmt.Errorf("invalid wake stream")
	}
	e := log[0]
	var p payload
	if e.Sequence == 0 || e.AggregateType != "wake" || e.AggregateID == "" || e.Type != "wake.created" || e.Revision != 1 {
		return Wake{}, fmt.Errorf("invalid wake event")
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Wake{}, err
	}
	if err := validPayload(p, e.AggregateID); err != nil {
		return Wake{}, err
	}
	return Wake{p.ID, p.Source, p.RequestedAt, append([]byte(nil), p.Payload...), p.TargetType, p.TargetID, p.CorrelationID, p.TickKey, e.Sequence}, nil
}
