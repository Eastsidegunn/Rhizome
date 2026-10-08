// Package assembly instantiates Procedure templates into kernel objects
// (RHZ-063, FR-RHZ-092). It lives above both kernels: the knowledge kernel
// (procedure) never imports execution spawning (R1), and this package is the
// only one importing procedure and mission together — pinned by the boundary
// test until the RHZ-055 layer map absorbs it. All writes go through kernel
// services on one events.Port (single writer boundary).
package assembly

import (
	"fmt"
	"strings"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

type RunSpec struct {
	ProcedureID string
	GoalID      string
	RunID       string
	Params      map[string]string
	Actor       string
	Authority   trust.Authority
	Correlation string
}

type RunResult struct {
	MissionID string
	// StepMissionIDs in topological spawn order (prerequisites first).
	StepMissionIDs []string
	// GateQuestionIDs (RHZ-083, FR-RHZ-114): the question asked per
	// NeedsGate step, keyed by step mission ID.
	GateQuestionIDs map[string]string
}

// GateTitle is the deterministic title of the gate question assembly.Run asks
// for a NeedsGate step (RHZ-083, FR-RHZ-114): "<run> · <step> 게이트". The
// question ID derives from title+body+recommendation (question.IDFor), so one
// (run, step, template) maps to one question — a re-submitted run cannot
// double-ask.
func GateTitle(runID, stepID string) string { return runID + " · " + stepID + " 게이트" }

// Run spawns one instance of a procedure: 1 run = 1 mission, each step a
// child mission (the cockpit's task node), materialized as spawn edges
// (run→step) and dependency edges (dependent step→prerequisite step). Order
// enforcement is the planner's business (stage 2) — this stage instantiates
// the structure. Inputs are pinned into every mission.created, so the run is
// recomputable without the procedure stream.
//
// RHZ-083 (FR-RHZ-114): every NeedsGate step additionally gets one gate
// question (question.asked via question.Service.Ask — the single writer for
// that aggregate; never a direct Append) bound to the step mission, asked
// right after the step's mission and edges, in TopoOrder. Body is the step
// Action, recommendation the template's Step.Recommendation, requestedBy the
// run actor (Ask applies its own unverified-local-operator convention, as
// the relay's question.ask does), correlation the run correlation.
//
// Zero partial writes still holds: the run/step mission IDs AND the gate
// question IDs are validated before the first append. A re-submitted run is
// refused by the existing "run mission already exists" rule before any
// write, which is what makes re-submission safe; a question ID that already
// exists for a different binding (another mission, a goal, unbound) is a
// collision and also refuses the whole run up front.
func Run(store events.Port, spec RunSpec) (RunResult, error) {
	if store == nil {
		return RunResult{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(spec.ProcedureID) == "" || strings.TrimSpace(spec.GoalID) == "" || strings.TrimSpace(spec.RunID) == "" {
		return RunResult{}, fmt.Errorf("procedure id, goal id and run id are required")
	}
	p, err := (procedure.Service{Store: store}).Get(spec.ProcedureID)
	if err != nil {
		return RunResult{}, fmt.Errorf("procedure: %w", err)
	}
	if _, err := projector.ReplayGoal(store.List("goal", spec.GoalID)); err != nil {
		return RunResult{}, fmt.Errorf("goal: %w", err)
	}
	order, err := procedure.TopoOrder(p.Steps)
	if err != nil {
		return RunResult{}, err
	}
	runMissionID := "mission-" + spec.RunID
	// 선검증: run·step 미션 ID가 전부 미사용이어야 어떤 append도 시작한다
	// (append-only 저널엔 롤백이 없다 — 부분 쓰기 0).
	ids := []string{runMissionID}
	for _, step := range order {
		ids = append(ids, runMissionID+"-"+step.ID)
	}
	for _, id := range ids {
		if len(store.List("mission", id)) > 0 {
			return RunResult{}, fmt.Errorf("mission %q already exists", id)
		}
	}
	// 선검증 2 (RHZ-083): 게이트 question의 actor 불량·ID 충돌도 append 전에
	// 전부 거부한다. actor 규칙은 question.NormalizeActor 그대로(빈 값·맨
	// 접두사 "unverified-local-operator:"도 거부) — edge는 접두사만으로
	// 채워 통과시키므로 여기서 먼저 막아 부분 쓰기 0을 지킨다.
	for _, step := range order {
		if !step.NeedsGate {
			continue
		}
		if _, err := question.NormalizeActor(spec.Actor); err != nil {
			return RunResult{}, fmt.Errorf("actor required: step %q needs a gate question", step.ID)
		}
		stepMissionID := runMissionID + "-" + step.ID
		qid, err := question.IDFor(GateTitle(spec.RunID, step.ID), step.Action, step.Recommendation)
		if err != nil {
			return RunResult{}, err
		}
		log := store.List("question", qid)
		if len(log) == 0 {
			continue
		}
		q, err := question.Replay(log)
		if err != nil {
			return RunResult{}, fmt.Errorf("gate question %q: %w", qid, err)
		}
		if q.MissionID != stepMissionID {
			return RunResult{}, fmt.Errorf("gate question %q for step %q already exists bound to %q", qid, step.ID, q.MissionID)
		}
	}
	success := strings.Join(p.SuccessConditions, "; ")
	if strings.TrimSpace(success) == "" {
		success = "procedure " + p.ID
	}
	corr := spec.Correlation
	if corr == "" {
		corr = "procedure.run:" + spec.RunID
	}
	ms := mission.Service{Store: store}
	es := edge.Service{Store: store}
	qs := question.Service{Store: store}
	if _, err := ms.CreateFromProcedure(runMissionID, spec.GoalID, spec.RunID, success, p.ID, p.Revision, spec.Params); err != nil {
		return RunResult{}, err
	}
	result := RunResult{MissionID: runMissionID, GateQuestionIDs: map[string]string{}}
	for _, step := range order {
		stepMissionID := runMissionID + "-" + step.ID
		if _, err := ms.CreateFromProcedure(stepMissionID, spec.GoalID, step.Action, step.Action, p.ID, p.Revision, spec.Params); err != nil {
			return result, err
		}
		spawn := edge.Spec{ID: "edge-spawn-" + spec.RunID + "-" + step.ID, From: edge.Endpoint{Type: "mission", ID: runMissionID}, To: edge.Endpoint{Type: "mission", ID: stepMissionID}, Kind: edge.Spawn, Actor: spec.Actor, Correlation: corr}
		if _, err := es.Create(spawn, spec.Authority); err != nil {
			return result, err
		}
		for _, after := range step.After {
			dep := edge.Spec{ID: "edge-dep-" + spec.RunID + "-" + step.ID + "-" + after, From: edge.Endpoint{Type: "mission", ID: stepMissionID}, To: edge.Endpoint{Type: "mission", ID: runMissionID + "-" + after}, Kind: edge.Dependency, Actor: spec.Actor, Correlation: corr}
			if _, err := es.Create(dep, spec.Authority); err != nil {
				return result, err
			}
		}
		if step.NeedsGate {
			q, err := qs.Ask(GateTitle(spec.RunID, step.ID), step.Action, step.Recommendation, stepMissionID, "", spec.Actor, corr)
			if err != nil {
				return result, fmt.Errorf("gate question for step %q: %w", step.ID, err)
			}
			result.GateQuestionIDs[stepMissionID] = q.ID
		}
		result.StepMissionIDs = append(result.StepMissionIDs, stepMissionID)
	}
	return result, nil
}
