// RHZ-075 (FR-RHZ-108) 어댑터: gate는 mission 또는 goal 중 정확히 하나에 묶인다. goal-bound gate는
// goal 노드의 member-of로 투영되어 고아로 뜨지 않는다. 투영은 상태의 함수이며 ID는 임의다.
import { describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';
import { MEMBER_OF, projectRhizomeNodes } from '../src/nodes.js';

const goal = (id: string) => ({ id, name: id, attention: false, state: 'active' });
const task = (id: string, missionId: string) => ({
  id, missionId, name: id, state: 'running', hasProgress: false, attention: false,
});
const gate = (id: string, binding: { missionId?: string; goalId?: string }) => ({
  id, state: 'pending', superseded: false, ...binding,
});

const raw = {
  missions: [goal('g-1')],
  tasks: [task('m-1', 'g-1')],
  gates: [
    gate('q-goal', { goalId: 'g-1' }),
    gate('q-mission', { missionId: 'm-1' }),
    gate('q-none', {}),
    gate('q-both', { missionId: 'm-1', goalId: 'g-1' }),
    // decided gate: must be dropped by liveGates (rule unchanged by RHZ-075)
    { ...gate('q-decided', { goalId: 'g-1' }), state: 'approved', verification: { status: 'claimed', claimKind: 'relayed' } },
  ],
  deliverables: [],
  edges: [],
};
const p = adaptWorkspaceBody(raw);
const { nodes } = projectRhizomeNodes(raw, p);
const byId = (id: string) => nodes.find((n) => n.id === id)!;
const memberOfs = (id: string) => byId(id).relations.filter((r) => r.type === MEMBER_OF);

describe('RHZ075 (FR-RHZ-108): gate 소속 = missionId xor goalId', () => {
  it('모든 노드가 계약 모양을 만족한다', () => {
    for (const n of nodes) expect(nodeProblem(n), n.id).toBeNull();
  });
  it('goalId만 있는 gate → 그 goal 노드로 member-of 정확히 하나', () => {
    expect(memberOfs('q-goal')).toEqual([{ type: MEMBER_OF, target: 'g-1' }]);
    expect(byId('q-goal').relations).toHaveLength(1);
  });
  it('missionId만 있는 gate → task 노드로 member-of (기존 동작 유지)', () => {
    expect(memberOfs('q-mission')).toEqual([{ type: MEMBER_OF, target: 'm-1' }]);
  });
  it('둘 다 없는 gate → relations 비어 있음', () => {
    expect(byId('q-none').relations).toEqual([]);
  });
  it('둘 다 있는 gate(방어) → missionId 우선, member-of 하나만', () => {
    expect(memberOfs('q-both')).toEqual([{ type: MEMBER_OF, target: 'm-1' }]);
    expect(byId('q-both').relations).toHaveLength(1);
  });
  it('투영이 goalId를 그대로 싣는다(없으면 빈 문자열)', () => {
    const g = (id: string) => p.gates.find((x) => x.id === id)!;
    expect(g('q-goal').goalId).toBe('g-1');
    expect(g('q-goal').missionId).toBe('');
    expect(g('q-mission').goalId).toBe('');
    expect(g('q-none').goalId).toBe('');
    expect(g('q-both')).toMatchObject({ missionId: 'm-1', goalId: 'g-1' });
  });
  it('liveGates 규칙은 그대로: waiting gate만 남고 decided gate는 빠진다', () => {
    expect(p.gates.map((g) => g.id).sort()).toEqual(['q-both', 'q-goal', 'q-mission', 'q-none']);
    expect(nodes.find((n) => n.id === 'q-decided')).toBeUndefined();
  });
});
