package janusadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/gaterequest"
	"rhizome/internal/wake"
)

// ReplaySource stands in for the hx replay process: it receives the operator
// config and the durable session coordinates and returns the full NDJSON
// stream (always from seq 1; ParseReplay skips the cursor prefix).
type ReplaySource func(cfg RunConfig, sessionDB, traceID string) (io.Reader, error)

// ErrorHook receives every per-item loop failure. scope is the aggregate type
// ("execution", "approval", or "loop" for assembly faults), id the aggregate.
type ErrorHook func(scope, id string, err error)

// MinObserveInterval is the smallest accepted observation period: every tick
// spawns an hx replay process per bound execution, so sub-second polling is
// refused (RHZ-046 D9).
const MinObserveInterval = time.Second

// MaxTailPages bounds the events_tail `more` follow-ups in one tick per
// execution (FR-RHZ-119): beyond it the tick processes the range read so far
// and the next tick continues from the durable cursor. Never loop forever.
const MaxTailPages = 8

// TailSource reads one events_tail page: sessionID is the session trace_id,
// fromSeq the durable cursor (events with seq > fromSeq are returned).
type TailSource func(sessionID string, fromSeq int64) (TailResult, error)

// payloadKinds are the session-log kinds whose meaning lives in the payload
// (ParseReplay/collectReplay, JANUS gen.Kind*): a tail page containing any
// of them cannot be observed from envelopes alone and routes the tick to hx
// replay.
var payloadKinds = map[string]bool{
	"subagent/approval_request": true,
	"policy/decision":           true,
	"subagent/done":             true,
	"session/end":               true,
}

// Loop wires the RHZ-043 adapter parts into rhizome serve (FR-RHZ-077). Its
// automatic behaviors are exactly four (RHZ-046, RHZ-047 D18):
// replay observation of bound executions, approval pass-through
// (input_recorded -> durable dispatch -> socket submit), stop submission for
// durable stop_requested intents, and surfacing of observed approval
// requests as pending gates (Wake -> wait_human Decision -> approvalrequest;
// a reversible record of an observed fact that raises it to a human — never
// a decision). It never starts executions: StartExecution stays an explicit
// program path (RHZ-046 D7), and the loop makes no contract judgment of its
// own.
//
// Surfacing retry safety (RHZ-047 D22, re-based by RHZ-093 / FR-RHZ-119 gate
// B′): surfacing runs BEFORE the cursor may advance, and the cursor never
// advances past an OPEN, UNSURFACED approval request — open meaning no
// durable approval/approvalrequest aggregate for its key, no policy/decision
// for it in the session log, and the session not exited. A failed surface
// therefore holds the cursor; the next tick re-reads the same range through
// hx replay and retries. No process state is involved: cursor,
// approvalrequest and approval aggregates are all durable, so a restart makes
// the same judgment.
//
// Observation is a hybrid (FR-RHZ-119): when Tail is wired, each tick first
// pages the socket events_tail from the durable cursor (at most MaxTailPages
// pages). Tail envelopes carry no payload, so a page containing a kind that
// needs one (approval request, policy decision, done, session end), an
// exited session, or an unavailable socket (it dies with hx run) sends the
// tick down the existing hx replay path, which is therefore the only place a
// terminal state is ever concluded. Tail-only pages advance the cursor with
// the same execution.observed event replay writes (no new event types).
//
// v1 constraint: RunConfig carries one ApprovalEndpoint, so a serve process
// assumes at most one concurrently active JANUS session (the level 1-2 smoke
// shape). Multi-session serving needs the session->socket mapping registry
// (contract open item) and is a follow-up task.
type Loop struct {
	ES     execution.Service
	AS     approval.Service
	Client Client
	Replay ReplaySource
	// Tail is the socket events_tail read (Client.EventsTail in serve). nil
	// keeps the pre-RHZ-093 behavior: every tick observes through hx replay.
	Tail TailSource
	// IdleTimeout (FR-RHZ-119): when the tail reports a
	// running session whose last_activity_ts (JANUS ts, Unix ms) is at least
	// this old, the loop records the existing execution.stop_requested
	// (reason budget_exceeded, actor rhizome:idle-timeout) once; the regular
	// stop submission then relays it. 0 disables. Judged on the tail path
	// only — the replay path makes no idle judgment.
	IdleTimeout time.Duration
	// Now supplies the idle clock; nil = time.Now. Injected for determinism.
	Now     func() time.Time
	Cfg     RunConfig
	OnError ErrorHook
	// Broadcast is invoked after a tick that made at least one durable write,
	// so the existing workspace projection surface reflects loop observations.
	Broadcast func()

	mu sync.Mutex // one tick at a time: the loop never multiplies writers
	// stopAcked remembers process-lifetime stop receipts (stop_accepted or
	// already_terminal). Receipts are not durable by contract v1.4 §7, so a
	// restarted serve resubmits; the deterministic stop_id keeps that safe.
	stopAcked map[string]bool
}

