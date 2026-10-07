// RHZ-080 (FR-RHZ-111) 어댑터 D1: task(Rhizome mission)의 assignee가 /detail
// items에 'assignee' 라벨(자유 문자열, 엔진 불해석)로 보이고, 미배정 task의
// items는 기존과 동일하다. 투영(nodes/relations/workspaceWire)은 무접촉.
import { describe, expect, it } from 'vitest';
import { validateNodeDetail } from '@gunnflow/contract';
import { detailItems, detailOf } from '../src/details.js';

const body = {
  missions: [{ id: 'g-1', name: 'g-1', attention: false, state: 'active' }],
  tasks: [
    { id: 'm-assigned', missionId: 'g-1', name: 'a', state: 'running', currentAction: 'wiring', hasProgress: false, attention: false, handle: 'm-aaaaaaaa', assignee: 'agent-a' },
    { id: 'm-plain', missionId: 'g-1', name: 'b', state: 'queued', hasProgress: false, attention: false, handle: 'm-bbbbbbbb' },
    { id: 'm-empty', missionId: 'g-1', name: 'c', state: 'queued', hasProgress: false, attention: false, handle: 'm-cccccccc', assignee: '' },
  ],
  gates: [],
  deliverables: [],
  edges: [],
};

describe('RHZ080 (FR-RHZ-111): task assignee in /detail', () => {
  it('assigned task: assignee label between blockedReason and state, contract-valid', () => {
    expect(detailItems(body, 'm-assigned')).toEqual([
      { label: 'currentAction', text: 'wiring' },
      { label: 'assignee', text: 'agent-a' },
      { label: 'state', text: 'running' },
    ]);
    const d = detailOf({ revision: 7, body }, 'm-assigned');
    expect(d).toBeDefined();
    const r = validateNodeDetail(d);
    expect(r.ok, r.ok ? '' : r.problems.join('; ')).toBe(true);
    expect(d!.revision).toBe(7);
  });
  it('unassigned task: items unchanged (no assignee label), empty string omitted too', () => {
    expect(detailItems(body, 'm-plain')).toEqual([{ label: 'state', text: 'queued' }]);
    expect(detailItems(body, 'm-empty')).toEqual([{ label: 'state', text: 'queued' }]);
  });
});
