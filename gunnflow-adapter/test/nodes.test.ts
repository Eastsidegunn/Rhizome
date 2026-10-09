// Rhizome → contract NodeProjection[]: well-formed nodes, a synthesized root
// carrying mission creation, vocabulary passed through, and the adapter
// passing the contract's conformance suite against a mock Rhizome.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { CONTRACT_VERSION, WORKSPACE_ROOT_KIND, nodeProblem } from '@gunnflow/contract';
import { defineConformanceSuite } from '@gunnflow/contract/conformance';
import { coveredActions, validateWiringConfig, type WiringConfig } from '@gunnflow/contract/wiring';
import { adaptWithReport, contractVersion, createRhizomeUpstream, detailOf, executionForWire, nodesOf } from '../src/index.js';

/** A wire body as Rhizome's workspace/http.go emits it (empty collections serialize as null). */
const wireBody = {
  missions: [{ id: 'goal-demo', name: 'demo', attention: false }],
  tasks: [
    { id: 'mission-demo', missionId: 'goal-demo', name: 'demo', state: 'running', hasProgress: false, attention: false },
    { id: 'mission-x', missionId: 'goal-demo', name: 'x', state: 'blocked', hasProgress: true, progress: 0.4, attention: true },
    { id: 'mission-y', missionId: 'goal-demo', name: 'y', state: 'paused', hasProgress: false, attention: true },
  ],
  gates: [
    { id: 'approval-1', state: 'pending', superseded: false },
    { id: 'approval-0', state: 'approved', superseded: true },
  ],
  // missionId is a mission id = a task id in /v1/workspace (a task IS its mission);
  // the goal id would be dropped by the live filter.
  deliverables: [{ id: 'd-1', kind: 'document', missionId: 'mission-demo', sourceRef: 'blob:abc', summary: 'Report v1' }],
  edges: [{ id: 'e-1', from: { type: 'mission', id: 'mission-demo' }, to: { type: 'deliverable', id: 'd-1' }, edgeKind: 'produces' }],
  counts: { running: 1, needsYou: 0, blocked: 1 },
  attention: [
    { kind: 'waiting_for_human', refId: 'mission-x', cause: 'mission waiting' },
    { kind: 'waiting_for_human', refId: 'nowhere', cause: 'dangling' },
  ],
  sessions: null,
};

const adapted = adaptWithReport({ revision: 7, body: wireBody });
const nodes = nodesOf(adapted.envelope);
const report = adapted.report;
const byId = (id: string) => nodes.find((n) => n.id === id)!;

