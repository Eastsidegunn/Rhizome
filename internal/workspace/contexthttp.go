package workspace

// RHZ-064 (FR-RHZ-093): GET /v1/context?task=<missionID> — 핸드오프 읽기
// 경로. retrieval.Deterministic으로 결정론 컨텍스트 번들을 조립하고(같은
// 입력 = 같은 바이트), 지식이 반환된 조회마다 KnowledgeUseTrace 1건을 serve
// writer로 기록한다. 번들은 지식 평면 상태만의 함수라 의도적으로 저널
// revision을 싣지 않는다 — trace 누적이 응답에 피드백되면 응답 바이트가
// 달라진다.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
	"rhizome/internal/question"
	"rhizome/internal/relation"
	"rhizome/internal/retrieval"
	"rhizome/internal/surface"
	"rhizome/internal/trace"
	"rhizome/internal/trust"
)

// allRelationTypes: FollowRelations 전 타입(결정론 고정 순서).
var allRelationTypes = []relation.Type{relation.AppliesWhen, relation.Contradicts, relation.DerivedFrom, relation.FailsWhen, relation.PrerequisiteOf, relation.Supports}

type contextBundle struct {
	Task struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
		// Assignee: RHZ-080 (FR-RHZ-111), additive (omitempty). Nested, so
		// the RHZ-068 trailing `,"steps":[]}` pin is unaffected.
		Assignee string `json:"assignee,omitempty"`
		// RHZ-082 (FR-RHZ-113), additive and nested: the same progress
		// slots as /v1/workspace tasks[].
		CurrentAction string  `json:"currentAction,omitempty"`
		Progress      float64 `json:"progress,omitempty"`
		HasProgress   bool    `json:"hasProgress"`
		// Prompt: RHZ-131 (FR-RHZ-172), opt-in and nested. The mission's
		// prompt (domain.Mission.Success) is filled only for
		// ?include=prompt, so a request without it stays byte-identical to
		// the pre-RHZ-131 bundle (pinned goldens) and the `,"steps":[]}`
		// suffix pin is unaffected.
		Prompt string `json:"prompt,omitempty"`
	} `json:"task"`
	Goal struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		State       string `json:"state"`
	} `json:"goal"`
	Memories  []contextMemoryDTO    `json:"memories"`
	Knowledge []contextKnowledgeDTO `json:"knowledge"`
	Relations []contextRelationDTO  `json:"relations"`
	// Steps projects a procedure.run instance (RHZ-068, FR-RHZ-097): the
	// step missions this mission spawned, in procedure.TopoOrder. It MUST
	// stay the last field — the additive guarantee is pinned as a byte suffix
	// (`,"steps":[]}`) so the pre-RHZ-068 prefix is provably unchanged.
	Steps []contextStepDTO `json:"steps"`
}
type contextStepDTO struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	State string   `json:"state"`
	After []string `json:"after"`
	// Gates are the internal gates (questions) asked against this step.
	Gates []contextGateDTO `json:"gates"`
}
type contextGateDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	// RHZ-078 (FR-RHZ-109): the latest answer's reason/actor, so a worker
	// reading its step sees what a changes_requested gate asks for. Additive
	// (omitempty): pending gates serialize exactly as before.
	DecisionReason string           `json:"decisionReason,omitempty"`
	DecidedBy      string           `json:"decidedBy,omitempty"`
	DecidedAt      string           `json:"decidedAt,omitempty"`
	Verification   *verificationDTO `json:"verification,omitempty"`
}
type contextMemoryDTO struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Content  string   `json:"content"`
	SourceID string   `json:"sourceId"`
	Tags     []string `json:"tags"`
	// Seq: RHZ-086 (FR-RHZ-116), additive. The journal Sequence of the
	// memory's first event — the recency key clients order by. The task
	// bundle keeps its by-ID order; the goal bundle is served newest-first.
	Seq uint64 `json:"seq"`
}

