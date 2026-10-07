// RHZ-094 (FR-RHZ-121): the direct wire's execution surface (contract 0.3.0,
// WIRE.md §execution) against a mock Rhizome /v1/execution. E1 projection
// mapping, E2 window and caps, E3 route 200/404 + terminal, E4 SSE republish
// grammar, E5 validate guard → 500, E6 the other wire paths unchanged.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import {
  DIRECT_WIRE,
  EXECUTION_MAX_EVENTS,
  EXECUTION_MAX_KIND_CHARS,
  EXECUTION_MAX_LABEL_CHARS,
  validateExecutionSnapshot,
  type ExecutionSnapshot,
} from '@gunnflow/contract';
import { SseDecoder, createRhizomeUpstream, projectExecution, type RhizomeExecBody } from '../src/index.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';

const TS = 1_700_000_000_000;

/* ---- E1: projection mapping (pure) ---- */
describe('FR-RHZ-121 E1: Rhizome execution body → contract ExecutionSnapshot', () => {
  it('renames and carries every Rhizome field verbatim: ts→at, janusState→upstreamSessionState, usage totals, kind untouched; label is the empty "no summary" (actor is not a summary)', () => {
    const body: RhizomeExecBody = {
      taskId: 'mission-demo',
      sessions: [
        { id: 'exec-1', taskId: 'mission-demo', state: 'running', label: 'demo run', janusState: 'running', usageInTotal: 12, usageOutTotal: 34, lastActivityTs: TS + 5 },
      ],
      events: [
        { sessionId: 'exec-1', seq: 1, kind: 'subagent/spawn', actor: 'janus', ts: TS, usageIn: 0, usageOut: 0 },
        { sessionId: 'exec-1', seq: 2, kind: 'message', actor: 'worker', ts: TS + 5, usageIn: 12, usageOut: 34 },
      ],
    };
    const snap = projectExecution(body);
    expect(snap).toEqual({
      sessions: [
        { id: 'exec-1', taskId: 'mission-demo', state: 'running', label: 'demo run', upstreamSessionState: 'running', usageInTotal: 12, usageOutTotal: 34, lastActivityTs: TS + 5 },
      ],
      events: [
        { seq: 1, at: TS, sessionId: 'exec-1', kind: 'subagent/spawn', label: '' },
        { seq: 2, at: TS + 5, sessionId: 'exec-1', kind: 'message', label: '' },
      ],
    });
    expect(validateExecutionSnapshot(snap).ok).toBe(true);
    // Contract event keys are closed: Rhizome's usageIn/usageOut/actor are not carried, and no status is invented.
    for (const e of snap.events) expect(Object.keys(e).sort()).toEqual(['at', 'kind', 'label', 'seq', 'sessionId']);
    // actor is never re-purposed as the contract's summary label.
    expect(snap.events.some((e) => e.label === 'janus' || e.label === 'worker')).toBe(false);
  });

  it('omits what Rhizome did not send: no label/janusState/usage/lastActivity → keys absent, never synthesized; event label is always the empty string', () => {
    const snap = projectExecution({
      taskId: 'mission-demo',
      sessions: [{ id: 'exec-1', taskId: 'mission-demo', state: 'running' }, { id: 'exec-2', taskId: 'mission-demo', state: 'ended', label: '' }],
      events: [{ sessionId: 'exec-1', seq: 7, kind: 'session/end', actor: '', ts: TS, usageIn: 0, usageOut: 0 }],
    });
    expect(snap.sessions[0]).toEqual({ id: 'exec-1', taskId: 'mission-demo', state: 'running' });
    expect(snap.sessions[1]).toEqual({ id: 'exec-2', taskId: 'mission-demo', state: 'ended' });
    for (const s of snap.sessions) {
      expect(s).not.toHaveProperty('upstreamSessionState');
      expect(s).not.toHaveProperty('usageInTotal');
      expect(s).not.toHaveProperty('usageOutTotal');
      expect(s).not.toHaveProperty('lastActivityTs');
    }
    // `label` is a required contract key and Rhizome has no event summary: '' is the honest value, nothing is filled in.
    expect(snap.events[0]).toEqual({ seq: 7, at: TS, sessionId: 'exec-1', kind: 'session/end', label: '' });
    expect(snap).not.toHaveProperty('truncatedBefore');
    expect(validateExecutionSnapshot(snap).ok).toBe(true);
    // Rhizome's null collections (Go empty slices) are the empty snapshot.
    expect(projectExecution({ taskId: 'mission-demo', sessions: null, events: null })).toEqual({ sessions: [], events: [] });
  });

  it('state table: Rhizome running|killed|ended pass verbatim (Accepted/Observing→running is Rhizome\'s mapping); paused is never produced; JANUS pending stays under upstreamSessionState', () => {
    const states = ['running', 'killed', 'ended'] as const;
    const snap = projectExecution({
      taskId: 'mission-demo',
      sessions: states.map((state, i) => ({ id: `exec-${i}`, taskId: 'mission-demo', state, janusState: i === 0 ? 'pending' : 'exited' })),
      events: [],
    });
    expect(snap.sessions.map((s) => s.state)).toEqual(['running', 'killed', 'ended']);
    // An Accepted (pre-spawn) session: Rhizome says running, JANUS says pending — both carried, neither re-interpreted.
    expect(snap.sessions[0]).toMatchObject({ state: 'running', upstreamSessionState: 'pending' });
    expect(snap.sessions.some((s) => s.state === 'paused')).toBe(false);
    expect(validateExecutionSnapshot(snap).ok).toBe(true);
    // A state outside the enum is passed through untouched (and refused by the validator), not re-mapped.
    const rogue = projectExecution({ taskId: 't', sessions: [{ id: 'x', taskId: 't', state: 'intent' }], events: [] });
    expect(rogue.sessions[0]!.state).toBe('intent');
    expect(validateExecutionSnapshot(rogue).ok).toBe(false);
  });
});