describe('Rhizome generic nodes', () => {
  it('every node passes the contract node check; ids are unique', () => {
    for (const n of nodes) expect(nodeProblem(n), n.id).toBeNull();
    expect(new Set(nodes.map((n) => n.id)).size).toBe(nodes.length);
  });

  it('synthesizes exactly one workspace root, under an id Rhizome ids cannot take', () => {
    const roots = nodes.filter((n) => n.kind === WORKSPACE_ROOT_KIND);
    expect(roots).toHaveLength(1);
    expect(roots[0]).toMatchObject({ id: '~workspace', label: 'Workspace', state: { value: 'unstated' } });
    // Even a colliding upstream id does not displace or merge with the root. The
    // colliding node must survive the live filter to collide, so it is a live task.
    const colliding = adaptWithReport({
      revision: 1,
      body: { ...wireBody, tasks: [...wireBody.tasks, { id: '~workspace', missionId: 'goal-demo', name: 'odd', state: 'running', hasProgress: false, attention: false }] },
    });
    const ids = nodesOf(colliding.envelope).map((n) => n.id);
    expect(new Set(ids).size).toBe(ids.length);
    const root = nodesOf(colliding.envelope).find((n) => n.kind === WORKSPACE_ROOT_KIND)!;
    expect(root.id).toBe('~~workspace');
    expect(nodesOf(colliding.envelope).find((n) => n.id === '~workspace')!.kind).toBe('task');
  });

  it('root mission.create mirrors the documented /v1/intent acceptance surface (enabled, name text required)', () => {
    const root = nodes.find((n) => n.kind === WORKSPACE_ROOT_KIND)!;
    expect(root.capabilities).toEqual([{ action: 'mission.create', level: 'enabled', decision: { input: { required: true } } }]);
  });

  it('tasks: name as label, state verbatim, membership + edges as relations, mirrored actions', () => {
    const t = byId('mission-demo');
    expect(t).toMatchObject({ kind: 'task', label: 'demo', state: { value: 'running' } });
    expect(t.relations).toEqual([{ type: 'member-of', target: 'goal-demo' }, { type: 'produces', target: 'd-1' }]);
    expect(t.capabilities).toEqual([
      { action: 'task.pause', level: 'enabled' },
      { action: 'task.resume', level: 'enabled' },
      { action: 'task.instruct', level: 'enabled', decision: { input: { required: true } } },
    ]);
    // Rewire needs an existing edgeId in Rhizome; nothing declares it.
    expect(nodes.some((n) => n.capabilities.some((c) => c.action === 'edge.rewire'))).toBe(false);
  });

  it('attention: list entries and the object\'s own flag are both projected; no since', () => {
    // mission-x carries a list entry AND its flag: the list does not replace the flag.
    expect(byId('mission-x').attention).toEqual([{ cause: 'waiting_for_human' }, { cause: 'flagged' }]);
    expect(byId('mission-y').attention).toEqual([{ cause: 'flagged' }]);
    expect(byId('mission-demo').attention).toEqual([]);
  });

  it('invalid attention entries are not projected but counted in the integration report', () => {
    expect(nodes.some((n) => n.id === 'nowhere')).toBe(false);
    expect(report).toEqual({ attentionDropped: 1 });
    const messy = adaptWithReport({
      revision: 1,
      body: { ...wireBody, attention: [null, 'x', { refId: 'mission-demo' }, { kind: '', refId: 'mission-demo' }, { kind: 'k', refId: 7 }, { kind: 'k', refId: 'mission-demo' }] },
    });
    expect(messy.report).toEqual({ attentionDropped: 5 });
    expect(nodesOf(messy.envelope).find((n) => n.id === 'mission-demo')!.attention).toEqual([{ cause: 'k' }]);
  });

  it('pending question and JANUS approval gates require reject and requestChanges input', () => {
    const gateBody = {
      ...wireBody,
      gates: [
        { id: 'question-1', source: 'internal', state: 'pending', superseded: false },
        { id: 'approval-1', source: 'janus', state: 'pending', superseded: false },
        { id: 'decided-1', source: 'janus', state: 'rejected', superseded: false },
      ],
      gateCapabilities: {
        'question-1': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
        'approval-1': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
        'decided-1': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
      },
    };
    const gateNodes = nodesOf(adaptWithReport({ revision: 8, body: gateBody }).envelope);
    const gateById = (id: string) => gateNodes.find((n) => n.id === id)!;
    const expected = [
      { action: 'gate.approve', level: 'enabled' },
      { action: 'gate.reject', level: 'enabled', decision: { input: { required: true } } },
      { action: 'gate.requestChanges', level: 'enabled', decision: { input: { required: true } } },
    ];
    expect(gateById('question-1').capabilities).toEqual(expected);
    expect(gateById('approval-1').capabilities).toEqual(expected);
    expect(gateById('decided-1').capabilities).toEqual([]);
  });

  it('gates carry no invented label or actions; missions and deliverables have no state of their own', () => {
    const g = byId('approval-1');
    expect(g.label).toBeUndefined();
    expect(g.state.value).toBe('waiting');
    expect(g.capabilities).toEqual([]);
    // approval-0 (superseded) is dropped by the live filter — not surfaced.
    expect(nodes.find((n) => n.id === 'approval-0')).toBeUndefined();
    // RHZ-076 (FR-RHZ-104): goal-demo has no incoming contains edge → top-level kind "goal" (was "mission").
    expect(byId('goal-demo')).toMatchObject({ kind: 'goal', label: 'demo', state: { value: 'unstated' } });
    expect(byId('d-1')).toMatchObject({ kind: 'deliverable', label: 'Report v1', state: { value: 'unstated' } });
  });
});

