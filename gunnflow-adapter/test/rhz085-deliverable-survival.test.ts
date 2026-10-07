// RHZ-085 ① (FR-RHZ-115) 어댑터: deliverable 생존 규칙 = 소속 goal이 non-terminal이면 노출.
// mission-bound deliverable의 task가 완료(hidden)여도 부모 goal이 live면 남고, member-of는
// 숨은 task가 아니라 goal로 승격된다(관계가 dangling하지 않게). artifacts/goal-bound 규칙은 불변.
import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';
import { MEMBER_OF, projectRhizomeNodes } from '../src/nodes.js';

const HEX = createHash('sha256').update('deliverable bytes').digest('hex');
const BLOB = `sha256:${HEX}`;

const goal = (id: string, state = 'active') => ({ id, name: id, attention: false, state });
const task = (id: string, missionId: string, state = 'running') => ({
  id, missionId, name: id, state, hasProgress: false, attention: false,
});
const deliverable = (id: string, binding: { missionId?: string; goalId?: string }, sourceRef = 'exec-1') => ({
  id, kind: 'record', sourceRef, summary: id, missionId: '', ...binding,
});
const produces = (from: { type: string; id: string }, to: string) => ({
  id: `edge-produces-${from.id}-${to}`, from, to: { type: 'deliverable', id: to }, edgeKind: 'produces',
});

const raw = {
  missions: [goal('g-live'), goal('g-done', 'achieved')],
  tasks: [
    task('m-live', 'g-live'),
    task('m-done', 'g-live', 'completed'),
    task('m-failed', 'g-live', 'failed'),
    task('m-under-done', 'g-done', 'completed'),
    task('m-running-under-done', 'g-done'),
  ],
  gates: [],
  deliverables: [
    deliverable('d-of-done', { missionId: 'm-done' }, BLOB), // D1
    deliverable('d-of-failed', { missionId: 'm-failed' }), // D1 (failed is terminal too)
    deliverable('d-of-live', { missionId: 'm-live' }), // D2
    deliverable('d-under-done-goal', { missionId: 'm-under-done' }), // D3
    deliverable('d-under-done-goal-live-task', { missionId: 'm-running-under-done' }), // D3
    deliverable('d-goal', { goalId: 'g-live' }), // D4
    deliverable('d-goal-done', { goalId: 'g-done' }), // D4
    deliverable('d-orphan', { missionId: 'm-unknown' }), // no task → goal unknown → hidden
  ],
  edges: [
    produces({ type: 'mission', id: 'm-done' }, 'd-of-done'),
    produces({ type: 'mission', id: 'm-live' }, 'd-of-live'),
    produces({ type: 'goal', id: 'g-live' }, 'd-goal'),
  ],
};
const p = adaptWorkspaceBody(raw);
const { nodes } = projectRhizomeNodes(raw, p);
const byId = (id: string) => nodes.find((n) => n.id === id);
const memberOfs = (id: string) => byId(id)!.relations.filter((r) => r.type === MEMBER_OF);
const liveNodeIds = new Set(nodes.map((n) => n.id));

describe('RHZ085 ① (FR-RHZ-115): deliverable은 소속 goal이 live면 남는다', () => {
  it('모든 노드가 계약 모양을 만족하고, 어떤 관계도 숨은 노드를 가리키지 않는다', () => {
    for (const n of nodes) {
      expect(nodeProblem(n), n.id).toBeNull();
      for (const r of n.relations) expect(liveNodeIds.has(r.target), `${n.id} → ${r.target}`).toBe(true);
    }
  });

  it('D1: 완료된 task의 deliverable은 live goal 아래 남고 member-of → goal (숨은 task 아님), artifact 유지 (FR-RHZ-115)', () => {
    expect(p.tasks.map((t) => t.id)).not.toContain('m-done');
    expect(p.deliverables.find((d) => d.id === 'd-of-done')).toMatchObject({ missionId: 'm-done', promotedGoalId: 'g-live' });
    expect(memberOfs('d-of-done')).toEqual([{ type: MEMBER_OF, target: 'g-live' }]);
    expect(byId('d-of-done')!.relations).toHaveLength(1);
    expect(byId('d-of-done')!.artifacts).toEqual([
      { id: 'd-of-done', mediaType: 'application/octet-stream', digest: HEX, access: { kind: 'snapshot' } },
    ]);
    // failed is terminal too → same promotion.
    expect(memberOfs('d-of-failed')).toEqual([{ type: MEMBER_OF, target: 'g-live' }]);
    // The produces edge from the hidden task is not a live relationship.
    expect(p.edges.find((e) => e.to === 'd-of-done')).toBeUndefined();
  });

  it('D2: live task의 deliverable은 그대로 (member-of → task, 승격 없음) (FR-RHZ-115)', () => {
    const d = p.deliverables.find((x) => x.id === 'd-of-live')!;
    expect(d).toMatchObject({ missionId: 'm-live' });
    expect('promotedGoalId' in d).toBe(false);
    expect('goalId' in d).toBe(false);
    expect(memberOfs('d-of-live')).toEqual([{ type: MEMBER_OF, target: 'm-live' }]);
    expect(byId('m-live')!.relations).toContainEqual({ type: 'produces', target: 'd-of-live' });
  });

  it('D3: 소속 goal이 terminal이면 deliverable은 숨는다 — task가 완료든 running이든 (FR-RHZ-115)', () => {
    expect(byId('d-under-done-goal')).toBeUndefined();
    expect(byId('d-under-done-goal-live-task')).toBeUndefined();
    expect(byId('d-orphan')).toBeUndefined();
    expect(p.deliverables.map((d) => d.id).sort()).toEqual(['d-goal', 'd-of-done', 'd-of-failed', 'd-of-live']);
  });

  it('D3: goal이 뒤늦게 terminal이 되면 승격돼 있던 deliverable도 함께 사라진다 (FR-RHZ-115)', () => {
    const closed = { ...raw, missions: [goal('g-live', 'cancelled'), goal('g-done', 'achieved')] };
    const q = adaptWorkspaceBody(closed);
    expect(q.deliverables).toEqual([]);
  });

  it('D4: goal-bound deliverable 규칙(RHZ-081)은 불변 — goal live면 남고 member-of → goal, terminal이면 숨는다 (FR-RHZ-115)', () => {
    const d = p.deliverables.find((x) => x.id === 'd-goal')!;
    expect(d).toMatchObject({ missionId: '', goalId: 'g-live' });
    expect('promotedGoalId' in d).toBe(false);
    expect(memberOfs('d-goal')).toEqual([{ type: MEMBER_OF, target: 'g-live' }]);
    expect(byId('g-live')!.relations).toContainEqual({ type: 'produces', target: 'd-goal' });
    expect(byId('d-goal-done')).toBeUndefined();
  });

  it('승격 deliverable은 live goal 집계 밖의 goal(목록에 없는 goal)로는 추론하지 않는다', () => {
    const noGoalListed = { ...raw, missions: [], tasks: [task('m-live', 'g-live'), task('m-done', 'g-live', 'completed')] };
    const q = adaptWorkspaceBody(noGoalListed);
    // g-live is known live only via its live task m-live; m-done's deliverable survives on that fact.
    expect(q.deliverables.map((d) => d.id).sort()).toEqual(['d-goal', 'd-of-done', 'd-of-live']);
    const onlyTerminal = { ...noGoalListed, tasks: [task('m-done', 'g-live', 'completed')] };
    expect(adaptWorkspaceBody(onlyTerminal).deliverables).toEqual([]);
  });
});
