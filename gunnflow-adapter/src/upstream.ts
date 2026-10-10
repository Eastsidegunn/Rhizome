/**
 * Rhizome upstream adapter — the live Rhizome workspace surface. The upstream
 * workspace HTTP DTOs define the wire shape.
 * Pure transport: GET /v1/workspace + SSE /v1/workspace/stream for
 * projections (v1 DTOs adapted to the cockpit's projection shape in
 * workspaceWire.ts), POST /v1/intent for human intents (PascalCase response
 * adapted to {accepted, reason}). No judgment, no synthesis.
 *
 * Execution surface v1 (contract ①): GET+SSE per task at
 * /v1/execution/{taskId}. Rhizome streams whole-body envelopes; this adapter
 * diffs them into the session/events deltas the BFF route already speaks —
 * dedup only, no new facts. Unknown taskIds are 404 and stay empty here.
 * RHZ-094 (FR-RHZ-121): the same feed also hands the direct wire whole
 * bodies (executionWire / subscribeExecutionBody) for /execution/:taskId.
 * RHZ-095 (FR-RHZ-122): a feed with no subscriber for EXEC_FEED_IDLE_MS is
 * dropped (its Rhizome SSE closed); the next request recreates it lazily.
 * The terminal surface is unsupported and produces an empty projection.
 */
import type {
  UpstreamIntentResult,
  UpstreamProjectionEnvelope,
  WorkspaceUpstream,
} from '@gunnflow/upstream-port';
import { lookupCapability, validateIntent, type Intent, type NodeProjection } from '@gunnflow/contract';
import { adaptWorkspaceBody } from './workspaceWire.js';
import { projectRhizomeNodes, type RhizomeIntegrationReport } from './nodes.js';
import type { RhizomeExecBody, RhizomeExecSession } from './execution.js';
import { prepareSigner, SIGNER_REFUSAL, type SigningOptions } from './signing.js';

const RECONNECT_MS = 2000;
/** RHZ-095 (FR-RHZ-122): the timer pair the execFeed idle TTL runs on (the real clock unless a test injects one). */
export interface IdleTimers {
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}
const REAL_IDLE_TIMERS: IdleTimers = {
  setTimeout(fn, ms) {
    const t = setTimeout(fn, ms);
    t.unref?.(); // an idle feed never holds the process open
    return t;
  },
  clearTimeout(handle) {
    clearTimeout(handle as ReturnType<typeof setTimeout>);
  },
};
/** RHZ-095 (FR-RHZ-122): default idle TTL of a per-task execution feed with no subscriber. */
export const EXEC_FEED_IDLE_MS = 5 * 60 * 1000;
const TERMINAL_UNSUPPORTED = 'Rhizome provides no terminal surface yet (its terminal endpoint answers 501)';
const STREAM_UNSUPPORTED = 'Rhizome provides no contiguous-seq stream channel yet';

/** Fields the cockpit's intents carry; anything else is dropped before sending. */
export const INTENT_FIELDS: ReadonlySet<string> = new Set([
  'name',
  'prompt',
  'from',
  'to',
  'edgeKind',
  'taskId',
  'gateId',
  'requestId',
  'sessionId',
  'instruction',
  'reason',
  'memo',
  'input',
  'inputRef',
  'decision',
  'edit',
  'attestation',
]);

/**
 * Rhizome addresses intents by kind + a typed id field; the contract Intent
 * addresses by nodeId + action. Terminal intents still arrive in their
 * stand-in shape and pass through.
 */
