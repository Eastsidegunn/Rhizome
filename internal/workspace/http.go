package workspace

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"rhizome/internal/events"
	"rhizome/internal/trust"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type intentRequest struct {
	Intent
	Actor string `json:"actor"`
}

type missionDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Attention bool   `json:"attention"`
	// State carries the goal lifecycle state (RHZ-061, FR-RHZ-090; additive).
	State string `json:"state"`
	// Success carries the goal's success criterion (RHZ-067, FR-RHZ-096;
	// additive, omitempty keeps the wire unchanged for empty values).
	Success string `json:"success,omitempty"`
	// Handle is the short deterministic node handle (RHZ-073, FR-RHZ-103;
	// additive, always present). See handle.go for the rule.
	Handle string `json:"handle"`
}
type taskDTO struct {
	ID            string  `json:"id"`
	MissionID     string  `json:"missionId"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	CurrentAction string  `json:"currentAction,omitempty"`
	Progress      float64 `json:"progress,omitempty"`
	HasProgress   bool    `json:"hasProgress"`
	BlockedReason string  `json:"blockedReason,omitempty"`
	Attention     bool    `json:"attention"`
	Handle        string  `json:"handle"` // RHZ-073 (FR-RHZ-103), additive.
	// Assignee is the current assignee (RHZ-080, FR-RHZ-111; additive,
	// omitempty: an unassigned task serializes exactly as before). Last field.
	Assignee string `json:"assignee,omitempty"`
}
type gateDTO struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	HumanDecision string `json:"humanDecision,omitempty"`
	JanusDecision string `json:"janusDecision,omitempty"`
	Superseded    bool   `json:"superseded"`
	MissionID     string `json:"missionId,omitempty"`
	GoalID        string `json:"goalId,omitempty"` // RHZ-075 (FR-RHZ-108), additive.
	Name          string `json:"name,omitempty"`
	// RHZ-047 (D19, additive): omitted when absent per the projection 관례.
	RequestDigest  string `json:"requestDigest,omitempty"`
	DisplaySummary string `json:"displaySummary,omitempty"`
	ExpiresAt      int64  `json:"expiresAt,omitempty"`
	Source         string `json:"source,omitempty"`
	Body           string `json:"body,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	DecisionReason string `json:"decisionReason,omitempty"`
	DecidedBy      string `json:"decidedBy,omitempty"`
	// FR-RHZ-142: derived from the decision event envelope; absent for open gates.
	DecidedAt    string           `json:"decidedAt,omitempty"`
	Handle       string           `json:"handle"` // RHZ-073 (FR-RHZ-103), additive.
	Verification *verificationDTO `json:"verification,omitempty"`
}
type verificationDTO struct {
	Status        string `json:"status"`
	ClaimKind     string `json:"claimKind,omitempty"`
	Assurance     string `json:"assurance,omitempty"`
	KeyID         string `json:"keyId,omitempty"`
	KeyRevokedNow bool   `json:"keyRevokedNow,omitempty"`
}