func (l *Loop) report(scope, id string, err error) {
	if l.OnError != nil {
		l.OnError(scope, id, err)
		return
	}
	fmt.Fprintf(os.Stderr, "janus loop %s %s: %v\n", scope, id, err)
}

func aggregateIDs(s events.Port, typ string) []string {
	seen := map[string]bool{}
	ids := []string{}
	for _, e := range s.All() {
		if e.AggregateType == typ && !seen[e.AggregateID] {
			seen[e.AggregateID] = true
			ids = append(ids, e.AggregateID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Tick runs one observation cycle. Errors are surfaced per item through
// OnError and never kill the cycle; a failed item retries from the same
// durable state on the next tick (contract §5.2: same-cursor safe retry).
func (l *Loop) Tick() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ES.Store == nil || l.AS.Store == nil {
		l.report("loop", "", fmt.Errorf("nil event store"))
		return
	}
	if l.Replay == nil {
		l.report("loop", "", fmt.Errorf("nil replay source"))
		return
	}
	if l.stopAcked == nil {
		l.stopAcked = map[string]bool{}
	}
	wrote := false
	for _, id := range aggregateIDs(l.ES.Store, "execution") {
		r, err := execution.Replay(l.ES.Store.List("execution", id))
		if err != nil {
			l.report("execution", id, err)
			continue
		}
		l.observeExecution(id, r, &wrote)
		l.submitStop(id)
	}
	for _, id := range aggregateIDs(l.AS.Store, "approval") {
		l.relayApproval(id, &wrote)
	}
	if wrote && l.Broadcast != nil {
		l.Broadcast()
	}
}

// observeExecution observes one bound, active execution: events_tail first
// when wired (tail-only ranges advance the cursor), otherwise or on fallback
// the hx replay stream. All durable writes go through ObserveBatch /
// ObserveApprovals: the cursor advances only after the batch's approval
// observations are durable and no open approval request is left unsurfaced
// (D22 gate), and terminal or unknown executions are never resumed
// automatically.
func (l *Loop) observeExecution(id string, r execution.Ref, wrote *bool) {
	if r.Binding == nil || (r.State != execution.Accepted && r.State != execution.Observing) {
		return
	}
	from, err := cursorNumber(r.Cursor)
	if err != nil {
		l.report("execution", id, err)
		return
	}
	if l.Tail != nil && l.observeTail(id, r, from, wrote) {
		return
	}
	stream, err := l.Replay(l.Cfg, r.Binding.SessionDB, r.Binding.TraceID)
	if err != nil {
		l.report("execution", id, err)
		return
	}
	b, err := ParseReplay(stream, r.ExternalID, from)
	if err != nil {
		l.report("execution", id, err)
		return
	}
	b.SessionRef = r.Binding.SessionDB
	// D22 gate (B′): surface BEFORE any cursor advance. While an open request
	// is still unsurfaced only the approval observations are written and the
	// cursor is held, so the next tick re-reads the same range and retries.
	open := l.surfaceRequests(id, r, b, wrote)
	before := len(l.ES.Store.All())
	if open > 0 {
		_, err = ObserveApprovals(l.ES, l.AS, id, r.Binding.SessionDB, b)
	} else {
		_, err = ObserveBatch(l.ES, l.AS, id, r.Binding.SessionDB, b)
	}
	if err != nil {
		l.report("execution", id, err)
	} else if len(l.ES.Store.All()) > before {
		*wrote = true
	}
}

// observeTail pages events_tail from the durable cursor. It returns true when
// the tick is fully handled here (tail-only advance, nothing new, or a
// report-only fault) and false when the tick must continue through hx replay:
// a payload kind on a page, an exited session, or a socket that is
// unavailable / does not know the session (it dies with hx run, so terminal
// observation always lands through replay).
func (l *Loop) observeTail(id string, r execution.Ref, from int64, wrote *bool) bool {
	cur := from
	var last TailResult
	for page := 0; page < MaxTailPages; page++ {
		res, err := l.Tail(r.Binding.TraceID, cur)
		if err != nil {
			// LOG_INVALID / REQUEST_MISMATCH are report-only (the log itself or
			// our request is wrong; replay would not help). Every other tail
			// failure — unavailable socket, unknown session, log unavailable,
			// any other reason, a parse failure — falls back to hx replay, the
			// authoritative path, so a session end is always concluded.
			var te ErrTail
			if errors.As(err, &te) && (te.Reason == "LOG_INVALID" || te.Reason == "REQUEST_MISMATCH") {
				l.report("execution", id, err)
				return true
			}
			return false
		}
		if err = checkTailPage(res, r.ExternalID, cur); err != nil {
			// Gap/foreign trace: report only, no durable write (same as a
			// corrupt replay today); the next tick retries from the same cursor.
			l.report("execution", id, err)
			return true
		}
		for _, ev := range res.Events {
			if payloadKinds[ev.Kind] {
				return false
			}
		}
		last, cur = res, res.NextFromSeq
		if !res.More {
			break
		}
	}
	if last.Session.State == "exited" {
		return false // Terminal facts live in payloads: conclude through replay.
	}
	if cur != from {
		next, err := Cursor19(cur)
		if err != nil {
			l.report("execution", id, err)
			return true
		}
		if _, err = l.ES.ObserveState(id, next, "JANUS tail: observing", r.Binding.SessionDB, execution.Observing); err != nil {
			l.report("execution", id, err)
			return true
		}
		*wrote = true
	}
	l.idleStop(id, r, last.Session, wrote)
	return true
}

// IdleStopActor and IdleStopReason identify an idle-timeout stop request
// (FR-RHZ-119): the contract reason vocabulary has no idle
// value, so budget_exceeded (time budget) is used and the actor names the
// policy.
const (
	IdleStopReason = "budget_exceeded"
	IdleStopActor  = "rhizome:idle-timeout"
)

// idleStop records the idle-timeout stop request once. The judgment uses
// only JANUS's own last_activity_ts and the injected clock — no process
// state; a durable StopRequested (any reason) makes it a no-op, so a restart
// never re-requests (execution.RequestStop is idempotent anyway).
func (l *Loop) idleStop(id string, r execution.Ref, st TailSession, wrote *bool) {
	if l.IdleTimeout <= 0 || st.State != "running" || st.LastActivityTS <= 0 || r.StopRequested {
		return
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	if now().UnixMilli()-st.LastActivityTS < l.IdleTimeout.Milliseconds() {
		return
	}
	before := len(l.ES.Store.All())
	if _, err := l.ES.RequestStop(id, IdleStopReason, 0, IdleStopActor); err != nil {
		l.report("execution", id, err)
		return
	}
	if len(l.ES.Store.All()) > before {
		*wrote = true
	}
}

// checkTailPage applies the replay contiguity guard to one tail page: every
// envelope belongs to the bound trace, seq runs exactly from+1, from+2, …
// with no gap, and next_from_seq is the last seq read (from when empty).
func checkTailPage(res TailResult, wantTrace string, from int64) error {
	through := from
	for _, ev := range res.Events {
		if ev.TraceID != wantTrace {
			return ErrSessionReplaced
		}
		if ev.Seq <= 0 || ev.Seq-1 != through {
			return fmt.Errorf("%w: tail sequence", ErrObservationCorrupt)
		}
		through = ev.Seq
	}
	if res.NextFromSeq != through {
		return fmt.Errorf("%w: tail next_from_seq", ErrObservationCorrupt)
	}
	return nil
}

// surfaceRequests raises observed, still-undecided approval requests to the
// human surface (behavior ④, D18). For each request with neither a durable
// input nor a pending record, the socket pending query supplies digest and
// display metadata — the only contract source of both — and the durable chain
// Wake -> wait_human Decision -> approvalrequest is written (D21: the last
// record marks completion; earlier leftovers from a crash are converged
// over). Non-pending query answers are skipped: decided/expired facts belong
// to replay observation, unknown retries next tick. No human decision is
// ever fabricated here (수동 고정).
//
// It returns the number of requests left OPEN and unsurfaced after this pass
// (FR-RHZ-119 D22 gate): no approval/approvalrequest aggregate for the key,
// no policy/decision for it in the log, session not ended. A non-zero return
// holds the observation cursor. Requests the log already resolved and
// requests of an ended session are not open: JANUS denies at exit, so they
// are not surfacing targets and must not block liveness.
func (l *Loop) surfaceRequests(execID string, r execution.Ref, b Batch, wrote *bool) (open int) {
	store := l.ES.Store
	resolved := map[approval.RequestKey]bool{}
	for _, a := range b.ApprovalResponses {
		resolved[a.Key] = true
	}
	for _, req := range b.ApprovalRequests {
		id := approval.IDFor(req.Key)
		if len(store.List("approval", id)) > 0 || len(store.List("approvalrequest", id)) > 0 {
			continue // Input already recorded, or already surfaced: no query.
		}
		if resolved[req.Key] || b.SessionEnded {
			continue // Resolved in the log or session over: not a surfacing target, no query.
		}
		open++
		res, err := l.Client.Query(req.Key)
		if err != nil {
			l.report("approval", id, err)
			continue
		}
		if res.Status != "pending" {
			continue
		}
		if strings.TrimSpace(res.RequestDigest) == "" {
			l.report("approval", id, fmt.Errorf("pending answer without request_digest"))
			continue
		}
		tick := fmt.Sprintf("%s:%s:%d", execID, r.ExternalID, req.Seq)
		wakeID := "wake-" + id
		if len(store.List("wake", wakeID)) == 0 {
			if _, err = (wake.Service{Store: store}).Create(wake.Wake{ID: wakeID, Source: wake.Event, RequestedAt: time.Now().UTC(), TargetType: "mission", TargetID: r.MissionID, CorrelationID: id, TickKey: tick}); err != nil {
				l.report("approval", id, err)
				continue
			}
			*wrote = true
		}
		decID := "dec-" + id
		if len(store.List("decision", decID)) == 0 {
			d := decision.Decision{ID: decID, MissionID: r.MissionID, Reason: "approval_request " + req.Key.RequestID, Kind: decision.WaitHuman,
				Evidence: []decision.Evidence{{SourceType: "execution", SourceID: execID}, {SourceType: "approvalrequest", SourceID: id}}}
			if _, err = (decision.Service{Store: store}).CreateWithTickCorrelation(d, tick, id); err != nil {
				l.report("approval", id, err)
				continue
			}
			*wrote = true
		}
		if _, err = (gaterequest.Service{Store: store}).Record(gaterequest.Ref{Key: req.Key, Name: req.Name, Reason: req.Reason,
			RequestDigest: res.RequestDigest, PolicyHash: res.PolicyHash, DisplaySummary: res.DisplaySummary, ExpiresAt: res.ExpiresAt,
			MissionID: r.MissionID, ExecutionID: execID, DecisionID: decID}); err != nil {
			l.report("approval", id, err)
			continue
		}
		*wrote = true
		open-- // Surfaced durably in this pass: no longer holds the cursor.
	}
	return open
}

// submitStop relays a durable stop_requested intent over the shared socket.
// The receipt is classification only: it is remembered in process memory,
// never written to the journal — cancelled is concluded solely from replay.
func (l *Loop) submitStop(id string) {
	if l.stopAcked[id] {
		return
	}
	r, err := execution.Replay(l.ES.Store.List("execution", id))
	if err != nil {
		return // already reported by the caller's replay this tick
	}
	if !r.StopRequested || (r.State != execution.Accepted && r.State != execution.Observing) {
		return
	}
	res, err := SubmitStop(l.ES, l.Client, id)
	if err != nil {
		l.report("execution", id, err)
		return
	}
	if res.Status == "stop_accepted" || res.Status == "already_terminal" {
		l.stopAcked[id] = true
	}
}

// relayApproval passes one human decision through: the durable
// approval.response_dispatched record always precedes socket contact, a
// Dispatched approval is resubmitted without a second dispatch record
// (contract §4 resubmission is a lookup; RHZ-046 D5 guard), and the socket
// response is never recorded — durable observation comes only from stage-B
// replay. The loop never fabricates approval inputs.
func (l *Loop) relayApproval(id string, wrote *bool) {
	a, err := approval.Replay(l.AS.Store.List("approval", id))
	if err != nil {
		l.report("approval", id, err)
		return
	}
	switch a.State {
	case approval.InputRecorded:
		if a, err = l.AS.Dispatch(id, a.ResponseID); err != nil {
			l.report("approval", id, err)
			return
		}
		*wrote = true
	case approval.Dispatched:
		// D5: no duplicate dispatch append; submit-only retry.
	default:
		return // Observed is terminal.
	}
	if _, err = l.Client.Submit(a.Key, a.ResponseID, a.HumanDecision, a.Reason); err != nil {
		l.report("approval", id, err)
	}
}

// Run ticks until ctx is done. Ticks never overlap and a failing tick never
// terminates the loop.
func (l *Loop) Run(ctx context.Context, interval time.Duration) error {
	if interval < MinObserveInterval {
		return fmt.Errorf("observe interval below %s", MinObserveInterval)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			l.Tick()
		}
	}
}
