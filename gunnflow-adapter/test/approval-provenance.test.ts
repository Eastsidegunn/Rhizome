// RHZ-105 (FR-RHZ-133): additive gate provenance ingestion and projection.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { detailItems, detailOf, type WireDetailBody } from '../src/details.js';
import { APPROVAL_UNVERIFIED, WORKSPACE_NODE_ID } from '../src/nodes.js';
import { adaptWithReport, nodesOf, SseDecoder } from '../src/upstream.js';

const ENV = 'RHIZOME_UNVERIFIED_DECIDED_MAX';
const originalEnv = process.env[ENV];

beforeEach(() => {
  delete process.env[ENV];
});
afterEach(() => {
  if (originalEnv === undefined) delete process.env[ENV];
  else process.env[ENV] = originalEnv;
});

type TestGate = NonNullable<WireDetailBody['gates']>[number] & {
  missionId?: string;
  goalId?: string;
  name?: string;
};

const gate = (id: string, extra: Partial<TestGate> = {}): TestGate => ({
  id,
  state: 'approved',
  superseded: false,
  body: 'request body',
  decisionReason: 'approved',
  ...extra,
});

const bodyOf = (gates: TestGate[], extra: Record<string, unknown> = {}) => ({
  missions: [],
  tasks: [],
  gates,
  deliverables: [],
  edges: [],
  counts: { running: 0, needsYou: 0, blocked: 0 },
  attention: [],
  capabilities: {},
  gateCapabilities: {},
  ...extra,
});

