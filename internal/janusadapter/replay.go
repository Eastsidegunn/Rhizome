package janusadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/execution"
)

var (
	ErrObservationCorrupt = errors.New("OBSERVATION_CORRUPT")
	ErrSessionReplaced    = errors.New("SESSION_REPLACED")
	ErrCursorMigration    = errors.New("cursor migration required")
	spanRE                = regexp.MustCompile(`^[0-9a-f]{16}$`)
	replayTraceRE         = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type SubagentDone struct{ Result, Status string }
type ApprovalResponse struct {
	Seq                int64
	Key                approval.RequestKey
	Decision           approval.Decision
	Reason, ResponseID string
}

// ApprovalRequest is the observed request identity plus display strings only
// (RHZ-047): the raw args stay behind in the session log — they are never
// copied into a Batch or persisted anywhere in Rhizome (비복제 헌장).
type ApprovalRequest struct {
	Seq          int64
	Key          approval.RequestKey
	Name, Reason string
}

// Batch contains summaries from the WHOLE replay, including the cursor prefix.
// SessionRef is supplied by the caller from the durable binding, never inferred.
// No raw, args, or approval-request body is retained.
type Batch struct {
	SessionRef, TraceID string
	FromSeq, ThroughSeq int64
	Done                []SubagentDone
	SessionEnded        bool
	ApprovalResponses   []ApprovalResponse
	ApprovalRequests    []ApprovalRequest
}

type replayEnvelope struct {
	Kind    string          `json:"kind"`
	Seq     int64           `json:"seq"`
	TraceID string          `json:"trace_id"`
	SpanID  string          `json:"span_id"`
	Payload json.RawMessage `json:"payload"`
}

// FR-RHZ-076: parsing is store-free and never returns a partial batch.
func ParseReplay(r io.Reader, wantTrace string, fromSeq int64) (Batch, error) {
	if r == nil || fromSeq < 0 || !replayTraceRE.MatchString(wantTrace) {
		return Batch{}, fmt.Errorf("invalid replay arguments")
	}
	out := Batch{TraceID: wantTrace, FromSeq: fromSeq}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return Batch{}, fmt.Errorf("%w: read: %w", ErrObservationCorrupt, err)
		}
		if len(line) == 0 && err == io.EOF {
			break
		}
		ev, e := decodeReplayLine(line)
		if e != nil {
			return Batch{}, e
		}
		if ev.TraceID != wantTrace {
			return Batch{}, ErrSessionReplaced
		}
		// Compare by subtraction to avoid overflow at int64max.
		if ev.Seq <= 0 || ev.Seq-1 != out.ThroughSeq {
			return Batch{}, fmt.Errorf("%w: sequence", ErrObservationCorrupt)
		}
		if e = collectReplay(&out, ev); e != nil {
			return Batch{}, e
		}
		out.ThroughSeq = ev.Seq
		if err == io.EOF {
			break
		}
	}
	if out.ThroughSeq < fromSeq {
		return Batch{}, fmt.Errorf("%w: history shorter than cursor", ErrObservationCorrupt)
	}
	return out, nil
}

// SessionEvent is the read-only projection of one JANUS session-log row for
// the outbound execution emit (FR-RHZ-083, T25 multi-turn). It carries only
// the envelope facts JANUS already records — seq, kind, actor, ts and the two
// usage counters — never the raw payload/args (비복제 헌장). It is a pure
// projection: no synthesis, no gap inference, no second writer.
type SessionEvent struct {
	Seq               int64
	Kind, Actor       string
	TS                int64
	UsageIn, UsageOut int64
}

