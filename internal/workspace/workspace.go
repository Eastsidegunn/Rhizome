package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"rhizome/internal/approval"
	"rhizome/internal/assembly"
	"rhizome/internal/decision"
	"rhizome/internal/deliverable"
	"rhizome/internal/domain"
	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/gaterequest"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
	"rhizome/internal/question"
	"rhizome/internal/source"
	"rhizome/internal/surface"
	"sort"
	"strings"
	"time"
)

type Mission struct {
	ID, Name string
	// State is the goal lifecycle state (RHZ-061, FR-RHZ-090): the cockpit
	// cannot close or sweep what it cannot see.
	State string
	// Success carries the goal's success criterion (RHZ-067, FR-RHZ-096):
	// the one goal detail the surface was missing.
	Success   string
	Attention bool
}
type Task struct {
	ID, MissionID, Name, State, CurrentAction, BlockedReason string
	Progress                                                 float64
	HasProgress, Attention                                   bool
	// Assignee is the mission's current assignee (RHZ-080, FR-RHZ-111);
	// "" = unassigned.
	Assignee string
}
type Gate struct {
	ID, State, HumanDecision, JanusDecision, MissionID, Name string
	// GoalID is set for goal-bound internal gates (RHZ-075, FR-RHZ-108);
	// JANUS gates never carry one.
	GoalID     string
	Superseded bool
	// RHZ-047 (D19, additive): digest and display material for the gate —
	// digest from the durable input or the surfaced pending record,
	// DisplaySummary/ExpiresAt from the pending record when one exists.
	// ExpiresAt is display-only and never compared to a clock (D25).
	RequestDigest, DisplaySummary                           string
	ExpiresAt                                               int64
	Source, Body, Recommendation, DecisionReason, DecidedBy string
	// DecidedAt is derived on read from the terminal decision event envelope.
	// It is never persisted in a domain payload.
	DecidedAt    string
	Verification *GateVerification
}
type GateVerification struct{ Status, ClaimKind string }
type AttentionItem struct{ Kind, RefID, Cause, SourceRef, IncidentRef string }

// Capability levels (RHZ-070, FR-RHZ-099) — the cockpit vocabulary the
// adapter already reads: enabled / disabled / hidden.
const (
	capEnabled  = "enabled"
	capDisabled = "disabled"
	capHidden   = "hidden"
)

// TaskCapabilities is what the operator may do to one task (mission), derived
// strictly from the domain transition table and the task.pause/task.resume
// relay guards in RelayIntent — never wider than what the relay accepts.
type TaskCapabilities struct{ Pause, Resume, Instruct string }

// GateCapabilities is what the operator may do to one internal gate
// (question). JANUS gates (approval/approvalrequest) are out of scope and
// absent from the map.
type GateCapabilities struct{ Approve, Reject, RequestChanges string }
type Projection struct {
	Revision     uint64
	Missions     []Mission
	Tasks        []Task
	Gates        []Gate
	Deliverables []deliverable.Deliverable
	Edges        []edge.Edge
	Counts       struct{ Running, NeedsYou, Blocked int }
	Attention    []AttentionItem
	// Capabilities/GateCapabilities (RHZ-070, FR-RHZ-099) are always non-nil
	// (empty → {} on the wire), keyed by task id / gate id.
	Capabilities     map[string]TaskCapabilities
	GateCapabilities map[string]GateCapabilities
	// handles (RHZ-073, FR-RHZ-103) is the handle index for this journal
	// state, consumed by toDTO; derived from the same All() scan as Revision.
	handles handleIndex
	// deliverableSeq (RHZ-089, FR-RHZ-120) is the journal Sequence of each
	// deliverable's first event — its registration order — consumed only by
	// the ?deliverableOrder=desc filter of GET /v1/workspace (http.go).
	// Derived from the same All() scan as Revision; never serialized.
	deliverableSeq map[string]uint64
}

func Snapshot(s events.Port) (Projection, error) {
	var p Projection
	if s == nil {
		return p, fmt.Errorf("nil store")
	}
	p.Capabilities, p.GateCapabilities = map[string]TaskCapabilities{}, map[string]GateCapabilities{}
	all := s.All()
	p.handles = buildHandleIndex(all)
	goals, mids, gids, qids, dids, eids, reqs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range all {
		if e.Sequence > p.Revision {
			p.Revision = e.Sequence
		}
		switch e.AggregateType {
		case "goal":
			goals[e.AggregateID] = true
		case "mission":
			mids[e.AggregateID] = true
		case "approval":
			gids[e.AggregateID] = true
		case "question":
			qids[e.AggregateID] = true
		case "deliverable":
			if !dids[e.AggregateID] {
				if p.deliverableSeq == nil {
					p.deliverableSeq = map[string]uint64{}
				}
				p.deliverableSeq[e.AggregateID] = e.Sequence
			}
			dids[e.AggregateID] = true
		case "edge":
			eids[e.AggregateID] = true
		case "approvalrequest":
			reqs[e.AggregateID] = true
		}
	}
	ids := func(m map[string]bool) []string {
		a := make([]string, 0, len(m))
		for id := range m {
			a = append(a, id)
		}
		sort.Strings(a)
		return a
	}
	for _, id := range ids(goals) {
		g, e := projector.ReplayGoal(s.List("goal", id))
		if e != nil {
			return p, e
		}
		p.Missions = append(p.Missions, Mission{ID: g.ID, Name: g.Description, State: string(g.State), Success: g.Success})
	}
	for _, id := range ids(mids) {
		m, x := projector.ReplayMission(s.List("mission", id))
		if x != nil {
			return p, x
		}
		sv, x := (surface.Service{Store: s}).ByMission(m.ID)
		if x != nil && len(s.List("surface", "surface-"+m.ID)) > 0 {
			return p, x
		}
		// RHZ-082 (FR-RHZ-113): the lifecycle BlockedReason (blocked
		// transition) wins while the mission is blocked; otherwise the
		// display-only reason recorded by mission.progress shows.
		// The display reason must not outlive the work (it would otherwise stick
		// on completed missions): hidden once the mission is terminal, shown in
		// every non-terminal state (incl. ready after an unblock — P4).
		blocked := ""
		switch m.State {
		case domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled:
		default:
			blocked = sv.BlockedReason
		}
		if m.State == domain.MissionBlocked && m.BlockedReason != "" {
			blocked = m.BlockedReason
		}
		t := Task{ID: m.ID, MissionID: m.GoalID, Name: m.Description, State: mapState(m.State), BlockedReason: blocked, Attention: m.State == domain.MissionWaitingHuman, CurrentAction: sv.CurrentAction, Progress: sv.Progress, HasProgress: sv.HasProgress, Assignee: m.Assignee}
		p.Tasks = append(p.Tasks, t)
		p.Capabilities[m.ID] = taskCapabilities(m.State)
		if m.State == domain.MissionWaitingHuman {
			p.Counts.NeedsYou++
			p.Attention = append(p.Attention, AttentionItem{"waiting_for_human", m.ID, "mission waiting", "", ""})
		}
	}
	for _, id := range ids(gids) {
		a, x := (approval.Service{Store: s}).Get(id)
		if x != nil {
			return p, x
		}
		st := "pending"
		if a.State == approval.Observed {
			if a.JanusDecision == approval.Allow {
				st = "approved"
			} else {
				st = "rejected"
			}
		}
		sup, _ := (approval.Service{Store: s}).IsSuperseded(id)
		g := Gate{ID: a.ID, Source: "janus", State: st, HumanDecision: string(a.HumanDecision), JanusDecision: string(a.JanusDecision), Superseded: sup, Name: a.GateName, RequestDigest: a.RequestDigest, Verification: approvalVerification(a)}
		if (st == "approved" || st == "rejected") && a.HumanDecision != "" {
			g.DecidedAt = latestEventCreatedAt(s.List("approval", id), "approval.input_recorded")
		}
		if a.DecisionID != "" {
			d, er := decision.Replay(s.List("decision", a.DecisionID))
			if er != nil {
				return p, er
			}
			g.MissionID = d.MissionID
		}
		// A surfaced pending record for the same identity contributes its
		// display material even after the input landed (Q2 continuity).
		if reqs[id] {
			gr, er := gaterequest.Replay(s.List("approvalrequest", id))
			if er != nil {
				return p, er
			}
			g.DisplaySummary, g.ExpiresAt = gr.DisplaySummary, gr.ExpiresAt
			if g.MissionID == "" {
				g.MissionID = gr.MissionID
			}
			if g.Name == "" {
				g.Name = gr.Name
			}
		}
		p.Gates = append(p.Gates, g)
	}
	// RHZ-047: surfaced requests without a durable human input appear as
	// pending gates under the SAME id the eventual input will use.
	for _, id := range ids(reqs) {
		if gids[id] {
			continue
		}
		gr, er := gaterequest.Replay(s.List("approvalrequest", id))
		if er != nil {
			return p, er
		}
		p.Gates = append(p.Gates, Gate{ID: gr.ID, Source: "janus", State: "pending", Name: gr.Name, MissionID: gr.MissionID, RequestDigest: gr.RequestDigest, DisplaySummary: gr.DisplaySummary, ExpiresAt: gr.ExpiresAt})
	}
	for _, id := range ids(qids) {
		q, x := (question.Service{Store: s}).Get(id)
		if x != nil {
			return p, x
		}
		st := questionState(q)
		g := Gate{ID: q.ID, State: st, MissionID: q.MissionID, GoalID: q.GoalID, Name: q.Title, RequestDigest: q.Digest, Source: "internal", Body: q.Body, Recommendation: q.Recommendation, DecisionReason: q.Reason, DecidedBy: q.ActorRef, Verification: claimedVerification(q.Verification)}
		if st == "approved" || st == "rejected" {
			g.DecidedAt = latestEventCreatedAt(s.List("question", id), "question.answered")
		}
		p.Gates = append(p.Gates, g)
		p.GateCapabilities[q.ID] = gateCapabilities(st, q.MissionID != "" || q.GoalID != "")
		// RHZ-085 (FR-RHZ-115): counts.needsYou = missions waiting_for_human
		// + internal gates (questions) pending. A pending question is a ball in
		// the human's court; changes_requested (RHZ-078) is not counted — the
		// worker has the ball until it re-asks or the human approves/rejects.
		// JANUS gates (approval/approvalrequest) keep their existing (zero)
		// contribution: their pending state is JANUS's to resolve.
		if st == "pending" {
			p.Counts.NeedsYou++
		}
	}
	for _, id := range ids(dids) {
		d, x := (deliverable.Service{Store: s}).Get(id)
		if x != nil {
			return p, x
		}
		p.Deliverables = append(p.Deliverables, d)
	}
	for _, id := range ids(eids) {
		x, er := (edge.Service{Store: s}).Get(id)
		if er != nil {
			return p, er
		}
		p.Edges = append(p.Edges, x)
	}
	execs := map[string]bool{}
	for _, e := range all {
		if e.AggregateType == "execution" {
			execs[e.AggregateID] = true
		}
	}
	seenIncidents := map[string]bool{}
	for _, id := range ids(execs) {
		r, er := execution.Replay(s.List("execution", id))
		if er != nil {
			return p, er
		}
		if r.State != execution.Unknown {
			continue
		}
		switch r.UnknownClass {
		case execution.ExternalEffectPossible, execution.DeadlineNear, execution.NeedsHuman:
			p.Attention = append(p.Attention, AttentionItem{Kind: "execution_unknown", RefID: r.ID, Cause: string(r.UnknownClass), SourceRef: r.SourceRef, IncidentRef: r.IncidentRef})
		case execution.Repeated:
			if r.IncidentRef != "" && !seenIncidents[r.IncidentRef] {
				seenIncidents[r.IncidentRef] = true
				p.Attention = append(p.Attention, AttentionItem{Kind: "execution_unknown", RefID: r.ID, Cause: string(r.UnknownClass), SourceRef: r.SourceRef, IncidentRef: r.IncidentRef})
			}
		}
	}
	for _, t := range p.Tasks {
		if t.State == "running" {
			p.Counts.Running++
		}
		if t.State == "blocked" {
			p.Counts.Blocked++
		}
	}
	sort.Slice(p.Attention, func(i, j int) bool {
		if p.Attention[i].Kind != p.Attention[j].Kind {
			return p.Attention[i].Kind < p.Attention[j].Kind
		}
		if p.Attention[i].RefID != p.Attention[j].RefID {
			return p.Attention[i].RefID < p.Attention[j].RefID
		}
		return p.Attention[i].IncidentRef < p.Attention[j].IncidentRef
	})
	sort.Slice(p.Missions, func(i, j int) bool { return p.Missions[i].ID < p.Missions[j].ID })
	sort.Slice(p.Tasks, func(i, j int) bool { return p.Tasks[i].ID < p.Tasks[j].ID })
	sort.Slice(p.Gates, func(i, j int) bool { return p.Gates[i].ID < p.Gates[j].ID })
	sort.Slice(p.Deliverables, func(i, j int) bool { return p.Deliverables[i].ID < p.Deliverables[j].ID })
	sort.Slice(p.Edges, func(i, j int) bool { return p.Edges[i].ID < p.Edges[j].ID })
	return p, nil
}

