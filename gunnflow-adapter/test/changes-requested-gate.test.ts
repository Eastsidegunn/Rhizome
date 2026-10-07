// RHZ-078 (FR-RHZ-109) 어댑터: internal gate의 세 번째 결정 'changes_requested'는
// 비-terminal(승인/거절 대기)이므로 조종석에 남고, state 값은 그대로 통과하며,
// member-of는 변하지 않는다. /detail은 reason을 'decision'으로 보인다.
import { describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';
import { MEMBER_OF, NEEDS_HUMAN, projectRhizomeNodes } from '../src/nodes.js';
import { detailItems } from '../src/details.js';

const raw = {
  missions: [{ id: 'g-1', name: 'g-1', attention: false, state: 'active' }],
  tasks: [{ id: 'm-1', missionId: 'g-1', name: 'm-1', state: 'running', hasProgress: false, attention: false }],
  gates: [
    { id: 'q-pending', state: 'pending', superseded: false, missionId: 'm-1', requestDigest: 'rhz-question-v1:a' },
    { id: 'q-changes', state: 'changes_requested', superseded: false, missionId: 'm-1', requestDigest: 'rhz-question-v1:b', body: 'req', decisionReason: 'make it shorter', decidedBy: 'unverified-local-operator:alice' },
    { id: 'q-changes-goal', state: 'changes_requested', superseded: false, goalId: 'g-1', requestDigest: 'rhz-question-v1:c' },
    { id: 'q-approved', state: 'approved', superseded: false, missionId: 'm-1', decisionReason: 'ok', decidedBy: 'bob' },
  ],
  deliverables: [],
  edges: [],
  gateCapabilities: {
    'q-pending': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
    'q-changes': { approve: 'enabled', reject: 'enabled', requestChanges: 'hidden' },
    'q-changes-goal': { approve: 'enabled', reject: 'enabled', requestChanges: 'hidden' },
    'q-approved': { approve: 'hidden', reject: 'hidden', requestChanges: 'hidden' },
  },
};
const p = adaptWorkspaceBody(raw);
const { nodes } = projectRhizomeNodes(raw, p);
const byId = (id: string) => nodes.find((n) => n.id === id)!;

describe('RHZ078 (FR-RHZ-109): changes_requested gate', () => {
  it('stays live on the cockpit; decided gates still leave', () => {
    expect(p.gates.map((g) => g.id).sort()).toEqual(['q-changes', 'q-changes-goal', 'q-pending']);
    expect(nodes.map((n) => n.id)).not.toContain('q-approved');
  });
  it('state passes through as changes_requested (pending still maps to waiting)', () => {
    expect(p.gates.find((g) => g.id === 'q-changes')!.state).toBe('changes_requested');
    expect(byId('q-changes').state.value).toBe('changes_requested');
    expect(byId('q-pending').state.value).toBe('waiting');
  });
  it('every node holds the contract shape', () => {
    for (const n of nodes) expect(nodeProblem(n), n.id).toBeNull();
  });
  it('member-of unchanged: mission-bound → task, goal-bound → goal', () => {
    expect(byId('q-changes').relations).toEqual([{ type: MEMBER_OF, target: 'm-1' }]);
    expect(byId('q-changes-goal').relations).toEqual([{ type: MEMBER_OF, target: 'g-1' }]);
  });
  it('capabilities come from gateCapabilities: approve/reject enabled, requestChanges hidden', () => {
    const caps = byId('q-changes').capabilities;
    expect(caps.map((c) => [c.action, c.level])).toEqual([
      ['gate.approve', 'enabled'],
      ['gate.reject', 'enabled'],
      ['gate.requestChanges', 'hidden'],
    ]);
    expect(byId('q-pending').capabilities.map((c) => c.action)).toContain('gate.requestChanges');
  });
  it('needs_human attention is only added for waiting (the ball is with the worker)', () => {
    expect(byId('q-pending').attention).toEqual([{ cause: NEEDS_HUMAN }]);
    expect(byId('q-changes').attention).toEqual([]);
  });
  it('/detail shows the change-request reason under decision, with decidedBy and digest', () => {
    expect(detailItems(raw, 'q-changes')).toEqual([
      { label: 'request', text: 'req' },
      { label: 'decision', text: 'make it shorter' },
      { label: 'decidedBy', text: 'unverified-local-operator:alice' },
      { label: 'digest', text: 'rhz-question-v1:b' },
    ]);
  });
});