// ProjectSessionEvents renders the WHOLE JANUS session log (hx replay NDJSON)
// as an ordered, contiguous SessionEvent slice (FR-RHZ-083). It reuses the
// same envelope validation and seq contiguity guard as ParseReplay: seq starts
// at 1 and each row is exactly +1, any gap/reorder/regression is
// ErrObservationCorrupt (NO gap inference), a foreign trace_id is
// ErrSessionReplaced. It never persists anything and never mutates its input;
// per-session usage totals and idle/last-activity are the consumer's job to
// derive from ts + usage across the returned events (no server-side synthesis).
func ProjectSessionEvents(r io.Reader, wantTrace string) ([]SessionEvent, error) {
	if r == nil || !replayTraceRE.MatchString(wantTrace) {
		return nil, fmt.Errorf("invalid replay arguments")
	}
	out := []SessionEvent{}
	var through int64
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("%w: read: %w", ErrObservationCorrupt, err)
		}
		if len(line) == 0 && err == io.EOF {
			break
		}
		ev, e := decodeReplayLine(line)
		if e != nil {
			return nil, e
		}
		if ev.TraceID != wantTrace {
			return nil, ErrSessionReplaced
		}
		// Compare by subtraction to avoid overflow at int64max (contiguous seq).
		if ev.Seq <= 0 || ev.Seq-1 != through {
			return nil, fmt.Errorf("%w: sequence", ErrObservationCorrupt)
		}
		// decodeReplayLine already type-checked these; capture the values.
		var meta struct {
			Actor    string `json:"actor"`
			TS       int64  `json:"ts"`
			UsageIn  int64  `json:"usage_in"`
			UsageOut int64  `json:"usage_out"`
		}
		if json.Unmarshal(line, &meta) != nil {
			return nil, fmt.Errorf("%w: envelope meta", ErrObservationCorrupt)
		}
		out = append(out, SessionEvent{Seq: ev.Seq, Kind: ev.Kind, Actor: meta.Actor, TS: meta.TS, UsageIn: meta.UsageIn, UsageOut: meta.UsageOut})
		through = ev.Seq
		if err == io.EOF {
			break
		}
	}
	return out, nil
}

func decodeReplayLine(line []byte) (replayEnvelope, error) {
	var ev replayEnvelope
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return ev, fmt.Errorf("%w: invalid json object", ErrObservationCorrupt)
	}
	for k, v := range fields {
		var target any
		switch k {
		case "actor", "kind", "parent_span_id", "raw", "span_id", "trace_id":
			target = new(string)
		case "seq", "ts", "usage_in", "usage_out":
			target = new(int64)
		case "payload":
			target = new(map[string]json.RawMessage)
		default:
			return ev, fmt.Errorf("%w: unknown envelope field %s", ErrObservationCorrupt, k)
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, target) != nil {
			return ev, fmt.Errorf("%w: envelope field %s", ErrObservationCorrupt, k)
		}
	}
	for _, k := range []string{"actor", "kind", "seq", "span_id", "trace_id", "ts"} {
		if _, ok := fields[k]; !ok {
			return ev, fmt.Errorf("%w: missing %s", ErrObservationCorrupt, k)
		}
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return ev, fmt.Errorf("%w: envelope: %v", ErrObservationCorrupt, err)
	}
	if ev.Kind == "" || !replayTraceRE.MatchString(ev.TraceID) || !spanRE.MatchString(ev.SpanID) {
		return ev, fmt.Errorf("%w: envelope identity", ErrObservationCorrupt)
	}
	return ev, nil
}

// Known payload string fields are type-checked even in the cursor prefix.
// Unknown payload fields, including approval args, are not copied to Batch.
func payloadStrings(raw json.RawMessage, fields ...string) (map[string]json.RawMessage, error) {
	var p map[string]json.RawMessage
	if len(raw) != 0 && json.Unmarshal(raw, &p) != nil {
		return nil, fmt.Errorf("%w: payload", ErrObservationCorrupt)
	}
	for _, k := range fields {
		if v, ok := p[k]; ok {
			var s string
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &s) != nil {
				return nil, fmt.Errorf("%w: payload field %s", ErrObservationCorrupt, k)
			}
		}
	}
	return p, nil
}

