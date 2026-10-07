// RHZ-076 (FR-RHZ-104) 어댑터: goal 계층을 조종석 kind 3층으로 투영.
// 최상위 goal(들어오는 contains 엣지 없음) → "goal", contains 자식 goal → "mission",
// Rhizome mission → "task". 판정은 raw 스냅샷의 contains 엣지 전체의 함수(ID 하드코딩 없음):
// 부모가 terminal로 숨겨진 고아 goal·contains 사이클은 보수적으로 "mission" 유지.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { DIRECT_WIRE, WORKSPACE_ROOT_KIND, validateNodeDetail } from '@gunnflow/contract';
import { adaptWithReport, adaptWorkspaceBody, detailOf, nodesOf } from '../src/index.js';
import { goalKind } from '../src/nodes.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';

const goal = (id: string, state = 'active', success?: string) => ({ id, name: id, attention: false, state, ...(success ? { success } : {}) });
const task = (id: string, missionId: string, state = 'running') => ({
  id, missionId, name: id, state, hasProgress: false, attention: false,
});
const contains = (id: string, from: string, to: string) => ({
  id, from: { type: 'goal', id: from }, to: { type: 'goal', id: to }, edgeKind: 'contains',
});
const kindsOf = (body: unknown) =>
  Object.fromEntries(nodesOf(adaptWithReport({ revision: 1, body }).envelope).filter((n) => n.kind !== WORKSPACE_ROOT_KIND).map((n) => [n.id, n.kind]));

describe('RHZ076 T1 (FR-RHZ-104): 3층 fixture root goal → child goal → Rhizome mission', () => {
  const body = {
    missions: [goal('g-root'), goal('g-child')],
    tasks: [task('m-example', 'g-child')],
    gates: [], deliverables: [],
    edges: [contains('e-1', 'g-root', 'g-child')],
  };

  it('kinds는 goal / mission / task', () => {
    expect(kindsOf(body)).toEqual({ 'g-root': 'goal', 'g-child': 'mission', 'm-example': 'task' });
  });
  it('relations·capabilities는 kind와 무관: 최상위 goal도 contains 엣지를 그대로 나르고 capability는 없다', () => {
    const nodes = nodesOf(adaptWithReport({ revision: 1, body }).envelope);
    const root = nodes.find((n) => n.id === 'g-root')!;
    expect(root.relations).toEqual([{ type: 'contains', target: 'g-child' }]);
    expect(root.capabilities).toEqual([]);
    expect(nodes.find((n) => n.id === 'm-example')!.relations).toEqual([{ type: 'member-of', target: 'g-child' }]);
  });
  it('contains 사이클의 구성원은 모두 들어오는 엣지가 있으므로 전부 "mission"(보수적)', () => {
    const cyclic = { ...body, missions: [goal('g-a'), goal('g-b')], tasks: [], edges: [contains('e-ab', 'g-a', 'g-b'), contains('e-ba', 'g-b', 'g-a')] };
    expect(kindsOf(cyclic)).toEqual({ 'g-a': 'mission', 'g-b': 'mission' });
  });
});

describe('RHZ076 T2 (FR-RHZ-104): 부모도 자식도 없는 단일 goal', () => {
  it('→ "goal"', () => {
    expect(kindsOf({ missions: [goal('g-solo')], tasks: [], gates: [], deliverables: [], edges: [] })).toEqual({ 'g-solo': 'goal' });
  });
  it('goalKind는 상태의 함수: 같은 id라도 contains 대상이면 mission, 아니면 goal', () => {
    expect(goalKind('x', new Set())).toBe('goal');
    expect(goalKind('x', new Set(['x']))).toBe('mission');
  });
});

describe('RHZ076 T3 (FR-RHZ-104): 부모 goal이 terminal(숨김)인 고아 goal', () => {
  const body = {
    missions: [goal('g-parent', 'achieved'), goal('g-orphan'), goal('g-top')],
    tasks: [],
    gates: [], deliverables: [],
    edges: [contains('e-po', 'g-parent', 'g-orphan')],
  };
  const kinds = kindsOf(body);

  it('부모가 숨겨져 live 엣지가 사라져도 raw contains 엣지가 있으므로 "mission" 유지', () => {
    expect(kinds['g-orphan']).toBe('mission');
    expect(kinds['g-parent']).toBeUndefined();
  });
  it('같은 스냅샷의 진짜 최상위 goal은 "goal"', () => {
    expect(kinds['g-top']).toBe('goal');
  });
});

describe('RHZ076 T5 (FR-RHZ-104): superseded contains 엣지는 containment가 아니다(edge.rewire, RHZ-066)', () => {
  const body = {
    missions: [goal('g-p'), goal('g-a'), goal('g-b')],
    tasks: [], gates: [], deliverables: [],
    edges: [contains('e1', 'g-p', 'g-a'), { ...contains('e2', 'g-p', 'g-b'), supersedes: 'e1' }],
  };
  const kinds = kindsOf(body);

  it('e2가 e1을 supersede → A는 "goal", B는 "mission", P는 "goal"', () => {
    expect(kinds).toEqual({ 'g-p': 'goal', 'g-a': 'goal', 'g-b': 'mission' });
  });
  it('projection.containedIds에도 superseded 엣지의 대상은 없다', () => {
    expect(adaptWorkspaceBody(body).containedIds).toEqual(['g-b']);
  });
});

describe('RHZ076 T4 (FR-RHZ-104): kind "goal" 노드의 /detail은 RHZ-072 경로 그대로(success 항목)', () => {
  const wireBody = {
    missions: [goal('g-top', 'active', 'example outcome is stable'), goal('g-child', 'active', 'repo shipped')],
    tasks: [task('m-1', 'g-child')],
    gates: [], deliverables: [],
    edges: [contains('e-1', 'g-top', 'g-child')],
    counts: { running: 1, needsYou: 0, blocked: 0 },
    attention: [],
  };
  const REVISION = 3;
  const mock = Fastify({ logger: false });
  let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => ({ revision: REVISION, body: wireBody }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: REVISION, body: wireBody })}\n\n`);
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

  it('wire가 g-top을 kind "goal"로 내고, GET /detail/g-top은 200 + success 항목', async () => {
    const nodes = (await (await fetch(`${server.url}${DIRECT_WIRE.nodes}`)).json()) as { nodes: Array<{ id: string; kind: string }> };
    expect(nodes.nodes.find((n) => n.id === 'g-top')?.kind).toBe('goal');
    expect(nodes.nodes.find((n) => n.id === 'g-child')?.kind).toBe('mission');
    const res = await fetch(`${server.url}${DIRECT_WIRE.detail}/g-top`);
    expect(res.status).toBe(200);
    const r = validateNodeDetail(await res.json());
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
    expect(r.ok && r.detail.revision).toBe(REVISION);
    expect(r.ok && r.detail.items).toEqual([{ label: 'success', text: 'example outcome is stable' }]);
  });
  it('detailOf는 kind를 보지 않는다: goal/mission 둘 다 같은 success 분기', () => {
    const wire = { revision: REVISION, body: wireBody };
    expect(detailOf(wire, 'g-top')?.items).toEqual([{ label: 'success', text: 'example outcome is stable' }]);
    expect(detailOf(wire, 'g-child')?.items).toEqual([{ label: 'success', text: 'repo shipped' }]);
  });
});
