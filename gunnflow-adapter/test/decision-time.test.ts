// FR-RHZ-143: decision time reaches the cockpit only through contract-sanctioned
// attention `since` and detail paths; the closed node shape stays unchanged.
import { describe, expect, it } from 'vitest';
import { nodeProblem, validateNodeDetail, type NodeProjection } from '@gunnflow/contract';
import { detailOf } from '../src/details.js';
import { APPROVAL_UNVERIFIED } from '../src/nodes.js';
import { adaptWithReport, nodesOf } from '../src/upstream.js';

const DECIDED_AT = '2026-10-08T03:12:00.000000001Z';

const gate = (
  id: string,
  state: 'pending' | 'changes_requested' | 'approved' | 'rejected' = 'approved',
  decidedAt?: string,
) => ({
  id,
  state,
  superseded: false,
  body: 'request body',
  decisionReason: state === 'approved' ? 'ship it' : 'do not ship',
  ...(decidedAt ? { decidedAt } : {}),
});

const bodyOf = (gates: ReturnType<typeof gate>[]) => ({
  missions: [],
  tasks: [],
  gates,
  deliverables: [],
  edges: [],
  counts: { running: 0, needsYou: 0, blocked: 0 },
  attention: [],
  capabilities: {},
  gateCapabilities: {},
});

const projectionOf = (body: ReturnType<typeof bodyOf>) =>
  nodesOf(adaptWithReport({ revision: 143, body }).envelope);

const nodeById = (nodes: NodeProjection[], id: string) => nodes.find((node) => node.id === id)!;

describe('FR-RHZ-143 decided gate time', () => {
  it('sets approval_unverified since to the exact wire string and omits it when absent', () => {
    const nodes = projectionOf(bodyOf([
      gate('timed', 'approved', DECIDED_AT),
      gate('legacy'),
    ]));

    expect(nodeById(nodes, 'timed').attention).toEqual([
      { cause: APPROVAL_UNVERIFIED, since: DECIDED_AT },
    ]);
    expect(nodeById(nodes, 'legacy').attention).toEqual([{ cause: APPROVAL_UNVERIFIED }]);
    expect(nodeById(nodes, 'legacy').attention[0]).not.toHaveProperty('since');
  });

  it('puts 결정 시각 immediately after 승인 상태 for approved/rejected gates and omits it when absent', () => {
    const body = bodyOf([
      gate('approved', 'approved', DECIDED_AT),
      gate('rejected', 'rejected', DECIDED_AT),
      gate('legacy'),
      gate('pending', 'pending', DECIDED_AT),
      gate('changes', 'changes_requested', DECIDED_AT),
    ]);

    for (const id of ['approved', 'rejected']) {
      const detail = detailOf({ revision: 143, body }, id)!;
      expect(validateNodeDetail(detail).ok).toBe(true);
      const statusIndex = detail.items.findIndex((item) => item.label === '승인 상태');
      expect(detail.items[statusIndex + 1]).toEqual({ label: '결정 시각', text: DECIDED_AT });
    }

    expect(detailOf({ revision: 143, body }, 'legacy')!.items.map((item) => item.label))
      .not.toContain('결정 시각');
    for (const id of ['pending', 'changes']) {
      expect(detailOf({ revision: 143, body }, id)!.items.map((item) => item.label))
        .not.toContain('결정 시각');
    }
  });

  it('never attaches since to a waiting gate even when decidedAt is present', () => {
    const nodes = projectionOf(bodyOf([gate('waiting', 'pending', DECIDED_AT)]));
    expect(nodeById(nodes, 'waiting').attention).toEqual([{ cause: 'needs_human' }]);
  });

  it('keeps every node contract-valid and never widens a node with decidedAt', () => {
    const nodes = projectionOf(bodyOf([
      gate('timed', 'approved', DECIDED_AT),
      gate('legacy', 'rejected'),
    ]));

    for (const node of nodes) {
      expect(nodeProblem(node), node.id).toBeNull();
      expect(node).not.toHaveProperty('decidedAt');
    }
  });

  it('is byte-identical to the previous gate projection when decidedAt is absent', () => {
    const node = nodeById(projectionOf(bodyOf([gate('legacy')])), 'legacy');
    const previous = {
      id: 'legacy',
      kind: 'gate',
      state: { value: 'approved' },
      relations: [],
      capabilities: [],
      attention: [{ cause: APPROVAL_UNVERIFIED }],
      artifacts: [],
    };

    expect(node).toEqual(previous);
    expect(JSON.stringify(node)).toBe(JSON.stringify(previous));
  });

  it('projects attention and detail deterministically', () => {
    const body = bodyOf([gate('timed', 'approved', DECIDED_AT)]);
    const wire = { revision: 143, body };

    expect(JSON.stringify(projectionOf(body))).toBe(JSON.stringify(projectionOf(body)));
    expect(JSON.stringify(detailOf(wire, 'timed'))).toBe(JSON.stringify(detailOf(wire, 'timed')));
  });
});