type trustDTO struct {
	JournalID    string `json:"journalId"`
	GenesisKeyID string `json:"genesisKeyId"`
}
type attentionDTO struct {
	Kind        string `json:"kind"`
	RefID       string `json:"refId"`
	Cause       string `json:"cause"`
	SourceRef   string `json:"sourceRef,omitempty"`
	IncidentRef string `json:"incidentRef,omitempty"`
}
type requestDTO struct {
	ID          string   `json:"id"`
	Handle      string   `json:"handle"`
	Name        string   `json:"name"`
	State       string   `json:"state"`
	MissionID   string   `json:"missionId,omitempty"`
	GoalID      string   `json:"goalId,omitempty"`
	Why         string   `json:"why"`
	Where       string   `json:"where"`
	Commands    []string `json:"commands"`
	After       string   `json:"after"`
	Rollback    string   `json:"rollback,omitempty"`
	RequestedBy string   `json:"requestedBy"`
	CreatedAt   string   `json:"createdAt"`
	ClosedBy    string   `json:"closedBy,omitempty"`
	ClosedAt    string   `json:"closedAt,omitempty"`
	Memo        string   `json:"memo,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}
type requestCapabilityDTO struct {
	Complete string `json:"complete"`
	Unable   string `json:"unable"`
}
type dto struct {
	Missions     []missionDTO     `json:"missions"`
	Tasks        []taskDTO        `json:"tasks"`
	Gates        []gateDTO        `json:"gates"`
	Deliverables []deliverableDTO `json:"deliverables"`
	Edges        []edgeDTO        `json:"edges"`
	Counts       struct {
		Running  int `json:"running"`
		NeedsYou int `json:"needsYou"`
		Blocked  int `json:"blocked"`
	} `json:"counts"`
	Attention []attentionDTO `json:"attention"`
	// FR-RHZ-158: omitted together when the journal contains no requests.
	// They precede the long-pinned final capability fields.
	Requests            []requestDTO                    `json:"requests,omitempty"`
	RequestCapabilities map[string]requestCapabilityDTO `json:"requestCapabilities,omitempty"`
	Trust               *trustDTO                       `json:"trust,omitempty"`
	// RHZ-070 (FR-RHZ-099, additive): always present ({} when empty), keyed
	// by task id / internal gate id. These MUST stay the last two fields so
	// the pre-RHZ-070 prefix of the body is provably unchanged (A1 pins it).
	Capabilities     map[string]taskCapabilityDTO `json:"capabilities"`
	GateCapabilities map[string]gateCapabilityDTO `json:"gateCapabilities"`
}
type taskCapabilityDTO struct {
	Pause    string `json:"pause"`
	Resume   string `json:"resume"`
	Instruct string `json:"instruct"`
}
type gateCapabilityDTO struct {
	Approve        string `json:"approve"`
	Reject         string `json:"reject"`
	RequestChanges string `json:"requestChanges"`
}
type deliverableDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MissionID string `json:"missionId"`
	SourceRef string `json:"sourceRef"`
	Summary   string `json:"summary"`
	Handle    string `json:"handle"` // RHZ-073 (FR-RHZ-103), additive.
	// GoalID is set for a goal-bound deliverable (RHZ-081, FR-RHZ-112);
	// additive suffix, absent for mission-bound and pre-081 journals.
	GoalID string `json:"goalId,omitempty"`
}
type edgeDTO struct {
	ID          string            `json:"id"`
	From        map[string]string `json:"from"`
	To          map[string]string `json:"to"`
	EdgeKind    string            `json:"edgeKind"`
	Supersedes  string            `json:"supersedes,omitempty"`
	Actor       string            `json:"actor"`
	Correlation string            `json:"correlation"`
}
type envelope struct {
	Revision uint64 `json:"revision"`
	Body     dto    `json:"body"`
}

// requestIntentHasUnknownField detects exact, case-sensitive top-level keys only for
// RHZ-118 request intents. Other intent kinds retain their existing decoder.
func requestIntentHasUnknownField(raw json.RawMessage, kind string) bool {
	var allowed map[string]bool
	switch kind {
	case "request.create":
		allowed = map[string]bool{
			"kind": true, "actor": true, "name": true, "missionId": true,
			"goalId": true, "why": true, "where": true, "commands": true,
			"after": true, "rollback": true, "correlationId": true,
		}
	case "request.complete":
		allowed = map[string]bool{"kind": true, "actor": true, "requestId": true, "memo": true, "correlationId": true}
	case "request.unable", "request.cancel":
		allowed = map[string]bool{"kind": true, "actor": true, "requestId": true, "reason": true, "correlationId": true}
	default:
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return true
	}
	for key := range object {
		if !allowed[key] {
			return true
		}
	}
	return false
}

func toDTO(p Projection) dto {
	d := dto{Missions: []missionDTO{}, Tasks: []taskDTO{}, Gates: []gateDTO{}, Deliverables: []deliverableDTO{}, Edges: []edgeDTO{}, Attention: []attentionDTO{}, Capabilities: map[string]taskCapabilityDTO{}, GateCapabilities: map[string]gateCapabilityDTO{}}
	if p.Trust != nil {
		d.Trust = &trustDTO{JournalID: p.Trust.JournalID, GenesisKeyID: p.Trust.GenesisKeyID}
	}
	// RHZ-070 (FR-RHZ-099): map keys are emitted in sorted order by
	// encoding/json, so the wire is deterministic for a given journal.
	for id, c := range p.Capabilities {
		d.Capabilities[id] = taskCapabilityDTO{c.Pause, c.Resume, c.Instruct}
	}
	for id, g := range p.GateCapabilities {
		d.GateCapabilities[id] = gateCapabilityDTO{g.Approve, g.Reject, g.RequestChanges}
	}
	for _, m := range p.Missions {
		d.Missions = append(d.Missions, missionDTO{m.ID, m.Name, m.Attention, m.State, m.Success, p.handles.of("g", m.ID)})
	}
	for _, t := range p.Tasks {
		d.Tasks = append(d.Tasks, taskDTO{t.ID, t.MissionID, t.Name, t.State, t.CurrentAction, t.Progress, t.HasProgress, t.BlockedReason, t.Attention, p.handles.of("m", t.ID), t.Assignee})
	}
	for _, g := range p.Gates {
		var verification *verificationDTO
		if g.Verification != nil {
			verification = &verificationDTO{Status: g.Verification.Status, ClaimKind: g.Verification.ClaimKind, Assurance: g.Verification.Assurance, KeyID: g.Verification.KeyID, KeyRevokedNow: g.Verification.KeyRevokedNow}
		}
		d.Gates = append(d.Gates, gateDTO{ID: g.ID, State: g.State, HumanDecision: g.HumanDecision, JanusDecision: g.JanusDecision, Superseded: g.Superseded, MissionID: g.MissionID, GoalID: g.GoalID, Name: g.Name, RequestDigest: g.RequestDigest, DisplaySummary: g.DisplaySummary, ExpiresAt: g.ExpiresAt, Source: g.Source, Body: g.Body, Recommendation: g.Recommendation, DecisionReason: g.DecisionReason, DecidedBy: g.DecidedBy, DecidedAt: g.DecidedAt, Handle: p.handles.of(gateHandleTag(g.Source), g.ID), Verification: verification})
	}
	for _, a := range p.Attention {
		d.Attention = append(d.Attention, attentionDTO{a.Kind, a.RefID, a.Cause, a.SourceRef, a.IncidentRef})
	}
	if p.RequestCapabilities != nil {
		d.RequestCapabilities = map[string]requestCapabilityDTO{}
		for id, c := range p.RequestCapabilities {
			d.RequestCapabilities[id] = requestCapabilityDTO{Complete: c.Complete, Unable: c.Unable}
		}
	}
	for _, r := range p.Requests {
		commands := append([]string{}, r.Commands...)
		d.Requests = append(d.Requests, requestDTO{
			ID: r.ID, Handle: p.handles.of("r", r.ID), Name: r.Name, State: r.State,
			MissionID: r.MissionID, GoalID: r.GoalID, Why: r.Why, Where: r.Where,
			Commands: commands, After: r.After, Rollback: r.Rollback, RequestedBy: r.RequestedBy,
			CreatedAt: r.CreatedAt, ClosedBy: r.ClosedBy, ClosedAt: r.ClosedAt, Memo: r.Memo, Reason: r.Reason,
		})
	}
	d.Counts.Running, d.Counts.NeedsYou, d.Counts.Blocked = p.Counts.Running, p.Counts.NeedsYou, p.Counts.Blocked
	for _, x := range p.Deliverables {
		d.Deliverables = append(d.Deliverables, deliverableDTO{x.ID, x.Kind, x.MissionID, x.SourceRef, x.Summary, p.handles.of("d", x.ID), x.GoalID})
	}
	for _, x := range p.Edges {
		d.Edges = append(d.Edges, edgeDTO{x.ID, map[string]string{"type": x.From.Type, "id": x.From.ID}, map[string]string{"type": x.To.Type, "id": x.To.ID}, string(x.Kind), x.Supersedes, x.Actor, x.Correlation})
	}
	return d
}

// filterAssignee answers GET /v1/workspace?assignee=<x> (RHZ-080,
// FR-RHZ-111): "my work" is a question about the task list, so only tasks
// are narrowed (exact match on assignee); missions, gates, deliverables,
// edges, attention, capabilities and counts are returned unchanged. Counts
// deliberately stay the global figures — the dashboard numbers are facts
// about the whole workspace, not a recomputation over the filtered list, and
// keeping them global keeps the filtered body a strict subset of the
// unfiltered one. No parameter → the DTO is returned untouched (byte-
// identical to the unfiltered route). The SSE stream ignores the parameter.
func filterAssignee(d dto, assignee string) dto {
	if assignee == "" {
		return d
	}
	tasks := []taskDTO{}
	for _, t := range d.Tasks {
		if t.Assignee == assignee {
			tasks = append(tasks, t)
		}
	}
	d.Tasks = tasks
	return d
}

// deliverableQuery is the parsed GET /v1/workspace deliverable filter
// (RHZ-089, FR-RHZ-120). Zero value = no parameter given.
type deliverableQuery struct {
	kind, goal, mission string
	limit               int  // 0 = unlimited; validated > 0 when given
	desc                bool // deliverableOrder=desc
}

// parseDeliverableQuery reads the deliverable* parameters. Errors are the
// caller's 400: deliverableLimit must be an integer > 0 ("0", negatives and
// non-numbers are rejected rather than silently meaning "all"), and
// deliverableOrder must be "asc" or "desc". An absent parameter is the
// neutral value, so an empty query parses to the zero deliverableQuery.
func parseDeliverableQuery(q url.Values) (deliverableQuery, error) {
	var dq deliverableQuery
	dq.kind, dq.goal, dq.mission = q.Get("deliverableKind"), q.Get("deliverableGoal"), q.Get("deliverableMission")
	if raw := q.Get("deliverableLimit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return dq, errors.New("invalid deliverableLimit: want integer > 0")
		}
		dq.limit = n
	}
	switch q.Get("deliverableOrder") {
	case "", "asc":
	case "desc":
		dq.desc = true
	default:
		return dq, errors.New("invalid deliverableOrder: want asc or desc")
	}
	return dq, nil
}

// filterDeliverables answers the deliverable* parameters of GET
// /v1/workspace (RHZ-089, FR-RHZ-120): mmx-turn deliverables accumulate
// every turn, so a puller needs "the last N of my kind" without the whole
// monotonically growing list. Only deliverables[] is touched, in this fixed
// order: filter → order → limit. Missions, tasks, gates, edges, attention,
// capabilities and counts are returned unchanged (same rule as
// filterAssignee: the filtered body is a strict subset of the unfiltered
// one, counts stay global facts). No parameter → the DTO is returned
// untouched, byte-identical to today's route.
//
//   - deliverableKind: exact match on kind.
//   - deliverableGoal / deliverableMission: exact BINDING match — a
//     goal-bound deliverable (goalId set) matches only deliverableGoal, a
//     mission-bound one only deliverableMission. A goal filter does NOT
//     reach mission-bound deliverables of that goal's missions; binding is
//     the one fact the deliverable itself records, and the produces edge
//     already tells the rest. Either may be an RHZ-073 handle (g-…/m-…),
//     resolved through the same index that stamps the handles on the DTO;
//     an unknown handle or ID matches nothing. Filters intersect, so giving
//     both yields the empty list (no deliverable has two bindings).
//   - deliverableOrder: asc (default) keeps today's order (by id); desc is
//     newest registration first — descending journal Sequence of the
//     deliverable's first event, the mmx pull "등록순 마지막" semantics. A
//     deliverable's seq is fixed once appended, so the same journal always
//     yields the same bytes.
//   - deliverableLimit: keep the first n AFTER ordering, so desc+limit is
//     "the newest n".
//
// The SSE stream ignores all of these, as it ignores ?assignee=.
func filterDeliverables(d dto, p Projection, dq deliverableQuery) dto {
	if dq == (deliverableQuery{}) {
		return d
	}
	goal, mission := p.handles.resolve(dq.goal), p.handles.resolve(dq.mission)
	out := []deliverableDTO{}
	for _, x := range d.Deliverables {
		if dq.kind != "" && x.Kind != dq.kind {
			continue
		}
		if dq.goal != "" && (x.GoalID == "" || x.GoalID != goal) {
			continue
		}
		if dq.mission != "" && (x.MissionID == "" || x.MissionID != mission) {
			continue
		}
		out = append(out, x)
	}
	if dq.desc {
		sort.SliceStable(out, func(i, j int) bool { return p.deliverableSeq[out[i].ID] > p.deliverableSeq[out[j].ID] })
	}
	if dq.limit > 0 && len(out) > dq.limit {
		out = out[:dq.limit]
	}
	d.Deliverables = out
	return d
}

// gateHandleTag picks the handle stream for a gate row (RHZ-073): internal
// gates are questions ("q"), everything else is a JANUS gate ("a").
func gateHandleTag(source string) string {
	if source == "internal" {
		return "q"
	}
	return "a"
}

type HTTPServer struct {
	Store events.Port
	Trust *trust.Verifier
	// EnforceJANUS requires signed, Guard-verified inputs for both JANUS allow
	// and deny decisions. It is composition-root policy, never intent input.
	EnforceJANUS bool
	// ExecEvents projects bound JANUS session logs for /v1/execution (FR-RHZ-083,
	// T25). nil = adapter disabled: the events array stays empty (backward
	// compatible). Injected by the composition root, never constructed here.
	ExecEvents SessionEventSource
	// ExecInject delivers task.instruct text to the mission's running JANUS
	// session (FR-RHZ-119). nil = not wired: instructions are recorded only,
	// exactly the pre-RHZ-093 behavior. Injected by the composition root.
	ExecInject ExecInjector
	// ExecStart starts a JANUS execution for mission.start (FR-RHZ-123).
	// nil = not wired: mission.start is rejected with "execution start
	// unavailable" and zero writes. Injected by the composition root.
	ExecStart ExecStarter
	// Blobs serves GET /v1/blob (RHZ-056, FR-RHZ-086), and POST /v1/blob when
	// the value also implements BlobPutter (RHZ-081, FR-RHZ-112). nil = routes
	// disabled (404), same 미배선 convention as ExecEvents. Injected by the
	// composition root. uploadMu serializes the [Put → Register] critical
	// section of POST so concurrent identical uploads register exactly once.
	Blobs    BlobGetter
	uploadMu sync.Mutex
	// IndexRepo/IndexOut serve GET /v1/codeindex (RHZ-059, FR-RHZ-089). Both
	// empty = route disabled (404), same 미배선 convention. indexMu serializes
	// the [read latest → append main.advanced] critical section so concurrent
	// queries of a fresh sha emit exactly once.
	IndexRepo string
	IndexOut  string
	indexMu   sync.Mutex
	// contextMu serializes the [count → trace.Create] critical section of
	// GET /v1/context (RHZ-064, FR-RHZ-093) so concurrent queries never
	// collide on the state-deterministic trace id.
	contextMu sync.Mutex
	mu        sync.Mutex
	subs      map[chan Projection]bool
	execSubs  map[chan struct{}]bool
}

func NewHTTP(store events.Port) *HTTPServer {
	return &HTTPServer{Store: store, Trust: trust.NewAnchorless(), subs: map[chan Projection]bool{}, execSubs: map[chan struct{}]bool{}}
}

// Broadcast notifies workspace subscribers with the given projection and
// execution-stream subscribers with a recompute signal (RHZ-046 D11: one
// entry point for accepted changes; D12: every accepted change is pushed to
// every subscriber — revision de-duplication is the consumer's job per §2).
func (h *HTTPServer) Broadcast(p Projection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
	for ch := range h.execSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (h *HTTPServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/knowledge" && r.Method == http.MethodGet {
			h.serveKnowledgeSnapshot(w, r)
			return
		}
		if r.URL.Path == "/v1/workspace" && r.Method == http.MethodGet {
			q := r.URL.Query()
			// RHZ-089 (FR-RHZ-120): reject a malformed deliverable query
			// before any work; GET never writes either way.
			dq, e := parseDeliverableQuery(q)
			if e != nil {
				http.Error(w, e.Error(), 400)
				return
			}
			p, e := Snapshot(h.Store, h.Trust)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// Independent narrowings: ?assignee= touches tasks[] only,
			// deliverable* touches deliverables[] only.
			json.NewEncoder(w).Encode(envelope{p.Revision, filterDeliverables(filterAssignee(toDTO(p), q.Get("assignee")), p, dq)})
			return
		}
		if r.URL.Path == "/v1/workspace/stream" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			p, e := Snapshot(h.Store, h.Trust)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			ch := make(chan Projection, 8)
			h.mu.Lock()
			h.subs[ch] = true
			h.mu.Unlock()
			defer func() { h.mu.Lock(); delete(h.subs, ch); h.mu.Unlock() }()
			write := func(kind string, x Projection) {
				b, _ := json.Marshal(envelope{x.Revision, toDTO(x)})
				w.Write([]byte("event: " + kind + "\ndata: " + string(b) + "\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			write("snapshot", p)
			for {
				select {
				case <-r.Context().Done():
					return
				case x := <-ch:
					write("projection", x)
				}
			}
		}
		if strings.HasPrefix(r.URL.Path, "/v1/execution/") {
			// RHZ-046 part 2: the contract-defined surface is exactly these
			// two GET routes; everything else falls through to 404 (D14).
			// RHZ-094 (FR-RHZ-121): mission ids may contain "/" (e.g.
			// "mission.complete/fail intent"), so route on the ESCAPED path
			// (a literal "/" arrives as %2F) and unescape the one segment;
			// a RHZ-073 handle is accepted in place of the id. Callers that
			// send a raw "/" still split into two segments and get 404.
			parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/execution/"), "/")
			taskID := ""
			if len(parts) >= 1 {
				if u, e := url.PathUnescape(parts[0]); e == nil {
					taskID = buildHandleIndex(h.Store.All()).resolve(u)
				}
			}
			if r.Method == http.MethodGet && len(parts) == 1 && taskID != "" {
				h.serveExecutionSnapshot(w, r, taskID)
				return
			}
			if r.Method == http.MethodGet && len(parts) == 2 && taskID != "" && parts[1] == "stream" {
				h.serveExecutionStream(w, r, taskID)
				return
			}
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/v1/context" && r.Method == http.MethodGet {
			h.serveContext(w, r)
			return
		}
		if r.URL.Path == "/v1/codeindex" && r.Method == http.MethodGet {
			h.serveCodeIndex(w, r)
			return
		}
		if r.URL.Path == "/v1/blob" && r.Method == http.MethodPost {
			// RHZ-081 (FR-RHZ-112): byte upload; other methods on the bare
			// path fall through to 404.
			h.serveBlobUpload(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/blob/") {
			// RHZ-056 (FR-RHZ-086): exactly one GET route. Extra segments and
			// other methods fall through to 404, matching the manual-routing
			// convention of this handler.
			seg := strings.TrimPrefix(r.URL.Path, "/v1/blob/")
			if r.Method == http.MethodGet && seg != "" && !strings.Contains(seg, "/") {
				h.serveBlob(w, r, seg)
				return
			}
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/terminal/") {
			w.WriteHeader(http.StatusNotImplemented)
			json.NewEncoder(w).Encode(map[string]string{"reason": "JANUS adapter 의존"})
			return
		}
		if r.URL.Path == "/v1/intent" && r.Method == http.MethodPost {
			if storePoisoned(h.Store) {
				servePoisoned(w)
				return
			}
			var raw json.RawMessage
			if json.NewDecoder(r.Body).Decode(&raw) != nil {
				http.Error(w, "invalid json", 400)
				return
			}
			var in intentRequest
			if json.Unmarshal(raw, &in) != nil {
				http.Error(w, "invalid json", 400)
				return
			}
			in.Intent.requestUnknownField = requestIntentHasUnknownField(raw, in.Kind)
			res, e := RelayIntentHooks(h.Store, in.Intent, in.Actor, trust.Authority{}, RelayHooks{Inject: h.ExecInject, Start: h.ExecStart, EnforceJANUS: h.EnforceJANUS})
			if storePoisoned(h.Store) || errors.Is(e, events.ErrPoisoned) {
				servePoisoned(w)
				return
			}
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(res)
			if res.Accepted {
				// FR-RHZ-080: note changes reuse workspace "projection" push as
				// a GET /v1/knowledge requery signal, not a knowledge payload.
				if p, x := Snapshot(h.Store, h.Trust); x == nil {
					h.Broadcast(p)
				}
			}
			return
		}
		http.NotFound(w, r)
	})
}

func (h *HTTPServer) serveExecutionSnapshot(w http.ResponseWriter, r *http.Request, taskID string) {
	p, err := ExecutionSnapshot(h.Store, taskID, h.ExecEvents)
	if errors.Is(err, ErrUnknownTask) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(executionEnvelope{p.Revision, toExecutionDTO(p)})
}

func (h *HTTPServer) serveExecutionStream(w http.ResponseWriter, r *http.Request, taskID string) {
	p, err := ExecutionSnapshot(h.Store, taskID, h.ExecEvents)
	if errors.Is(err, ErrUnknownTask) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan struct{}, 8)
	h.mu.Lock()
	h.execSubs[ch] = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.execSubs, ch); h.mu.Unlock() }()
	write := func(kind string, x ExecutionProjection) {
		b, _ := json.Marshal(executionEnvelope{x.Revision, toExecutionDTO(x)})
		w.Write([]byte("event: " + kind + "\ndata: " + string(b) + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	write("snapshot", p)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			x, err := ExecutionSnapshot(h.Store, taskID, h.ExecEvents)
			if err != nil {
				return // The stream cannot claim facts it can no longer derive.
			}
			write("projection", x)
		}
	}
}