export function toRhizomeAddress(i: Record<string, unknown>): { kind: string | undefined; rest: Record<string, unknown> } {
  if (typeof i.action !== 'string' || typeof i.nodeId !== 'string') {
    const { intent: kind, ...rest } = i as { intent?: string } & Record<string, unknown>;
    return { kind, rest };
  }
  const { action, nodeId } = i as { action: string; nodeId: string };
  // Only the contract's slots are carried over by name; the idempotency key is not sent.
  const slots: Record<string, unknown> = {};
  for (const slot of ['decision', 'edit', 'attestation'] as const) if (i[slot] !== undefined) slots[slot] = i[slot];
  if (action === 'edge.rewire') {
    const { decision, ...others } = slots as { decision?: { option?: string } };
    return { kind: action, rest: { ...others, from: nodeId, to: decision?.option, edgeKind: 'dependency' } };
  }
  const address =
    action.startsWith('gate.') ? { gateId: nodeId }
    : action.startsWith('request.') ? { requestId: nodeId }
    : action.startsWith('task.') || action === 'artifact.edit' ? { taskId: nodeId }
    : action.startsWith('session.') ? { sessionId: nodeId }
    : {};
  return { kind: action, rest: { ...slots, ...address } };
}

/**
 * Gate decisions must be bound to the request they answer: Rhizome refuses
 * gate.approve / gate.reject / gate.requestChanges without the gate's
 * requestDigest. The adapter supplies it from its own latest snapshot and
 * never trusts a digest the cockpit sends (FR-RHZ-163).
 */
export const DIGEST_BOUND_KINDS: ReadonlySet<string> = new Set(['gate.approve', 'gate.reject', 'gate.requestChanges']);
export const DIGEST_UNAVAILABLE = 'gate digest unavailable in adapter snapshot';

export function snapshotDigest(wire: UpstreamProjectionEnvelope, gateId: unknown): string | undefined {
  if (typeof gateId !== 'string') return undefined;
  const gates = (wire.body as { gates?: Array<{ id?: unknown; requestDigest?: unknown }> }).gates ?? [];
  const gate = gates.find((g) => g.id === gateId);
  return typeof gate?.requestDigest === 'string' && gate.requestDigest !== '' ? gate.requestDigest : undefined;
}

/** The gate consumer is a property of Rhizome's raw snapshot, not the cockpit projection. */
export function snapshotGateConsumer(wire: UpstreamProjectionEnvelope, gateId: unknown): 'question' | 'approval' | undefined {
  if (typeof gateId !== 'string') return undefined;
  const gates = (wire.body as { gates?: Array<{ id?: unknown; source?: unknown }> }).gates ?? [];
  const gate = gates.find((g) => g.id === gateId);
  return gate === undefined ? undefined : gate.source === 'internal' ? 'question' : 'approval';
}

/** Rhizome falls back to instruction only for requestChanges; stored text itself is not trimmed. */
export function effectiveReason(kind: string | undefined, fields: Record<string, unknown>): string {
  const reason = typeof fields.reason === 'string' ? fields.reason : '';
  if (kind !== 'gate.requestChanges' || reason.trim() !== '') return reason;
  return typeof fields.instruction === 'string' ? fields.instruction : '';
}

/** Rhizome names the human's text by intent kind: `name` for missions, `instruction` for directions. */
const TEXT_FIELD: Record<string, string> = {
  'mission.create': 'name',
  'task.instruct': 'instruction',
  'gate.reject': 'reason',
  'gate.requestChanges': 'instruction',
  'request.complete': 'memo',
  'request.unable': 'reason',
};

export function toRhizomeFields(kind: string | undefined, fields: Record<string, unknown>): Record<string, unknown> {
  const target = kind ? TEXT_FIELD[kind] : undefined;
  const decision = fields.decision as { text?: unknown } | undefined;
  if (kind?.startsWith('request.')) {
    const { decision: _decision, ...rest } = fields;
    return target && typeof decision?.text === 'string' ? { ...rest, [target]: decision.text } : rest;
  }
  if (!target || typeof decision?.text !== 'string') return fields;
  const { decision: _d, ...rest } = fields;
  // Rhizome requires a success criterion for missions; the mission's own text stands in when none is given.
  const prompt = kind === 'mission.create' && rest.prompt === undefined ? { prompt: decision.text } : {};
  return { ...rest, [target]: decision.text, ...prompt };
}

/* ---- RHZ-132 (FR-RHZ-171): answering a blocked note ---- */

export const NOTE_ANSWER_ACTION = 'note.answer';
export const NOTE_ANSWER_TEXT_REQUIRED = 'answer text required';
export const NOTE_NOT_BLOCKED = 'note is not blocked in adapter snapshot';

