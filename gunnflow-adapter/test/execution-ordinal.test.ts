// RHZ-095 (FR-RHZ-122): multi-session tasks on the execution surface —
// O1–O5 the adapter's task-global event ordinal (src/execution.ts) and T1–T4
// the execFeed idle TTL (src/upstream.ts) against a mock Rhizome that counts
// its /v1/execution/{id}/stream connections.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { EXECUTION_MAX_EVENTS, validateExecutionSnapshot } from '@gunnflow/contract';
import { orderEvents, projectExecution, type RhizomeExecBody, type RhizomeExecEvent } from '../src/execution.js';
import { EXEC_FEED_IDLE_MS, createRhizomeUpstream, type IdleTimers } from '../src/upstream.js';

const TS = 1_700_000_000_000;
const TASK = 'mission-multi';
const ev = (sessionId: string, seq: number, kind = 'message'): RhizomeExecEvent => ({ sessionId, seq, kind, actor: 'worker', ts: TS + seq, usageIn: 0, usageOut: 0 });
const session = (id: string, state = 'ended') => ({ id, taskId: TASK, state });

/** Two sessions as Rhizome emits them: sessions[] sorted by id, events grouped per session, per-session seq from 1. */
const twoSessions = (): RhizomeExecBody => ({
  taskId: TASK,
  sessions: [session('exec-a'), session('exec-b', 'running')],
  events: [ev('exec-a', 1, 'subagent/spawn'), ev('exec-a', 2), ev('exec-a', 3, 'session/end'), ev('exec-b', 1, 'subagent/spawn'), ev('exec-b', 2)],
});

