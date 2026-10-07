// Rhizome upstream adapter against a mock Rhizome surface (contract per
// Rhizome workspace HTTP surface / internal/workspace/http.go).
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import type { WorkspaceUpstream } from '@gunnflow/upstream-port';
import { SseDecoder, contractVersion, createRhizomeUpstream, createUpstream } from '../src/index.js';
import { toRhizomeAddress, toRhizomeFields } from '../src/upstream.js';
import { refusedIntentShapes } from '@gunnflow/contract/conformance';

const received: Array<Record<string, unknown>> = [];
let revision = 3;
const envelope = () => ({
  revision,
  body: {
    missions: [{ id: 'goal-demo', name: 'demo', attention: false }],
    tasks: [
      {
        id: 'mission-demo',
        missionId: 'goal-demo',
        name: 'demo',
        state: 'queued',
        hasProgress: false,
        attention: false,
      },
    ],
    gates: [],
    deliverables: [],
    edges: [],
    counts: { running: 0, needsYou: 0, blocked: 0 },
    attention: [],
  },
});

const subscribers = new Set<() => void>();
const app = Fastify({ logger: false });
app.get('/v1/workspace', async () => envelope());
app.get('/v1/workspace/stream', (req, reply) => {
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  const write = (kind: string) =>
    reply.raw.write(`event: ${kind}\ndata: ${JSON.stringify(envelope())}\n\n`);
  write('snapshot');
  const push = () => write('projection');
  subscribers.add(push);
  req.raw.on('close', () => subscribers.delete(push));
});
app.post('/v1/intent', async (req, reply) => {
  const body = req.body as Record<string, unknown>;
  received.push(body);
  if (body.sessionId === 'fail.json') return reply.code(422).send({ Accepted: false, Reason: 'upstream says no: mission locked' });
  if (body.sessionId === 'fail.text') return reply.code(500).type('text/plain').send('plain upstream failure');
  if (body.sessionId === 'fail.empty') return reply.code(503).send('');
  if (body.kind === 'mission.create' && !body.prompt) {
    return { Accepted: false, Reason: 'prompt required (success criterion)' };
  }
  if (typeof body.kind === 'string' && body.kind.startsWith('session.')) {
    return { Accepted: false, Reason: 'JANUS T17-19 표면 의존' };
  }
  revision++;
  for (const push of subscribers) push();
  return { Accepted: true, Reason: '' };
});

/* --- execution surface v1 mock (contract ①) --- */
type WireSession = { id: string; taskId: string; state: string; label?: string };
let execSessions: WireSession[] = [{ id: 'exec-1', taskId: 'mission-demo', state: 'running' }];
let execRevision = 5;
const execSubscribers = new Set<() => void>();
const execEnvelope = () => ({
  revision: execRevision,
  body: { taskId: 'mission-demo', sessions: execSessions, events: [] },
});
app.get('/v1/execution/:taskId', async (req, reply) => {
  const { taskId } = req.params as { taskId: string };
  if (taskId !== 'mission-demo') return reply.code(404).send('unknown task');
  return execEnvelope();
});
app.get('/v1/execution/:taskId/stream', (req, reply) => {
  const { taskId } = req.params as { taskId: string };
  if (taskId !== 'mission-demo') return reply.code(404).send('unknown task');
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  const write = (kind: string) =>
    reply.raw.write(`event: ${kind}\ndata: ${JSON.stringify(execEnvelope())}\n\n`);
  write('snapshot');
  const push = () => write('projection');
  execSubscribers.add(push);
  req.raw.on('close', () => execSubscribers.delete(push));
});

let baseUrl = '';
let upstream: WorkspaceUpstream & { close(): void };

beforeAll(async () => {
  await app.listen({ port: 0, host: '127.0.0.1' });
  const address = app.server.address();
  baseUrl = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
  upstream = await createRhizomeUpstream(baseUrl);
});
afterAll(async () => {
  upstream.close();
  await app.close();
});