/**
 * The Rhizome intent a cockpit note.answer becomes: note.create of memoryKind
 * `answer` on the blocked note's own mission (taken from the adapter's raw
 * snapshot, never from the cockpit), tagged `answer` and `re:<note id>`. The
 * content is `re: <note id>`, a blank line, then the human's text verbatim:
 * Rhizome derives a note id from its content alone, so the same short answer
 * ("확인") to two notes must not collide. Blank text and notes the snapshot
 * does not list as note_blocked are refused before anything reaches Rhizome.
 */
export function noteAnswerIntent(
  wire: UpstreamProjectionEnvelope,
  noteId: unknown,
  decision: unknown,
): { ok: true; fields: Record<string, unknown> } | { ok: false; reason: string } {
  const textValue = (decision as { text?: unknown } | null | undefined)?.text;
  if (typeof textValue !== 'string' || textValue.trim() === '') return { ok: false, reason: NOTE_ANSWER_TEXT_REQUIRED };
  const listed = (wire.body as { attention?: unknown } | null)?.attention;
  const entry = (Array.isArray(listed) ? listed : []).find(
    (a) => typeof a === 'object' && a !== null && (a as { kind?: unknown }).kind === 'note_blocked' && (a as { refId?: unknown }).refId === noteId,
  ) as { missionId?: unknown } | undefined;
  if (typeof noteId !== 'string' || !entry || typeof entry.missionId !== 'string' || entry.missionId === '') {
    return { ok: false, reason: NOTE_NOT_BLOCKED };
  }
  return {
    ok: true,
    fields: { kind: 'note.create', memoryKind: 'answer', missionId: entry.missionId, tags: ['answer', `re:${noteId}`], content: `re: ${noteId}\n\n${textValue}` },
  };
}

/** The upstream's own refusal text from an error response, verbatim when present. */
async function upstreamReason(res: Response): Promise<string | undefined> {
  const text = (await res.text().catch(() => '')).trim();
  if (!text) return undefined;
  try {
    const body = JSON.parse(text) as { Reason?: unknown; reason?: unknown };
    const reason = body.Reason ?? body.reason;
    if (typeof reason === 'string' && reason) return reason;
  } catch {
    // not JSON: the text itself is the upstream's message
  }
  return text;
}

export async function createRhizomeUpstream(
  baseUrl: string,
  options: {
    /** RHZ-095 (FR-RHZ-122): idle TTL of a subscriber-less execution feed (ms); default EXEC_FEED_IDLE_MS. Injectable for tests. */
    execFeedIdleMs?: number;
    /** RHZ-095 (FR-RHZ-122): the clock the idle TTL runs on; default the real timers (unref'd). Injectable for tests. */
    timers?: IdleTimers;
    /** RHZ-117: required/off signing mode and its injectable OS seams. Omitted means off. */
    signing?: SigningOptions;
  } = {},
): Promise<
  WorkspaceUpstream & {
    close(): void;
    integrationReport(): RhizomeIntegrationReport;
    /** The latest Rhizome /v1/workspace envelope as received (same generation as snapshot()). */
    rawSnapshot(): UpstreamProjectionEnvelope;
    /**
     * RHZ-094 (FR-RHZ-121): the Rhizome execution body behind the direct
     * wire's GET /execution/:taskId; undefined = Rhizome 404 (no execution).
     */
    executionWire(taskId: string): Promise<RhizomeExecBody | undefined>;
    /** Full Rhizome execution body on every change (the wire's SSE republish source). */
    subscribeExecutionBody(taskId: string, listener: (body: RhizomeExecBody) => void): () => void;
  }
