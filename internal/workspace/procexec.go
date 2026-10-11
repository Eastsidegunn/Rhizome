package workspace

// RHZ-145 (FR-RHZ-189): the procedure executor — ops' dependency order run
// by the system. It holds no state of its own: every pass recomputes
// assembly.Plan from the journal and applies it through the existing
// relays (mission.start for a ready step; a blocked note + mission blocked
// for a stalled run). A pass over an unchanged journal writes nothing: a
// startable step the launcher refuses (busy, claimed) stays planned and is
// retried on the next pass, in the same order — that is the queue.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"rhizome/internal/assembly"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
	"rhizome/internal/trust"
)

// ProcedureExecutorActor is the actor of every executor relay.
const ProcedureExecutorActor = "procedure-executor"

// DefaultProcedureInterval is the executor's polling period.
const DefaultProcedureInterval = 15 * time.Second

// ProcedureExecutor drives procedure runs whose run mission is running.
type ProcedureExecutor struct {
	Store events.Port
	// Hooks returns the relay seams (Start is the mission.start backend).
	Hooks func() RelayHooks
	// Models are the models the start backend can run; a step naming any
	// other model stalls its run instead of starting. "" always passes.
	Models []string
	// OnWrite runs after a pass that wrote (SSE re-publish).
	OnWrite func()
	// OnError receives refusals and faults, once per (mission, reason).
	OnError func(msg string)

	mu   sync.Mutex
	last map[string]string
	kick chan struct{}
	once sync.Once
}

func (x *ProcedureExecutor) init() {
	x.once.Do(func() { x.kick = make(chan struct{}, 1) })
}

// Kick asks Run for a pass now (mission completed, gate answered, slot
// freed). Non-blocking; kicks coalesce.
func (x *ProcedureExecutor) Kick() {
	x.init()
	select {
	case x.kick <- struct{}{}:
	default:
	}
}

// Run passes once at start (restart recovery), then every interval and on
// every Kick, until ctx ends.
func (x *ProcedureExecutor) Run(ctx context.Context, interval time.Duration) {
	x.init()
	if interval <= 0 {
		interval = DefaultProcedureInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		x.Pass()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-x.kick:
		}
	}
}

// Pass applies the current plan once and returns how many writes it made
// (a start = 1, a stall = 1).
func (x *ProcedureExecutor) Pass() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.Store == nil || storePoisoned(x.Store) {
		return 0
	}
	if x.last == nil {
		x.last = map[string]string{}
	}
	hooks := RelayHooks{}
	if x.Hooks != nil {
		hooks = x.Hooks()
	}
	wrote := 0
	stalled := map[string]bool{}
	for _, a := range assembly.Plan(x.Store) {
		if stalled[a.RunID] {
			continue
		}
		if a.Stall != "" {
			if x.stall(a.RunMissionID, a.Stall) {
				wrote++
			}
			stalled[a.RunID] = true
			continue
		}
		st := a.Start
		if st.Model != "" && !x.modelOK(st.Model) {
			if x.stall(a.RunMissionID, fmt.Sprintf("step %s model %q not in launcher ledger", st.StepID, st.Model)) {
				wrote++
			}
			stalled[a.RunID] = true
			continue
		}
		in := Intent{Kind: "mission.start", MissionID: st.MissionID, Instruction: st.Instruction, Workdir: st.Workdir, SessionMode: st.SessionMode}
		res, err := RelayIntentHooks(x.Store, in, ProcedureExecutorActor, trust.Authority{}, hooks)
		switch {
		case err != nil:
			x.report(st.MissionID, "start: "+err.Error())
		case !res.Accepted:
			x.report(st.MissionID, "start refused: "+res.Reason)
		default:
			delete(x.last, st.MissionID)
			wrote++
		}
	}
	if wrote > 0 && x.OnWrite != nil {
		x.OnWrite()
	}
	return wrote
}

func (x *ProcedureExecutor) modelOK(model string) bool {
	for _, m := range x.Models {
		if m == model {
			return true
		}
	}
	return false
}

// report forwards a refusal once per (mission, reason): a full launcher is
// refused on every pass and must not flood the log.
func (x *ProcedureExecutor) report(missionID, msg string) {
	if x.last[missionID] == msg {
		return
	}
	x.last[missionID] = msg
	if x.OnError != nil {
		x.OnError("procedure executor " + missionID + ": " + msg)
	}
}

type procedureStepDTO struct {
	ID           string   `json:"id"`
	MissionID    string   `json:"missionId"`
	After        []string `json:"after"`
	MissionState string   `json:"missionState"`
	Phase        string   `json:"phase,omitempty"`
	Gate         string   `json:"gate,omitempty"`
	GateID       string   `json:"gateId,omitempty"`
	Workdir      string   `json:"workdir,omitempty"`
	SessionMode  string   `json:"sessionMode,omitempty"`
	Model        string   `json:"model,omitempty"`
}

type procedureRunDTO struct {
	RunID       string             `json:"runId"`
	MissionID   string             `json:"missionId"`
	ProcedureID string             `json:"procedureId"`
	State       string             `json:"state"`
	Driven      bool               `json:"driven"`
	Stalled     string             `json:"stalled,omitempty"`
	Done        int                `json:"done"`
	Total       int                `json:"total"`
	Steps       []procedureStepDTO `json:"steps"`
}

// serveProcedureRun is GET /v1/procedure/runs/<runId>: the executor's view
// of one run (rhizome procedure status). Read only.
func (h *HTTPServer) serveProcedureRun(w http.ResponseWriter, runID string) {
	rs, err := assembly.Status(h.Store, runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	dto := procedureRunDTO{RunID: rs.RunID, MissionID: rs.MissionID, ProcedureID: rs.ProcedureID, State: string(rs.State), Driven: rs.Driven(), Stalled: rs.Stalled, Done: rs.Done, Total: rs.Total, Steps: []procedureStepDTO{}}
	for _, s := range rs.Steps {
		dto.Steps = append(dto.Steps, procedureStepDTO{ID: s.StepID, MissionID: s.MissionID, After: append([]string{}, s.After...), MissionState: string(s.MissionState), Phase: s.Phase, Gate: s.Gate, GateID: s.GateID, Workdir: s.Workdir, SessionMode: s.SessionMode, Model: s.Model})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dto)
}

// stall marks the run for a human: a blocked note on the run mission
// (attention note_blocked), then running → blocked with the reason, after
// which the run is no longer driven. Reports true when it wrote.
func (x *ProcedureExecutor) stall(runMissionID, reason string) bool {
	ms, err := projector.ReplayMission(x.Store.List("mission", runMissionID))
	if err != nil || ms.State != domain.MissionRunning {
		return false
	}
	why := "procedure stalled: " + reason
	note := Intent{Kind: "note.create", MemoryKind: string(memory.Blocked), MissionID: runMissionID, Tags: []string{"procedure-executor"}, Content: why}
	if res, err := RelayIntentHooks(x.Store, note, ProcedureExecutorActor, trust.Authority{}, RelayHooks{}); err != nil || !res.Accepted {
		x.report(runMissionID, fmt.Sprintf("stall note: %v %s", err, res.Reason))
		return false
	}
	if _, err := (mission.Service{Store: x.Store}).TransitionWithReason(runMissionID, ms.Revision, domain.MissionBlocked, why); err != nil {
		x.report(runMissionID, "stall: "+err.Error())
	}
	return true
}
