// RHZ-141 (FR-RHZ-185): the direct wire's terminal routes (Gunnflow GF-T2)
// against a fake Rhizome /v1/terminals — list, open, stream relay, input,
// resize, close, 404/409/422/501 — with output and input bytes (control
// characters, non-ASCII, invalid UTF-8) unchanged end to end; and every
// capability action the adapter emits carries a label in the wiring draft.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import Fastify, { type FastifyReply } from 'fastify';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import type { WiringConfig } from '@gunnflow/contract/wiring';
import { startRhizomeDirectServer } from '../src/serveDirect.js';
import { adaptWithReport, nodesOf, SseDecoder } from '../src/upstream.js';

const SNAPSHOT = Buffer.from([0x1b, 0x5b, 0x33, 0x31, 0x6d, 0x00, 0x07, 0x0d, 0x0a, ...Buffer.from('한글 ✓ '), 0xff, 0xfe, 0x1b, 0x5b, 0x30, 0x6d]);
const OUTPUT = Buffer.from([0x08, 0x7f, 0x1b, 0x5d, 0x30, 0x3b, ...Buffer.from('제목'), 0x07, 0xc3, 0x28, 0x0a]);
const INPUT = Buffer.from([0x03, 0x1b, 0x5b, 0x41, ...Buffer.from('ls ü→'), 0x0d, 0x80]);
const b64 = (b: Buffer) => b.toString('base64');

const workspaceBody = { missions: [], tasks: [], gates: [], deliverables: [], edges: [], counts: { running: 0, needsYou: 0, blocked: 0 }, attention: [], capabilities: {}, gateCapabilities: {} };