> {
  // In required mode this completes the executable/parent/key checks before
  // any upstream connection (and, for serveDirect, before listen()).
  const signer = await prepareSigner(options.signing);
  const listeners = new Set<(p: UpstreamProjectionEnvelope) => void>();
  const mediaTypes = new Map<string, string>(); // blob id → served Content-Type
  const inflight = new Set<string>();
  const mediaTypeOf = (blobId: string) => mediaTypes.get(blobId);
  let closed = false;
  let abort = new AbortController();
  const execFeedIdleMs = options.execFeedIdleMs ?? EXEC_FEED_IDLE_MS;
  const idleTimers = options.timers ?? REAL_IDLE_TIMERS;

  let lastWire = await fetchRawSnapshot(baseUrl);
  const init = adaptWithReport(lastWire, mediaTypeOf);
  let current: UpstreamProjectionEnvelope = init.envelope;
  let report: RhizomeIntegrationReport = init.report;

  // The content-addressed blob refs a snapshot's deliverables point at.
  const blobRefsOf = (wire: UpstreamProjectionEnvelope): string[] => {
    const dels = ((wire.body as { deliverables?: unknown } | null)?.deliverables ?? []) as Array<{ sourceRef?: unknown }>;
    return dels.flatMap((d) => (typeof d?.sourceRef === 'string' && d.sourceRef.startsWith('sha256:') ? [d.sourceRef] : []));
  };

  const applyEnvelope = (wire: UpstreamProjectionEnvelope) => {
    lastWire = wire;
    const adapted = adaptWithReport(wire, mediaTypeOf);
    current = adapted.envelope;
    report = adapted.report;
    for (const l of listeners) l(current);
    ensureMediaTypes(blobRefsOf(wire));
  };

  // Learn each blob's media type from its served Content-Type so the artifact
  // meta can carry it. Rhizome has no metadata-only endpoint (/v1/blob is
  // GET-only), so this reads the blob once and caches it; the bytes are drained
  // and discarded. On learning a new type, re-project so the open snapshot
  // gains it. A failed probe stays uncached and is retried on the next snapshot.
  const ensureMediaTypes = (refs: string[]) => {
    const missing = refs.filter((r) => !mediaTypes.has(r) && !inflight.has(r));
    if (missing.length === 0) return;
    for (const r of missing) inflight.add(r);
    void (async () => {
      let learned = false;
      await Promise.all(
        missing.map(async (ref) => {
          try {
            const res = await fetch(`${baseUrl}/v1/blob/${ref}`);
            if (res.ok) {
              mediaTypes.set(ref, res.headers.get('content-type') ?? 'application/octet-stream');
              learned = true;
            }
            await res.arrayBuffer().catch(() => undefined); // drain to free the socket
          } catch {
            // leave uncached; the next snapshot retries
          } finally {
            inflight.delete(ref);
          }
        }),
      );
      if (learned && !closed) applyEnvelope(lastWire);
    })();
  };

  // Kick media-type resolution for the first snapshot's artifacts.
  ensureMediaTypes(blobRefsOf(lastWire));

  // Rhizome's SSE may drop events for slow consumers (v1). After any
  // disconnect we re-enter via a fresh stream, whose first frame is a full
  // snapshot — that is the resync.
  void (async () => {
    while (!closed) {
      try {
        abort = new AbortController();
        await consumeStream(
          `${baseUrl}/v1/workspace/stream`,
          applyEnvelope,
          () => closed,
          abort.signal,
        );
      } catch {
        // fall through to reconnect delay
      }
      if (!closed) await sleep(RECONNECT_MS);
    }
  })();

  /* ---- execution feeds: one per task, lazily started ---- */
  type WireSession = RhizomeExecSession;
  type ExecBody = RhizomeExecBody;
  type ExecDelta =
    | { type: 'events'; events: unknown[] }
    | { type: 'session'; session: WireSession };
  interface ExecFeed {
    current: ExecBody;
    /** True once Rhizome's SSE delivered a frame: `current` is then the live body, no fetch needed. */
    live: boolean;
    listeners: Set<(delta: ExecDelta) => void>;
    abort: AbortController;
    /** Seed the cache from a GET envelope (not a stream frame: does not mark the feed live). */
    applySnapshot?: (envelope: UpstreamProjectionEnvelope) => void;
    /** RHZ-095 (FR-RHZ-122): armed while the feed has no subscriber; fires = drop the feed. */
    idleTimer?: unknown;
  }
  const execFeeds = new Map<string, ExecFeed>();
  // RHZ-094 (FR-RHZ-121): whole-body listeners for the direct wire's
  // /execution/:taskId/stream (full republish grammar). Keyed by task, not
  // by feed, so a feed dropped on 404 and restarted later keeps its tail.
  const execBodyListeners = new Map<string, Set<(body: ExecBody) => void>>();
  const emptyExec = (taskId: string): ExecBody => ({ taskId, sessions: [], events: [] });

  /*
   * RHZ-095 (FR-RHZ-122): execFeed idle TTL. A subscriber is a stream client:
   * a delta listener (port subscribeExecution) or a whole-body listener (the
   * direct wire's /execution/:taskId/stream). A plain GET (executionWire) and
   * the in-process executionSnapshot read do NOT subscribe, so a feed they
   * started expires too. With 0 subscribers continuously for execFeedIdleMs
   * the feed's Rhizome SSE is aborted and the feed dropped; a later request
   * recreates it (lazy start, as before). A subscriber attaching within the
   * TTL cancels the timer; an active stream is never expired.
   */
  const subscriberCount = (taskId: string, feed: ExecFeed) => feed.listeners.size + (execBodyListeners.get(taskId)?.size ?? 0);
  const dropFeed = (taskId: string, feed: ExecFeed) => {
    if (feed.idleTimer !== undefined) idleTimers.clearTimeout(feed.idleTimer);
    feed.idleTimer = undefined;
    feed.abort.abort();
    if (execFeeds.get(taskId) === feed) execFeeds.delete(taskId);
  };
  const reviewIdle = (taskId: string) => {
    const feed = execFeeds.get(taskId);
    if (!feed || closed) return;
    const idle = subscriberCount(taskId, feed) === 0;
    if (idle && feed.idleTimer === undefined) {
      feed.idleTimer = idleTimers.setTimeout(() => {
        feed.idleTimer = undefined;
        if (execFeeds.get(taskId) === feed && subscriberCount(taskId, feed) === 0) dropFeed(taskId, feed);
      }, execFeedIdleMs);
    } else if (!idle && feed.idleTimer !== undefined) {
      idleTimers.clearTimeout(feed.idleTimer);
      feed.idleTimer = undefined;
    }
  };

  function execFeed(taskId: string): ExecFeed {
    let feed = execFeeds.get(taskId);
    if (feed) return feed;
    feed = {
      current: emptyExec(taskId),
      live: false,
      listeners: new Set(),
      abort: new AbortController(),
    };
    execFeeds.set(taskId, feed);
    reviewIdle(taskId); // a fresh feed has no subscriber until one attaches: TTL armed
    const applyExec = (envelope: UpstreamProjectionEnvelope, viaStream: boolean) => {
      const next = (envelope.body ?? emptyExec(taskId)) as ExecBody;
      const f = execFeeds.get(taskId);
      if (!f) return;
      // Diff whole-body projections into the deltas the BFF route forwards.
      const prevSessions = new Map((f.current.sessions ?? []).map((x) => [x.id, x]));
      const changed: WireSession[] = [];
      for (const session of next.sessions ?? []) {
        const prev = prevSessions.get(session.id);
        if (!prev || JSON.stringify(prev) !== JSON.stringify(session)) changed.push(session);
      }
      const prevMaxSeq = Math.max(0, ...(f.current.events ?? []).map((e) => e.seq ?? 0));
      const newEvents = (next.events ?? []).filter((e) => (e.seq ?? 0) > prevMaxSeq);
      const body: ExecBody = { taskId, sessions: next.sessions ?? [], events: next.events ?? [] };
      const bodyChanged = JSON.stringify(f.current) !== JSON.stringify(body);
      f.current = body;
      if (viaStream) f.live = true;
      for (const l of f.listeners) {
        if (newEvents.length > 0) l({ type: 'events', events: newEvents });
        for (const session of changed) l({ type: 'session', session });
      }
      // Full-body subscribers hear every change (the wire re-emits on change).
      if (bodyChanged) for (const l of execBodyListeners.get(taskId) ?? []) l(body);
    };
    feed.applySnapshot = (envelope) => applyExec(envelope, false);
    void (async () => {
      // The tail belongs to THIS feed object: once it is dropped (404, idle
      // TTL) or replaced by a recreated feed, this loop ends and leaves the
      // new feed's own tail alone.
      while (!closed && execFeeds.get(taskId) === feed) {
        try {
          await consumeStream(
            `${baseUrl}/v1/execution/${encodeURIComponent(taskId)}/stream`,
            (e) => applyExec(e, true),
            () => closed || execFeeds.get(taskId) !== feed,
            feed!.abort.signal,
          );
          const f = execFeeds.get(taskId);
          if (f) f.live = false; // stream ended: the next frame of a reconnect is the resync
        } catch (err) {
          // Unknown taskId is a contract 404 — stays empty, no retry storm.
          // The feed is dropped so a task that gains an execution later is
          // re-asked on its next request rather than latched as unknown.
          if (String(err).includes('(404)')) {
            if (execFeeds.get(taskId) === feed) dropFeed(taskId, feed!);
            return;
          }
          if (execFeeds.get(taskId) !== feed) return; // dropped (idle TTL / close): no reconnect
          const f = execFeeds.get(taskId);
          if (f) f.live = false;
        }
        if (!closed) await sleep(RECONNECT_MS);
      }
    })();
    return feed;
  }

  /**
   * RHZ-094 (FR-RHZ-121): the Rhizome execution body for one task, as the
   * direct wire needs it — `undefined` when Rhizome answers 404 (no mission
   * log = no execution for that task). A live feed answers from its cache;
   * otherwise one GET /v1/execution/{taskId} decides 404 vs 200 and seeds the
   * feed, whose SSE tail then carries the re-emits. Rhizome serves the
   * execution surface for every known mission (200 with empty sessions/events
   * when nothing is recorded), so there is no 501 to map: the wire never
   * answers 501 for execution. Transport failures reject.
   */
  async function executionWire(taskId: string): Promise<ExecBody | undefined> {
    const feed = execFeed(taskId);
    if (feed.live) return feed.current;
    const res = await fetch(`${baseUrl}/v1/execution/${encodeURIComponent(taskId)}`);
    if (res.status === 404) {
      await res.arrayBuffer().catch(() => undefined);
      return undefined;
    }
    if (!res.ok) throw new Error(`rhizome execution snapshot failed (${res.status})`);
    const envelope = (await res.json()) as UpstreamProjectionEnvelope;
    const f = execFeeds.get(taskId) ?? execFeed(taskId);
    if (!f.live) f.applySnapshot?.(envelope);
    return f.current;
  }

  async function postIntent(payload: Record<string, unknown>): Promise<UpstreamIntentResult> {
    const res = await fetch(`${baseUrl}/v1/intent`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      return { accepted: false, reason: (await upstreamReason(res)) ?? `rhizome relay error (${res.status})` };
    }
    // Go default serialization: {"Accepted":bool,"Reason":string}.
    const body = (await res.json()) as { Accepted?: boolean; Reason?: string };
    return { accepted: body.Accepted === true, reason: body.Reason || undefined };
  }

  return {
    snapshot: () => current,
    // RHZ-072 (FR-RHZ-101): the unadapted envelope behind `current`, for the
    // on-demand detail surface (fields the projection does not carry, e.g. a
    // gate's body). Set together with `current` in applyEnvelope, so the two
    // always share one revision; no fetch happens on read.
    rawSnapshot: () => lastWire,
    /** What the latest projection could not carry over from the wire (structural counts only). */
    integrationReport: () => report,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    async relayIntent(intent, actor) {
      const refusal = contractRefusal(intent, nodesOf(current));
      if (refusal) return { accepted: false, reason: refusal };
      if ((intent as { action?: unknown } | null)?.action === NOTE_ANSWER_ACTION) {
        const i = intent as { nodeId?: unknown; decision?: unknown };
        const answer = noteAnswerIntent(lastWire, i.nodeId, i.decision);
        if (!answer.ok) return { accepted: false, reason: answer.reason };
        return postIntent({ ...answer.fields, actor });
      }
      const { kind, rest } = toRhizomeAddress((intent ?? {}) as Record<string, unknown>);
      // Only known intent fields travel (the idempotency key is not one yet); kind and actor are set last.
      const fields = toRhizomeFields(kind, Object.fromEntries(Object.entries(rest).filter(([k]) => INTENT_FIELDS.has(k))));
      if (kind !== undefined && DIGEST_BOUND_KINDS.has(kind)) {
        // The digest comes from this adapter's snapshot only; a cockpit-sent value never reaches Rhizome.
        const digest = snapshotDigest(lastWire, rest.gateId);
        if (digest === undefined) return { accepted: false, reason: DIGEST_UNAVAILABLE };
        fields.digest = digest;

        if (signer) {
          const consumer = snapshotGateConsumer(lastWire, rest.gateId);
          // approval.requestChanges is an instruction, not a decision. It stays
          // on the byte-identical unsigned path (and keeps `instruction`).
          if (!(consumer === 'approval' && kind === 'gate.requestChanges')) {
            const gateId = rest.gateId as string;
            const reason = effectiveReason(kind, fields);
            const idempotencyKey = (intent as { idempotencyKey: string }).idempotencyKey;
            const signed = await signer.sign({
              gateId,
              kind,
              reason,
              correlationId: `cockpit:${idempotencyKey}`,
            });
            if (!signed.ok) return { accepted: false, reason: signed.reason };
            if (signed.response.digest !== digest) {
              return { accepted: false, reason: SIGNER_REFUSAL.digestMismatch };
            }
            if (kind === 'gate.requestChanges') delete fields.instruction;
            fields.reason = signed.response.reason;
            fields.verification = { signature: signed.response.signature };
            actor = 'signer';
          }
        }
      }
      return postIntent({ ...fields, kind, actor });
    },
    executionSnapshot: (taskId: string) => execFeed(taskId).current,
    subscribeExecution(taskId, listener) {
      const feed = execFeed(taskId);
      feed.listeners.add(listener as (delta: ExecDelta) => void);
      reviewIdle(taskId);
      return () => {
        const removed = feed.listeners.delete(listener as (delta: ExecDelta) => void);
        reviewIdle(taskId);
        return removed;
      };
    },
    executionWire,
    subscribeExecutionBody(taskId, listener) {
      let set = execBodyListeners.get(taskId);
      if (!set) {
        set = new Set();
        execBodyListeners.set(taskId, set);
      }
      set.add(listener);
      execFeed(taskId); // make sure the tail is running
      reviewIdle(taskId);
      return () => {
        set!.delete(listener);
        if (set!.size === 0) execBodyListeners.delete(taskId);
        reviewIdle(taskId);
      };
    },
    // The terminal and stream channels are unsupported: they have no
    // contiguous-seq contract yet.
    terminalSnapshot: () => ({ unsupported: TERMINAL_UNSUPPORTED }),
    subscribeTerminal: () => ({ unsupported: TERMINAL_UNSUPPORTED }),
    subscribeStream: () => ({ unsupported: STREAM_UNSUPPORTED }),
    close() {
      closed = true;
      abort.abort();
      for (const [taskId, feed] of execFeeds) dropFeed(taskId, feed);
      execFeeds.clear();
    },
  };
}

