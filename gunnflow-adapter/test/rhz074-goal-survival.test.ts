// RHZ-074 (FR-RHZ-102) 어댑터: goal 생존 규칙 — non-terminal goal은 live task가 없어도 조종석에
// 남고(goal=결과, mission 없는 goal 정상), terminal goal은 그 task까지 함께 숨긴다.
// 투영은 상태의 함수이며 ID는 임의다.
import { describe, expect, it } from 'vitest';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';
import { adaptWithReport, nodesOf } from '../src/index.js';

const goal = (id: string, state?: string) => ({ id, name: id, attention: false, state });
const task = (id: string, missionId: string, state: string) => ({
  id, missionId, name: id, state, hasProgress: false, attention: false,
});
const contains = (id: string, from: string, to: string) => ({
  id, from: { type: 'goal', id: from }, to: { type: 'goal', id: to }, edgeKind: 'contains',
});

describe('RHZ074 G1 (FR-RHZ-102): task 없는 active goal 노출', () => {
  const p = adaptWorkspaceBody({
    missions: [goal('g-lonely', 'active'), goal('g-paired', 'active')],
    tasks: [task('m-paired', 'g-paired', 'running')],
    gates: [], deliverables: [], edges: [],
  });

  it('live task가 없는 non-terminal goal도 missions에 남는다', () => {
    expect(p.missions.map((m) => m.id).sort()).toEqual(['g-lonely', 'g-paired']);
    expect(p.tasks.map((t) => t.id)).toEqual(['m-paired']);
  });
});

describe('RHZ074 G2 (FR-RHZ-102): terminal goal 숨김 + 그 task(queued 짝 mission)도 숨김', () => {
  const p = adaptWorkspaceBody({
    missions: [goal('g-done', 'achieved'), goal('g-junk', 'cancelled'), goal('g-live', 'active')],
    tasks: [
      task('m-done', 'g-done', 'queued'),
      task('m-junk', 'g-junk', 'running'),
      task('m-live', 'g-live', 'queued'),
    ],
    gates: [], deliverables: [], edges: [],
  });

  it('terminal goal은 사라진다', () => {
    expect(p.missions.map((m) => m.id)).toEqual(['g-live']);
  });
  it('terminal goal 아래의 non-terminal task는 고아로 뜨지 않고 함께 숨는다', () => {
    expect(p.tasks.map((t) => t.id)).toEqual(['m-live']);
  });
});

describe('RHZ074 G3 (FR-RHZ-102): contains 계층(root→child→issue, task 없음) 전부 생존', () => {
  const p = adaptWorkspaceBody({
    missions: [goal('g-root', 'active'), goal('g-child', 'active'), goal('g-issue', 'active')],
    tasks: [],
    gates: [], deliverables: [],
    edges: [contains('e-root-child', 'g-root', 'g-child'), contains('e-child-issue', 'g-child', 'g-issue')],
  });

  it('세 goal 모두 남는다', () => {
    expect(p.missions.map((m) => m.id).sort()).toEqual(['g-child', 'g-issue', 'g-root']);
  });
  it('contains 엣지도 양끝이 살아 있으므로 모두 남는다', () => {
    expect(p.edges.map((e) => e.id).sort()).toEqual(['e-child-issue', 'e-root-child']);
  });
});

// RHZ074 G4 (FR-RHZ-102): gate의 missionId를 그대로 나른다 — 바인딩된 gate는 조종석에서 그 task의
// member-of로 매달리고, missionId 없는 옛 journal gate는 relations []로 남는다(발명 없음).
describe('RHZ074 G4 (FR-RHZ-102): gate missionId 통과 → member-of 관계', () => {
  const body = {
    missions: [goal('g-live', 'active')],
    tasks: [task('m-live', 'g-live', 'running')],
    gates: [
      { id: 'approval-bound', state: 'pending', superseded: false, missionId: 'm-live' },
      { id: 'approval-legacy', state: 'pending', superseded: false },
    ],
    deliverables: [], edges: [],
  };
  const p = adaptWorkspaceBody(body);
  const nodes = nodesOf(adaptWithReport({ revision: 1, body }).envelope);
  const byId = (id: string) => nodes.find((n) => n.id === id)!;

  it('missionId가 있는 gate는 projection에 그대로 실린다', () => {
    expect(p.gates.find((g) => g.id === 'approval-bound')!.missionId).toBe('m-live');
    expect(p.gates.find((g) => g.id === 'approval-legacy')!.missionId).toBe('');
  });
  it('live task에 바인딩된 gate는 그 task로의 member-of 관계가 정확히 하나', () => {
    expect(byId('approval-bound').relations).toEqual([{ type: 'member-of', target: 'm-live' }]);
  });
  it('missionId 없는 gate(옛 journal)는 relations []', () => {
    expect(byId('approval-legacy').relations).toEqual([]);
  });
});