func collectReplay(out *Batch, ev replayEnvelope) error {
	switch ev.Kind {
	case "subagent/done":
		if _, err := payloadStrings(ev.Payload, "result", "status"); err != nil {
			return err
		}
		var d struct {
			Result string `json:"result"`
			Status string `json:"status"`
		}
		if json.Unmarshal(ev.Payload, &d) != nil || !doneStatus(d.Status) {
			return fmt.Errorf("%w: done status", ErrObservationCorrupt)
		}
		out.Done = append(out.Done, SubagentDone{Result: d.Result, Status: d.Status})
	case "session/end":
		out.SessionEnded = true // This kind has no required payload.
	case "subagent/approval_request":
		if _, err := payloadStrings(ev.Payload, "call_id", "name", "reason", "request_id"); err != nil {
			return err
		}
		var p struct {
			Name      string `json:"name"`
			Reason    string `json:"reason"`
			RequestID string `json:"request_id"`
		}
		// JANUS schema: request_id is required, minLength 1 (RHZ-047 L-e).
		if json.Unmarshal(ev.Payload, &p) != nil || p.RequestID == "" {
			return fmt.Errorf("%w: approval request_id required", ErrObservationCorrupt)
		}
		// Only the key and display strings cross into the Batch; args stays
		// behind in the session log (비복제 헌장).
		out.ApprovalRequests = append(out.ApprovalRequests, ApprovalRequest{Seq: ev.Seq, Key: approval.RequestKey{TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: p.RequestID}, Name: p.Name, Reason: p.Reason})
	case "policy/decision":
		if _, err := payloadStrings(ev.Payload, "decision", "profile_id", "request_id", "response_id", "reason", "decision_source", "actor_ref", "operation_id", "correlation_id", "human_intent_id"); err != nil {
			return err
		}
		var p struct {
			Decision   approval.Decision `json:"decision"`
			ProfileID  string            `json:"profile_id"`
			RequestID  string            `json:"request_id"`
			ResponseID string            `json:"response_id"`
			Reason     string            `json:"reason"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || !validDecision(p.Decision) || strings.TrimSpace(p.ProfileID) == "" {
			return fmt.Errorf("%w: policy decision", ErrObservationCorrupt)
		}
		if p.Decision == approval.Deny && strings.TrimSpace(p.Reason) == "" {
			return fmt.Errorf("%w: deny reason required", ErrObservationCorrupt)
		}
		// request_id is optional: local decisions without a relay key are valid.
		if p.RequestID != "" {
			out.ApprovalResponses = append(out.ApprovalResponses, ApprovalResponse{Seq: ev.Seq, Key: approval.RequestKey{TraceID: ev.TraceID, SpanID: ev.SpanID, RequestID: p.RequestID}, Decision: p.Decision, Reason: p.Reason, ResponseID: p.ResponseID})
		}
	}
	return nil
}

func Cursor19(seq int64) (string, error) {
	if seq < 0 {
		return "", fmt.Errorf("%w: negative cursor", ErrCursorMigration)
	}
	return fmt.Sprintf("%019d", seq), nil
}
func cursorNumber(s string) (int64, error) {
	if s == "" {
		return 0, nil
	} // Legacy empty cursor remains supported.
	if len(s) != 19 {
		return 0, ErrCursorMigration
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrCursorMigration
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrCursorMigration
	}
	return n, nil
}
func doneStatus(s string) bool { return s == "ok" || s == "error" || s == "stopped" }

// v1 supplementary classification (provisional, not part of the ratified
// mapping). Collector completion has no dedicated v1
// kind and is not inferred.
func batchState(b Batch) execution.State {
	if !b.SessionEnded {
		return execution.Observing
	}
	if len(b.Done) == 0 {
		return execution.Unknown
	}
	state := execution.Succeeded
	for _, d := range b.Done {
		if d.Status == "error" {
			return execution.Failed
		}
		if d.Status == "stopped" {
			state = execution.Cancelled
		}
	}
	return state
}

func validateBatch(b Batch) error {
	if b.FromSeq < 0 || b.ThroughSeq < b.FromSeq || !replayTraceRE.MatchString(b.TraceID) {
		return fmt.Errorf("%w: batch bounds/trace", ErrObservationCorrupt)
	}
	for _, d := range b.Done {
		if !doneStatus(d.Status) {
			return fmt.Errorf("%w: batch done", ErrObservationCorrupt)
		}
	}
	var last int64
	for _, a := range b.ApprovalResponses {
		if a.Seq <= last || a.Seq > b.ThroughSeq || a.Key.TraceID != b.TraceID || !spanRE.MatchString(a.Key.SpanID) || a.Key.RequestID == "" || !validDecision(a.Decision) || (a.Decision == approval.Deny && strings.TrimSpace(a.Reason) == "") {
			return fmt.Errorf("%w: batch approval", ErrObservationCorrupt)
		}
		last = a.Seq
	}
	var lastReq int64
	for _, q := range b.ApprovalRequests {
		if q.Seq <= lastReq || q.Seq > b.ThroughSeq || q.Key.TraceID != b.TraceID || !spanRE.MatchString(q.Key.SpanID) || q.Key.RequestID == "" {
			return fmt.Errorf("%w: batch request", ErrObservationCorrupt)
		}
		lastReq = q.Seq
	}
	return nil
}

// ObserveBatch must run within the caller's single-writer boundary. It creates
// observations only. Multi-aggregate appends are NOT a transaction: on an append
// failure previous approval observations remain durable, but the execution cursor
// does not advance. Reprocessing skips exactly matching durable observations.
func ObserveBatch(es execution.Service, as approval.Service, execID, source string, b Batch) (execution.Ref, error) {
	return observeBatch(es, as, execID, source, b, false)
}

// ObserveApprovals is ObserveBatch with the cursor held (FR-RHZ-119, D22
// gate B′): the batch is validated and its approval observations are made
// durable exactly as ObserveBatch does, but no execution.observed is written
// and the cursor stays where it is. The loop uses it while an open approval
// request in the batch is still unsurfaced, so the next tick re-reads the
// same range and retries surfacing.
func ObserveApprovals(es execution.Service, as approval.Service, execID, source string, b Batch) (execution.Ref, error) {
	return observeBatch(es, as, execID, source, b, true)
}

func observeBatch(es execution.Service, as approval.Service, execID, source string, b Batch, holdCursor bool) (execution.Ref, error) {
	if es.Store == nil || as.Store == nil {
		return execution.Ref{}, fmt.Errorf("nil event store")
	}
	log := es.Store.List("execution", execID)
	// Preserve an explicit migration error even if execution.Replay would wrap it.
	for _, ev := range log {
		var p struct{ Cursor string }
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return execution.Ref{}, err
		}
		if _, err := cursorNumber(p.Cursor); err != nil {
			return execution.Ref{}, err
		}
	}
	r, err := execution.Replay(log)
	if err != nil {
		return execution.Ref{}, err
	}
	current, err := cursorNumber(r.Cursor)
	if err != nil {
		return execution.Ref{}, err
	}
	switch r.State {
	case execution.Succeeded, execution.Failed, execution.Cancelled, execution.Unknown:
		return r, nil // No approval writes either; never automatically resolve unknown.
	case execution.Accepted, execution.Observing:
	default:
		return execution.Ref{}, fmt.Errorf("not observable")
	}
	if err = validateBatch(b); err != nil {
		return execution.Ref{}, err
	}
	if b.TraceID != r.ExternalID {
		return execution.Ref{}, ErrSessionReplaced
	}
	if strings.TrimSpace(source) == "" || (b.SessionRef != "" && b.SessionRef != source) || (r.Binding != nil && (r.Binding.TraceID != b.TraceID || r.Binding.SessionDB != source)) {
		return execution.Ref{}, fmt.Errorf("session reference mismatch")
	}
	if b.ThroughSeq < current {
		return execution.Ref{}, fmt.Errorf("%w: history shorter than cursor", ErrObservationCorrupt)
	}
	// One snapshot, one replay per approval aggregate; validate before any writes.
	streams := map[string][]events.Event{}
	for _, ev := range as.Store.All() {
		if ev.AggregateType == "approval" {
			streams[ev.AggregateID] = append(streams[ev.AggregateID], ev)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	refs := map[approval.RequestKey]approval.Ref{}
	for _, id := range ids {
		a, e := approval.Replay(streams[id])
		if e != nil {
			return execution.Ref{}, e
		}
		refs[a.Key] = a
	}
	type observation struct {
		id       string
		response ApprovalResponse
	}
	pending := []observation{}
	for _, response := range b.ApprovalResponses {
		a, ok := refs[response.Key]
		if !ok {
			continue
		} // Intentional: JANUS local/unrelated decisions have no Rhizome gate.
		if a.State == approval.Observed {
			if a.ResponseSeq != response.Seq || a.JanusDecision != response.Decision || a.JanusReason != response.Reason {
				return execution.Ref{}, fmt.Errorf("approval observation conflict")
			}
			continue
		}
		pending = append(pending, observation{id: a.ID, response: response})
		a.State, a.ResponseSeq, a.JanusDecision, a.JanusReason = approval.Observed, response.Seq, response.Decision, response.Reason
		refs[a.Key] = a // Detect conflicting responses within the same batch before writes.
	}
	for _, p := range pending {
		if _, err = as.Observe(p.id, p.response.Seq, p.response.Decision, p.response.Reason); err != nil {
			return execution.Ref{}, err
		}
	}
	if b.ThroughSeq == current || holdCursor {
		return r, nil
	}
	state := batchState(b)
	if state == execution.Unknown {
		// MarkUnknown has no cursor field. Keep the cursor unchanged; terminal/unknown
		// early return makes repeat consumption a no-op until explicit reconciliation.
		return es.MarkUnknown(execID, execution.NeedsHuman, "", source, fmt.Sprintf("session/end at seq %d without subagent/done", b.ThroughSeq))
	}
	next, err := Cursor19(b.ThroughSeq)
	if err != nil {
		return execution.Ref{}, err
	}
	return es.ObserveState(execID, next, "JANUS replay: "+string(state), source, state)
}