/**
 * v1 workspace DTOs → the cockpit's projection shape, with the contract's
 * generic nodes alongside (additive field); revision carried as-is.
 */
export function adaptEnvelope(wire: UpstreamProjectionEnvelope): UpstreamProjectionEnvelope {
  return adaptWithReport(wire).envelope;
}

export function adaptWithReport(
  wire: UpstreamProjectionEnvelope,
  mediaTypeOf?: (blobId: string) => string | undefined,
): {
  envelope: UpstreamProjectionEnvelope;
  report: RhizomeIntegrationReport;
} {
  const body = adaptWorkspaceBody(wire.body);
  const { nodes, report } = projectRhizomeNodes(wire.body, body, mediaTypeOf);
  return { envelope: { revision: wire.revision, body: { ...body, nodes } }, report };
}

/** The generic nodes of an adapted envelope. */
export function nodesOf(envelope: UpstreamProjectionEnvelope): NodeProjection[] {
  return ((envelope.body as { nodes?: NodeProjection[] } | null)?.nodes ?? []);
}

/** Session intents keep their stand-in addressing until the contract carries sessions. */
const isSessionStandIn = (i: Record<string, unknown>) =>
  typeof i.intent === 'string' && i.intent.startsWith('session.') && !('nodeId' in i) && !('action' in i);

