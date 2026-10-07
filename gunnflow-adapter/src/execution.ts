/**
 * RHZ-094 (FR-RHZ-121) / RHZ-095 (FR-RHZ-122): Rhizome /v1/execution/{missionId} → the contract's
 * ExecutionSnapshot (Gunnflow contract 0.3.0, WIRE.md §execution). Pure
 * translation of one Rhizome execution body (internal/workspace/
 * executionhttp.go DTOs) into the wire shape — field renames and omissions
 * only. Nothing is synthesized: a value absent upstream is absent here, a
 * value present upstream is carried verbatim (kind/label/state vocabulary is
 * Rhizome's own; the cockpit displays it and never interprets it).
 *
 * Session state table (Rhizome executionhttp.go executionSessionState):
 *
 *   Rhizome ExecutionRef state      Rhizome wire `state`   contract `state`
 *   Accepted, Observing             running                running (verbatim)
 *   Cancelled                       killed                 killed  (verbatim)
 *   Succeeded, Failed               ended                  ended   (verbatim)
 *   intent, dispatch_claimed, …     (not emitted)          (no session)
 *   —                               —                      paused  (never produced: Rhizome has no paused)
 *
 * Rhizome's `state` is already the contract enum, so it is passed as-is: an
 * Accepted (pre-spawn, JANUS "pending") session is `running` because Rhizome
 * says so, not because this module decided. Rhizome's richer per-session word
 * `janusState` (pending|running|exited — a JANUS-log derivation) travels under
 * the contract's neutral name `upstreamSessionState`, verbatim.
 *
 * Event mapping: ts → at (JANUS Unix ms, untouched), kind as-is, seq → the
 * task-global ordinal below,
 * label = '' always: the contract's `label` is the upstream's SUMMARY of the
 * event and Rhizome emits none — its `actor` (who emitted) is not a summary
 * and is not re-purposed as one (RHZ-094). `label` is a
 * required key, so the honest "no summary" is the empty string. Rhizome
 * events carry no status (none is sent); their usageIn/usageOut/actor are not
 * contract event keys (closed key set) and are dropped — the per-session
 * totals Rhizome derives from usage are what the wire carries.
 *
 * Event `seq` — RHZ-095 (FR-RHZ-122), task-global ordinal: the contract's
 * validator wants `seq` strictly increasing over the WHOLE task, while
 * Rhizome's `seq` is per session (JANUS replay, 1.. per session log; contract
 * ② semantics, kept in the core — a core-side task-global ordinal was rejected
 * because contract ② defines seq as the session log's own position and no
 * Gunnflow relaxation was requested). So the wire's `seq` is the ADAPTER's
 * ordinal, computed by `orderEvents`:
 *   1. sessions in Rhizome's sessions[] order (executionhttp.go emits them
 *      sorted by session id, each session's events appended in that order);
 *      events naming a session the body does not carry rank after all carried
 *      sessions, by first appearance (still carried: the validator refuses them);
 *   2. within a session by Rhizome's per-session seq ascending;
 *   3. ordinal = (sum of the max per-session seq of every EARLIER session)
 *      + the event's own per-session seq.
 * Properties: same input → same output; a single-session task is byte-identical
 * to the RHZ-094 output (offset 0, ordinal == per-session seq); appending events
 * to the LAST session never renumbers earlier events. Caveat, stated honestly: a
 * new event in an EARLIER session (a still-running session while a later one
 * exists — possible, sessions of one task may overlap) or a newly bound session
 * whose id sorts before an existing one DOES renumber everything after it; the
 * ordinal is stable for a fixed snapshot, not across such snapshots. JANUS's
 * per-session seq is not carried (closed contract key set).
 *
 * Bounds (EXECUTION_MAX_*): events are windowed to the newest
 * EXECUTION_MAX_EVENTS and the cut is declared with `truncatedBefore` (= the
 * first carried ORDINAL, exactly the contract's "seq < truncatedBefore exists
 * upstream but is not carried"); session label / kind / upstreamSessionState
 * strings are cut to their caps. Sessions are NOT windowed (a cut session list has no
 * honest marker in the contract) — an over-cap list is left for the wire's
 * validate guard to refuse with its reason.
 */
import {
  EXECUTION_MAX_EVENTS,
  EXECUTION_MAX_KIND_CHARS,
  EXECUTION_MAX_LABEL_CHARS,
  EXECUTION_MAX_STATUS_CHARS,
  validateExecutionSnapshot,
  type ExecutionEvent,
  type ExecutionSession,
  type ExecutionSnapshot,
} from '@gunnflow/contract';

/** One session as Rhizome's executionSessionDTO emits it (omitempty fields may be absent). */
export interface RhizomeExecSession {
  id: string;
  taskId: string;
  state: string;
  label?: string;
  janusState?: string;
  usageInTotal?: number;
  usageOutTotal?: number;
  lastActivityTs?: number;
}

/** One event as Rhizome's executionEventDTO emits it. */
export interface RhizomeExecEvent {
  sessionId: string;
  seq: number;
  kind: string;
  actor?: string;
  ts: number;
  usageIn?: number;
  usageOut?: number;
}