/* ---- E2: window and caps ---- */
describe('FR-RHZ-121 E2: bounds — newest-events window with truncatedBefore, label/kind caps', () => {
  it(`keeps the newest ${EXECUTION_MAX_EVENTS} events and declares the cut as the first carried seq`, () => {
    const n = EXECUTION_MAX_EVENTS + 3;
    const events = Array.from({ length: n }, (_, i) => ({ sessionId: 's', seq: i + 1, kind: 'message', actor: 'a', ts: TS + i, usageIn: 0, usageOut: 0 }));
    const snap = projectExecution({ taskId: 't', sessions: [{ id: 's', taskId: 't', state: 'running' }], events });
    expect(snap.events).toHaveLength(EXECUTION_MAX_EVENTS);
    expect(snap.events[0]!.seq).toBe(4);
    expect(snap.events.at(-1)!.seq).toBe(n);
    expect(snap.truncatedBefore).toBe(4);
    const r = validateExecutionSnapshot(snap);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
    // Exactly at the cap: nothing is cut and no cut is declared.
    const exact = projectExecution({ taskId: 't', sessions: [{ id: 's', taskId: 't', state: 'running' }], events: events.slice(0, EXECUTION_MAX_EVENTS) });
    expect(exact.events).toHaveLength(EXECUTION_MAX_EVENTS);
    expect(exact).not.toHaveProperty('truncatedBefore');
  });

  it('cuts over-long session label/kind/upstreamSessionState to the contract caps so the snapshot validates', () => {
    const snap = projectExecution({
      taskId: 't',
      sessions: [{ id: 's', taskId: 't', state: 'running', label: 'L'.repeat(EXECUTION_MAX_LABEL_CHARS + 50), janusState: 'S'.repeat(100) }],
      events: [{ sessionId: 's', seq: 1, kind: 'k'.repeat(EXECUTION_MAX_KIND_CHARS + 20), actor: 'a'.repeat(EXECUTION_MAX_LABEL_CHARS + 1), ts: TS, usageIn: 0, usageOut: 0 }],
    });
    expect(snap.sessions[0]!.label).toHaveLength(EXECUTION_MAX_LABEL_CHARS);
    expect(snap.sessions[0]!.upstreamSessionState).toHaveLength(64);
    expect(snap.events[0]!.kind).toHaveLength(EXECUTION_MAX_KIND_CHARS);
    expect(snap.events[0]!.label).toBe('');
    const r = validateExecutionSnapshot(snap);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
  });
});

/* ---- E3–E6: the wire against a mock Rhizome ---- */
const wireBody = {
  missions: [{ id: 'goal-a', name: 'a', attention: false }],
  tasks: [{ id: 'mission-demo', missionId: 'goal-a', name: 'demo', state: 'running', hasProgress: false, attention: false }],
  gates: [],
  deliverables: [],
  edges: [],
  counts: { running: 1, needsYou: 0, blocked: 0 },
  attention: [],
};

