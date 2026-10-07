// RHZ-072 (FR-RHZ-101) H1: the direct wire's GET /detail/:nodeId route against
// a mock Rhizome. 200 bodies hold the contract shape and the snapshot's
// revision; 404 for no-detail/unknown nodes; the other wire paths are unchanged.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { DIRECT_WIRE, validateNodeDetail } from '@gunnflow/contract';
import { WORKSPACE_NODE_ID } from '../src/nodes.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';

const BODY = '# Publish?\n\nRelease **v3**.';
const wireBody = {
  missions: [{ id: 'goal-a', name: 'ship v3', attention: false, state: 'active', success: 'v3 live' }],
  tasks: [
    { id: 'm-build', missionId: 'goal-a', name: 'build', state: 'running', currentAction: 'compiling', progress: 0.5, hasProgress: true, attention: false },
    { id: 'm-done', missionId: 'goal-a', name: 'done', state: 'completed', currentAction: 'finished', hasProgress: false, attention: false },
  ],
  gates: [
    { id: 'q-pending', state: 'pending', superseded: false, name: 'Publish?', requestDigest: 'sha256:abc', body: BODY, recommendation: 'approve' },
    { id: 'q-bare', state: 'pending', superseded: false },
    { id: 'q-approved', state: 'approved', superseded: false, body: BODY, decisionReason: 'ok', decidedBy: 'op' },
  ],
  deliverables: [{ id: 'd-1', kind: 'document', missionId: 'm-build', sourceRef: 'sha256:deadbeef', summary: 'Report' }],
  edges: [],
  counts: { running: 1, needsYou: 1, blocked: 0 },
  attention: [],
};
const REVISION = 9;

describe('GET /detail/:nodeId on the direct wire (FRRHZ101 H1)', () => {
  const mock = Fastify({ logger: false });
  let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;
  let workspaceFetches = 0;

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => {
      workspaceFetches += 1;
      return { revision: REVISION, body: wireBody };
    });
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: REVISION, body: wireBody })}\n\n`);
    });
    mock.get('/v1/blob/:id', async (_req, reply) => reply.code(404).send({ reason: 'none' }));
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const address = mock.server.address();
    const port = typeof address === 'object' && address ? address.port : 0;
    server = await startRhizomeDirectServer({ rhizomeUrl: `http://127.0.0.1:${port}`, port: 0 });
  });
  afterAll(async () => {
    await server.close();
    await mock.close();
  });

  const get = (path: string) => fetch(`${server.url}${path}`);

  it('a pending gate answers 200 with a contract-valid NodeDetail at the snapshot revision', async () => {
    const res = await get(`${DIRECT_WIRE.detail}/q-pending`);
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('application/json');
    const body = (await res.json()) as unknown;
    const r = validateNodeDetail(body);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
    const nodes = (await (await get(DIRECT_WIRE.nodes)).json()) as { revision: number };
    expect(r.ok && r.detail.revision).toBe(nodes.revision);
    expect(r.ok && r.detail.revision).toBe(REVISION);
    expect(r.ok && r.detail.items).toEqual([
      { label: 'request', text: BODY },
      { label: 'recommendation', text: 'approve' },
      { label: 'digest', text: 'sha256:abc' },
    ]);
  });

  it('the workspace root and a task answer 200 from the same snapshot, without a further Rhizome fetch', async () => {
    const before = workspaceFetches;
    const root = (await (await get(`${DIRECT_WIRE.detail}/${encodeURIComponent(WORKSPACE_NODE_ID)}`)).json()) as { revision: number; items: unknown[] };
    expect(validateNodeDetail(root).ok).toBe(true);
    expect(root.items).toEqual([
      { label: 'running', text: '1' },
      { label: 'needsYou', text: '1' },
      { label: 'blocked', text: '0' },
    ]);
    const task = (await (await get(`${DIRECT_WIRE.detail}/m-build`)).json()) as { revision: number; items: unknown[] };
    expect(validateNodeDetail(task).ok).toBe(true);
    expect(task.items).toEqual([
      { label: 'currentAction', text: 'compiling' },
      { label: 'progress', text: '0.5' },
      { label: 'state', text: 'running' },
    ]);
    const mission = (await (await get(`${DIRECT_WIRE.detail}/goal-a`)).json()) as { items: unknown[] };
    expect(mission.items).toEqual([
      { label: 'success', text: 'v3 live' },
    ]);
    expect(workspaceFetches).toBe(before);
  });

  it('404 for a deliverable, a gate with nothing to say, nodes off the live wire, and unknown ids — never an empty 200', async () => {
    expect((await get(`${DIRECT_WIRE.detail}/d-1`)).status).toBe(404);
    expect((await get(`${DIRECT_WIRE.detail}/q-bare`)).status).toBe(404);
    // Decided gates and terminal tasks are not on the wire (cockpit = in progress) → no such node.
    expect((await get(`${DIRECT_WIRE.detail}/q-approved`)).status).toBe(404);
    expect((await get(`${DIRECT_WIRE.detail}/m-done`)).status).toBe(404);
    expect((await get(`${DIRECT_WIRE.detail}/does-not-exist`)).status).toBe(404);
    // No /detail body ever carries an empty items list.
    for (const id of ['d-1', 'q-bare', 'does-not-exist']) {
      const res = await get(`${DIRECT_WIRE.detail}/${id}`);
      const body = (await res.json()) as { items?: unknown[] };
      expect(body.items).toBeUndefined();
    }
  });

  it('the other wire paths are unchanged: /nodes serves, /detail without id and non-GET are off the wire', async () => {
    const nodes = (await (await get(DIRECT_WIRE.nodes)).json()) as { revision: number; nodes: Array<{ id: string }> };
    expect(nodes.revision).toBe(REVISION);
    expect(nodes.nodes.map((n) => n.id)).toContain('q-pending');
    expect((await get('/detail')).status).toBe(404);
    expect((await get('/detail/')).status).toBe(404);
    expect((await fetch(`${server.url}${DIRECT_WIRE.detail}/q-pending`, { method: 'POST' })).status).toBe(404);
    expect((await get('/artifact/d-1/deadbeef')).status).toBe(404);
  });
});