describe('Rhizome upstream adapter', () => {
  it('refuses to start without a reachable Rhizome', async () => {
    await expect(createRhizomeUpstream('http://127.0.0.1:1')).rejects.toThrow();
  });

  it('exposes the composition-root factory, honouring the configured url', async () => {
    const u = await createUpstream({ url: baseUrl });
    // RHZ-072 (FR-RHZ-101): re-packed to contract 0.2.0 (node detail surface); 0.x is minor-strict.
    // RHZ-094 (FR-RHZ-121): re-packed to contract 0.3.0 (execution surface on the wire); 0.2.x is rejected.
    expect(contractVersion).toBe('0.3.0');
    expect(u.snapshot().revision).toBeGreaterThanOrEqual(3);
    u.close?.();
  });

  it('serves the initial GET snapshot adapted to the cockpit shape', () => {
    const snap = upstream.snapshot();
    expect(snap.revision).toBeGreaterThanOrEqual(3);
    const body = snap.body as { tasks: Array<{ id: string; kind: string }>; capabilities: object };
    expect(body.tasks[0]).toMatchObject({ id: 'mission-demo', kind: 'task' });
    expect(body.capabilities).toHaveProperty('mission-demo');
  });

  it('adapts intents to {kind, actor, …} and PascalCase results to {accepted, reason}', async () => {
    const accepted = await upstream.relayIntent(
      { nodeId: '~workspace', action: 'mission.create', decision: { text: 'demo' }, idempotencyKey: 'k-0' },
      'fake-actor:test',
    );
    expect(accepted).toEqual({ accepted: true, reason: undefined });
    expect(received.at(-1)).toEqual({ kind: 'mission.create', name: 'demo', prompt: 'demo', actor: 'fake-actor:test' });
    // The contract Intent maps onto Rhizome's kind + typed id; the idempotency key is not sent.
    await upstream.relayIntent({ nodeId: 'mission-demo', action: 'task.resume', idempotencyKey: 'k-1' }, 'a');
    expect(received.at(-1)).toEqual({ kind: 'task.resume', taskId: 'mission-demo', actor: 'a' });
  });

  it('session.* intents come back as normal upstream rejections', async () => {
    const res = await upstream.relayIntent({ intent: 'session.pause', sessionId: 's-1' }, 'a');
    expect(res).toEqual({ accepted: false, reason: 'JANUS T17-19 표면 의존' });
  });

  it('SSE projections reach subscribers after accepted intents', async () => {
    const seen: number[] = [];
    const unsubscribe = upstream.subscribe((e) => seen.push(e.revision));
    await upstream.relayIntent({ nodeId: 'mission-demo', action: 'task.resume', idempotencyKey: 'k-sse' }, 'a');
    await new Promise((r) => setTimeout(r, 150));
    expect(seen.length).toBeGreaterThan(0);
    expect(Math.max(...seen)).toBe(revision);
    unsubscribe();
  });

  it('execution v1: SSE snapshot converges the cache; diffs emit session deltas once', async () => {
    const deltas: Array<{ type: string }> = [];
    const unsubscribe = upstream.subscribeExecution('mission-demo', (d) =>
      deltas.push(d as { type: string }),
    );
    await new Promise((r) => setTimeout(r, 200));
    const snap = upstream.executionSnapshot('mission-demo') as { sessions: WireSession[] };
    expect(snap.sessions).toHaveLength(1);
    expect(snap.sessions[0]).toMatchObject({ id: 'exec-1', state: 'running' });
    // First snapshot produced exactly one session delta (empty→one session).
    expect(deltas.filter((d) => d.type === 'session')).toHaveLength(1);

    // Identical re-broadcast → no new deltas (pure dedup).
    for (const push of execSubscribers) push();
    await new Promise((r) => setTimeout(r, 100));
    expect(deltas.filter((d) => d.type === 'session')).toHaveLength(1);

    // A real state change flows through as one delta.
    execSessions = [{ id: 'exec-1', taskId: 'mission-demo', state: 'ended', label: 'done' }];
    execRevision++;
    for (const push of execSubscribers) push();
    await new Promise((r) => setTimeout(r, 100));
    const sessionDeltas = deltas.filter((d) => d.type === 'session');
    expect(sessionDeltas).toHaveLength(2);
    expect((sessionDeltas[1] as unknown as { session: WireSession }).session.state).toBe('ended');
    unsubscribe();
  });

  it('unknown taskId (404) stays an empty projection without retry storms', async () => {
    const snap = upstream.executionSnapshot('no-such-task');
    expect(snap).toEqual({ taskId: 'no-such-task', sessions: [], events: [] });
    await new Promise((r) => setTimeout(r, 150));
    expect(upstream.executionSnapshot('no-such-task')).toEqual({
      taskId: 'no-such-task',
      sessions: [],
      events: [],
    });
  });

  it('terminal and stream are reported as unsupported, not as empty', () => {
    expect(upstream.terminalSnapshot('s-x')).toMatchObject({ unsupported: expect.stringContaining('501') });
    expect(upstream.subscribeTerminal('s-x', () => undefined)).toMatchObject({ unsupported: expect.any(String) });
    expect(upstream.subscribeStream('s-x', 'pty', null, () => undefined)).toMatchObject({ unsupported: expect.any(String) });
  });

  it('session stand-ins: fields cannot overwrite kind or actor, and unknown fields are not sent', async () => {
    await upstream.relayIntent(
      { intent: 'session.pause', sessionId: 's-1', kind: 'mission.delete', actor: 'mallory', smuggled: 'x' },
      'fake-actor:test',
    );
    expect(received.at(-1)).toEqual({ kind: 'session.pause', sessionId: 's-1', actor: 'fake-actor:test' });
  });

  it('refuses the shared refused-shape fixture (old wire, extra fields) before anything is sent', async () => {
    const before = received.length;
    for (const [label, intent] of refusedIntentShapes('mission-demo', 'task.resume')) {
      const r = await upstream.relayIntent(intent, 'a');
      expect(r.accepted, label).toBe(false);
    }
    expect(received.length).toBe(before);
  });

  it('the human text in decision.text reaches Rhizome under its own field names', async () => {
    await upstream.relayIntent({ nodeId: '~workspace', action: 'mission.create', decision: { text: 'Docs' }, idempotencyKey: 'k' }, 'a');
    expect(received.at(-1)).toEqual({ kind: 'mission.create', name: 'Docs', prompt: 'Docs', actor: 'a' });
    await upstream.relayIntent({ nodeId: 'mission-demo', action: 'task.instruct', decision: { text: 'Cite sources' }, idempotencyKey: 'k' }, 'a');
    expect(received.at(-1)).toMatchObject({ kind: 'task.instruct', instruction: 'Cite sources' });
  });

  it('contract intents are checked against the adapter\'s own nodes before anything reaches Rhizome', async () => {
    const before = received.length;
    const refused = [
      await upstream.relayIntent({ nodeId: 't-1', action: 'task.resume', idempotencyKey: 'k' }, 'a'),
      // Rhizome rewires by superseding an existing edgeId; no node declares edge.rewire.
      await upstream.relayIntent({ nodeId: 'mission-demo', action: 'edge.rewire', decision: { option: 'x' }, idempotencyKey: 'k' }, 'a'),
      await upstream.relayIntent({ nodeId: 'mission-demo', action: 'task.instruct', idempotencyKey: 'k' }, 'a'),
      await upstream.relayIntent({ nodeId: '~workspace', action: 'mission.create', decision: { text: 'x' }, prompt: 'p', idempotencyKey: 'k' }, 'a'),
    ];
    for (const r of refused) {
      expect(r.accepted).toBe(false);
      expect(typeof r.reason).toBe('string');
    }
    expect(received.length).toBe(before);
  });

  it('HTTP errors return the upstream reason verbatim when it sent one', async () => {
    expect(await upstream.relayIntent({ intent: 'session.kill', sessionId: 'fail.json' }, 'a')).toEqual({
      accepted: false,
      reason: 'upstream says no: mission locked',
    });
    expect(await upstream.relayIntent({ intent: 'session.kill', sessionId: 'fail.text' }, 'a')).toEqual({ accepted: false, reason: 'plain upstream failure' });
    expect((await upstream.relayIntent({ intent: 'session.kill', sessionId: 'fail.empty' }, 'a')).reason).toContain('503');
  });
});

