// FR-RHZ-159: request node projection and the full retention matrix.
import Fastify from 'fastify';
import { describe, expect, it } from 'vitest';
import { DIRECT_WIRE, nodeProblem } from '@gunnflow/contract';
import { adaptWithReport, nodesOf } from '../src/upstream.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';

const base = {
  missions: [
    { id: 'goal-live', name: 'live', attention: false, state: 'active' },
    { id: 'goal-terminal', name: 'done', attention: false, state: 'achieved' },
  ],
  tasks: [
    { id: 'task-live', missionId: 'goal-live', name: 'live task', state: 'running', hasProgress: false, attention: false },
    { id: 'task-terminal', missionId: 'goal-live', name: 'done task', state: 'completed', hasProgress: false, attention: false },
  ],
  gates: [], deliverables: [], edges: [], attention: [],
  counts: { running: 1, needsYou: 2, blocked: 0 },
  capabilities: {}, gateCapabilities: {},
};

const body = {
  ...base,
  requests: [
    { id: 'r-wait-live', name: 'wait live', state: 'waiting', missionId: 'task-live', createdAt: '2026-10-08T01:02:03.4Z' },
    { id: 'r-close-live', name: 'closed live', state: 'done', goalId: 'goal-live', createdAt: '2026-10-08T01:02:04Z' },
    { id: 'r-wait-terminal', name: 'wait terminal', state: 'waiting', missionId: 'task-terminal', createdAt: '2026-10-08T01:02:05Z' },
    { id: 'r-close-terminal', name: 'closed terminal', state: 'unable', goalId: 'goal-terminal', createdAt: '2026-10-08T01:02:06Z', why: 'why', where: 'where', commands: [], after: 'after', reason: 'cannot', closedBy: 'operator', closedAt: '2026-10-08T02:00:00Z' },
  ],
  requestCapabilities: {
    'r-wait-live': { complete: 'enabled', unable: 'enabled' },
    'r-close-live': { complete: 'hidden', unable: 'hidden' },
    'r-wait-terminal': { complete: 'enabled', unable: 'enabled' },
    'r-close-terminal': { complete: 'hidden', unable: 'hidden' },
  },
};

describe('request nodes FR-RHZ-159', () => {
  const nodes = nodesOf(adaptWithReport({ revision: 9, body }).envelope);
  const request = (id: string) => nodes.find((n) => n.id === id);

  it('passes contract validation and emits state, membership, attention and capability shapes', () => {
    for (const node of nodes) expect(nodeProblem(node), node.id).toBeNull();
    expect(request('r-wait-live')).toMatchObject({
      kind: 'request', label: 'wait live', state: { value: 'waiting' }, artifacts: [],
      relations: [{ type: 'member-of', target: 'task-live' }],
      attention: [{ cause: 'needs_human_action', since: '2026-10-08T01:02:03.4Z' }],
      capabilities: [
        { action: 'request.complete', level: 'enabled', decision: { input: { required: false } } },
        { action: 'request.unable', level: 'enabled', decision: { input: { required: true } } },
      ],
    });
    expect(request('r-close-live')?.attention).toEqual([]);
  });

  it('pins waiting/closed × live/terminal target retention and serveDirect detail availability', async () => {
    expect(request('r-wait-live')?.relations).toEqual([{ type: 'member-of', target: 'task-live' }]);
    expect(request('r-close-live')?.relations).toEqual([{ type: 'member-of', target: 'goal-live' }]);
    expect(request('r-wait-terminal')?.relations).toEqual([]);
    expect(request('r-close-terminal')).toBeUndefined();

    const mock = Fastify({ logger: false });
    mock.get('/v1/workspace', async () => ({ revision: 9, body }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 9, body })}\n\n`);
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const address = mock.server.address();
    const rhizomeUrl = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
    const direct = await startRhizomeDirectServer({ rhizomeUrl, port: 0 });
    try {
      const response = await fetch(`${direct.url}${DIRECT_WIRE.detail}/r-close-terminal`);
      expect(response.status).toBe(404);
      expect(await response.json()).toEqual({ reason: 'no such node' });
    } finally {
      await direct.close();
      await mock.close();
    }
  });

  it('does not add a requests key or request nodes when the wire omits requests', () => {
    const adapted = adaptWorkspaceBody(base);
    expect(Object.prototype.hasOwnProperty.call(adapted, 'requests')).toBe(false);
    const requestFreeNodes = nodesOf(adaptWithReport({ revision: 1, body: base }).envelope);
    expect(requestFreeNodes.some((node) => node.kind === 'request')).toBe(false);
    expect(JSON.stringify(adapted)).not.toContain('"requests"');
  });
});
