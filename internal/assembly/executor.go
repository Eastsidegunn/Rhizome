package assembly

// RHZ-145 (FR-RHZ-189): the procedure executor's judgment. Plan and Status
// are pure functions of the journal — no clock, no in-memory queue — so a
// restarted serve recomputes exactly where every run stands, and the same
// journal always yields the same actions. The driver (workspace) applies
// them through the existing relays only: mission.start for a ready step,
// mission blocked (with reason) for a stalled run.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
	"rhizome/internal/question"
)

// Step states of Status beyond the mission state vocabulary.
const (
	StepWaiting     = "waiting"      // a prerequisite is not succeeded yet
	StepGatePending = "gate-pending" // prerequisites done, gate not approved
	StepReady       = "ready"        // startable now
)

// StepStatus is one step of a run as the executor sees it.
type StepStatus struct {
	StepID, MissionID string
	After             []string
	// MissionState is the step mission's state; Phase is the executor's
	// reading of a not-yet-started step (StepWaiting/StepGatePending/
	// StepReady), "" once the step mission has left planned/ready.
	MissionState domain.MissionState
	Phase        string
	// Gate is the gate question's decision ("" = no gate, "open" = asked
	// and unanswered).
	Gate, GateID string
	// Launch inputs resolved from the step, then the run params.
	Instruction, Workdir, SessionMode, Model string
}

// RunStatus is one procedure run: its run mission and steps in TopoOrder.
type RunStatus struct {
	RunID, MissionID, ProcedureID string
	State                         domain.MissionState
	Steps                         []StepStatus
	Done, Total                   int
	// Stalled names why the run cannot continue ("" = it can): a step
	// failed or was cancelled, or a gate was rejected.
	Stalled string
}

// Driven reports whether the executor acts on this run: only a run mission
// that is running (procedure.run with execute moves it there).
func (r RunStatus) Driven() bool { return r.State == domain.MissionRunning }

// Action is one executor write: Start a ready step, or Stall the run.
type Action struct {
	RunID, RunMissionID string
	Start               *StepStatus
	Stall               string
}

type runCreated struct {
	ID, Description   string
	ProcedureID       string            `json:"procedure_id"`
	ProcedureRevision uint64            `json:"procedure_revision"`
	Params            map[string]string `json:"params"`
}

// RunIDs lists every procedure run in the journal, sorted. A run mission is
// a procedure-created mission whose ID is "mission-" + its description (the
// run ID, as Run writes it) and whose step missions all exist.
func RunIDs(store events.Port) []string {
	var out []string
	for _, e := range store.All() {
		if e.AggregateType != "mission" || e.Type != "mission.created" || e.Revision != 1 {
			continue
		}
		var c runCreated
		if json.Unmarshal(e.Payload, &c) != nil || c.ProcedureID == "" || c.ID != "mission-"+c.Description {
			continue
		}
		if _, err := Status(store, c.Description); err == nil {
			out = append(out, c.Description)
		}
	}
	sort.Strings(out)
	return out
}

// Status recomputes one run from the journal.
func Status(store events.Port, runID string) (RunStatus, error) {
	if store == nil {
		return RunStatus{}, fmt.Errorf("nil event store")
	}
	runMissionID := "mission-" + runID
	log := store.List("mission", runMissionID)
	if len(log) == 0 {
		return RunStatus{}, fmt.Errorf("unknown run %q", runID)
	}
	var c runCreated
	if json.Unmarshal(log[0].Payload, &c) != nil || c.ProcedureID == "" || c.Description != runID {
		return RunStatus{}, fmt.Errorf("mission %q is not a procedure run", runMissionID)
	}
	rm, err := projector.ReplayMission(log)
	if err != nil {
		return RunStatus{}, err
	}
	p, err := (procedure.Service{Store: store}).Get(c.ProcedureID)
	if err != nil {
		return RunStatus{}, fmt.Errorf("procedure: %w", err)
	}
	order, err := procedure.TopoOrder(p.Steps)
	if err != nil {
		return RunStatus{}, err
	}
	rs := RunStatus{RunID: runID, MissionID: runMissionID, ProcedureID: p.ID, State: rm.State, Total: len(order)}
	state := map[string]domain.MissionState{}
	for _, step := range order {
		id := runMissionID + "-" + step.ID
		sm, err := projector.ReplayMission(store.List("mission", id))
		if err != nil {
			return RunStatus{}, fmt.Errorf("step %q: %w", step.ID, err)
		}
		state[step.ID] = sm.State
		st := StepStatus{StepID: step.ID, MissionID: id, After: append([]string(nil), step.After...), MissionState: sm.State}
		st.Instruction, st.Workdir, st.SessionMode, st.Model = launchInputs(step, c.Params)
		if step.NeedsGate {
			st.GateID, err = question.IDFor(GateTitle(runID, step.ID), step.Action, step.Recommendation)
			if err != nil {
				return RunStatus{}, err
			}
			st.Gate = "open"
			if q, err := question.Replay(store.List("question", st.GateID)); err == nil && q.Decision.Terminal() {
				st.Gate = string(q.Decision)
			}
		}
		switch sm.State {
		case domain.MissionSucceeded:
			rs.Done++
		case domain.MissionFailed, domain.MissionCancelled:
			if rs.Stalled == "" {
				rs.Stalled = fmt.Sprintf("step %s %s", step.ID, sm.State)
			}
		case domain.MissionPlanned, domain.MissionReady:
			st.Phase = StepReady
			for _, a := range step.After {
				if state[a] != domain.MissionSucceeded {
					st.Phase = StepWaiting
				}
			}
			if st.Phase == StepReady && step.NeedsGate && st.Gate != string(question.Approve) {
				st.Phase = StepGatePending
				if st.Gate == string(question.Reject) && rs.Stalled == "" {
					rs.Stalled = fmt.Sprintf("step %s gate rejected", step.ID)
				}
			}
		}
		rs.Steps = append(rs.Steps, st)
	}
	return rs, nil
}

// launchInputs resolves a step's mission.start inputs: the step field, else
// the run param of the same name, else "" (launcher default). Run params
// are appended to the instruction so the session sees them.
func launchInputs(step procedure.Step, params map[string]string) (instruction, workdir, mode, model string) {
	pick := func(v, key string) string {
		if strings.TrimSpace(v) != "" {
			return v
		}
		return params[key]
	}
	instruction = step.Instruction
	if strings.TrimSpace(instruction) == "" {
		instruction = step.Action
	}
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(instruction)
		b.WriteString("\n\nrun params:")
		for _, k := range keys {
			b.WriteString("\n- " + k + "=" + params[k])
		}
		instruction = b.String()
	}
	return instruction, pick(step.Workdir, "workdir"), pick(step.SessionMode, "sessionMode"), pick(step.Model, "model")
}

// Plan is every executor action the journal currently calls for, in run ID
// then step TopoOrder: deterministic, so a full launcher queues the rest in
// the same order on the next pass. A stalled run only stalls (no starts).
func Plan(store events.Port) []Action {
	var out []Action
	for _, id := range RunIDs(store) {
		rs, err := Status(store, id)
		if err != nil || !rs.Driven() {
			continue
		}
		if rs.Stalled != "" {
			out = append(out, Action{RunID: id, RunMissionID: rs.MissionID, Stall: rs.Stalled})
			continue
		}
		for i := range rs.Steps {
			if rs.Steps[i].Phase == StepReady {
				st := rs.Steps[i]
				out = append(out, Action{RunID: id, RunMissionID: rs.MissionID, Start: &st})
			}
		}
	}
	return out
}