type Session = { id: string; taskId: string; state: string; label?: string; janusState?: string; usageInTotal?: number; usageOutTotal?: number; lastActivityTs?: number };
let demoSessions: Session[] = [{ id: 'exec-1', taskId: 'mission-demo', state: 'running', label: 'demo run', janusState: 'running', usageInTotal: 3, usageOutTotal: 4, lastActivityTs: TS }];
let demoRevision = 11;
const demoEvents = [{ sessionId: 'exec-1', seq: 1, kind: 'subagent/spawn', actor: 'janus', ts: TS, usageIn: 3, usageOut: 4 }];
const EXEC: Record<string, () => { revision: number; body: unknown }> = {
  'mission-demo': () => ({ revision: demoRevision, body: { taskId: 'mission-demo', sessions: demoSessions, events: demoEvents } }),
  // Known mission, nothing recorded (no session yet / session-log source not wired): Rhizome's empty 200.
  'mission-empty': () => ({ revision: 2, body: { taskId: 'mission-empty', sessions: [], events: [] } }),
  // E5 fixtures: an event pointing at a session the snapshot does not carry; a session of another task.
  'mission-dangling': () => ({ revision: 3, body: { taskId: 'mission-dangling', sessions: [{ id: 'exec-9', taskId: 'mission-dangling', state: 'running' }], events: [{ sessionId: 'ghost', seq: 1, kind: 'message', actor: 'a', ts: TS, usageIn: 0, usageOut: 0 }] } }),
  'mission-stray': () => ({ revision: 4, body: { taskId: 'mission-stray', sessions: [{ id: 'exec-8', taskId: 'mission-other', state: 'ended' }], events: [] } }),
  'mission-a/b': () => ({ revision: 5, body: { taskId: 'mission-a/b', sessions: [{ id: 'exec-7', taskId: 'mission-a/b', state: 'ended' }], events: [] } }),
  // F2 fixture: valid first, then a republish that breaks the contract (dangling sessionId).
  'mission-turns-bad': () => ({ revision: 6, body: { taskId: 'mission-turns-bad', sessions: [{ id: 'exec-6', taskId: 'mission-turns-bad', state: 'running' }], events: turnsBadEvents } }),
};
let turnsBadEvents: Array<{ sessionId: string; seq: number; kind: string; actor: string; ts: number; usageIn: number; usageOut: number }> = [];
const execSubscribers = new Set<() => void>();