/** The `body` of Rhizome's execution envelope (GET and every SSE frame). */
export interface RhizomeExecBody {
  taskId: string;
  sessions: RhizomeExecSession[] | null;
  events: RhizomeExecEvent[] | null;
}

const cut = (s: string, max: number) => (s.length > max ? s.slice(0, max) : s);

function projectSession(s: RhizomeExecSession): ExecutionSession {
  const out: ExecutionSession = { id: s.id, taskId: s.taskId, state: s.state as ExecutionSession['state'] };
  if (typeof s.label === 'string' && s.label !== '') out.label = cut(s.label, EXECUTION_MAX_LABEL_CHARS);
  if (typeof s.janusState === 'string' && s.janusState !== '') {
    out.upstreamSessionState = cut(s.janusState, EXECUTION_MAX_STATUS_CHARS);
  }
  if (s.usageInTotal !== undefined) out.usageInTotal = s.usageInTotal;
  if (s.usageOutTotal !== undefined) out.usageOutTotal = s.usageOutTotal;
  if (s.lastActivityTs !== undefined) out.lastActivityTs = s.lastActivityTs;
  return out;
}

function projectEvent(e: RhizomeExecEvent, ordinal: number): ExecutionEvent {
  return {
    seq: ordinal, // RHZ-095 (FR-RHZ-122): the adapter's task-global ordinal, not JANUS's per-session seq
    at: e.ts,
    sessionId: e.sessionId,
    kind: cut(e.kind, EXECUTION_MAX_KIND_CHARS),
    label: '', // Rhizome has no event summary; actor is not one
  };
}

/**
 * RHZ-095 (FR-RHZ-122): the deterministic total order of a body's events and
 * each event's task-global ordinal (rule in the file header). Pure; the
 * input arrays are not mutated.
 */
export function orderEvents(body: RhizomeExecBody): Array<{ event: RhizomeExecEvent; ordinal: number }> {
  const rank = new Map<string, number>();
  for (const s of body.sessions ?? []) if (!rank.has(s.id)) rank.set(s.id, rank.size);
  const events = body.events ?? [];
  for (const e of events) if (!rank.has(e.sessionId)) rank.set(e.sessionId, rank.size); // dangling: after all carried sessions
  const sorted = events
    .map((event, index) => ({ event, index }))
    .sort((a, b) => rank.get(a.event.sessionId)! - rank.get(b.event.sessionId)! || a.event.seq - b.event.seq || a.index - b.index);
  const out: Array<{ event: RhizomeExecEvent; ordinal: number }> = [];
  let offset = 0; // sum of the max per-session seq of every earlier session
  let session: string | undefined;
  let maxSeq = 0;
  for (const { event } of sorted) {
    if (event.sessionId !== session) {
      offset += maxSeq;
      session = event.sessionId;
      maxSeq = 0;
    }
    if (event.seq > maxSeq) maxSeq = event.seq;
    out.push({ event, ordinal: offset + event.seq });
  }
  return out;
}

/** Rhizome execution body → contract ExecutionSnapshot (structure unchecked: the wire validates before serving). */
export function projectExecution(body: RhizomeExecBody): ExecutionSnapshot {
  const sessions = (body.sessions ?? []).map(projectSession);
  const all = orderEvents(body).map(({ event, ordinal }) => projectEvent(event, ordinal));
  if (all.length <= EXECUTION_MAX_EVENTS) return { sessions, events: all };
  // Window: keep the newest events (ordinal order), declare the cut by ordinal.
  const events = all.slice(all.length - EXECUTION_MAX_EVENTS);
  const first = events[0]!.seq;
  // truncatedBefore must be a positive seq; JANUS seqs start at 1, so a
  // non-positive first ordinal cannot be declared under the contract's rule.
  return first > 0 ? { sessions, events, truncatedBefore: first } : { sessions, events };
}

/**
 * The wire's own invariant on top of validateExecutionSnapshot: a snapshot
 * served under /execution/:taskId carries only that task's sessions (the BFF
 * rejects a stray one as a contract failure — mirror it here so the fault is
 * reasoned at the source).
 */
export function executionTaskProblem(snapshot: ExecutionSnapshot, taskId: string): string | null {
  const stray = snapshot.sessions.find((s) => s.taskId !== taskId);
  return stray ? `session '${stray.id}' carries taskId '${stray.taskId}', not the requested '${taskId}'` : null;
}

/**
 * A Rhizome execution body → the validated contract snapshot the wire may
 * serve under /execution/:taskId, or the reason it may not. Every 200 body
 * and every SSE frame passes here (the BFF would 502 a non-conforming one
 * anonymously; the same guard as /detail): the contract's structure check
 * (closed keys, state enum, strictly increasing seqs, every event's sessionId
 * naming a carried session, caps) plus the wire's own task invariant.
 */
export function executionForWire(
  body: RhizomeExecBody,
  taskId: string,
): { ok: true; snapshot: ExecutionSnapshot } | { ok: false; reason: string } {
  const check = validateExecutionSnapshot(projectExecution(body));
  if (!check.ok) return { ok: false, reason: `execution fails the contract: ${check.problems.join('; ')}` };
  const stray = executionTaskProblem(check.snapshot, taskId);
  if (stray) return { ok: false, reason: `execution fails the contract: ${stray}` };
  return { ok: true, snapshot: check.snapshot };
}