describe('FR-RHZ-133 approval provenance adapter round', () => {
  it('A1: decided detail uses exact status strings, appends assurance, and inserts status before decision', () => {
    const body = bodyOf([
      gate('relayed', { state: 'approved', verification: { status: 'claimed', claimKind: 'relayed' } }),
      gate('direct', { state: 'rejected', verification: { status: 'claimed', claimKind: 'session-direct', assurance: 'operator-present' } }),
      gate('claimed-missing-kind', { verification: { status: 'claimed' } }),
      gate('claimed-unknown-kind', { verification: { status: 'claimed', claimKind: 'future-kind' } }),
      gate('verified', { verification: { status: 'verified' } }),
      gate('verified-assured', { verification: { status: 'verified', assurance: 'hardware-backed' } }),
      gate('legacy', { verification: { status: 'legacy-asserted' } }),
      gate('missing'),
    ]);

    expect(detailItems(body, 'relayed')).toEqual([
      { label: 'request', text: 'request body' },
      { label: '승인 상태', text: 'claimed (relayed)' },
      { label: 'decision', text: 'approved' },
    ]);
    expect(detailItems(body, 'direct')).toEqual([
      { label: 'request', text: 'request body' },
      { label: '승인 상태', text: 'claimed (session-direct) (operator-present)' },
      { label: 'decision', text: 'approved' },
    ]);
    expect(detailItems(body, 'claimed-missing-kind')?.find((item) => item.label === '승인 상태')).toEqual({
      label: '승인 상태', text: 'claimed',
    });
    expect(detailItems(body, 'claimed-unknown-kind')?.find((item) => item.label === '승인 상태')).toEqual({
      label: '승인 상태', text: 'claimed',
    });
    expect(detailItems(body, 'verified')?.find((item) => item.label === '승인 상태')).toEqual({
      label: '승인 상태', text: 'verified',
    });
    expect(detailItems(body, 'verified-assured')?.find((item) => item.label === '승인 상태')).toEqual({
      label: '승인 상태', text: 'verified (hardware-backed)',
    });
    expect(detailItems(body, 'legacy')?.find((item) => item.label === '승인 상태')).toEqual({
      label: '승인 상태', text: 'legacy-asserted',
    });
    expect(detailItems(body, 'missing')?.find((item) => item.label === '승인 상태')).toEqual({
      label: '승인 상태', text: 'unverified',
    });
  });

  it('A2: pending and changes-requested gates have no approval-status item', () => {
    const body = bodyOf([
      gate('pending', {
        state: 'pending',
        verification: { status: 'claimed', claimKind: 'relayed' },
        decisionReason: 'draft only',
      }),
      gate('changes', {
        state: 'changes_requested',
        verification: { status: 'claimed', claimKind: 'relayed' },
        decisionReason: 'revise it',
      }),
    ]);
    const items = detailItems(body, 'pending')!;
    expect(items.map((item) => item.label)).not.toContain('승인 상태');
    expect(items.map((item) => item.label)).not.toContain('decision');
    expect(detailItems(body, 'changes')).toEqual([
      { label: 'request', text: 'request body' },
      { label: 'decision', text: 'revise it' },
    ]);
  });

  it('A3: decided unverified is retained with one attention cause and empty capabilities', () => {
    const raw = bodyOf(
      [gate('unverified')],
      {
        attention: [
          { kind: APPROVAL_UNVERIFIED, refId: 'unverified' },
          { kind: APPROVAL_UNVERIFIED, refId: 'unverified' },
        ],
        gateCapabilities: {
          unverified: { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
        },
      },
    );
    const node = nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope).find((n) => n.id === 'unverified')!;

    expect(node).toBeDefined();
    expect(node.attention).toEqual([{ cause: APPROVAL_UNVERIFIED }]);
    expect(node.capabilities).toEqual([]);
  });

  it('A3b: a rejected unverified gate is retained', () => {
    const raw = bodyOf([gate('rejected', { state: 'rejected', decisionReason: 'no' })]);
    const node = nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope).find((n) => n.id === 'rejected');

    expect(node).toMatchObject({
      id: 'rejected',
      state: { value: 'rejected' },
      attention: [{ cause: APPROVAL_UNVERIFIED }],
      capabilities: [],
    });
  });

  it('A4: decided claimed gates, including both current claim kinds, remain dropped', () => {
    const raw = bodyOf([
      gate('relayed', { verification: { status: 'claimed', claimKind: 'relayed' } }),
      gate('direct', { verification: { status: 'claimed', claimKind: 'session-direct' } }),
    ]);
    expect(nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope).map((n) => n.id)).toEqual([WORKSPACE_NODE_ID]);
  });

  it('A4b: superseded, unknown-status, and verified decided gates remain dropped without attention', () => {
    const raw = bodyOf(
      [
        gate('superseded', { superseded: true }),
        gate('unknown', { verification: { status: 'future-status' } }),
        gate('verified', { verification: { status: 'verified' } }),
      ],
      {
        attention: [
          { kind: APPROVAL_UNVERIFIED, refId: 'unknown' },
          { kind: APPROVAL_UNVERIFIED, refId: 'verified' },
        ],
      },
    );
    const result = adaptWithReport({ revision: 1, body: raw });
    const nodes = nodesOf(result.envelope);

    expect(nodes.map((node) => node.id)).toEqual([WORKSPACE_NODE_ID]);
    expect(nodes.flatMap((node) => node.attention).map((attention) => attention.cause)).not.toContain(APPROVAL_UNVERIFIED);
    expect(result.report.attentionDropped).toBe(2);
    expect(detailItems(raw, WORKSPACE_NODE_ID)?.map((item) => item.label)).not.toContain('승인 미확인 결정');
  });

  it('A4c: malformed verification is treated as absent for retention, attention, and root count', () => {
    const raw = bodyOf([
      gate('malformed', { verification: { status: 7 } as never }),
    ]);
    const nodes = nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope);
    const malformed = nodes.find((node) => node.id === 'malformed');

    expect(malformed?.attention).toEqual([{ cause: APPROVAL_UNVERIFIED }]);
    expect(detailItems(raw, WORKSPACE_NODE_ID)?.at(-1)).toEqual({ label: '승인 미확인 결정', text: '1' });
  });

  it('A4d: a decided-unverified gate under an achieved goal is neither retained nor counted', () => {
    const raw = bodyOf(
      [gate('dangling', { goalId: 'goal-done' })],
      { missions: [{ id: 'goal-done', name: 'Done', state: 'achieved', attention: false }] },
    );
    const nodes = nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope);

    expect(nodes.map((node) => node.id)).toEqual([WORKSPACE_NODE_ID]);
    expect(detailItems(raw, WORKSPACE_NODE_ID)?.map((item) => item.label)).not.toContain('승인 미확인 결정');
  });

  it('A5: 12 decided-unverified gates retain the last 10 while root detail reports all 12', () => {
    const gates = Array.from({ length: 12 }, (_, i) => gate(`gate-${String(i).padStart(2, '0')}`));
    const raw = bodyOf(gates);
    const ids = nodesOf(adaptWithReport({ revision: 3, body: raw }).envelope)
      .filter((n) => n.kind === 'gate')
      .map((n) => n.id);

    expect(ids).toEqual(gates.slice(2).map((g) => g.id));
    expect(detailItems(raw, WORKSPACE_NODE_ID)?.at(-1)).toEqual({ label: '승인 미확인 결정', text: '12' });
  });

  it('A6: RHIZOME_UNVERIFIED_DECIDED_MAX overrides the default cap', () => {
    process.env[ENV] = '3';
    const gates = Array.from({ length: 6 }, (_, i) => gate(`gate-${i}`));
    const ids = nodesOf(adaptWithReport({ revision: 1, body: bodyOf(gates) }).envelope)
      .filter((n) => n.kind === 'gate')
      .map((n) => n.id);
    expect(ids).toEqual(['gate-3', 'gate-4', 'gate-5']);
  });

  it('A7: every node in a mixed retained projection passes the vendored contract validator', () => {
    const raw = bodyOf([
      gate('unverified'),
      gate('legacy', { verification: { status: 'legacy-asserted' } }),
      gate('claimed', { verification: { status: 'claimed', claimKind: 'relayed' } }),
      gate('pending', { state: 'pending' }),
    ]);
    for (const node of nodesOf(adaptWithReport({ revision: 2, body: raw }).envelope)) {
      expect(nodeProblem(node), node.id).toBeNull();
    }
  });

  it('A8: a verification-free workspace without unverified decisions is deep-equal and byte-identical to pre-change output', () => {
    const raw = {
      missions: [{ id: 'goal-1', name: 'Goal', attention: false, state: 'active' }],
      tasks: [],
      gates: [{ id: 'gate-pending', state: 'pending', superseded: false, missionId: 'goal-1' }],
      deliverables: [],
      edges: [],
      counts: { running: 0, needsYou: 1, blocked: 0 },
      attention: [],
      capabilities: {},
      gateCapabilities: {
        'gate-pending': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
      },
    };
    const actual = adaptWithReport({ revision: 8, body: raw });
    const preChange = {
      envelope: {
        revision: 8,
        body: {
          revision: 0,
          missions: [{ kind: 'mission', id: 'goal-1', name: 'Goal', attention: false, state: 'active' }],
          tasks: [],
          gates: [{ kind: 'gate', id: 'gate-pending', missionId: 'goal-1', goalId: '', name: 'gate-pending', gateType: '', requestedAction: '', state: 'waiting' }],
          deliverables: [],
          edges: [],
          containedIds: [],
          counts: { running: 0, needsYou: 1, blocked: 0 },
          activities: [],
          sessions: [],
          capabilities: {},
          gateCapabilities: raw.gateCapabilities,
          effects: [],
          nodes: [
            {
              id: WORKSPACE_NODE_ID,
              kind: 'workspace',
              label: 'Workspace',
              state: { value: 'unstated' },
              relations: [],
              capabilities: [{ action: 'mission.create', level: 'enabled', decision: { input: { required: true } } }],
              attention: [],
              artifacts: [],
            },
            {
              id: 'goal-1', kind: 'goal', label: 'Goal', state: { value: 'active' }, relations: [], capabilities: [], attention: [], artifacts: [],
            },
            {
              id: 'gate-pending', kind: 'gate', state: { value: 'waiting' }, relations: [{ type: 'member-of', target: 'goal-1' }],
              capabilities: [
                { action: 'gate.approve', level: 'enabled' },
                { action: 'gate.reject', level: 'enabled' },
                { action: 'gate.requestChanges', level: 'enabled', decision: { input: { required: true } } },
              ],
              attention: [{ cause: 'needs_human' }], artifacts: [],
            },
          ],
        },
      },
      report: { attentionDropped: 0 },
    };

    expect(actual).toEqual(preChange);
    expect(JSON.stringify(actual)).toBe(JSON.stringify(preChange));
  });

  it('A9: GET/SSE-shaped ingestion and detail/projection output are deterministic', () => {
    const envelope = { revision: 9, body: bodyOf([
      gate('unverified'),
      gate('claimed', { verification: { status: 'claimed', claimKind: 'session-direct' } }),
    ]) };
    const decoder = new SseDecoder();
    const [frame] = decoder.push(`event: projection\ndata: ${JSON.stringify(envelope)}\n\n`);
    const fromSse = JSON.parse(frame!.data) as typeof envelope;

    expect(adaptWithReport(fromSse)).toEqual(adaptWithReport(envelope));
    expect(adaptWithReport(envelope)).toEqual(adaptWithReport(envelope));
    expect(detailOf(envelope, 'unverified')).toEqual(detailOf(envelope, 'unverified'));
  });
});