const mock = Fastify({ logger: false });
mock.get('/v1/workspace', async () => ({ revision: 1, body: wireBody }));
mock.get('/v1/workspace/stream', (_req, reply) => {
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 1, body: wireBody })}\n\n`);
});
mock.get('/v1/execution/*', async (req, reply) => {
  const rest = (req.params as { '*': string })['*'];
  const stream = rest.endsWith('/stream');
  const taskId = decodeURIComponent(stream ? rest.slice(0, -'/stream'.length) : rest);
  const envelope = EXEC[taskId];
  if (!envelope) return reply.code(404).send('unknown task');
  if (!stream) return envelope();
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  const write = (kind: string) => reply.raw.write(`event: ${kind}\ndata: ${JSON.stringify(envelope())}\n\n`);
  write('snapshot');
  const push = () => write('projection');
  execSubscribers.add(push);
  req.raw.on('close', () => execSubscribers.delete(push));
});

let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;
let rhizomeUrl = '';
beforeAll(async () => {
  await mock.listen({ port: 0, host: '127.0.0.1' });
  const address = mock.server.address();
  rhizomeUrl = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
  server = await startRhizomeDirectServer({ rhizomeUrl, port: 0 });
});
afterAll(async () => {
  await server.close();
  await mock.close();
});

const get = (path: string) => fetch(`${server.url}${path}`);
const execPath = (taskId: string) => `${DIRECT_WIRE.execution}/${encodeURIComponent(taskId)}`;

/** Reads `snapshot` frames off a wire SSE response, one at a time, with a timeout. */
function sseFrames(res: Response) {
  const reader = res.body!.getReader();
  const text = new TextDecoder();
  const sse = new SseDecoder();
  const queue: ExecutionSnapshot[] = [];
  const waiters: Array<(f: ExecutionSnapshot | undefined) => void> = [];
  let ended = false;
  void (async () => {
    while (true) {
      const { value, done } = await reader.read().catch(() => ({ value: undefined, done: true }));
      if (done) break;
      for (const frame of sse.push(text.decode(value, { stream: true }))) {
        if (frame.event !== DIRECT_WIRE.snapshotEvent) continue;
        const snap = JSON.parse(frame.data) as ExecutionSnapshot;
        const w = waiters.shift();
        if (w) w(snap);
        else queue.push(snap);
      }
    }
    ended = true;
    for (const w of waiters.splice(0)) w(undefined);
  })();
  return {
    next: (ms = 1500): Promise<ExecutionSnapshot | undefined> => {
      if (queue.length > 0) return Promise.resolve(queue.shift());
      if (ended) return Promise.resolve(undefined);
      return new Promise((resolve) => {
        const timer = setTimeout(() => {
          const i = waiters.indexOf(settle);
          if (i >= 0) waiters.splice(i, 1);
          resolve(undefined);
        }, ms);
        const settle = (f: ExecutionSnapshot | undefined) => {
          clearTimeout(timer);
          resolve(f);
        };
        waiters.push(settle);
      });
    },
    close: () => reader.cancel().catch(() => undefined),
    isEnded: () => ended,
  };
}

const expectedDemo = (): ExecutionSnapshot => ({
  sessions: demoSessions.map((s) => ({ ...s }) as ExecutionSnapshot['sessions'][number]).map((s) => {
    const { janusState, ...rest } = s as Session;
    return { ...rest, ...(janusState ? { upstreamSessionState: janusState } : {}) } as ExecutionSnapshot['sessions'][number];
  }),
  events: demoEvents.map((e) => ({ seq: e.seq, at: e.ts, sessionId: e.sessionId, kind: e.kind, label: '' })),
});

describe('FR-RHZ-121 E3: GET /execution/:taskId on the direct wire', () => {
  it('200: a contract-valid ExecutionSnapshot projected from Rhizome /v1/execution (application/json)', async () => {
    const res = await get(execPath('mission-demo'));
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('application/json');
    const body = (await res.json()) as unknown;
    const r = validateExecutionSnapshot(body);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
    expect(body).toEqual(expectedDemo());
  });

  it('200 with empty sessions/events for a known task with nothing recorded — Rhizome serves the surface, so never 501', async () => {
    const res = await get(execPath('mission-empty'));
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ sessions: [], events: [] });
  });

  it('404 {reason} for a task Rhizome has no execution for (its 404), including ids that are not nodes at all', async () => {
    for (const id of ['mission-nope', 'goal-a', 'does-not-exist']) {
      const res = await get(execPath(id));
      expect(res.status, id).toBe(404);
      expect(await res.json()).toEqual({ reason: 'no execution for this task' });
    }
  });

  // MOCK-ONLY: the Fastify mock decodes %2F in its wildcard; live Rhizome's router splits
  // the decoded r.URL.Path and currently 404s ids containing '/' (being fixed on the Rhizome
  // side separately). This proves the wire's own decoding, not the end-to-end path.
  it('the task id segment is percent-decoded (Rhizome ids may contain "/") [mock-only]', async () => {
    const res = await get(execPath('mission-a/b'));
    expect(res.status).toBe(200);
    expect(((await res.json()) as ExecutionSnapshot).sessions[0]).toEqual({ id: 'exec-7', taskId: 'mission-a/b', state: 'ended' });
  });

  it('terminal stays unsupported with the 501 wording on the port; the contract has no /terminal route, so the wire answers "not on the direct wire"', async () => {
    const upstream = await createRhizomeUpstream(rhizomeUrl);
    try {
      expect(upstream.terminalSnapshot('s-x')).toEqual({
        unsupported: 'Rhizome provides no terminal surface yet (its terminal endpoint answers 501)',
      });
    } finally {
      upstream.close();
    }
    const res = await get('/terminal/s-x');
    expect(res.status).toBe(404);
    expect(await res.json()).toEqual({ reason: 'not on the direct wire' });
  });
});

describe('FR-RHZ-121 E4: GET /execution/:taskId/stream — SSE `snapshot` frames, each the full snapshot', () => {
  it('first frame = the current snapshot (same body as GET); a Rhizome change re-emits the whole snapshot; an identical republish does not', async () => {
    const res = await fetch(`${server.url}${execPath('mission-demo')}/stream`, { headers: { accept: 'text/event-stream' } });
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('text/event-stream');
    const frames = sseFrames(res);
    const first = await frames.next();
    expect(first).toEqual(expectedDemo());
    expect(first).toEqual(await (await get(execPath('mission-demo'))).json());

    // Identical republish from Rhizome → no new frame on the wire.
    for (const push of execSubscribers) push();
    expect(await frames.next(300)).toBeUndefined();

    // A real change (session ended, usage grew) → one full snapshot frame, not a delta.
    demoSessions = [{ id: 'exec-1', taskId: 'mission-demo', state: 'ended', label: 'demo run', janusState: 'exited', usageInTotal: 30, usageOutTotal: 40, lastActivityTs: TS + 9 }];
    demoRevision += 1;
    for (const push of execSubscribers) push();
    const second = await frames.next();
    expect(second).toEqual(expectedDemo());
    expect(second!.sessions[0]).toMatchObject({ state: 'ended', upstreamSessionState: 'exited', usageInTotal: 30 });
    expect(second!.events).toHaveLength(1);
    expect(validateExecutionSnapshot(second!).ok).toBe(true);
    // GET now agrees with the last frame.
    expect(await (await get(execPath('mission-demo'))).json()).toEqual(second);
    await frames.close();
  });

  it('a republish that breaks the contract is never sent: the response ENDS (fault signal for the BFF to reconnect), and the reconnect GET path answers the reasoned 500', async () => {
    const res = await fetch(`${server.url}${execPath('mission-turns-bad')}/stream`, { headers: { accept: 'text/event-stream' } });
    expect(res.status).toBe(200);
    const frames = sseFrames(res);
    expect((await frames.next())!.sessions[0]).toMatchObject({ id: 'exec-6', state: 'running' });
    turnsBadEvents = [{ sessionId: 'ghost', seq: 1, kind: 'message', actor: 'a', ts: TS, usageIn: 0, usageOut: 0 }];
    for (const push of execSubscribers) push();
    expect(await frames.next()).toBeUndefined();
    await new Promise((r) => setTimeout(r, 50));
    expect(frames.isEnded(), 'the SSE response must have ended, not gone silent').toBe(true);
    const again = await get(execPath('mission-turns-bad'));
    expect(again.status).toBe(500);
    expect(((await again.json()) as { reason: string }).reason).toContain("'ghost' references no session");
  });

  it('404 for an unknown task is answered as HTTP 404 at route entry (the contract: same rules as GET), not as an SSE 200', async () => {
    const res = await fetch(`${server.url}${execPath('mission-nope')}/stream`, { headers: { accept: 'text/event-stream' } });
    expect(res.status).toBe(404);
    expect(res.headers.get('content-type')).toContain('application/json');
    expect(await res.json()).toEqual({ reason: 'no execution for this task' });
  });
});

describe('FR-RHZ-121 E5: the validate guard — a non-conforming projection never reaches the wire', () => {
  it('an event whose sessionId names no carried session → 500 with the contract\'s reason (GET and stream)', async () => {
    const res = await get(execPath('mission-dangling'));
    expect(res.status).toBe(500);
    const body = (await res.json()) as { reason: string };
    expect(body.reason).toContain('execution fails the contract');
    expect(body.reason).toContain("'ghost' references no session");
    const stream = await fetch(`${server.url}${execPath('mission-dangling')}/stream`, { headers: { accept: 'text/event-stream' } });
    expect(stream.status).toBe(500);
    expect(((await stream.json()) as { reason: string }).reason).toContain("'ghost' references no session");
  });

  it('a session carrying another task\'s id → 500 (the wire\'s own invariant, mirrored from the BFF)', async () => {
    const res = await get(execPath('mission-stray'));
    expect(res.status).toBe(500);
    expect(((await res.json()) as { reason: string }).reason).toContain("session 'exec-8' carries taskId 'mission-other', not the requested 'mission-stray'");
  });
});

describe('FR-RHZ-121 E6: the other wire paths are unchanged', () => {
  it('/nodes, /stream, /detail serve as before; /execution without id, trailing forms and non-GET are off the wire', async () => {
    const nodes = (await (await get(DIRECT_WIRE.nodes)).json()) as { revision: number; nodes: Array<{ id: string }> };
    expect(nodes.revision).toBe(1);
    expect(nodes.nodes.map((n) => n.id)).toContain('mission-demo');
    const detail = await get(`${DIRECT_WIRE.detail}/mission-demo`);
    expect(detail.status).toBe(200);
    const stream = await fetch(`${server.url}${DIRECT_WIRE.stream}`, { headers: { accept: 'text/event-stream' } });
    expect(stream.status).toBe(200);
    await stream.body!.cancel();
    for (const path of ['/execution', '/execution/', '/execution/mission-demo/', '/execution/mission-demo/stream/x', '/execution/mission-demo/other']) {
      expect((await get(path)).status, path).toBe(404);
    }
    expect((await fetch(`${server.url}${execPath('mission-demo')}`, { method: 'POST' })).status).toBe(404);
    expect((await get('/artifact/d-1/deadbeef')).status).toBe(404);
  });
});
