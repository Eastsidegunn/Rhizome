// FR-RHZ-106 어댑터: edge.rewire(RHZ-066)로 supersede된 엣지는 wire의 edges에서도 빠진다.
// RHZ-076은 containedIds에서만 제외했고, liveEdges는 옛 contains 대상을 부모의 relations에 남겼다.
import { describe, expect, it } from 'vitest';
import { adaptWithReport, adaptWorkspaceBody, nodesOf } from '../src/index.js';

const goal = (id: string) => ({ id, name: id, attention: false, state: 'active' });
const contains = (id: string, from: string, to: string) => ({
  id, from: { type: 'goal', id: from }, to: { type: 'goal', id: to }, edgeKind: 'contains',
});

describe('FR-RHZ-106: superseded 엣지는 liveEdges에서 제외', () => {
  const body = {
    missions: [goal('g-p'), goal('g-a'), goal('g-b'), goal('g-q'), goal('g-c')],
    tasks: [], gates: [], deliverables: [],
    edges: [
      contains('e1', 'g-p', 'g-a'),
      { ...contains('e2', 'g-p', 'g-b'), supersedes: 'e1' },
      contains('e3', 'g-q', 'g-c'),
    ],
  };

  it('rewire 뒤 P의 relations는 B를 담고 A는 담지 않는다', () => {
    const nodes = nodesOf(adaptWithReport({ revision: 1, body }).envelope);
    const p = nodes.find((n) => n.id === 'g-p')!;
    expect(p.relations).toEqual([{ type: 'contains', target: 'g-b' }]);
    expect(p.relations.some((r) => r.target === 'g-a')).toBe(false);
  });
  it('superseded 엣지 e1은 wire edges에 없고, 대체 엣지 e2와 무관한 엣지 e3는 남는다', () => {
    const ids = adaptWorkspaceBody(body).edges.map((e) => e.id);
    expect(ids).not.toContain('e1');
    expect(ids).toContain('e2');
    expect(ids).toContain('e3');
  });
});