describe('Rhizome addressing', () => {
  it('maps nodeId + action onto kind + typed id, rewire onto from/to, text onto Rhizome field names; the key is dropped', () => {
    const address = (i: Record<string, unknown>) => {
      const { kind, rest } = toRhizomeAddress(i);
      return { kind, ...toRhizomeFields(kind, rest) };
    };
    expect(address({ nodeId: 't-1', action: 'edge.rewire', decision: { option: 't-2' }, idempotencyKey: 'k' })).toEqual({
      kind: 'edge.rewire', from: 't-1', to: 't-2', edgeKind: 'dependency',
    });
    expect(address({ nodeId: 'g-1', action: 'gate.requestChanges', decision: { text: 'Shorter' }, idempotencyKey: 'k' })).toEqual({
      kind: 'gate.requestChanges', gateId: 'g-1', instruction: 'Shorter',
    });
    expect(address({ nodeId: 'g-1', action: 'gate.approve', idempotencyKey: 'k' })).toEqual({ kind: 'gate.approve', gateId: 'g-1' });
  });
});

describe('SSE framing', () => {
  it('handles CRLF and CR line ends, split CRLF across chunks, and multi-line data', () => {
    const d = new SseDecoder();
    const frames = [
      ...d.push('event: snapshot\r\ndata: {"a":\r'),
      ...d.push('\ndata: 1}\r\n\r\n: comment\n'),
      ...d.push('event:projection\rdata:x\r\r'),
      // The trailing CR is held until the next chunk shows it was not half of a CRLF.
      ...d.push('data: next'),
    ];
    expect(frames).toEqual([
      { event: 'snapshot', data: '{"a":\n1}' },
      { event: 'projection', data: 'x' },
    ]);
  });

  it('keeps an incomplete frame until its blank line arrives', () => {
    const d = new SseDecoder();
    expect(d.push('data: part')).toEqual([]);
    expect(d.push('ial\n')).toEqual([]);
    expect(d.push('\n')).toEqual([{ event: 'message', data: 'partial' }]);
  });
});
