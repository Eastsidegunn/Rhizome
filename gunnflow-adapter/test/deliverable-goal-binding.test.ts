// RHZ-081 (FR-RHZ-112) 어댑터: deliverable은 mission 또는 goal 중 정확히 하나에 묶인다. goal-bound
// deliverable은 goal 노드의 member-of로 투영되고, goal이 live인 동안 남으며, goal이 terminal이면
// 함께 숨는다. mission-bound 규칙은 RHZ-085 (FR-RHZ-115)로 "소속 goal이 live면 남는다"가 됐다.
import { describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';
import { MEMBER_OF, projectRhizomeNodes } from '../src/nodes.js';

const goal = (id: string, state = 'active') => ({ id, name: id, attention: false, state });
const task = (id: string, missionId: string, state = 'running') => ({
  id, missionId, name: id, state, hasProgress: false, attention: false,
});
const deliverable = (id: string, binding: { missionId?: string; goalId?: string }, summary = id) => ({
  id, kind: 'record', sourceRef: 'exec-1', summary, missionId: '', ...binding,
});
const produces = (from: { type: string; id: string }, to: string) => ({
  id: `edge-produces-${from.id}-${to}`, from, to: { type: 'deliverable', id: to }, edgeKind: 'produces',
});

const raw = {
  missions: [goal('g-live'), goal('g-done', 'achieved'), goal('g-task-only')],
  tasks: [task('m-1', 'g-task-only'), task('m-done', 'g-live', 'completed')],
  gates: [],
  deliverables: [
    deliverable('d-goal', { goalId: 'g-live' }),
    deliverable('d-goal-done', { goalId: 'g-done' }),
    deliverable('d-goal-of-task', { goalId: 'g-task-only' }),
    deliverable('d-mission', { missionId: 'm-1' }),
    deliverable('d-mission-done', { missionId: 'm-done' }),
    deliverable('d-none', {}),
  ],
  edges: [
    produces({ type: 'goal', id: 'g-live' }, 'd-goal'),
    produces({ type: 'mission', id: 'm-1' }, 'd-mission'),
  ],
};
const p = adaptWorkspaceBody(raw);
const { nodes } = projectRhizomeNodes(raw, p);
const byId = (id: string) => nodes.find((n) => n.id === id)!;
const memberOfs = (id: string) => byId(id).relations.filter((r) => r.type === MEMBER_OF);

describe('RHZ081 (FR-RHZ-112): deliverable 소속 = missionId xor goalId', () => {
  it('모든 노드가 계약 모양을 만족한다', () => {
    for (const n of nodes) expect(nodeProblem(n), n.id).toBeNull();
  });
  it('goalId만 있는 deliverable → 그 goal 노드로 member-of 정확히 하나 (+ produces 엣지는 goal 쪽에)', () => {
    expect(memberOfs('d-goal')).toEqual([{ type: MEMBER_OF, target: 'g-live' }]);
    expect(byId('d-goal').relations).toHaveLength(1);
    expect(byId('g-live').relations).toContainEqual({ type: 'produces', target: 'd-goal' });
  });
  it('missionId만 있는 deliverable → task 노드로 member-of (기존 동작 유지)', () => {
    expect(memberOfs('d-mission')).toEqual([{ type: MEMBER_OF, target: 'm-1' }]);
    expect(byId('m-1').relations).toContainEqual({ type: 'produces', target: 'd-mission' });
  });
  it('투영이 goalId를 goal-bound일 때만 싣는다 (mission-bound 투영 모양 불변)', () => {
    const d = (id: string) => p.deliverables.find((x) => x.id === id)!;
    expect(d('d-goal')).toMatchObject({ missionId: '', goalId: 'g-live' });
    expect('goalId' in d('d-mission')).toBe(false);
  });
  it('live 규칙: goal이 live면 남고, goal이 terminal이면 숨는다; mission-bound도 소속 goal 기준 (RHZ-085/FR-RHZ-115)', () => {
    expect(p.deliverables.map((d) => d.id).sort()).toEqual(['d-goal', 'd-goal-of-task', 'd-mission', 'd-mission-done']);
    expect(nodes.find((n) => n.id === 'd-goal-done')).toBeUndefined();
    // RHZ-085: completed task under live goal → the deliverable survives, promoted to the goal.
    expect(memberOfs('d-mission-done')).toEqual([{ type: MEMBER_OF, target: 'g-live' }]);
    expect(nodes.find((n) => n.id === 'd-none')).toBeUndefined();
  });
  it('goal이 terminal이 되면 그 goal-bound deliverable도 함께 사라진다', () => {
    const closed = { ...raw, missions: [goal('g-live', 'cancelled'), goal('g-done', 'achieved'), goal('g-task-only')] };
    const q = adaptWorkspaceBody(closed);
    expect(q.deliverables.map((d) => d.id).sort()).toEqual(['d-goal-of-task', 'd-mission']);
    expect(q.edges.find((e) => e.to === 'd-goal')).toBeUndefined();
  });
});
