package session

// RHZ-142 (FR-RHZ-186): session observation. serve runs `claudePath agents
// --json` on an interval and reconciles the registry with it, through
// session.opened / session.closed only: an unchanged observation writes
// nothing. A claude background session (a pty daemon) is attachable by its
// short id; an interactive one runs in some other terminal and is listed
// only (attachable=false). An observed session that disappears is closed.
// Launcher -p sessions (no Kind) are never touched here: their completion
// closes them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"rhizome/internal/events"
)

// Claude session kinds as `claude agents --json` reports them.
const (
	KindBackground  = "background"
	KindInteractive = "interactive"
)

// Close reasons of the observer.
const (
	ReasonGone    = "observed: session ended"
	ReasonChanged = "observed: session changed"
)

// AgentsTimeout bounds one `claude agents --json` run.
const AgentsTimeout = 10 * time.Second

// ErrAgents is the fixed error of a failed agents run (its output is never
// echoed); the observer writes nothing on it.
var ErrAgents = errors.New("claude agents --json failed")

// Agent is one entry of `claude agents --json`.
type Agent struct {
	ID        string `json:"id"` // short id: what `claude attach` takes
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	State     string `json:"state"`
	Cwd       string `json:"cwd"`
	Pid       int    `json:"pid"`
	StartedAt any    `json:"startedAt"`
}

// Attachable says whether `claude attach` can open the session.
func (a Agent) Attachable() bool { return a.Kind == KindBackground && a.ID != "" }

// StatusText is status, or state when status is absent.
func (a Agent) StatusText() string {
	if a.Status != "" {
		return a.Status
	}
	return a.State
}

// ListAgents runs `claudePath agents --json` and parses its array. Entries
// without a sessionId are dropped.
func ListAgents(claudePath string, timeout time.Duration) ([]Agent, error) {
	if claudePath == "" {
		return nil, ErrAgents
	}
	if timeout <= 0 {
		timeout = AgentsTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudePath, "agents", "--json")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, ErrAgents
	}
	var all []Agent
	if json.NewDecoder(bytes.NewReader(out)).Decode(&all) != nil {
		return nil, ErrAgents
	}
	res := make([]Agent, 0, len(all))
	for _, a := range all {
		if strings.TrimSpace(a.SessionID) != "" {
			res = append(res, a)
		}
	}
	return res, nil
}

// FindAgent returns the agents entry of one session id.
func FindAgent(agents []Agent, sessionID string) (Agent, bool) {
	for _, a := range agents {
		if a.SessionID == sessionID {
			return a, true
		}
	}
	return Agent{}, false
}