// latestEventCreatedAt returns the event-envelope time in an unambiguous UTC
// RFC3339 representation. RFC3339Nano preserves envelope precision, so two
// decisions made within one second can still be ranked honestly.
func latestEventCreatedAt(log []events.Event, eventType string) string {
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].Type == eventType {
			return log[i].CreatedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

func claimedVerification(claim *question.Verification) *GateVerification {
	if claim == nil {
		return nil
	}
	return &GateVerification{Status: "claimed", ClaimKind: claim.ClaimKind}
}

func approvalVerification(a approval.Ref) *GateVerification {
	if a.Verification != nil {
		return claimedVerification(a.Verification)
	}
	if a.ActorVerified {
		return &GateVerification{Status: "legacy-asserted"}
	}
	return nil
}

// questionState maps an internal gate (question) to the cockpit state
// vocabulary. Shared by Snapshot and the /v1/context steps[] projection
// (RHZ-068, FR-RHZ-097) so the two surfaces cannot drift.
func questionState(q question.Ref) string {
	switch q.Decision {
	case question.Approve:
		return "approved"
	case question.Reject:
		return "rejected"
	case question.RequestChanges:
		// RHZ-078 (FR-RHZ-109): non-terminal — the gate still awaits approve/reject.
		return "changes_requested"
	}
	return "pending"
}

// taskCapabilities (RHZ-070, FR-RHZ-099) derives the operator capability
// levels for a mission from its domain state. The rule is a function of
// domain.validMissionTransition AND the task.pause/task.resume guards in
// RelayIntent: pause is enabled only where running→paused is valid (running);
// resume only where the relay accepts (planned/ready/paused — waiting_for_human
// →running is domain-valid but the relay guard rejects it, so it is disabled
// here too). Terminal states hide pause/resume/instruct; everything else is
// disabled rather than hidden. A capability must never claim more than the
// relay will accept; capability_projection_test T1 pins that equality.
func taskCapabilities(st domain.MissionState) TaskCapabilities {
	switch st {
	case domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled:
		return TaskCapabilities{Pause: capHidden, Resume: capHidden, Instruct: capHidden}
	}
	c := TaskCapabilities{Pause: capDisabled, Resume: capDisabled, Instruct: capEnabled}
	if st == domain.MissionRunning {
		c.Pause = capEnabled
	}
	if st == domain.MissionPlanned || st == domain.MissionReady || st == domain.MissionPaused {
		c.Resume = capEnabled
	}
	return c
}

// gateCapabilities (RHZ-070, FR-RHZ-099; RHZ-078, FR-RHZ-109) derives the
// operator capability levels for an internal gate (question) from its state
// and whether it is bound to a mission or goal. Pending: approve/reject
// enabled; requestChanges enabled only when bound (the relay's question
// branch rejects an unbound question with "gate에 mission 연결 없음" — there is
// no worker to send the changes to — so it stays hidden there).
// changes_requested: approve/reject enabled, requestChanges hidden (a second
// request is rejected by the kernel). Decided (approved/rejected): all hidden.
// A capability must never claim more than the relay accepts; capability_projection_test T1
// and gate_changes_requested_test R5 pin that equality.
func gateCapabilities(state string, bound bool) GateCapabilities {
	switch state {
	case "pending":
		c := GateCapabilities{Approve: capEnabled, Reject: capEnabled, RequestChanges: capHidden}
		if bound {
			c.RequestChanges = capEnabled
		}
		return c
	case "changes_requested":
		return GateCapabilities{Approve: capEnabled, Reject: capEnabled, RequestChanges: capHidden}
	}
	return GateCapabilities{Approve: capHidden, Reject: capHidden, RequestChanges: capHidden}
}

func mapState(s domain.MissionState) string {
	switch s {
	case domain.MissionPlanned, domain.MissionReady:
		return "queued"
	case domain.MissionRunning:
		return "running"
	case domain.MissionWaitingResult, domain.MissionWaitingHuman:
		return "waiting"
	case domain.MissionPaused:
		return "paused"
	case domain.MissionBlocked:
		return "blocked"
	case domain.MissionSucceeded:
		return "completed"
	case domain.MissionCancelled:
		// RHZ-062 (FR-RHZ-091): without this case a cancelled mission fell
		// through to "failed" on the wire — the sweep would mislabel every
		// operator cancellation as a failure.
		return "cancelled"
	default:
		return "failed"
	}
}

type Intent struct {
	Kind                                                                              string
	ID, Name, Prompt, Instruction, Reason, GateID, EdgeID, From, To, EdgeKind, TaskID string
	Body, Recommendation, MissionID, CorrelationID                                    string
	// GoalID targets note.create's about edge at a goal (RHZ-057, FR-RHZ-087).
	GoalID string
	// RHZ-077 (FR-RHZ-105) goal.create wire. Success is the goal's success
	// criterion (required); when it is empty the relay falls back to Prompt
	// so a caller that copies the mission.create shape {name, prompt} still
	// works. Description defaults to Name. ParentGoalID (ID or RHZ-073
	// handle) declares contains(goal:parent → goal:new) in the same relay.
	Success      string `json:"success"`
	Description  string `json:"description"`
	ParentGoalID string `json:"parentGoalId"`
	PolicyRef    string `json:"policyRef"`
	// Params carries procedure.run instantiation inputs (RHZ-063, FR-RHZ-092).
	Params map[string]string `json:"params"`
	// Assignee is the mission.assign target (RHZ-080, FR-RHZ-111); the
	// optional note reuses Reason.
	Assignee string `json:"assignee"`
	// RHZ-082 (FR-RHZ-113) mission.progress wire. Progress is a pointer so
	// the JSON `"progress":0` is recorded as 0 while an absent key stays nil.
	CurrentAction string   `json:"currentAction"`
	Progress      *float64 `json:"progress"`
	BlockedReason string   `json:"blockedReason"`
	// RHZ-081 (FR-RHZ-112) deliverable.register wire. Kind is the intent
	// kind, so the deliverable's own kind rides DeliverableKind. SourceRef is
	// an existing sha256:/exec- ref or empty (the summary bytes are then
	// registered as the source). State is accepted only as ""/"completed"
	// and is not stored: the aggregate carries no lifecycle state.
	DeliverableKind string `json:"deliverableKind"`
	Summary         string `json:"summary"`
	SourceRef       string `json:"sourceRef"`
	State           string `json:"state"`
	// RHZ-065 (FR-RHZ-094) 저술 intent 와이어: statement는 Content, tags는
	// Tags, knowledgeId는 ID, reason은 Reason을 재사용한다.
	SourceMemoryID    string       `json:"sourceMemoryId"`
	KnowledgeKind     string       `json:"knowledgeKind"`
	Confidence        float64      `json:"confidence"`
	SourceKnowledgeID string       `json:"sourceKnowledgeId"`
	Trigger           string       `json:"trigger"`
	Steps             []IntentStep `json:"steps"`
	Preconditions     []string     `json:"preconditions"`
	SuccessConditions []string     `json:"successConditions"`
	FailureModes      []string     `json:"failureModes"`
	RecoverySteps     []string     `json:"recoverySteps"`
	Content           string       `json:"content"`
	MemoryKind        string       `json:"memoryKind"`
	Tags              []string     `json:"tags"`
	// Digest binds a gate.approve/reject to the request digest the human saw
	// (RHZ-047 D26, required for those kinds).
	Digest string
	// RHZ-092 (FR-RHZ-123) mission.start wire: Budget narrows the ledger
	// ceiling per axis (pointers so "tokens":0 is a rejected value, not an
	// absent one); SessionMode overrides the ledger default (oneshot|multiturn).
	// instruction reuses Instruction (empty = mission description).
	Budget      *BudgetOverride `json:"budget"`
	SessionMode string          `json:"sessionMode"`
	// Verification is decoded strictly by RelayIntentHooks only for gate
	// decisions. RawMessage preserves exact key casing for the V7 check.
	Verification json.RawMessage `json:"verification,omitempty"`
}

// BudgetOverride is the per-mission budget request of mission.start; a nil
// axis means "the ceiling".
type BudgetOverride struct {
	Tokens   *int64 `json:"tokens"`
	TimeMs   *int64 `json:"timeMs"`
	MaxDepth *int64 `json:"maxDepth"`
}

// IntentStep is the wire shape of one procedure.define step (RHZ-065).
type IntentStep struct {
	ID        string   `json:"id"`
	Action    string   `json:"action"`
	After     []string `json:"after"`
	NeedsGate bool     `json:"needsGate"`
	// Recommendation (RHZ-083, FR-RHZ-114) rides into the gate question
	// assembly.Run asks for a needsGate step; optional, additive.
	Recommendation string `json:"recommendation"`
}

// containsReachable reports whether target is reachable from start along
// live (non-superseded) contains edges (RHZ-066, FR-RHZ-095). Superseded
// edges are not live relationships and never block a declaration (지정 ②a).
func containsReachable(es edge.Service, start, target string) (bool, error) {
	visited := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == target {
			return true, nil
		}
		if visited[current] {
			continue
		}
		visited[current] = true
		edges, err := es.ByNode("goal", current)
		if err != nil {
			return false, err
		}
		for _, x := range edges {
			if x.Kind != edge.Contains || x.From.ID != current {
				continue
			}
			superseded, err := es.IsSuperseded(x.ID)
			if err != nil {
				return false, err
			}
			if !superseded {
				queue = append(queue, x.To.ID)
			}
		}
	}
	return false, nil
}