describe('Rhizome wiring config draft', () => {
  const draft = JSON.parse(readFileSync(join(import.meta.dirname, '..', 'wiring', 'rhizome.wiring.draft.json'), 'utf8')) as WiringConfig;

  it('passes the contract wiring validator', () => {
    expect(validateWiringConfig(draft)).toEqual({ ok: true, config: draft });
  });

  it('covers the vocabulary the adapter emits: every state and relation type, every declared action', () => {
    for (const n of nodes) {
      expect(draft.render?.[n.state.value], `state ${n.state.value}`).toBeDefined();
      for (const r of n.relations) expect(draft.relations?.[r.type], `relation ${r.type}`).toBeDefined();
      const covered = coveredActions(draft, n.kind);
      for (const c of n.capabilities) expect(covered.includes(c.action), `${n.kind}: ${c.action}`).toBe(true);
    }
  });
});

/* ---- conformance against a mock Rhizome that accepts everything: refusals must come from the adapter ---- */
const mock = Fastify({ logger: false });
mock.get('/v1/workspace', async () => ({ revision: 7, body: wireBody }));
mock.get('/v1/workspace/stream', (_req, reply) => {
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 7, body: wireBody })}\n\n`);
});
mock.post('/v1/intent', async () => ({ Accepted: true, Reason: '' }));
// RHZ-094 (FR-RHZ-121): one task with an execution, so the 0.3.x conformance
// check of the execution surface has a served snapshot (all-404 fails it).
const execEnvelope = {
  revision: 7,
  body: {
    taskId: 'mission-demo',
    sessions: [{ id: 'exec-1', taskId: 'mission-demo', state: 'running', label: 'demo run', janusState: 'running', usageInTotal: 3, usageOutTotal: 4, lastActivityTs: 1_700_000_000_000 }],
    events: [{ sessionId: 'exec-1', seq: 1, kind: 'subagent/spawn', actor: 'janus', ts: 1_700_000_000_000, usageIn: 3, usageOut: 4 }],
  },
};
mock.get('/v1/execution/:taskId', async (req, reply) => {
  const { taskId } = req.params as { taskId: string };
  if (taskId !== 'mission-demo') return reply.code(404).send('unknown task');
  return execEnvelope;
});
mock.get('/v1/execution/:taskId/stream', (req, reply) => {
  const { taskId } = req.params as { taskId: string };
  if (taskId !== 'mission-demo') return reply.code(404).send('unknown task');
  reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
  reply.raw.write(`event: snapshot\ndata: ${JSON.stringify(execEnvelope)}\n\n`);
});
let upstream: Awaited<ReturnType<typeof createRhizomeUpstream>>;
beforeAll(async () => {
  await mock.listen({ port: 0, host: '127.0.0.1' });
  const address = mock.server.address();
  upstream = await createRhizomeUpstream(`http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`);
});
afterAll(async () => {
  upstream.close();
  await mock.close();
});

it('the live adapter exposes the same integration report', () => {
  expect(upstream.integrationReport()).toEqual({ attentionDropped: 1 });
});

defineConformanceSuite('Rhizome adapter (mock Rhizome)', () => ({
  contractVersion,
  nodes: () => nodesOf(upstream.snapshot()),
  relay: (intent) => upstream.relayIntent(intent, 'conformance'),
  // RHZ-072 (FR-RHZ-101) C1: the detail surface, exactly as GET /detail/:nodeId serves it.
  detail: (nodeId) => {
    const node = nodesOf(upstream.snapshot()).find((n) => n.id === nodeId);
    return node ? detailOf(upstream.rawSnapshot(), nodeId, node.kind === WORKSPACE_ROOT_KIND ? node.id : undefined) : undefined;
  },
  // RHZ-094 (FR-RHZ-121): the execution surface, exactly as GET /execution/:taskId serves it
  // (Rhizome 404 → undefined; otherwise the projected body behind the same validate guard).
  execution: async (taskId) => {
    const body = await upstream.executionWire(taskId);
    if (body === undefined) return undefined;
    const checked = executionForWire(body, taskId);
    if (!checked.ok) throw new Error(checked.reason);
    return checked.snapshot;
  },
}));

it('claims the contract version it is checked against', () => {
  expect(contractVersion).toBe(CONTRACT_VERSION);
});
