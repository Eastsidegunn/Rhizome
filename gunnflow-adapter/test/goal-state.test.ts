// RHZ-061 어댑터 짝: goal.State 매핑 + terminal goal 숨김. 종료/취소된 goal은
// 조종석(진행 중)에서 사라지고, 살아남은 goal은 상태를 그대로 나른다.
import { describe, expect, it } from 'vitest';
import { adaptWithReport, nodesOf } from '../src/index.js';

// 각 goal에 live task를 하나씩 둬서 "live task 없는 mission 드롭" 규칙과 분리 —
// 순수하게 terminal-goal 드롭만 관찰한다.
const wireBody = {
  missions: [
    { id: 'g-active', name: 'active goal', attention: false, state: 'active' },
    { id: 'g-achieved', name: 'done goal', attention: false, state: 'achieved' },
    { id: 'g-cancelled', name: 'junk goal', attention: false, state: 'cancelled' },
    { id: 'g-nostate', name: 'legacy goal', attention: false },
  ],
  tasks: [
    { id: 't1', missionId: 'g-active', name: 't1', state: 'running', hasProgress: false, attention: false },
    { id: 't2', missionId: 'g-achieved', name: 't2', state: 'running', hasProgress: false, attention: false },
    { id: 't3', missionId: 'g-cancelled', name: 't3', state: 'running', hasProgress: false, attention: false },
    { id: 't4', missionId: 'g-nostate', name: 't4', state: 'running', hasProgress: false, attention: false },
  ],
  gates: [],
  deliverables: [],
  edges: [],
};

describe('RHZ-061 어댑터: goal state 매핑 + terminal 숨김', () => {
  const nodes = nodesOf(adaptWithReport({ revision: 1, body: wireBody }).envelope);
  const mission = (id: string) => nodes.find((n) => n.id === id);

  it('terminal goal(achieved/cancelled)은 조종석에서 사라진다', () => {
    expect(mission('g-achieved')).toBeUndefined();
    expect(mission('g-cancelled')).toBeUndefined();
  });

  it('진행 중 goal은 남고 상태를 그대로 나른다', () => {
    // RHZ-076 (FR-RHZ-104): contains 부모가 없는 goal은 조종석 최상위 kind "goal"(이전 "mission").
    expect(mission('g-active')).toMatchObject({ kind: 'goal', state: { value: 'active' } });
    // 상태 없는 레거시 goal은 unstated 폴백 — 발명 없음.
    expect(mission('g-nostate')).toMatchObject({ state: { value: 'unstated' } });
  });
});

// RHZ-062 짝: cancelled 미션(task 노드)도 조종석에서 사라진다(완전 sweep).
describe('RHZ-062 어댑터: cancelled task 숨김', () => {
  const body = {
    missions: [{ id: 'g1', name: 'g1', attention: false, state: 'active' }],
    tasks: [
      { id: 'task-live', missionId: 'g1', name: 'live', state: 'running', hasProgress: false, attention: false },
      { id: 'task-swept', missionId: 'g1', name: 'swept', state: 'cancelled', hasProgress: false, attention: false },
    ],
    gates: [], deliverables: [], edges: [],
  };
  const nodes = nodesOf(adaptWithReport({ revision: 1, body }).envelope);

  it('cancelled task는 사라지고 running task는 남는다', () => {
    expect(nodes.find((n) => n.id === 'task-swept')).toBeUndefined();
    expect(nodes.find((n) => n.id === 'task-live')).toMatchObject({ kind: 'task', state: { value: 'running' } });
  });
});