describe('FR-RHZ-122 O1–O5: task-global event ordinal over per-session Rhizome seqs', () => {
  it('O1: session A seq 1..3 + session B seq 1..2 → wire seq 1..5 strictly increasing, A then B, validator passes', () => {
    const snap = projectExecution(twoSessions());
    expect(snap.events.map((e) => e.seq)).toEqual([1, 2, 3, 4, 5]);
    expect(snap.events.map((e) => e.sessionId)).toEqual(['exec-a', 'exec-a', 'exec-a', 'exec-b', 'exec-b']);
    // Everything but seq is still the verbatim projection (at = ts, kind, label '').
    expect(snap.events[3]).toEqual({ seq: 4, at: TS + 1, sessionId: 'exec-b', kind: 'subagent/spawn', label: '' });
    const r = validateExecutionSnapshot(snap);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
    expect(snap).not.toHaveProperty('truncatedBefore');
  });

  it('O2: determinism — the same input twice yields identical bytes; an interleaved input order yields the same bytes too', () => {
    const a = JSON.stringify(projectExecution(twoSessions()));
    const b = JSON.stringify(projectExecution(twoSessions()));
    expect(a).toBe(b);
    const shuffled = twoSessions();
    shuffled.events = [shuffled.events![3]!, shuffled.events![2]!, shuffled.events![0]!, shuffled.events![4]!, shuffled.events![1]!];
    expect(JSON.stringify(projectExecution(shuffled))).toBe(a);
    // Input arrays are not mutated by the ordering.
    const body = twoSessions();
    const before = JSON.stringify(body);
    orderEvents(body);
    expect(JSON.stringify(body)).toBe(before);
  });

  it(`O3: window over >${EXECUTION_MAX_EVENTS} events across 2 sessions → newest kept, truncatedBefore == first kept ORDINAL (not a per-session seq), still valid`, () => {
    const nA = 1500;
    const nB = EXECUTION_MAX_EVENTS;
    const events = [...Array.from({ length: nA }, (_, i) => ev('exec-a', i + 1)), ...Array.from({ length: nB }, (_, i) => ev('exec-b', i + 1))];
    const snap = projectExecution({ taskId: TASK, sessions: [session('exec-a'), session('exec-b', 'running')], events });
    expect(snap.events).toHaveLength(EXECUTION_MAX_EVENTS);
    // 11500 ordinals, the newest 10000 kept: ordinals 1501..11500; the first kept is session B's seq 1 (ordinal nA + 1).
    expect(snap.events[0]).toMatchObject({ seq: nA + 1, sessionId: 'exec-b' });
    expect(snap.events.at(-1)!.seq).toBe(nA + nB);
    expect(snap.truncatedBefore).toBe(nA + 1);
    for (let i = 1; i < snap.events.length; i++) expect(snap.events[i]!.seq).toBe(snap.events[i - 1]!.seq + 1);
    const r = validateExecutionSnapshot(snap);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
  });

  it('O4: a single-session task is byte-identical to the RHZ-094 projection (golden captured at RHZ-094: ordinal == per-session seq)', () => {
    const body: RhizomeExecBody = {
      taskId: 'mission-one',
      sessions: [{ id: 'exec-1', taskId: 'mission-one', state: 'running', label: 'solo', janusState: 'running', usageInTotal: 5, usageOutTotal: 6, lastActivityTs: TS + 40 }],
      events: [
        { sessionId: 'exec-1', seq: 1, kind: 'subagent/spawn', actor: 'janus', ts: TS, usageIn: 0, usageOut: 0 },
        { sessionId: 'exec-1', seq: 2, kind: 'message', actor: 'worker', ts: TS + 10, usageIn: 2, usageOut: 3 },
        { sessionId: 'exec-1', seq: 3, kind: 'tool/call', actor: 'worker', ts: TS + 20, usageIn: 0, usageOut: 0 },
        { sessionId: 'exec-1', seq: 4, kind: 'tool/result', actor: 'janus', ts: TS + 30, usageIn: 0, usageOut: 0 },
        { sessionId: 'exec-1', seq: 5, kind: 'message', actor: 'worker', ts: TS + 40, usageIn: 3, usageOut: 3 },
      ],
    };
    const golden =
      '{"sessions":[{"id":"exec-1","taskId":"mission-one","state":"running","label":"solo","upstreamSessionState":"running","usageInTotal":5,"usageOutTotal":6,"lastActivityTs":1700000000040}],"events":[{"seq":1,"at":1700000000000,"sessionId":"exec-1","kind":"subagent/spawn","label":""},{"seq":2,"at":1700000000010,"sessionId":"exec-1","kind":"message","label":""},{"seq":3,"at":1700000000020,"sessionId":"exec-1","kind":"tool/call","label":""},{"seq":4,"at":1700000000030,"sessionId":"exec-1","kind":"tool/result","label":""},{"seq":5,"at":1700000000040,"sessionId":"exec-1","kind":"message","label":""}]}';
    expect(JSON.stringify(projectExecution(body))).toBe(golden);
    // A lone per-session seq (no offset) also survives untouched, as RHZ-094 E1 asserts.
    expect(projectExecution({ taskId: 't', sessions: [session('s')], events: [ev('s', 7)] }).events[0]!.seq).toBe(7);
  });

  it('O5: appending events to the LAST session keeps every earlier ordinal; an event appended to an EARLIER session renumbers what follows (the documented caveat)', () => {
    const base = projectExecution(twoSessions());
    const grown = twoSessions();
    grown.events!.push(ev('exec-b', 3));
    const next = projectExecution(grown);
    expect(next.events.slice(0, 5)).toEqual(base.events);
    expect(next.events[5]).toMatchObject({ seq: 6, sessionId: 'exec-b' });
    expect(validateExecutionSnapshot(next).ok).toBe(true);
    // Caveat, pinned so it is never silently relied upon: a new event in the earlier session shifts session B's ordinals.
    const earlier = twoSessions();
    earlier.events!.splice(3, 0, ev('exec-a', 4));
    const shifted = projectExecution(earlier);
    expect(shifted.events.slice(0, 3)).toEqual(base.events.slice(0, 3));
    expect(shifted.events.map((e) => e.seq)).toEqual([1, 2, 3, 4, 5, 6]);
    expect(shifted.events[4]).toMatchObject({ seq: 5, sessionId: 'exec-b', at: TS + 1 });
    expect(validateExecutionSnapshot(shifted).ok).toBe(true);
  });

  it('dangling events (a sessionId the body does not carry) rank after the carried sessions and are still carried, so the validator refuses them as before', () => {
    const body = twoSessions();
    body.events!.unshift(ev('ghost', 1));
    const snap = projectExecution(body);
    expect(snap.events.at(-1)).toMatchObject({ seq: 6, sessionId: 'ghost' });
    expect(validateExecutionSnapshot(snap).ok).toBe(false);
  });
});