// AllSessions replays every session stream, by id.
func AllSessions(store events.Port) ([]Session, error) {
	ids := map[string]bool{}
	for _, e := range store.All() {
		if e.AggregateType == AggregateType {
			ids[e.AggregateID] = true
		}
	}
	out := make([]Session, 0, len(ids))
	for id := range ids {
		s, err := Replay(store.List(AggregateType, id))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// observed is the registry view of one agents entry, merged over the
// session it already has (name, mission and execution links are kept).
func observed(a Agent, prev *Session) Opened {
	p := Opened{SessionID: a.SessionID, Name: a.Name, Mode: Interactive, Cwd: a.Cwd, Kind: a.Kind}
	if a.Attachable() {
		p.AttachID = a.ID
	}
	if prev != nil {
		p.MissionID, p.ExecutionID, p.DriverRef = prev.MissionID, prev.ExecutionID, prev.DriverRef
		if prev.Name != "" {
			p.Name = prev.Name
		}
		if prev.Cwd != "" {
			p.Cwd = prev.Cwd
		}
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = a.ID
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = a.SessionID
	}
	return p
}

// Reconcile applies one agents observation to the registry and returns the
// number of events written. Rules: an unknown or closed session is opened;
// an open one whose kind or attach id differs is closed and reopened with
// the observed values; an open observed session (Kind set) missing from the
// observation is closed. Entries with a non-absolute cwd or an unknown kind
// are skipped (and so, if open and observed before, closed).
func (s Service) Reconcile(agents []Agent) (int, error) {
	if s.Store == nil {
		return 0, errors.New("nil store")
	}
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	all, err := AllSessions(s.Store)
	if err != nil {
		return 0, err
	}
	byID := map[string]*Session{}
	for i := range all {
		byID[all[i].SessionID] = &all[i]
	}
	seen := map[string]bool{}
	writes := 0
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].SessionID < agents[j].SessionID })
	for _, a := range agents {
		if seen[a.SessionID] || !filepath.IsAbs(a.Cwd) || (a.Kind != KindBackground && a.Kind != KindInteractive) {
			continue
		}
		seen[a.SessionID] = true
		prev := byID[a.SessionID]
		switch {
		case prev != nil && prev.State == Open && prev.Kind == "":
			// a launcher -p session: its completion owns the stream
		case prev == nil || prev.State != Open:
			_, err := s.Open(observed(a, prev))
			keep(err)
			if err == nil {
				writes++
			}
		case prev.Kind != a.Kind || prev.AttachID != observed(a, prev).AttachID:
			if _, err := s.Close(a.SessionID, ReasonChanged); err != nil {
				keep(err)
				continue
			}
			writes++
			_, err := s.Open(observed(a, prev))
			keep(err)
			if err == nil {
				writes++
			}
		}
	}
	for _, x := range all {
		if x.State == Open && x.Kind != "" && !seen[x.SessionID] {
			_, err := s.Close(x.SessionID, ReasonGone)
			keep(err)
			if err == nil {
				writes++
			}
		}
	}
	return writes, firstErr
}

// reconcileMu orders Reconcile and Adopt within one process: a session the
// launcher starts with --bg is never half-claimed by a concurrent tick.
var reconcileMu sync.Mutex

// Adopt records a session the launcher started with --bg (RHZ-142). An
// observation tick may have opened it first, unlinked: an open stream with
// another payload is closed and reopened as p; an identical one writes
// nothing.
func (s Service) Adopt(p Opened) (Session, error) {
	reconcileMu.Lock()
	defer reconcileMu.Unlock()
	if log := s.Store.List(AggregateType, p.SessionID); len(log) > 0 {
		cur, err := Replay(log)
		if err != nil {
			return Session{}, err
		}
		if cur.State == Open {
			if cur.Opened == p {
				return cur, nil
			}
			if _, err := s.Close(p.SessionID, ReasonChanged); err != nil {
				return Session{}, err
			}
		}
	}
	return s.Open(p)
}

// Observer runs the agents observation for serve and keeps the latest
// statuses in memory (status changes are not journaled).
type Observer struct {
	Store      events.Port
	ClaudePath string
	Timeout    time.Duration
	// OnWrite runs after an observation that wrote events (serve
	// re-broadcasts the workspace). nil = nothing.
	OnWrite func()
	// OnError receives fixed-text faults. nil = dropped.
	OnError func(string)
	// list overrides ListAgents (tests only).
	list func() ([]Agent, error)

	mu     sync.Mutex
	status map[string]string
}

// Tick runs one observation: a failed agents run writes nothing.
func (o *Observer) Tick() (int, error) {
	list := o.list
	if list == nil {
		list = func() ([]Agent, error) { return ListAgents(o.ClaudePath, o.Timeout) }
	}
	agents, err := list()
	if err != nil {
		return 0, err
	}
	st := map[string]string{}
	for _, a := range agents {
		st[a.SessionID] = a.StatusText()
	}
	o.mu.Lock()
	o.status = st
	o.mu.Unlock()
	n, err := (Service{Store: o.Store}).Reconcile(agents)
	if n > 0 && o.OnWrite != nil {
		o.OnWrite()
	}
	return n, err
}

// Status is the last observed claude status of a session ("" = not seen).
func (o *Observer) Status(sessionID string) string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status[sessionID]
}

// Run ticks every interval until ctx ends; the first tick is immediate.
func (o *Observer) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := o.Tick(); err != nil && o.OnError != nil {
			o.OnError("session observe: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