// notesContextBundle (RHZ-086, FR-RHZ-116): GET /v1/context?goal=<id|handle>
// or ?mission=<id|handle>. A read-only projection of the about notes of one
// node — no knowledge retrieval, so (unlike the task path) no UseTrace is
// written and the journal is untouched; a cockpit click must never append a
// false usage trace. Exactly one of goal/mission is set (omitempty keeps the
// goal bundle's layout). Memories are newest-first by first-event Sequence.
type notesContextBundle struct {
	Goal *struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		State       string `json:"state"`
	} `json:"goal,omitempty"`
	Mission *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"mission,omitempty"`
	Memories []contextMemoryDTO `json:"memories"`
}

type contextKnowledgeDTO struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Statement      string  `json:"statement"`
	SourceMemoryID string  `json:"sourceMemoryId"`
	Confidence     float64 `json:"confidence"`
}
type contextRelationDTO struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (h *HTTPServer) serveContext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	taskID, goalID, missionID := q.Get("task"), q.Get("goal"), q.Get("mission")
	// RHZ-086 (FR-RHZ-116): exactly one of task/goal/mission.
	set := 0
	for _, v := range []string{taskID, goalID, missionID} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		http.Error(w, "task, goal and mission are mutually exclusive", http.StatusBadRequest)
		return
	}
	// RHZ-131 (FR-RHZ-172): include is a task-bundle opt-in whose only value
	// is "prompt". Like the other bad parameters here, an unknown value, or
	// include on a goal/mission notes bundle, is a 400 — never silently
	// ignored. It changes only the task.prompt field: the knowledge
	// retrieval and the UseTrace (Query "task:<id>") are the same with or
	// without it.
	include := q.Get("include")
	for _, v := range q["include"] {
		// Every repeated value is checked: include=prompt&include=bogus is a 400.
		if v != include || (v != "" && v != "prompt") {
			http.Error(w, "invalid include", http.StatusBadRequest)
			return
		}
	}
	if include != "" && taskID == "" {
		http.Error(w, "include requires task", http.StatusBadRequest)
		return
	}
	if goalID != "" {
		h.serveNotesContext(w, r, "goal", goalID)
		return
	}
	if missionID != "" {
		h.serveNotesContext(w, r, "mission", missionID)
		return
	}
	if taskID == "" {
		http.Error(w, "task parameter required", http.StatusBadRequest)
		return
	}
	if storePoisoned(h.Store) {
		servePoisoned(w)
		return
	}
	// RHZ-073 (FR-RHZ-103): task may be a handle; resolved once here, the
	// bundle (incl. task.id and the trace) is then identical to a by-ID query.
	if handleShape(taskID) {
		taskID = buildHandleIndex(h.Store.All()).resolve(taskID)
	}
	m, err := projector.ReplayMission(h.Store.List("mission", taskID))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	g, err := projector.ReplayGoal(h.Store.List("goal", m.GoalID))
	if err != nil {
		http.Error(w, "context assembly failed", http.StatusInternalServerError)
		return
	}
	// 시드 = RHZ-057 about 엣지의 역방향: 이 mission·goal에 걸린 memory들.
	es := edge.Service{Store: h.Store}
	seedSet := map[string]bool{}
	for _, node := range []edge.Endpoint{{Type: "mission", ID: taskID}, {Type: "goal", ID: g.ID}} {
		edges, err := es.ByNode(node.Type, node.ID)
		if err != nil {
			http.Error(w, "context assembly failed", http.StatusInternalServerError)
			return
		}
		for _, x := range edges {
			if x.Kind == edge.About && x.From.Type == "memory" && x.To == node {
				seedSet[x.From.ID] = true
			}
		}
	}
	seeds := make([]string, 0, len(seedSet))
	for id := range seedSet {
		seeds = append(seeds, id)
	}
	sort.Strings(seeds)
	bundle := contextBundle{Memories: []contextMemoryDTO{}, Knowledge: []contextKnowledgeDTO{}, Relations: []contextRelationDTO{}}
	bundle.Task.ID, bundle.Task.Name, bundle.Task.State, bundle.Task.Assignee = taskID, m.Description, mapState(m.State), m.Assignee
	if include == "prompt" {
		bundle.Task.Prompt = m.Success
	}
	sv, err := (surface.Service{Store: h.Store}).ByMission(taskID)
	if err != nil && len(h.Store.List("surface", "surface-"+taskID)) > 0 {
		http.Error(w, "surface replay failed", http.StatusInternalServerError)
		return
	}
	bundle.Task.CurrentAction, bundle.Task.Progress, bundle.Task.HasProgress = sv.CurrentAction, sv.Progress, sv.HasProgress
	// RHZ-068 (FR-RHZ-097): read-only projection — a cycle or an unresolved
	// dependency is a 500, never silently dropped (same tone as trace failures).
	if bundle.Steps, err = assembleSteps(h.Store, h.Trust, es, taskID); err != nil {
		http.Error(w, "context assembly failed", http.StatusInternalServerError)
		return
	}
	bundle.Goal.ID, bundle.Goal.Description, bundle.Goal.State = g.ID, g.Description, string(g.State)
	ret := retrieval.Deterministic{Store: h.Store}
	items := map[string]retrieval.Item{}
	relations := map[string]relation.Relation{}
	for _, memID := range seeds {
		mem, err := memory.Replay(h.Store.List("memory", memID))
		if err != nil {
			http.Error(w, "context assembly failed", http.StatusInternalServerError)
			return
		}
		bundle.Memories = append(bundle.Memories, memoryDTO(mem, memorySeq(h.Store, memID)))
		// 확정 지식만, 관계 전 타입 추적.
		res, err := ret.Retrieve(retrieval.Query{SourceMemoryID: memID, FollowRelations: allRelationTypes})
		if err != nil {
			http.Error(w, "context assembly failed", http.StatusInternalServerError)
			return
		}
		for _, item := range res.Items {
			items[item.Knowledge.ID] = item
		}
		for _, rel := range res.Relations {
			relations[rel.ID] = rel
		}
	}
	knowledgeIDs := make([]string, 0, len(items))
	for id := range items {
		knowledgeIDs = append(knowledgeIDs, id)
	}
	sort.Strings(knowledgeIDs)
	for _, id := range knowledgeIDs {
		k := items[id].Knowledge
		bundle.Knowledge = append(bundle.Knowledge, contextKnowledgeDTO{ID: k.ID, Kind: string(k.Kind), Statement: k.Statement, SourceMemoryID: k.SourceMemoryID, Confidence: k.Confidence})
	}
	relationIDs := make([]string, 0, len(relations))
	for id := range relations {
		relationIDs = append(relationIDs, id)
	}
	sort.Strings(relationIDs)
	for _, id := range relationIDs {
		rel := relations[id]
		bundle.Relations = append(bundle.Relations, contextRelationDTO{ID: rel.ID, Type: string(rel.Type), From: rel.From, To: rel.To})
	}
	// UseTrace: 지식이 반환된 조회당 정확 1건 (빈 조회는 trace 스키마가
	// RetrievedItemIDs를 요구하므로 생략). [카운트→Create]는
	// 임계구역: 동시 조회의 상태-결정론 ID 충돌 방지(codeindex 패턴).
	if len(knowledgeIDs) > 0 {
		ts := trace.Service{Store: h.Store}
		h.contextMu.Lock()
		existing, err := ts.ByMission(taskID)
		if err == nil {
			_, err = ts.Create(trace.Trace{
				ID:                   fmt.Sprintf("trace-task-%s-%d", taskID, len(existing)+1),
				Query:                "task:" + taskID,
				RetrievedItemIDs:     knowledgeIDs,
				TraversedRelationIDs: relationIDs,
				MissionID:            taskID,
				GoalID:               g.ID,
			})
		}
		h.contextMu.Unlock()
		if err != nil {
			if storePoisoned(h.Store) || errors.Is(err, events.ErrPoisoned) {
				servePoisoned(w)
				return
			}
			http.Error(w, "context trace record failed", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bundle)
}

// memoryDTO shapes one memory for the bundle (tags never null).
func memoryDTO(mem memory.Memory, seq uint64) contextMemoryDTO {
	tags := mem.Tags
	if tags == nil {
		tags = []string{}
	}
	return contextMemoryDTO{ID: mem.ID, Kind: string(mem.Kind), Content: mem.Content, SourceID: mem.SourceID, Tags: tags, Seq: seq}
}

// memorySeq is the journal Sequence of the memory's first event (0 if none).
func memorySeq(store events.Port, memID string) uint64 {
	if log := store.List("memory", memID); len(log) > 0 {
		return log[0].Sequence
	}
	return 0
}

// serveNotesContext (RHZ-086, FR-RHZ-116) serves the notes bundle of one
// goal or mission: its head plus the memories whose about edge targets it —
// for a mission, the same seed rule as the task path (the mission AND its
// goal) — newest-first by first-event Sequence. Read-only: no retrieval, no
// trace, no Append. The id may be a handle (RHZ-073).
func (h *HTTPServer) serveNotesContext(w http.ResponseWriter, r *http.Request, kind, id string) {
	if handleShape(id) {
		id = buildHandleIndex(h.Store.All()).resolve(id)
	}
	bundle := notesContextBundle{Memories: []contextMemoryDTO{}}
	var targets []edge.Endpoint
	switch kind {
	case "goal":
		g, err := projector.ReplayGoal(h.Store.List("goal", id))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		bundle.Goal = &struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			State       string `json:"state"`
		}{g.ID, g.Description, string(g.State)}
		targets = []edge.Endpoint{{Type: "goal", ID: g.ID}}
	case "mission":
		m, err := projector.ReplayMission(h.Store.List("mission", id))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		bundle.Mission = &struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}{id, m.Description, mapState(m.State)}
		targets = []edge.Endpoint{{Type: "mission", ID: id}, {Type: "goal", ID: m.GoalID}}
	default:
		http.Error(w, "unknown context kind", http.StatusBadRequest)
		return
	}
	es := edge.Service{Store: h.Store}
	seedSet := map[string]bool{}
	for _, target := range targets {
		edges, err := es.ByNode(target.Type, target.ID)
		if err != nil {
			http.Error(w, "context assembly failed", http.StatusInternalServerError)
			return
		}
		for _, x := range edges {
			if x.Kind == edge.About && x.From.Type == "memory" && x.To == target {
				seedSet[x.From.ID] = true
			}
		}
	}
	for memID := range seedSet {
		mem, err := memory.Replay(h.Store.List("memory", memID))
		if err != nil {
			http.Error(w, "context assembly failed", http.StatusInternalServerError)
			return
		}
		bundle.Memories = append(bundle.Memories, memoryDTO(mem, memorySeq(h.Store, memID)))
	}
	// Newest-first; Sequence is unique per event so the order is total
	// (ID tiebreak only guards a seq-0 corner).
	sort.Slice(bundle.Memories, func(i, j int) bool {
		a, b := bundle.Memories[i], bundle.Memories[j]
		if a.Seq != b.Seq {
			return a.Seq > b.Seq
		}
		return a.ID < b.ID
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bundle)
}

// assembleSteps (RHZ-068, FR-RHZ-097) reads back a procedure.run instance:
// the missions spawned by taskID (outgoing spawn edges), each with its
// outgoing dependency edges as `after` and the questions asked against it as
// `gates`. Order is procedure.TopoOrder — the same single source assembly.Run
// spawned with, so write and read share one rule; a cycle or a dependency on
// a mission outside the spawn set surfaces as an error. A mission that
// spawned nothing yields an empty (non-nil) slice.
func assembleSteps(store events.Port, verifier *trust.Verifier, es edge.Service, taskID string) ([]contextStepDTO, error) {
	out := []contextStepDTO{}
	attestations, err := verifier.Attestations(store)
	if err != nil {
		return nil, err
	}
	outgoing, err := es.ByNode("mission", taskID)
	if err != nil {
		return nil, err
	}
	self := edge.Endpoint{Type: "mission", ID: taskID}
	stepSet := map[string]bool{}
	for _, x := range outgoing {
		if x.Kind == edge.Spawn && x.From == self && x.To.Type == "mission" {
			stepSet[x.To.ID] = true
		}
	}
	if len(stepSet) == 0 {
		return out, nil
	}
	// Questions are indexed once per request, not per step (one All() scan).
	questionIDs := map[string]bool{}
	for _, e := range store.All() {
		if e.AggregateType == "question" {
			questionIDs[e.AggregateID] = true
		}
	}
	gatesByMission := map[string][]contextGateDTO{}
	qs := question.Service{Store: store}
	for id := range questionIDs {
		q, err := qs.Get(id)
		if err != nil {
			return nil, err
		}
		if stepSet[q.MissionID] {
			var verification *verificationDTO
			if derived, err := questionVerification(store, verifier, attestations, q); err != nil {
				return nil, err
			} else if derived != nil {
				verification = &verificationDTO{Status: derived.Status, ClaimKind: derived.ClaimKind, Assurance: derived.Assurance, KeyID: derived.KeyID, KeyRevokedNow: derived.KeyRevokedNow}
			}
			state := questionState(q)
			decidedAt := ""
			if state == "approved" || state == "rejected" {
				decidedAt = latestEventCreatedAt(store.List("question", q.ID), "question.answered")
			}
			gatesByMission[q.MissionID] = append(gatesByMission[q.MissionID], contextGateDTO{ID: q.ID, Name: q.Title, State: state, DecisionReason: q.Reason, DecidedBy: q.ActorRef, DecidedAt: decidedAt, Verification: verification})
		}
	}
	byID := map[string]contextStepDTO{}
	steps := make([]procedure.Step, 0, len(stepSet))
	for stepID := range stepSet {
		sm, err := projector.ReplayMission(store.List("mission", stepID))
		if err != nil {
			return nil, err
		}
		deps, err := es.ByNode("mission", stepID)
		if err != nil {
			return nil, err
		}
		from := edge.Endpoint{Type: "mission", ID: stepID}
		after := []string{}
		for _, x := range deps {
			if x.Kind == edge.Dependency && x.From == from && x.To.Type == "mission" {
				after = append(after, x.To.ID)
			}
		}
		sort.Strings(after)
		gates := gatesByMission[stepID]
		if gates == nil {
			gates = []contextGateDTO{}
		}
		sort.Slice(gates, func(i, j int) bool { return gates[i].ID < gates[j].ID })
		byID[stepID] = contextStepDTO{ID: stepID, Name: sm.Description, State: mapState(sm.State), After: after, Gates: gates}
		steps = append(steps, procedure.Step{ID: stepID, After: after})
	}
	order, err := procedure.TopoOrder(steps)
	if err != nil {
		return nil, fmt.Errorf("steps: %w", err)
	}
	for _, st := range order {
		out = append(out, byID[st.ID])
	}
	return out, nil
}
