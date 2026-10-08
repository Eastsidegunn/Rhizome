// RHZ-072 (FR-RHZ-101): node detail assembly from the Rhizome /v1/workspace
// snapshot (pure function). D1 gate, D2 workspace root, D3 task, D4 mission
// (goal), D5 deliverable/unknown → none + revision. Every served detail must
// hold the contract shape (validateNodeDetail, @gunnflow/contract 0.2.0).
import { describe, expect, it } from 'vitest';
import { validateNodeDetail } from '@gunnflow/contract';
import { detailItems, detailOf } from '../src/details.js';
import { WORKSPACE_NODE_ID } from '../src/nodes.js';

const BODY = '# Publish?\n\nRelease **v3** to prod.\n\n- evidence: 41/42 tests';

/** A wire body exactly as Rhizome's workspace/http.go emits it (gate fields per RHZ-047/050). */
const wireBody = {
  missions: [
    { id: 'goal-a', name: 'ship v3', attention: false, state: 'active', success: 'v3 live on prod' },
    { id: 'goal-b', name: 'no criterion', attention: false, state: 'active' },
    { id: 'goal-empty', name: '', attention: false, state: 'active' },
  ],
  tasks: [
    { id: 'm-full', missionId: 'goal-a', name: 'build', state: 'running', currentAction: 'compiling', progress: 0.5, hasProgress: true, blockedReason: 'disk full', attention: false },
    { id: 'm-action', missionId: 'goal-a', name: 'draft', state: 'running', currentAction: 'writing', hasProgress: false, attention: false },
    { id: 'm-progress', missionId: 'goal-a', name: 'test', state: 'running', progress: 0, hasProgress: true, attention: false },
    { id: 'm-noprogress', missionId: 'goal-a', name: 'test2', state: 'running', progress: 0.4, hasProgress: false, attention: false },
    { id: 'm-blocked', missionId: 'goal-a', name: 'wait', state: 'blocked', blockedReason: 'waiting for gate', hasProgress: false, attention: true },
    { id: 'm-bare', missionId: 'goal-a', name: 'bare', state: 'queued', hasProgress: false, attention: false },
    { id: 'm-nothing', missionId: 'goal-a', name: 'nothing', state: '', hasProgress: false, attention: false },
  ],
  gates: [
    { id: 'q-pending', state: 'pending', superseded: false, name: 'Publish?', requestDigest: 'sha256:abc', body: BODY, recommendation: 'approve' },
    { id: 'q-approved', state: 'approved', superseded: false, name: 'Publish?', requestDigest: 'sha256:abc', body: BODY, recommendation: 'approve', decisionReason: 'looks good', decidedBy: 'operator' },
    { id: 'q-pending-draft', state: 'pending', superseded: false, body: BODY, decisionReason: 'draft reason', decidedBy: '' },
    { id: 'q-bare', state: 'pending', superseded: false },
  ],
  deliverables: [{ id: 'd-1', kind: 'document', missionId: 'm-full', sourceRef: 'sha256:deadbeef', summary: 'Report' }],
  edges: [],
  counts: { running: 4, needsYou: 1, blocked: 2 },
  attention: [],
};
const wire = { revision: 42, body: wireBody };

const validated = (nodeId: string, rootId?: string) => {
  const d = detailOf(wire, nodeId, rootId);
  expect(d, nodeId).toBeDefined();
  const r = validateNodeDetail(d);
  expect(r.ok, `${nodeId}: ${r.ok ? '' : r.problems.join('; ')}`).toBe(true);
  return d!;
};

describe('D1 gate detail (FRRHZ101)', () => {
  it('pending gate: request/recommendation/digest as text, decision/decidedBy omitted', () => {
    const d = validated('q-pending');
    expect(d.items).toEqual([
      { label: 'request', text: BODY },
      { label: 'recommendation', text: 'approve' },
      { label: 'digest', text: 'sha256:abc' },
    ]);
    expect(d.items.map((i) => i.label)).not.toContain('decision');
    expect(d.items.map((i) => i.label)).not.toContain('decidedBy');
    for (const i of d.items) expect(i.artifact).toBeUndefined();
  });

  it('approved gate: decision and decidedBy included, in order', () => {
    const d = validated('q-approved');
    expect(d.items).toEqual([
      { label: 'request', text: BODY },
      { label: 'recommendation', text: 'approve' },
      { label: '승인 상태', text: 'unverified' },
      { label: 'decision', text: 'looks good' },
      { label: 'decidedBy', text: 'operator' },
      { label: 'digest', text: 'sha256:abc' },
    ]);
  });

  it('a pending gate never shows a decision; empty strings are omitted', () => {
    const d = validated('q-pending-draft');
    expect(d.items).toEqual([{ label: 'request', text: BODY }]);
  });

  it('a gate with no fields at all has no detail', () => {
    expect(detailOf(wire, 'q-bare')).toBeUndefined();
  });
});