describe('terminal routes on the direct wire FR-RHZ-185', () => {
  const mock = Fastify({ logger: false });
  let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;
  let disabled = false;
  let live: Set<string>;
  const calls: Array<{ method: string; url: string; body?: unknown }> = [];

  beforeEach(() => {
    disabled = false;
    live = new Set(['t-live', 't-del', 't-sess']);
    calls.length = 0;
  });

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => ({ revision: 1, body: workspaceBody }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 1, body: workspaceBody })}\n\n`);
    });
    mock.addHook('onRequest', async (req, reply) => {
      if (!req.url.startsWith('/v1/terminals')) return;
      calls.push({ method: req.method, url: req.url });
      if (disabled) return reply.code(501).type('text/plain').send('terminal bridge requires a loopback bind\n');
    });
    mock.addHook('preHandler', async (req) => {
      if (req.url.startsWith('/v1/terminals') && req.body !== undefined) calls[calls.length - 1]!.body = req.body;
    });
    mock.get('/v1/terminals', async () => ({
      sessionTargets: true,
      sessions: [
        { sessionId: 's-1', name: 'build session', missionId: 'm-build', mode: 'interactive', cwd: '/tmp' },
        { sessionId: 's-2', name: '', mode: 'interactive', cwd: '/tmp' },
      ],
      terminals: [
        { terminalId: 't-sess', kind: 'session', sessionId: 's-1', cwd: '/tmp', cols: 80, rows: 24, subscribers: 1, openedAt: 1 },
        { terminalId: 't-shell', kind: 'shell', cwd: '/Users/me', cols: 80, rows: 24, subscribers: 0, openedAt: 2 },
      ],
    }));
    mock.post('/v1/terminals', async (req, reply) => {
      const b = req.body as { target: { session?: { sessionId: string } } };
      if (b.target.session?.sessionId === 's-gone') return reply.code(404).type('text/plain').send('session is not open\n');
      return reply.code(201).send({ terminalId: 't-new' });
    });
    const known = (id: string, reply: FastifyReply) =>
      live.has(id) ? undefined : reply.code(404).type('text/plain').send('404 page not found\n');
    mock.get('/v1/terminals/:id/stream', (req, reply) => {
      const id = (req.params as { id: string }).id;
      if (known(id, reply)) return reply;
      reply.hijack();
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ data: b64(SNAPSHOT) })}\n\n`);
      // split one event across writes: the relay must reassemble it
      const out = `event: output\ndata: ${JSON.stringify({ data: b64(OUTPUT) })}\n\n`;
      reply.raw.write(out.slice(0, 9));
      reply.raw.write(out.slice(9));
      live.delete(id);
      reply.raw.end(`event: exit\ndata: ${JSON.stringify({ exitCode: 130 })}\n\n`);
    });
    for (const op of ['input', 'resize']) {
      mock.post(`/v1/terminals/:id/${op}`, async (req, reply) => known((req.params as { id: string }).id, reply) ?? reply.code(204).send());
    }
    mock.delete('/v1/terminals/:id', async (req, reply) => {
      const id = (req.params as { id: string }).id;
      if (known(id, reply)) return reply;
      live.delete(id);
      return { terminalId: id, exited: true, exitCode: 129 };
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const address = mock.server.address();
    const port = typeof address === 'object' && address ? address.port : 0;
    server = await startRhizomeDirectServer({ rhizomeUrl: `http://127.0.0.1:${port}`, port: 0 });
  });
  afterAll(async () => {
    await server.close();
    await mock.close();
  });

  const call = (method: string, path: string, body?: unknown) =>
    fetch(`${server.url}${path}`, { method, ...(body === undefined ? {} : { headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) }) });
  const events = async (res: Response) => {
    const decoder = new SseDecoder();
    return decoder.push(await res.text()).map((e) => ({ event: e.event, data: JSON.parse(e.data) as Record<string, unknown> }));
  };

  it('FR-RHZ-185 GET /terminals lists session targets as attachable (session id, name, mission node) and the open terminals', async () => {
    const res = await call('GET', '/terminals');
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual([
      { terminalId: 's-1', label: 'build session', kind: 'attachable', state: 'open', nodeId: 'm-build' },
      { terminalId: 's-2', label: 's-2', kind: 'attachable', state: 'open' },
      { terminalId: 't-sess', label: 'build session', kind: 'attachable', state: 'running', nodeId: 'm-build' },
      { terminalId: 't-shell', label: '/Users/me', kind: 'shell', state: 'running' },
    ]);
  });

  it('FR-RHZ-185 POST /terminals translates attach/shell targets to Rhizome and answers {terminalId}', async () => {
    const attach = await call('POST', '/terminals', { target: { attach: 's-1' }, cols: 120, rows: 40 });
    expect(attach.status).toBe(200);
    expect(await attach.json()).toEqual({ terminalId: 't-new' });
    const shell = await call('POST', '/terminals', { target: { shell: { cwd: '/tmp' } }, cols: 80, rows: 24 });
    expect(await shell.json()).toEqual({ terminalId: 't-new' });
    expect(calls.map((c) => c.body)).toEqual([
      { target: { session: { sessionId: 's-1' } }, cols: 120, rows: 40 },
      { target: { shell: { cwd: '/tmp' } }, cols: 80, rows: 24 },
    ]);
  });

  it('FR-RHZ-185 POST /terminals refusals are 422 {reason}: Rhizome refusal relayed, malformed target never sent', async () => {
    const gone = await call('POST', '/terminals', { target: { attach: 's-gone' }, cols: 80, rows: 24 });
    expect(gone.status).toBe(422);
    expect(await gone.json()).toEqual({ reason: 'session is not open' });
    for (const bad of [{ target: {}, cols: 80, rows: 24 }, { target: { attach: 's-1', shell: { cwd: '/' } }, cols: 80, rows: 24 }, { target: { attach: 's-1' } }]) {
      const res = await call('POST', '/terminals', bad);
      expect(res.status).toBe(422);
      expect(((await res.json()) as { reason: string }).reason).toBeTruthy();
    }
    expect(calls).toHaveLength(1);
  });

  it('FR-RHZ-185 stream relays snapshot·data·exit{code} with the base64 bytes unchanged, then input to the ended terminal is 409', async () => {
    const res = await call('GET', '/terminals/t-live/stream');
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('text/event-stream');
    const got = await events(res);
    expect(got).toEqual([
      { event: 'snapshot', data: { data: b64(SNAPSHOT) } },
      { event: 'data', data: { data: b64(OUTPUT) } },
      { event: 'exit', data: { code: 130 } },
    ]);
    expect(Buffer.from(got[0]!.data.data as string, 'base64').equals(SNAPSHOT)).toBe(true);
    expect(Buffer.from(got[1]!.data.data as string, 'base64').equals(OUTPUT)).toBe(true);
    const after = await call('POST', '/terminals/t-live/input', { data: b64(INPUT) });
    expect(after.status).toBe(409);
  });

  it('FR-RHZ-185 input and resize relay as is → 204', async () => {
    const input = await call('POST', '/terminals/t-sess/input', { data: b64(INPUT) });
    expect(input.status).toBe(204);
    const resize = await call('POST', '/terminals/t-sess/resize', { cols: 100, rows: 30 });
    expect(resize.status).toBe(204);
    expect(calls.map((c) => [c.method, c.url, c.body])).toEqual([
      ['POST', '/v1/terminals/t-sess/input', { data: b64(INPUT) }],
      ['POST', '/v1/terminals/t-sess/resize', { cols: 100, rows: 30 }],
    ]);
    expect(Buffer.from((calls[0]!.body as { data: string }).data, 'base64').equals(INPUT)).toBe(true);
  });

  it('FR-RHZ-185 DELETE → 204; the closed terminal then answers 409, an unknown id 404', async () => {
    expect((await call('DELETE', '/terminals/t-del')).status).toBe(204);
    expect(calls.map((c) => [c.method, c.url])).toEqual([['DELETE', '/v1/terminals/t-del']]);
    expect((await call('POST', '/terminals/t-del/resize', { cols: 80, rows: 24 })).status).toBe(409);
    expect((await call('GET', '/terminals/t-del/stream')).status).toBe(409);
    expect((await call('DELETE', '/terminals/t-del')).status).toBe(409);
    for (const [method, path] of [
      ['GET', '/terminals/t-nope/stream'],
      ['POST', '/terminals/t-nope/input'],
      ['POST', '/terminals/t-nope/resize'],
      ['DELETE', '/terminals/t-nope'],
    ] as const) {
      expect((await call(method, path, method === 'POST' ? {} : undefined)).status, `${method} ${path}`).toBe(404);
    }
  });

  it("FR-RHZ-185 Rhizome's 501 (bridge off) passes through on every route", async () => {
    disabled = true;
    for (const [method, path, body] of [
      ['GET', '/terminals', undefined],
      ['POST', '/terminals', { target: { shell: { cwd: '/tmp' } }, cols: 80, rows: 24 }],
      ['GET', '/terminals/t-sess/stream', undefined],
      ['POST', '/terminals/t-sess/input', { data: '' }],
      ['POST', '/terminals/t-sess/resize', { cols: 80, rows: 24 }],
      ['DELETE', '/terminals/t-sess', undefined],
    ] as const) {
      const res = await call(method, path, body);
      expect(res.status, `${method} ${path}`).toBe(501);
      expect(await res.json()).toEqual({ reason: 'terminal bridge requires a loopback bind' });
    }
  });
});

