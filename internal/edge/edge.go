package edge

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/approval"
	"rhizome/internal/deliverable"
	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/projector"
	"sort"
	"strings"
)

type Endpoint struct{ Type, ID string }
type Kind string

const (
	Dependency Kind = "dependency"
	Spawn      Kind = "spawn"
	Produces   Kind = "produces"
	Gate       Kind = "gate"
	// RHZ-057 (FR-RHZ-087): first-class relations, publish-only in stage 1 —
	// shape checks below, no asymmetry/acyclicity/dedup rules (stage 2).
	Contains Kind = "contains"
	About    Kind = "about"
)

type Edge struct {
	ID                 string
	From, To           Endpoint
	Kind               Kind
	Actor, Correlation string
	Verified           bool
	Supersedes         string
	Revision           uint64
}
type Service struct{ Store events.Port }
type payload struct {
	ID                 string
	From, To           Endpoint
	Kind               Kind
	Actor, Correlation string
	Verified           bool
	Supersedes         string
}

func valid(p payload, agg string) error {
	if p.ID == "" || p.ID != agg || p.From.Type == "" || p.From.ID == "" || p.To.Type == "" || p.To.ID == "" || p.From == p.To || strings.TrimSpace(p.Actor) == "" || strings.TrimSpace(p.Correlation) == "" {
		return fmt.Errorf("invalid edge")
	}
	for _, x := range []string{p.From.Type, p.To.Type} {
		if x != "goal" && x != "mission" && x != "gate" && x != "deliverable" && x != "memory" {
			return fmt.Errorf("invalid endpoint type")
		}
	}
	if p.Kind != Dependency && p.Kind != Spawn && p.Kind != Produces && p.Kind != Gate && p.Kind != Contains && p.Kind != About {
		return fmt.Errorf("invalid edge kind")
	}
	// FR-RHZ-087 shape whitelist: memory endpoints exist only on about edges,
	// and only as the From side.
	if (p.From.Type == "memory" || p.To.Type == "memory") && p.Kind != About {
		return fmt.Errorf("memory endpoint")
	}
	if p.Kind == Contains && (p.From.Type != "goal" || p.To.Type != "goal") {
		return fmt.Errorf("contains endpoint")
	}
	if p.Kind == About && (p.From.Type != "memory" || (p.To.Type != "goal" && p.To.Type != "mission")) {
		return fmt.Errorf("about endpoint")
	}
	if p.Kind == Produces && p.To.Type != "deliverable" {
		return fmt.Errorf("produces target")
	}
	if (p.Kind == Dependency || p.Kind == Spawn) && (p.From.Type == "gate" || p.To.Type == "gate") {
		return fmt.Errorf("gate endpoint")
	}
	if p.Kind == Gate && ((p.From.Type == "gate") == (p.To.Type == "gate")) {
		return fmt.Errorf("gate endpoint")
	}
	if p.Supersedes == p.ID {
		return fmt.Errorf("self supersede")
	}
	if !p.Verified && !strings.HasPrefix(p.Actor, "unverified-local-operator:") {
		return fmt.Errorf("actor prefix")
	}
	return nil
}
func (s Service) exists(e Endpoint) error {
	switch e.Type {
	case "goal":
		_, x := projector.ReplayGoal(s.Store.List("goal", e.ID))
		return x
	case "mission":
		_, x := projector.ReplayMission(s.Store.List("mission", e.ID))
		return x
	case "gate":
		_, x := approval.Replay(s.Store.List("approval", e.ID))
		return x
	case "deliverable":
		_, x := deliverable.Replay(s.Store.List("deliverable", e.ID))
		return x
	case "memory":
		_, x := memory.Replay(s.Store.List("memory", e.ID))
		return x
	}
	return fmt.Errorf("invalid endpoint")
}
func (s Service) Create(x Edge) (Edge, error) {
	if s.Store == nil {
		return Edge{}, fmt.Errorf("nil store")
	}
	if !x.Verified && !strings.HasPrefix(x.Actor, "unverified-local-operator:") {
		x.Actor = "unverified-local-operator:" + x.Actor
	}
	p := payload{x.ID, x.From, x.To, x.Kind, x.Actor, x.Correlation, x.Verified, x.Supersedes}
	if e := valid(p, x.ID); e != nil {
		return Edge{}, e
	}
	if e := s.exists(x.From); e != nil {
		return Edge{}, e
	}
	if e := s.exists(x.To); e != nil {
		return Edge{}, e
	}
	if x.Supersedes != "" {
		if _, e := s.Get(x.Supersedes); e != nil {
			return Edge{}, e
		}
	}
	b, _ := json.Marshal(p)
	ev := events.Event{AggregateType: "edge", AggregateID: x.ID, Revision: 1, Type: "edge.declared", Payload: b}
	if _, e := Replay([]events.Event{ev}); e != nil {
		return Edge{}, e
	}
	if e := s.Store.Append(0, ev); e != nil {
		return Edge{}, e
	}
	return Replay(s.Store.List("edge", x.ID))
}
func (s Service) Rewire(old string, x Edge) (Edge, error) {
	if s.Store == nil {
		return Edge{}, fmt.Errorf("nil store")
	}
	if _, e := s.Get(old); e != nil {
		return Edge{}, e
	}
	if x.ID == old {
		return Edge{}, fmt.Errorf("self supersede")
	}
	x.Supersedes = old
	return s.Create(x)
}
func Replay(log []events.Event) (Edge, error) {
	if len(log) != 1 {
		return Edge{}, fmt.Errorf("invalid edge stream")
	}
	e := log[0]
	if e.AggregateType != "edge" || e.Revision != 1 || e.Type != "edge.declared" {
		return Edge{}, events.ErrRevisionConflict
	}
	var p payload
	if json.Unmarshal(e.Payload, &p) != nil || valid(p, e.AggregateID) != nil {
		return Edge{}, fmt.Errorf("invalid edge")
	}
	return Edge{p.ID, p.From, p.To, p.Kind, p.Actor, p.Correlation, p.Verified, p.Supersedes, 1}, nil
}
func (s Service) Get(id string) (Edge, error) {
	if s.Store == nil {
		return Edge{}, fmt.Errorf("nil store")
	}
	return Replay(s.Store.List("edge", id))
}
func (s Service) ByNode(t, id string) ([]Edge, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil store")
	}
	m := map[string]bool{}
	for _, e := range s.Store.All() {
		if e.AggregateType == "edge" {
			m[e.AggregateID] = true
		}
	}
	out := []Edge{}
	for x := range m {
		r, e := s.Get(x)
		if e != nil {
			return nil, e
		}
		if (r.From.Type == t && r.From.ID == id) || (r.To.Type == t && r.To.ID == id) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s Service) IsSuperseded(id string) (bool, error) {
	if s.Store == nil {
		return false, fmt.Errorf("nil store")
	}
	for _, e := range s.Store.All() {
		if e.AggregateType != "edge" {
			continue
		}
		r, x := s.Get(e.AggregateID)
		if x != nil {
			return false, x
		}
		if r.Supersedes == id {
			return true, nil
		}
	}
	return false, nil
}