describe('D2 workspace root detail (FRRHZ101)', () => {
  it('counts and the uncapped unverified-decision count become text items in fixed order', () => {
    const d = validated(WORKSPACE_NODE_ID);
    expect(d.items).toEqual([
      { label: 'running', text: '4' },
      { label: 'needsYou', text: '1' },
      { label: 'blocked', text: '2' },
      { label: '승인 미확인 결정', text: '1' },
    ]);
    for (const i of d.items) expect(typeof i.text).toBe('string');
  });

  it('honours the collision-shifted root id and retains the provenance count without status counts', () => {
    expect(validated('~~workspace', '~~workspace').items).toHaveLength(4);
    expect(detailOf(wire, WORKSPACE_NODE_ID, '~~workspace')).toBeUndefined();
    expect(detailOf({ revision: 1, body: { ...wireBody, counts: undefined } }, WORKSPACE_NODE_ID)?.items).toEqual([
      { label: '승인 미확인 결정', text: '1' },
    ]);
  });

  it('has no root detail when neither status counts nor unverified decisions exist', () => {
    expect(detailOf({ revision: 1, body: { gates: [] } }, WORKSPACE_NODE_ID)).toBeUndefined();
  });
});

describe('D3 task detail (FRRHZ101)', () => {
  it('all three present: currentAction, progress (string), blockedReason, state', () => {
    expect(validated('m-full').items).toEqual([
      { label: 'currentAction', text: 'compiling' },
      { label: 'progress', text: '0.5' },
      { label: 'blockedReason', text: 'disk full' },
      { label: 'state', text: 'running' },
    ]);
  });

  it('currentAction only', () => {
    expect(validated('m-action').items).toEqual([
      { label: 'currentAction', text: 'writing' },
      { label: 'state', text: 'running' },
    ]);
  });

  it('progress only when hasProgress — 0 is a value, a progress number without hasProgress is not', () => {
    expect(validated('m-progress').items).toEqual([
      { label: 'progress', text: '0' },
      { label: 'state', text: 'running' },
    ]);
    expect(validated('m-noprogress').items).toEqual([{ label: 'state', text: 'running' }]);
  });

  it('blockedReason only', () => {
    expect(validated('m-blocked').items).toEqual([
      { label: 'blockedReason', text: 'waiting for gate' },
      { label: 'state', text: 'blocked' },
    ]);
  });

  it('none of the three: only state remains; with nothing at all → no detail (never empty items)', () => {
    expect(validated('m-bare').items).toEqual([{ label: 'state', text: 'queued' }]);
    expect(detailOf(wire, 'm-nothing')).toBeUndefined();
    expect(detailItems(wireBody, 'm-nothing')).toEqual([]);
  });
});

describe('D4 mission (goal) detail (FRRHZ101)', () => {
  it('success criterion (RHZ-067) only — the name is already the node label', () => {
    expect(validated('goal-a').items).toEqual([{ label: 'success', text: 'v3 live on prod' }]);
  });

  it('without success → no detail (no description item: goal DTO has only name)', () => {
    expect(detailOf(wire, 'goal-b')).toBeUndefined();
    expect(detailOf(wire, 'goal-empty')).toBeUndefined();
  });
});

describe('D5 deliverable, unknown node, revision (FRRHZ101)', () => {
  it('deliverable → no detail (its artifacts are already in the projection)', () => {
    expect(detailOf(wire, 'd-1')).toBeUndefined();
    expect(detailItems(wireBody, 'd-1')).toBeUndefined();
  });

  it('unknown node → no detail', () => {
    expect(detailOf(wire, 'nope')).toBeUndefined();
    expect(detailOf({ revision: 3, body: null }, 'q-pending')).toBeUndefined();
  });

  it('revision is the snapshot revision, per snapshot', () => {
    expect(validated('q-pending').revision).toBe(42);
    expect(detailOf({ revision: 7, body: wireBody }, 'q-pending')!.revision).toBe(7);
    expect(detailOf({ revision: 7, body: wireBody }, WORKSPACE_NODE_ID)!.revision).toBe(7);
    expect(detailOf({ revision: 7, body: wireBody }, 'm-full')!.revision).toBe(7);
  });
});