// runningSessionFor finds the mission's single running, bound execution and
// returns its session trace id (FR-RHZ-119). Running = Accepted or Observing
// with a durable binding (the same active set the adapter loop observes);
// terminal, unknown, unbound and other missions' executions are ignored. More
// than one running session is ambiguous (v1 serves one session per mission)
// and is reported as a reason, never injected.
func runningSessionFor(s events.Port, missionID string) (traceID, reason string, err error) {
	ids := map[string]bool{}
	for _, e := range s.All() {
		if e.AggregateType == "execution" {
			ids[e.AggregateID] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		r, e := execution.Replay(s.List("execution", id))
		if e != nil {
			return "", "", e
		}
		if r.MissionID != missionID || r.Binding == nil || (r.State != execution.Accepted && r.State != execution.Observing) {
			continue
		}
		if traceID != "" {
			return "", "multiple running executions for mission", nil
		}
		traceID = r.Binding.TraceID
	}
	return traceID, "", nil
}

type RelayResult struct {
	Accepted bool
	Reason   string
	// ExecutionID is set by mission.start (FR-RHZ-123): the execution that
	// was started, found (idempotent re-submission) or left behind by a
	// run-level failure. Empty for every other intent.
	ExecutionID string `json:"executionId,omitempty"`
}

// ExecStartRequest is one mission.start after the relay resolved the mission
// (FR-RHZ-123): Instruction already defaults to the mission description.
type ExecStartRequest struct {
	MissionID, Instruction, SessionMode string
	Budget                              BudgetOverride
	// Actor is the relay's actor, unverified-prefixed like task.progress;
	// journaled as the intent's provenance actor only (FR-RHZ-124).
	Actor string
}

// ExecStartPlan is the zero-write preview of a start: the deterministic
// execution id and, when the request's key already exists, that execution's
// state (the execution package's state vocabulary; "" = none).
type ExecStartPlan struct {
	ExecutionID string
	Existing    string
}

// ExecStarter starts a JANUS execution for a mission (adapter-side, injected
// by the composition root; workspace never imports the adapter). Prepare
// validates against the operating ledger and looks the idempotency key up
// without writing; a non-empty reason is the rejection verbatim. Start
// appends the execution intent/claim and submits hx run; its reason reports
// a run-level refusal (the execution id is still returned), err a store fault.
type ExecStarter interface {
	Prepare(ExecStartRequest) (ExecStartPlan, string, error)
	Start(ExecStartRequest) (executionID, reason string, err error)
}

// RelayHooks are the composition-root seams the relay may call after the
// durable record (task.instruct) or before/after the mission transition
// (mission.start). nil members = not wired.
type RelayHooks struct {
	Inject ExecInjector
	Start  ExecStarter
}

// ExecInjector delivers an instruction text to a running external session
// (JANUS send_message via the adapter, FR-RHZ-119). traceID is the bound
// session id. It returns (seq, "", nil) when accepted, (seq, reason, nil)
// when the external system rejected it (reason verbatim; seq > 0 only when
// the text was logged but not delivered), or a transport error. It is
// injected by the composition root; workspace never imports the adapter.
type ExecInjector func(traceID, text string) (seq int64, reason string, err error)

// resumeToRunning is the task.resume transition chain (planned → ready →
// running, ready/paused → running), shared with mission.start (FR-RHZ-123).
// It returns the rejection reason, "" on success.
func resumeToRunning(s events.Port, m mission.Service, id string, ms domain.Mission) string {
	if ms.State == domain.MissionPlanned {
		if _, e := m.Transition(id, ms.Revision, domain.MissionReady); e != nil {
			return e.Error()
		}
		ms2, er := projector.ReplayMission(s.List("mission", id))
		if er != nil {
			return "ready에서 중단됨: " + er.Error()
		}
		if _, e := m.Transition(id, ms2.Revision, domain.MissionRunning); e != nil {
			return "ready에서 중단됨: " + e.Error()
		}
		return ""
	}
	if _, e := m.Transition(id, ms.Revision, domain.MissionRunning); e != nil {
		return e.Error()
	}
	return ""
}

// RelayIntent is RelayIntentWith without an injector: every behavior before
// RHZ-093 is unchanged (task.instruct is recorded only).
func RelayIntent(s events.Port, in Intent, actor string, verified bool) (RelayResult, error) {
	return RelayIntentWith(s, in, actor, verified, nil)
}

// RelayIntentWith relays one intent; inject, when non-nil, is used by
// task.instruct after the instruction is durable (FR-RHZ-119).
func RelayIntentWith(s events.Port, in Intent, actor string, verified bool, inject ExecInjector) (RelayResult, error) {
	return RelayIntentHooks(s, in, actor, verified, RelayHooks{Inject: inject})
}

// RelayIntentHooks relays one intent with every composition-root seam
// (FR-RHZ-119 inject, FR-RHZ-123 start).
func RelayIntentHooks(s events.Port, in Intent, actor string, verified bool, hooks RelayHooks) (result RelayResult, err error) {
	inject := hooks.Inject
	if s == nil {
		return RelayResult{}, fmt.Errorf("nil store")
	}
	if storePoisoned(s) {
		return RelayResult{}, events.ErrPoisoned
	}
	defer func() {
		if storePoisoned(s) {
			result = RelayResult{}
			err = events.ErrPoisoned
		}
	}()
	var verification *question.Verification
	if len(in.Verification) != 0 {
		switch in.Kind {
		case "gate.approve", "gate.reject", "gate.requestChanges":
		default:
			return RelayResult{Reason: "invalid verification"}, nil
		}
		if verified {
			return RelayResult{Reason: "invalid verification"}, nil
		}
		var err error
		verification, err = question.DecodeIntentVerification(in.Verification, actor)
		if err != nil {
			return RelayResult{Reason: "invalid verification"}, nil
		}
	}
	// RHZ-073 (FR-RHZ-103): handles → IDs at the entrance, before any case;
	// every case below still sees and stores IDs only.
	in = resolveHandles(s, in)
	m := mission.Service{Store: s}
	switch in.Kind {
	case "mission.create":
		if in.Name == "" {
			return RelayResult{Reason: "mission name required"}, nil
		}
		if in.Prompt == "" {
			return RelayResult{Reason: "prompt required (success criterion)"}, nil
		}
		gid := "goal-" + in.Name
		mid := "mission-" + in.Name
		if in.GoalID != "" {
			// RHZ-071 (FR-RHZ-100): goalId given — no goal.created; the
			// mission is assigned under an existing, non-terminal goal.
			// Validation lives here at the relay entrance only.
			g, e := projector.ReplayGoal(s.List("goal", in.GoalID))
			if e != nil {
				return RelayResult{Reason: "goal not found"}, nil
			}
			switch g.State {
			case domain.GoalAchieved, domain.GoalFailed, domain.GoalCancelled:
				return RelayResult{Reason: "goal is terminal"}, nil
			}
			if _, e := m.Create(mid, in.GoalID, in.Name, in.Prompt); e != nil {
				return RelayResult{Reason: e.Error()}, nil
			}
			return RelayResult{Accepted: true}, nil
		}
		if _, e := m.CreateGoal(gid, in.Name, in.Prompt, ""); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if _, e := m.Create(mid, gid, in.Name, in.Prompt); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "goal.create":
		// RHZ-077 (FR-RHZ-105): the operator registers an outcome (goal) directly —
		// goal.created only, zero mission events. With parentGoalId the
		// contains edge is declared on the RHZ-066 path (same edge shape and
		// deterministic ID as edge.declare). Every rejection happens before
		// the first append: the journal is append-only, so a goal without
		// its parent edge must never be half-written.
		if in.Name == "" {
			return RelayResult{Reason: "goal name required"}, nil
		}
		success := in.Success
		if success == "" {
			success = in.Prompt
		}
		if strings.TrimSpace(success) == "" {
			return RelayResult{Reason: "success required (success criterion)"}, nil
		}
		description := in.Description
		if description == "" {
			description = in.Name
		}
		gid := "goal-" + in.Name
		if in.ParentGoalID != "" {
			parent, e := projector.ReplayGoal(s.List("goal", in.ParentGoalID))
			if e != nil {
				return RelayResult{Reason: "parent goal not found"}, nil
			}
			switch parent.State {
			case domain.GoalAchieved, domain.GoalFailed, domain.GoalCancelled:
				return RelayResult{Reason: "parent goal is terminal"}, nil
			}
		}
		if _, e := m.CreateGoal(gid, description, success, in.PolicyRef); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if in.ParentGoalID != "" {
			// The new goal has no edges yet, so the cycle guard of
			// edge.declare is vacuous here; existence of both endpoints is
			// the kernel's (edge.Service) own check.
			x := edge.Edge{ID: "edge-contains-" + in.ParentGoalID + "-" + gid, From: edge.Endpoint{Type: "goal", ID: in.ParentGoalID}, To: edge.Endpoint{Type: "goal", ID: gid}, Kind: edge.Contains, Actor: actor, Correlation: relayCorrelation(in.CorrelationID, actor, verified), Verified: verified}
			if _, e := (edge.Service{Store: s}).Create(x); e != nil {
				return RelayResult{Reason: "contains edge: " + e.Error()}, nil
			}
		}
		return RelayResult{Accepted: true}, nil
	case "note.create":
		if strings.TrimSpace(in.Content) == "" {
			return RelayResult{Reason: "content required"}, nil
		}
		if strings.TrimSpace(in.MemoryKind) == "" {
			return RelayResult{Reason: "memory kind required"}, nil
		}
		if len([]byte(in.Content)) > 16*1024 {
			return RelayResult{Reason: "content exceeds 16KiB limit"}, nil
		}
		kind := memory.Kind(in.MemoryKind)
		if !validMemoryKind(kind) {
			return RelayResult{Reason: fmt.Sprintf("unknown memory kind %q", in.MemoryKind)}, nil
		}
		sum := sha256.Sum256([]byte(in.Content))
		id := "note-" + hex.EncodeToString(sum[:])
		if existing := s.List("memory", id); len(existing) > 0 {
			old, er := memory.Replay(existing)
			if er != nil {
				return RelayResult{Reason: er.Error()}, nil
			}
			if old.Content == in.Content && old.Kind == kind && sameStrings(old.Tags, in.Tags) && old.GoalID == in.GoalID && old.MissionID == in.MissionID {
				// RHZ-057: this idempotent early-return is the primary guard
				// against duplicate about edges — it precedes the dual-write.
				// The deterministic edge id + revision-0 append guard is only a
				// second backstop, not a dedup rule (stage 2).
				return RelayResult{Accepted: true}, nil
			}
			return RelayResult{Reason: "memory metadata conflicts with existing note"}, nil
		}
		// RHZ-057 (FR-RHZ-087): targets are validated before any append so a
		// bad target rejects with zero partial writes (append-only journal has
		// no transactions to roll back).
		if in.GoalID != "" {
			if _, e := projector.ReplayGoal(s.List("goal", in.GoalID)); e != nil {
				return RelayResult{Reason: fmt.Sprintf("unknown goal %q", in.GoalID)}, nil
			}
		}
		if in.MissionID != "" {
			if _, e := projector.ReplayMission(s.List("mission", in.MissionID)); e != nil {
				return RelayResult{Reason: fmt.Sprintf("unknown mission %q", in.MissionID)}, nil
			}
		}
		// Validate the complete note before crossing the source writer boundary.
		src, err := (source.Service{Store: s}).Register([]byte(in.Content), "text/markdown", "note://ui")
		if err != nil {
			return RelayResult{Reason: err.Error()}, nil
		}
		m2 := memory.Memory{ID: id, Kind: kind, Content: in.Content, SourceType: "blob", SourceID: src.BlobID, Confidence: 1, GoalID: in.GoalID, MissionID: in.MissionID, Tags: append([]string(nil), in.Tags...)}
		if _, err := (memory.Service{Store: s}).Create(m2); err != nil {
			return RelayResult{Reason: err.Error()}, nil
		}
		// FR-RHZ-087 dual-write: the FK above coexists with first-class about
		// edges; reverse queries read the edges only, never the FK.
		corr := in.CorrelationID
		if corr == "" {
			corr = "relay"
		}
		short := strings.TrimPrefix(id, "note-")
		if len(short) > 12 {
			short = short[:12]
		}
		for _, target := range []edge.Endpoint{{Type: "goal", ID: in.GoalID}, {Type: "mission", ID: in.MissionID}} {
			if target.ID == "" {
				continue
			}
			x := edge.Edge{ID: "edge-about-" + short + "-" + target.ID, From: edge.Endpoint{Type: "memory", ID: id}, To: target, Kind: edge.About, Actor: actor, Correlation: corr, Verified: verified}
			if _, e := (edge.Service{Store: s}).Create(x); e != nil {
				return RelayResult{Reason: "about edge: " + e.Error()}, nil
			}
		}
		return RelayResult{Accepted: true}, nil
	case "goal.resolve", "goal.fail":
		// RHZ-061 (FR-RHZ-090): operator terminal — an append-only decision
		// event, never a hard delete. decisionID is deterministic and
		// actor-free so ApplyGoalDecision's idempotency doubles as resubmit
		// idempotency; who closed it rides the correlation.
		if in.GoalID == "" {
			return RelayResult{Reason: "goalId required"}, nil
		}
		to := domain.GoalAchieved
		if in.Kind == "goal.fail" {
			to = domain.GoalFailed
		}
		corr := relayCorrelation(in.CorrelationID, actor, verified)
		if _, e := m.ApplyGoalDecision(in.GoalID, "decision-"+in.Kind+"-"+in.GoalID, corr, to); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "goal.update":
		// RHZ-087 (FR-RHZ-117): edit a goal's description and/or success —
		// one append-only goal.updated event, ID and state untouched (a
		// "rename" is a description change; goal-<name> stays). The kernel
		// validates before append (both fields blank, terminal goal,
		// revision) and accepts identical values with zero writes, so a
		// rejection or a resubmit never writes. goalId may be an RHZ-073
		// handle: resolveHandles already turned it into an ID above.
		if in.GoalID == "" {
			return RelayResult{Reason: "goalId required"}, nil
		}
		g, e := projector.ReplayGoal(s.List("goal", in.GoalID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if _, e := m.UpdateGoal(in.GoalID, g.Revision, in.Description, in.Success, in.Reason); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "goal.cancel":
		// RHZ-061: sweep path — a plain transition event (no decision id);
		// goalTransitionValid rejects terminal→anything before any append.
		if in.GoalID == "" {
			return RelayResult{Reason: "goalId required"}, nil
		}
		g, e := projector.ReplayGoal(s.List("goal", in.GoalID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if _, e := m.TransitionGoal(in.GoalID, g.Revision, domain.GoalCancelled); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "knowledge.create":
		// RHZ-065 (FR-RHZ-094): candidate 저술 — 결정론 ID로 멱등(내용
		// 동치면 무이벤트 수용, 불일치는 정직한 거부). 실재 검증은 커널
		// Service(validateSource)가 수행한다.
		if strings.TrimSpace(in.SourceMemoryID) == "" || strings.TrimSpace(in.Content) == "" || strings.TrimSpace(in.KnowledgeKind) == "" {
			return RelayResult{Reason: "sourceMemoryId, content (statement) and knowledgeKind required"}, nil
		}
		conf := in.Confidence
		if conf == 0 {
			conf = 0.5 // 미지정 = 중립
		}
		ksum := sha256.Sum256([]byte(in.SourceMemoryID + "\x00" + in.Content))
		kid := "know-" + hex.EncodeToString(ksum[:])[:12]
		if existing := s.List("knowledge", kid); len(existing) > 0 {
			old, er := knowledge.Replay(existing)
			if er != nil {
				return RelayResult{Reason: er.Error()}, nil
			}
			if old.Statement == in.Content && string(old.Kind) == in.KnowledgeKind && old.SourceMemoryID == in.SourceMemoryID && sameStrings(old.Tags, in.Tags) && old.Confidence == conf {
				return RelayResult{Accepted: true}, nil
			}
			return RelayResult{Reason: "knowledge conflicts with existing item"}, nil
		}
		if _, e := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: kid, Kind: knowledge.Kind(in.KnowledgeKind), Statement: in.Content, SourceMemoryID: in.SourceMemoryID, Confidence: conf, Tags: append([]string(nil), in.Tags...)}); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "knowledge.promote":
		// RHZ-065: candidate→promoted. 재승격은 Promote가 거부(멱등-수용
		// 아님 — goal.cancel과 같은 의미론).
		if in.ID == "" || strings.TrimSpace(in.Reason) == "" {
			return RelayResult{Reason: "id (knowledge) and reason required"}, nil
		}
		if _, e := (knowledge.Service{Store: s}).Promote(in.ID, in.Reason); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "procedure.define":
		// RHZ-065: 템플릿 저술. 기존과 전체 동치면 멱등 수용, 불일치는 거부
		// (procedure.updated는 비범위 — 불변 템플릿 경계). 소스 지식
		// 실재·kind·DAG 검증은 procedure.Create가 수행한다.
		if in.ID == "" || in.SourceKnowledgeID == "" || strings.TrimSpace(in.Trigger) == "" || len(in.Steps) == 0 {
			return RelayResult{Reason: "id, sourceKnowledgeId, trigger and steps required"}, nil
		}
		steps := make([]procedure.Step, 0, len(in.Steps))
		for _, st := range in.Steps {
			steps = append(steps, procedure.Step{ID: st.ID, Action: st.Action, After: append([]string(nil), st.After...), NeedsGate: st.NeedsGate, Recommendation: st.Recommendation})
		}
		next := procedure.Procedure{ID: in.ID, SourceKnowledgeID: in.SourceKnowledgeID, Trigger: in.Trigger, Preconditions: append([]string(nil), in.Preconditions...), Steps: steps, SuccessConditions: append([]string(nil), in.SuccessConditions...), FailureModes: append([]string(nil), in.FailureModes...), RecoverySteps: append([]string(nil), in.RecoverySteps...)}
		if existing := s.List("procedure", in.ID); len(existing) > 0 {
			old, er := procedure.Replay(existing)
			if er != nil {
				return RelayResult{Reason: er.Error()}, nil
			}
			old.Revision = 0
			oldRaw, _ := json.Marshal(old)
			nextRaw, _ := json.Marshal(next)
			if string(oldRaw) == string(nextRaw) {
				return RelayResult{Accepted: true}, nil
			}
			return RelayResult{Reason: "procedure conflicts with existing template (procedure.updated is out of scope)"}, nil
		}
		if _, e := (procedure.Service{Store: s}).Create(next); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "procedure.run":
		// RHZ-063 (FR-RHZ-092): manual trigger — the assembly runner spawns
		// the instance; this relay only carries the spec (writes stay behind
		// kernel services on the same store).
		if in.ID == "" || in.Name == "" || in.GoalID == "" {
			return RelayResult{Reason: "id (procedure), name (run id) and goalId required"}, nil
		}
		if _, e := assembly.Run(s, assembly.RunSpec{ProcedureID: in.ID, GoalID: in.GoalID, RunID: in.Name, Params: in.Params, Actor: actor, Verified: verified, Correlation: in.CorrelationID}); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "mission.cancel":
		// RHZ-062 (FR-RHZ-091): operator mission terminal, RHZ-061 goal.cancel
		// 짝 — append-only; Transition validates before append (no poison
		// path). Succeeded/Failed are not written here: RHZ-069 (FR-RHZ-098)
		// opened operator complete/fail below, from running·waiting_for_result
		// only, through the decision writer ApplyMissionDecision.
		if in.MissionID == "" {
			return RelayResult{Reason: "missionId required"}, nil
		}
		cur, e := projector.ReplayMission(s.List("mission", in.MissionID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if _, e := m.Transition(in.MissionID, cur.Revision, domain.MissionCancelled); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "mission.assign":
		// RHZ-080 (FR-RHZ-111): hand a mission to someone — one append-only
		// mission.assigned event, reassignment allowed (last wins). The
		// kernel validates before append (empty assignee, terminal mission,
		// revision), so a rejection never writes. missionId may be an RHZ-073
		// handle: resolveHandles already turned it into an ID above.
		if in.MissionID == "" {
			return RelayResult{Reason: "missionId required"}, nil
		}
		cur, e := projector.ReplayMission(s.List("mission", in.MissionID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if _, e := m.Assign(in.MissionID, cur.Revision, in.Assignee, in.Reason); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "deliverable.register":
		return relayDeliverableRegister(s, in, actor, verified)
	case "mission.progress":
		// RHZ-082 (FR-RHZ-113): record "what is happening now" with NO state
		// transition — one surface.progressed event through the surface
		// kernel. Allowed only where work is live (running, waiting_for_result,
		// waiting_for_human, blocked); terminal → ErrInvalidState, planned/
		// ready/paused → actionable rejection (task.resume first). A
		// blockedReason here is display-only and never moves the mission.
		if in.MissionID == "" {
			return RelayResult{Reason: "missionId required"}, nil
		}
		cur, e := projector.ReplayMission(s.List("mission", in.MissionID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if cur.State == domain.MissionSucceeded || cur.State == domain.MissionFailed || cur.State == domain.MissionCancelled {
			return RelayResult{Reason: domain.ErrInvalidState.Error()}, nil
		}
		if cur.State != domain.MissionRunning && cur.State != domain.MissionWaitingResult && cur.State != domain.MissionWaitingHuman && cur.State != domain.MissionBlocked {
			return RelayResult{Reason: string(cur.State) + "에서 진척 기록 불가: task.resume 먼저"}, nil
		}
		who := actor
		if !verified && !strings.HasPrefix(who, "unverified-local-operator:") {
			who = "unverified-local-operator:" + who
		}
		if _, e := (surface.Service{Store: s}).Progress(in.MissionID, who, in.CurrentAction, in.Progress, in.BlockedReason); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "mission.complete", "mission.fail":
		// RHZ-069 (FR-RHZ-098): operator terminal for a mission — the
		// goal.resolve/fail 짝. One append-only decision event with a
		// deterministic, actor-free decisionID; who closed it rides the
		// correlation. Allowed only where the state machine has a direct
		// transition: running·waiting_for_result (RHZ-069) and, since RHZ-079
		// (FR-RHZ-110), waiting_for_human·blocked — the operator closes a
		// mission that waits on a human or is blocked with one honest event
		// (no fabricated running/ready hops). planned/ready/paused still get
		// an actionable rejection (task.resume first) with zero writes,
		// terminal states the same ErrInvalidState as mission.cancel.
		if in.MissionID == "" {
			return RelayResult{Reason: "missionId required"}, nil
		}
		to := domain.MissionSucceeded
		if in.Kind == "mission.fail" {
			if strings.TrimSpace(in.Reason) == "" {
				return RelayResult{Reason: "reason required"}, nil
			}
			to = domain.MissionFailed
		}
		cur, e := projector.ReplayMission(s.List("mission", in.MissionID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if cur.State == domain.MissionSucceeded || cur.State == domain.MissionFailed || cur.State == domain.MissionCancelled {
			return RelayResult{Reason: domain.ErrInvalidState.Error()}, nil
		}
		if cur.State != domain.MissionRunning && cur.State != domain.MissionWaitingResult && cur.State != domain.MissionWaitingHuman && cur.State != domain.MissionBlocked {
			return RelayResult{Reason: string(cur.State) + "에서 종결 불가: task.resume 먼저"}, nil
		}
		corr := relayCorrelation(in.CorrelationID, actor, verified)
		if _, e := m.ApplyMissionDecision(in.MissionID, cur.Revision, to, "decision-"+in.Kind+"-"+in.MissionID, corr, in.Reason); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "task.pause", "task.resume":
		ms, e := projector.ReplayMission(s.List("mission", in.TaskID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if in.Kind == "task.resume" {
			if ms.State != domain.MissionPlanned && ms.State != domain.MissionReady && ms.State != domain.MissionPaused {
				return RelayResult{Reason: domain.ErrInvalidState.Error()}, nil
			}
			if reason := resumeToRunning(s, m, in.TaskID, ms); reason != "" {
				return RelayResult{Reason: reason}, nil
			}
			return RelayResult{Accepted: true}, nil
		}
		if _, e = m.Transition(in.TaskID, ms.Revision, domain.MissionPaused); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "mission.start":
		// RHZ-092 (FR-RHZ-123): start a JANUS execution for a board mission.
		// Rejection order, all with ZERO writes: missionId → mission exists →
		// non-terminal (ErrInvalidState) → hook wired → ledger/budget/key
		// (Prepare). Only then: planned/ready/paused → running through the
		// task.resume chain, then execution.intent → dispatch_claimed →
		// accepted via the hook (the smoke sequence). A key that already
		// names a running execution is an idempotent lookup: Accepted, no
		// write. The loop never starts executions (RHZ-046 D7).
		if in.MissionID == "" {
			return RelayResult{Reason: "missionId required"}, nil
		}
		log := s.List("mission", in.MissionID)
		if len(log) == 0 {
			return RelayResult{Reason: fmt.Sprintf("unknown mission %q", in.MissionID)}, nil
		}
		cur, e := projector.ReplayMission(log)
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if cur.State == domain.MissionSucceeded || cur.State == domain.MissionFailed || cur.State == domain.MissionCancelled {
			return RelayResult{Reason: domain.ErrInvalidState.Error()}, nil
		}
		// Startable states: planned/ready/paused reach running
		// through the task.resume chain; running is already live. blocked and
		// the waiting_* states are refused — starting an execution there would
		// skip the lifecycle step that should release them.
		switch cur.State {
		case domain.MissionPlanned, domain.MissionReady, domain.MissionPaused, domain.MissionRunning:
		default:
			return RelayResult{Reason: string(cur.State) + "에서 실행 시작 불가"}, nil
		}
		if hooks.Start == nil {
			return RelayResult{Reason: "execution start unavailable"}, nil
		}
		who := actor
		if !verified && !strings.HasPrefix(who, "unverified-local-operator:") {
			who = "unverified-local-operator:" + who
		}
		req := ExecStartRequest{MissionID: in.MissionID, Instruction: strings.TrimSpace(in.Instruction), SessionMode: in.SessionMode, Actor: who}
		if req.Instruction == "" {
			req.Instruction = cur.Description
		}
		if in.Budget != nil {
			req.Budget = *in.Budget
		}
		plan, reason, e := hooks.Start.Prepare(req)
		if e != nil {
			return RelayResult{}, e
		}
		if reason != "" {
			return RelayResult{Reason: reason, ExecutionID: plan.ExecutionID}, nil
		}
		// ensureRunning moves the mission to running only once an execution is
		// live: a failed start must not leave a phantom running
		// mission. Re-replayed because Start only appends execution events.
		ensureRunning := func() string {
			ms, er := projector.ReplayMission(s.List("mission", in.MissionID))
			if er != nil {
				return er.Error()
			}
			if ms.State == domain.MissionPlanned || ms.State == domain.MissionReady || ms.State == domain.MissionPaused {
				return resumeToRunning(s, m, in.MissionID, ms)
			}
			return ""
		}
		if plan.Existing == "accepted" || plan.Existing == "observing" {
			// Idempotent lookup; also converges a retry whose transition step
			// failed after an earlier successful start.
			if reason := ensureRunning(); reason != "" {
				return RelayResult{Reason: reason, ExecutionID: plan.ExecutionID}, nil
			}
			return RelayResult{Accepted: true, ExecutionID: plan.ExecutionID}, nil
		}
		id, reason, e := hooks.Start.Start(req)
		if e != nil {
			// A concurrent identical start lost the race (duplicate key on the
			// intent, or a revision conflict on the claim). The winner owns the
			// execution: answer idempotently from the store.
			if errors.Is(e, events.ErrRevisionConflict) || strings.Contains(e.Error(), "duplicate idempotency key") {
				if p2, r2, e2 := hooks.Start.Prepare(req); e2 == nil && r2 == "" && p2.ExecutionID != "" {
					return RelayResult{Accepted: true, ExecutionID: p2.ExecutionID}, nil
				}
			}
			return RelayResult{}, e
		}
		if reason != "" {
			return RelayResult{Reason: reason, ExecutionID: id}, nil
		}
		if reason := ensureRunning(); reason != "" {
			return RelayResult{Reason: reason, ExecutionID: id}, nil
		}
		return RelayResult{Accepted: true, ExecutionID: id}, nil
	case "task.instruct":
		// FR-RHZ-119: RECORD first, then inject. The
		// instruction fact is durable regardless of the injection outcome; a
		// rejection is reported as Accepted:false with the reason verbatim and
		// the record stays. No automatic retry: send_message is not idempotent
		// (contract ② v1.6 §3) — a retry may inject twice, so this relay
		// submits exactly once and consumers dedupe via the user/message kind
		// (seq) in the session log.
		if _, e := (surface.Service{Store: s}).Instruct(in.TaskID, in.Instruction, actor, verified, "relay"); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if inject == nil {
			return RelayResult{Accepted: true}, nil
		}
		traceID, reason, e := runningSessionFor(s, in.TaskID)
		if e != nil {
			return RelayResult{}, e
		}
		if reason != "" {
			return RelayResult{Accepted: false, Reason: reason}, nil
		}
		if traceID == "" {
			return RelayResult{Accepted: true}, nil // No running session: recorded only, as before.
		}
		seq, reason, e := inject(traceID, in.Instruction)
		if e != nil {
			return RelayResult{Accepted: false, Reason: e.Error()}, nil
		}
		// Rejection reasons are passed verbatim (contract vocabulary; the
		// cockpit shows them as "recorded, not delivered"). An accepted inject
		// carries no Reason — a Reason on Accepted:true would read as an error
		// downstream; message_seq stays in the relay log only.
		_ = seq
		if reason != "" {
			return RelayResult{Accepted: false, Reason: reason}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "question.ask":
		// RHZ-071 (FR-RHZ-100): a gate must belong to a mission. RHZ-075
		// (FR-RHZ-108): or to a goal — exactly one of missionId/goalId.
		// Required/exactly-one and existence are checked at the relay
		// entrance; terminal state is the kernel's reason; replay rules
		// unchanged.
		mid, gid := strings.TrimSpace(in.MissionID), strings.TrimSpace(in.GoalID)
		if mid == "" && gid == "" {
			return RelayResult{Reason: "missionId or goalId required"}, nil
		}
		if mid != "" && gid != "" {
			return RelayResult{Reason: "both missionId and goalId given"}, nil
		}
		if mid != "" {
			if _, e := projector.ReplayMission(s.List("mission", mid)); e != nil {
				return RelayResult{Reason: "mission not found"}, nil
			}
		} else if _, e := projector.ReplayGoal(s.List("goal", gid)); e != nil {
			return RelayResult{Reason: "goal not found"}, nil
		}
		q, e := (question.Service{Store: s}).Ask(in.Name, in.Body, in.Recommendation, mid, gid, actor, in.CorrelationID)
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		_ = q
		return RelayResult{Accepted: true}, nil
	case "gate.approve", "gate.reject":
		if q, e := (question.Service{Store: s}).Get(in.GateID); e == nil {
			if in.Digest == "" {
				return RelayResult{Reason: "digest required"}, nil
			}
			d := question.Approve
			if in.Kind == "gate.reject" {
				d = question.Reject
			}
			if _, e = (question.Service{Store: s}).Answer(q.ID, d, in.Reason, actor, in.Digest, verification); e != nil {
				return RelayResult{Reason: e.Error()}, nil
			}
			return RelayResult{Accepted: true}, nil
		}
		// RHZ-050: do not reuse approval here; relayApproval submits every
		// approval aggregate to JANUS sockets, which would leak internal decisions.
		// RHZ-047 (FR-RHZ-078): a human decision is recorded only against an
		// observed request, digest-checked against what the human saw.
		if _, e := (approval.Service{Store: s}).Get(in.GateID); e == nil {
			return RelayResult{Reason: "gate already has input"}, nil
		}
		gr, e := (gaterequest.Service{Store: s}).Get(in.GateID)
		if e != nil {
			return RelayResult{Reason: "관측된 승인 요청 없음"}, nil
		}
		if in.Digest == "" {
			return RelayResult{Reason: "digest required"}, nil
		}
		if approval.VerifyDigest(in.Digest, gr.RequestDigest) != nil {
			return RelayResult{Reason: "digest mismatch"}, nil
		}
		d := approval.Allow
		if in.Kind == "gate.reject" {
			d = approval.Deny
			if strings.TrimSpace(in.Reason) == "" {
				return RelayResult{Reason: "reason required"}, nil
			}
		}
		// Expiry is never pre-judged here (D25): a late approve is recorded
		// and submitted; finality is JANUS's durable expired{deny}.
		name := gr.Name
		if name == "" {
			name = "approval"
		}
		g := approval.GateFields{GateName: name, GateType: "approval", RequestedAction: name, RiskTier: "logged",
			Request: approval.Request{Target: "janus", RequestedBy: actor, RequestedAt: time.Now().UTC().Format(time.RFC3339), Reason: gr.Reason}}
		if _, e = (approval.Service{Store: s}).RecordInputWithGate(gr.Key, d, in.Reason, approval.ResponseIDFor(gr.ID), gr.RequestDigest, actor, "relay", gr.DecisionID, verified, g, verification); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "gate.requestChanges":
		// RHZ-078 (FR-RHZ-109): internal gate (question) first, as gate.approve/
		// reject do. The human sends the request back with a reason; the kernel
		// records question.answered{decision: requestChanges} and the gate stays
		// open for a later approve/reject. The cockpit's decision text for this
		// kind travels as `instruction` (gunnflow-adapter upstream mapping), so
		// it is accepted as the reason when `reason` is absent.
		if q, e := (question.Service{Store: s}).Get(in.GateID); e == nil {
			if q.MissionID == "" && q.GoalID == "" {
				return RelayResult{Reason: "gate에 mission 연결 없음"}, nil
			}
			if in.Digest == "" {
				return RelayResult{Reason: "digest required"}, nil
			}
			reason := in.Reason
			if strings.TrimSpace(reason) == "" {
				reason = in.Instruction
			}
			if _, e = (question.Service{Store: s}).Answer(q.ID, question.RequestChanges, reason, actor, in.Digest, verification); e != nil {
				return RelayResult{Reason: e.Error()}, nil
			}
			return RelayResult{Accepted: true}, nil
		}
		if verification != nil {
			return RelayResult{Reason: "invalid verification"}, nil
		}
		decisionID := ""
		if a, e := (approval.Service{Store: s}).Get(in.GateID); e == nil && a.DecisionID != "" {
			decisionID = a.DecisionID
		} else if gr, e2 := (gaterequest.Service{Store: s}).Get(in.GateID); e2 == nil && gr.DecisionID != "" {
			decisionID = gr.DecisionID // RHZ-047: pending gates carry the link too.
		}
		if decisionID == "" {
			return RelayResult{Reason: "gate에 mission 연결 없음"}, nil
		}
		d, e := decision.Replay(s.List("decision", decisionID))
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		if _, e = (surface.Service{Store: s}).Instruct(d.MissionID, in.Instruction, actor, verified, in.GateID); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "edge.declare":
		// RHZ-066 (FR-RHZ-095): contains(goal→goal) 선언 — 계층의 키스톤.
		// contains 전용: 프로세스 엣지(spawn/dependency/about/gate)는 각자
		// 소유 경로만 — 위조 시도는 침묵 무시가 아니라 소리내어 거부한다.
		if in.EdgeKind != "" && in.EdgeKind != string(edge.Contains) {
			return RelayResult{Reason: "edge.declare is contains only"}, nil
		}
		parseGoal := func(v string) (string, bool) {
			parts := strings.SplitN(v, ":", 2)
			if len(parts) != 2 || parts[0] != "goal" || parts[1] == "" {
				return "", false
			}
			return parts[1], true
		}
		fromGoal, okFrom := parseGoal(in.From)
		toGoal, okTo := parseGoal(in.To)
		if !okFrom || !okTo {
			return RelayResult{Reason: `from and to must be "goal:<id>"`}, nil
		}
		es := edge.Service{Store: s}
		declaredID := "edge-contains-" + fromGoal + "-" + toGoal
		if _, err := es.Get(declaredID); err == nil {
			superseded, serr := es.IsSuperseded(declaredID)
			if serr != nil {
				return RelayResult{Reason: serr.Error()}, nil
			}
			if superseded {
				// 거짓 Accept 금지: rewire로 치워진 관계에
				// "성공"을 돌려주면 보드에 없는 A⊃B를 있다고 말하게 된다.
				return RelayResult{Reason: "superseded contains edge re-declaration is out of scope"}, nil
			}
			// live 동일 선언 — ID가 from/to에서 파생되므로 내용 불일치는
			// 구조적으로 불가능: 존재가 곧 멱등의 근거다.
			return RelayResult{Accepted: true}, nil
		}
		// 전이 사이클 가드(지정 ①): live contains 그래프에서 to→…→from
		// 도달 가능하면 거부(역방향 2-사이클 포함). 자기참조(from==to)는
		// 커널 valid의 소유라 여기선 건너뛴다(검증 분담 — 중복 금지).
		if fromGoal != toGoal {
			reachable, err := containsReachable(es, toGoal, fromGoal)
			if err != nil {
				return RelayResult{Reason: err.Error()}, nil
			}
			if reachable {
				return RelayResult{Reason: "contains cycle rejected"}, nil
			}
		}
		corr := in.CorrelationID
		if corr == "" {
			corr = "relay"
		}
		x := edge.Edge{ID: declaredID, From: edge.Endpoint{Type: "goal", ID: fromGoal}, To: edge.Endpoint{Type: "goal", ID: toGoal}, Kind: edge.Contains, Actor: actor, Correlation: corr, Verified: verified}
		if _, e := es.Create(x); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "edge.rewire":
		if in.EdgeID == "" {
			return RelayResult{Reason: "edgeId required"}, nil
		}
		parse := func(v string) edge.Endpoint {
			parts := strings.SplitN(v, ":", 2)
			if len(parts) != 2 {
				return edge.Endpoint{}
			}
			return edge.Endpoint{Type: parts[0], ID: parts[1]}
		}
		x := edge.Edge{ID: in.ID, From: parse(in.From), To: parse(in.To), Kind: edge.Kind(in.EdgeKind), Actor: actor, Correlation: "relay", Verified: verified}
		if _, e := (edge.Service{Store: s}).Rewire(in.EdgeID, x); e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	case "session.pause", "session.resume", "session.fork", "session.stdin", "session.kill":
		return RelayResult{Reason: "JANUS T17-19 표면 의존"}, nil
	default:
		return RelayResult{Reason: "unknown intent"}, nil
	}
}

// relayDeliverableRegister is the deliverable.register intent (RHZ-081,
// FR-RHZ-112): an operator/agent records a produced artifact under exactly
// one mission or goal. Writes, all through kernel writers and only after
// every rejection point: source.registered (only when sourceRef is empty —
// the summary bytes become the source, D28 precedent), deliverable.declared,
// edge.declared produces(binding → deliverable). The deliverable ID is
// content-derived (binding ‖ kind ‖ summary ‖ sourceRef), so the same
// registration is idempotent (Accepted, zero writes) and a run that crashed
// between the two appends is repaired by appending only the missing edge.
func relayDeliverableRegister(s events.Port, in Intent, actor string, verified bool) (RelayResult, error) {
	mid, gid := strings.TrimSpace(in.MissionID), strings.TrimSpace(in.GoalID)
	if mid == "" && gid == "" {
		return RelayResult{Reason: "missionId or goalId required"}, nil
	}
	if mid != "" && gid != "" {
		return RelayResult{Reason: "both missionId and goalId given"}, nil
	}
	// Binding must exist and must not be cancelled/failed. succeeded/achieved
	// (Known limit: the idempotent/edge-repair branch below runs
	// after this check, so a deliverable left without its produces edge by a
	// crash cannot be repaired once the binding is cancelled/failed. Accepted:
	// the deliverable stays readable; the edge only matters for live work.)
	// stay allowed: recording a deliverable after completion is the
	// canonical flow (the kernel test on a succeeded mission pins it).
	var from edge.Endpoint
	if mid != "" {
		ms, e := projector.ReplayMission(s.List("mission", mid))
		if e != nil {
			return RelayResult{Reason: "mission not found"}, nil
		}
		if ms.State == domain.MissionCancelled || ms.State == domain.MissionFailed {
			return RelayResult{Reason: "mission is terminal"}, nil
		}
		from = edge.Endpoint{Type: "mission", ID: mid}
	} else {
		g, e := projector.ReplayGoal(s.List("goal", gid))
		if e != nil {
			return RelayResult{Reason: "goal not found"}, nil
		}
		if g.State == domain.GoalCancelled || g.State == domain.GoalFailed {
			return RelayResult{Reason: "goal is terminal"}, nil
		}
		from = edge.Endpoint{Type: "goal", ID: gid}
	}
	kind, summary := strings.TrimSpace(in.DeliverableKind), strings.TrimSpace(in.Summary)
	if kind == "" {
		return RelayResult{Reason: "deliverableKind required"}, nil
	}
	if summary == "" {
		return RelayResult{Reason: "summary required"}, nil
	}
	if st := strings.TrimSpace(in.State); st != "" && st != "completed" {
		return RelayResult{Reason: "state not supported"}, nil
	}
	ref := strings.TrimSpace(in.SourceRef)
	srcSvc := source.Service{Store: s}
	needSource := false
	switch {
	case ref == "":
		// Empty sourceRef: the summary itself is the artifact. Its blob id is
		// content-addressed, so it is known before any write.
		sum := sha256.Sum256([]byte(summary))
		ref = "sha256:" + hex.EncodeToString(sum[:])
		needSource = true
	case strings.HasPrefix(ref, "sha256:"):
		if _, e := srcSvc.Get(ref); e != nil {
			return RelayResult{Reason: "source not found"}, nil
		}
	case strings.HasPrefix(ref, "exec-"):
		if gid != "" {
			return RelayResult{Reason: "execution source needs missionId"}, nil
		}
		x, e := execution.Replay(s.List("execution", ref))
		if e != nil {
			return RelayResult{Reason: "execution not found"}, nil
		}
		if x.MissionID != mid {
			return RelayResult{Reason: "execution mission mismatch"}, nil
		}
	default:
		return RelayResult{Reason: "invalid source ref"}, nil
	}
	id := deliverableRegisterID(from, kind, summary, ref)
	edgeID := "edge-produces-" + from.ID + "-" + id
	corr := relayCorrelation(in.CorrelationID, actor, verified)
	ds, es := deliverable.Service{Store: s}, edge.Service{Store: s}
	if _, e := ds.Get(id); e == nil {
		// Same binding ‖ kind ‖ summary ‖ sourceRef by construction of the ID:
		// idempotent. Only the produces edge can be missing (crash between
		// the two appends) — repair it, else zero writes.
		if _, e := es.Get(edgeID); e == nil {
			return RelayResult{Accepted: true}, nil
		}
		if _, e := es.Create(edge.Edge{ID: edgeID, From: from, To: edge.Endpoint{Type: "deliverable", ID: id}, Kind: edge.Produces, Actor: actor, Correlation: corr, Verified: verified}); e != nil {
			return RelayResult{Reason: "produces edge: " + e.Error()}, nil
		}
		return RelayResult{Accepted: true}, nil
	}
	if needSource {
		src, e := srcSvc.Register([]byte(summary), "text/markdown", "intent://deliverable")
		if e != nil {
			return RelayResult{Reason: e.Error()}, nil
		}
		ref = src.BlobID
	}
	d := deliverable.Deliverable{ID: id, Kind: kind, MissionID: mid, GoalID: gid, SourceRef: ref, Summary: summary}
	if _, e := ds.Create(d); e != nil {
		return RelayResult{Reason: e.Error()}, nil
	}
	if _, e := es.Create(edge.Edge{ID: edgeID, From: from, To: edge.Endpoint{Type: "deliverable", ID: id}, Kind: edge.Produces, Actor: actor, Correlation: corr, Verified: verified}); e != nil {
		return RelayResult{Reason: "produces edge: " + e.Error()}, nil
	}
	return RelayResult{Accepted: true}, nil
}

// deliverableRegisterID is the deterministic, actor-free deliverable ID of
// deliverable.register (FR-RHZ-112): deliv-<sha256(binding ‖ kind ‖ summary
// ‖ sourceRef)[:12 hex]>. binding is "mission:<id>" or "goal:<id>"; the NUL
// separators keep the fields from bleeding into one another. Hex-only
// suffixes cannot collide with coordinator deliv-mission-* or legacy ids.
func deliverableRegisterID(from edge.Endpoint, kind, summary, sourceRef string) string {
	h := sha256.New()
	h.Write([]byte(from.Type + ":" + from.ID))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(summary))
	h.Write([]byte{0})
	h.Write([]byte(sourceRef))
	return "deliv-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// relayCorrelation derives the correlation an operator decision event carries
// when the intent brought none: "relay:<actor>", with the unverified-local-
// operator prefix applied to an unverified actor (RHZ-061 rule, pinned by
// goal_lifecycle_test). Shared by goal.resolve/fail and mission.complete/fail
// (RHZ-069, FR-RHZ-098) so the two surfaces cannot drift.
func relayCorrelation(given, actor string, verified bool) string {
	if given != "" {
		return given
	}
	who := actor
	if !verified && !strings.HasPrefix(who, "unverified-local-operator:") {
		who = "unverified-local-operator:" + who
	}
	return "relay:" + who
}