/**
 * The contract's structure checks, run before anything reaches Rhizome:
 * addressed node exists, action declared and enabled, no unknown keys, slots
 * paired, required input present. Pre-contract wire shapes are refused; only
 * session stand-ins pass (Rhizome answers those itself).
 */
export function contractRefusal(intent: unknown, nodes: readonly NodeProjection[]): string | undefined {
  if (typeof intent === 'object' && intent !== null && !Array.isArray(intent) && isSessionStandIn(intent as Record<string, unknown>)) {
    return undefined;
  }
  const i = intent as Partial<Intent> | null;
  const action = typeof i?.action === 'string' ? i.action : '';
  const node = typeof i?.nodeId === 'string' ? nodes.find((n) => n.id === i.nodeId) : undefined;
  const check = validateIntent(intent, lookupCapability(node, action));
  return check.ok ? undefined : check.reason;
}

async function fetchRawSnapshot(baseUrl: string): Promise<UpstreamProjectionEnvelope> {
  const res = await fetch(`${baseUrl}/v1/workspace`);
  if (!res.ok) throw new Error(`rhizome snapshot failed (${res.status})`);
  return (await res.json()) as UpstreamProjectionEnvelope;
}

/**
 * SSE framing per the HTML standard: LF, CRLF or CR line ends; a blank line
 * ends an event; repeated `data:` lines join with LF; one optional space after
 * the colon is stripped; comment lines are ignored.
 */