/* ---- T1–T4: execFeed idle TTL against a mock Rhizome that counts stream connections ---- */
const wireBody = { missions: [], tasks: [{ id: TASK, missionId: 'g', name: 'm', state: 'running', hasProgress: false, attention: false }], gates: [], deliverables: [], edges: [], counts: { running: 1, needsYou: 0, blocked: 0 }, attention: [] };
const execEnvelope = () => ({ revision: 7, body: twoSessions() });
let openStreams = 0;
let streamOpens = 0;
let execGets = 0;

// forceCloseConnections: the SSE handlers write to reply.raw and never end the
// response, so close() must drop those sockets or afterAll can hang (seen on
// Linux/Node 22 CI, RHZ-097). Teardown only; no assertion changes. FR-RHZ-122.
const mock = Fastify({ logger: false, forceCloseConnections: true });
mock.get('/v1/workspace', async () => ({ revision: 1, body: wireBody }));
mock.get('/v1/workspace/stream', (_req, reply) => {
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 1, body: wireBody })}\n\n`);
});
mock.get('/v1/execution/*', async (req, reply) => {
  const rest = (req.params as { '*': string })['*'];
  if (!rest.endsWith('/stream')) {
    execGets++;
    return execEnvelope();
  }
  openStreams++;
  streamOpens++;
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  reply.raw.write(`event: snapshot\ndata: ${JSON.stringify(execEnvelope())}\n\n`);
  req.raw.on('close', () => {
    openStreams--;
  });
});

let rhizomeUrl = '';
beforeAll(async () => {
  await mock.listen({ port: 0, host: '127.0.0.1' });
  const address = mock.server.address();
  rhizomeUrl = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
});
afterAll(async () => {
  await mock.close();
});
/** Waits on real I/O (the SSE sockets) until `cond` holds; the TTL clock below is advanced only explicitly. */
async function until(cond: () => boolean, what: string, ms = 3000): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (cond()) return;
    await new Promise<void>((r) => setTimeout(r, 5));
  }
  throw new Error(`timed out waiting for: ${what}`);
}

/**
 * A deterministic clock injected as the upstream's idle-TTL timers. The global
 * timers stay real on purpose: faking them (vi.useFakeTimers) stalls undici's
 * reconnect after an aborted SSE socket, which is exactly what T3 exercises.
 */
function fakeClock(): { timers: IdleTimers; advance(ms: number): Promise<void>; pending(): number } {
  let now = 0;
  let nextId = 0;
  const pending = new Map<number, { at: number; fn: () => void }>();
  return {
    timers: {
      setTimeout(fn, ms) {
        pending.set(++nextId, { at: now + ms, fn });
        return nextId;
      },
      clearTimeout(handle) {
        pending.delete(handle as number);
      },
    },
    async advance(ms) {
      const target = now + ms;
      for (;;) {
        let due: [number, { at: number; fn: () => void }] | undefined;
        for (const entry of pending) if (entry[1].at <= target && (!due || entry[1].at < due[1].at)) due = entry;
        if (!due) break;
        pending.delete(due[0]);
        now = due[1].at;
        due[1].fn();
        await new Promise<void>((r) => setImmediate(r)); // let the abort propagate
      }
      now = target;
    },
    pending: () => pending.size,
  };
}

describe('FR-RHZ-122 T1–T4: execFeed idle TTL', () => {
  it('T1: a feed started by a plain GET (no subscriber) expires after the default TTL — the Rhizome SSE is closed and the feed dropped', async () => {
    const clock = fakeClock();
    const up = await createRhizomeUpstream(rhizomeUrl, { timers: clock.timers });
    try {
      expect(EXEC_FEED_IDLE_MS).toBe(5 * 60 * 1000);
      const before = streamOpens;
      const body = await up.executionWire(TASK);
      expect(body?.sessions).toHaveLength(2);
      await until(() => streamOpens === before + 1, 'feed SSE opened');
      await until(() => openStreams === 1, 'one open stream');
      expect(clock.pending()).toBe(1); // the TTL is armed: no subscriber
      await clock.advance(EXEC_FEED_IDLE_MS - 1);
      await new Promise<void>((r) => setTimeout(r, 50));
      expect(openStreams).toBe(1); // not yet
      await clock.advance(1);
      await until(() => openStreams === 0, 'feed SSE closed by the idle TTL');
      expect(streamOpens).toBe(before + 1); // no reconnect of the dropped feed
      await new Promise<void>((r) => setTimeout(r, 50));
      expect(streamOpens).toBe(before + 1);
      expect(openStreams).toBe(0);
      expect(clock.pending()).toBe(0);
    } finally {
      up.close();
    }
  });

  it('T2: a subscriber attaching within the TTL cancels the expiry; after it leaves, a fresh TTL runs', async () => {
    const clock = fakeClock();
    const up = await createRhizomeUpstream(rhizomeUrl, { execFeedIdleMs: 1000, timers: clock.timers });
    try {
      const before = streamOpens;
      await up.executionWire(TASK);
      await until(() => openStreams === 1, 'one open stream');
      await clock.advance(900);
      const off = up.subscribeExecutionBody(TASK, () => undefined);
      expect(clock.pending()).toBe(0); // the subscriber cancelled the armed TTL
      await clock.advance(5000);
      expect(openStreams).toBe(1);
      expect(streamOpens).toBe(before + 1);
      off();
      expect(clock.pending()).toBe(1); // a fresh TTL
      await clock.advance(999);
      expect(openStreams).toBe(1);
      await clock.advance(1);
      await until(() => openStreams === 0, 'closed after the subscriber left');
    } finally {
      up.close();
    }
  });

  it('T3: a request after expiry recreates the feed lazily (new GET + new SSE connection) and serves the same body', async () => {
    const clock = fakeClock();
    const up = await createRhizomeUpstream(rhizomeUrl, { execFeedIdleMs: 1000, timers: clock.timers });
    try {
      const opens = streamOpens;
      const gets = execGets;
      const first = await up.executionWire(TASK);
      await until(() => openStreams === 1, 'one open stream');
      await clock.advance(1000);
      await until(() => openStreams === 0, 'expired');
      const again = await up.executionWire(TASK);
      expect(JSON.stringify(again)).toBe(JSON.stringify(first));
      expect(execGets).toBe(gets + 2); // the feed was gone: a fresh GET decided 200
      await until(() => streamOpens === opens + 2 && openStreams === 1, 'recreated feed SSE');
      // The recreated feed expires on its own TTL too.
      await clock.advance(1000);
      await until(() => openStreams === 0, 'recreated feed expired');
      // And the port-side delta subscription also counts as a subscriber (keeps a recreated feed).
      const off = up.subscribeExecution(TASK, () => undefined);
      await until(() => openStreams === 1, 'feed for the delta subscriber');
      await clock.advance(5000);
      expect(openStreams).toBe(1);
      off();
      await clock.advance(1000);
      await until(() => openStreams === 0, 'expired after the delta subscriber left');
    } finally {
      up.close();
    }
  });

  it('T4: an active stream subscriber never expires, however long the clock runs', async () => {
    const clock = fakeClock();
    const up = await createRhizomeUpstream(rhizomeUrl, { execFeedIdleMs: 1000, timers: clock.timers });
    try {
      const before = streamOpens;
      const frames: RhizomeExecBody[] = [];
      const off = up.subscribeExecutionBody(TASK, (b) => frames.push(b));
      await until(() => openStreams === 1, 'one open stream');
      await until(() => frames.length === 1, 'first body delivered');
      expect(clock.pending()).toBe(0); // never armed while a stream client is attached
      for (let i = 0; i < 10; i++) await clock.advance(1000);
      expect(openStreams).toBe(1);
      expect(streamOpens).toBe(before + 1);
      // executionSnapshot (in-process read) is not a subscriber, but the stream client still is.
      expect((up.executionSnapshot(TASK) as RhizomeExecBody).sessions).toHaveLength(2);
      off();
      await clock.advance(1000);
      await until(() => openStreams === 0, 'closed once the last subscriber left');
    } finally {
      up.close();
    }
  });
});