describe('wiring draft labels every emitted action FR-RHZ-185', () => {
  const draft = JSON.parse(readFileSync(join(import.meta.dirname, '..', 'wiring', 'rhizome.wiring.draft.json'), 'utf8')) as WiringConfig;
  // Every capability source enabled: task, gate (internal, waiting → gate.ask),
  // request, blocked note, and the workspace root.
  const body = {
    missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
    tasks: [{ id: 'task', missionId: 'goal', name: 'task', state: 'running', hasProgress: false, attention: false }],
    gates: [{ id: 'q-1', state: 'pending', superseded: false, name: 'ship?', missionId: 'task', source: 'internal', requestDigest: 'rhz-question-v1:x' }],
    deliverables: [],
    edges: [],
    counts: { running: 1, needsYou: 3, blocked: 0 },
    attention: [{ kind: 'note_blocked', refId: 'note-a', cause: 'blocked', missionId: 'task' }],
    capabilities: { task: { pause: 'enabled', resume: 'disabled', instruct: 'enabled', cancel: 'enabled', forceReplan: 'enabled' } },
    gateCapabilities: { 'q-1': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' } },
    requests: [{ id: 'r-1', name: 'hand task', state: 'waiting', missionId: 'task', createdAt: '2026-10-10T01:02:03Z' }],
    requestCapabilities: { 'r-1': { complete: 'enabled', unable: 'enabled' } },
  };
  const emitted = [...new Set(nodesOf(adaptWithReport({ revision: 1, body }).envelope).flatMap((n) => n.capabilities.map((c) => c.action)))].sort();

  it('FR-RHZ-185 the fixture reaches the whole action vocabulary (the check below is not vacuous)', () => {
    expect(emitted).toEqual([
      'gate.approve', 'gate.ask', 'gate.reject', 'gate.requestChanges',
      'mission.create', 'note.answer',
      'request.complete', 'request.unable',
      'task.cancel', 'task.forceReplan', 'task.instruct', 'task.pause', 'task.resume',
    ]);
  });

  it('FR-RHZ-185 every emitted capability action has a non-empty label in the draft actions', () => {
    const missing = emitted.filter((a) => !(Object.hasOwn(draft.actions ?? {}, a) && draft.actions![a]!.label.trim()));
    expect(missing).toEqual([]);
  });
});