export class SseDecoder {
  private buffer = '';
  private event = '';
  private data: string[] = [];

  push(chunk: string): Array<{ event: string; data: string }> {
    this.buffer += chunk;
    const out: Array<{ event: string; data: string }> = [];
    // A trailing CR may be the first half of CRLF; keep it for the next chunk.
    const holdCr = this.buffer.endsWith('\r');
    const text = holdCr ? this.buffer.slice(0, -1) : this.buffer;
    const lines = text.split(/\r\n|\r|\n/);
    this.buffer = (lines.pop() ?? '') + (holdCr ? '\r' : '');
    for (const line of lines) {
      if (line === '') {
        if (this.data.length > 0) out.push({ event: this.event || 'message', data: this.data.join('\n') });
        this.event = '';
        this.data = [];
        continue;
      }
      if (line.startsWith(':')) continue;
      const colon = line.indexOf(':');
      const field = colon === -1 ? line : line.slice(0, colon);
      let value = colon === -1 ? '' : line.slice(colon + 1);
      if (value.startsWith(' ')) value = value.slice(1);
      if (field === 'event') this.event = value;
      else if (field === 'data') this.data.push(value);
    }
    return out;
  }
}

async function consumeStream(
  url: string,
  onEnvelope: (e: UpstreamProjectionEnvelope) => void,
  isClosed: () => boolean,
  signal: AbortSignal,
): Promise<void> {
  const res = await fetch(url, {
    headers: { accept: 'text/event-stream' },
    signal,
  });
  if (!res.ok || !res.body) throw new Error(`rhizome stream failed (${res.status})`);
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  const sse = new SseDecoder();
  while (!isClosed()) {
    const { value, done } = await reader.read();
    if (done) return;
    for (const frame of sse.push(decoder.decode(value, { stream: true }))) {
      if (frame.event === 'snapshot' || frame.event === 'projection') {
        onEnvelope(JSON.parse(frame.data) as UpstreamProjectionEnvelope);
      }
    }
  }
  await reader.cancel().catch(() => undefined);
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
