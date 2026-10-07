// Rhizome v1 workspace DTOs map onto the cockpit's projection shape without
// invention; already-adapted items pass through untouched.
import { describe, expect, it } from 'vitest';
import { adaptWorkspaceBody } from '../src/workspaceWire.js';

/** A wire body exactly as Rhizome's workspace/http.go emits it. */
const wireBody = {
  missions: [{ id: 'goal-demo', name: 'demo', attention: false }],
  tasks: [
    {
      id: 'mission-demo',
      missionId: 'goal-demo',
      name: 'demo',
      state: 'running',
      currentAction: 'writing draft',
      hasProgress: false,
      attention: false,
    },
    {
      id: 'mission-x',
      missionId: 'goal-demo',
      name: 'x',
      state: 'blocked',
      blockedReason: 'upstream says so',
      progress: 0.4,
      hasProgress: true,
      attention: true,
    },
  ],
  gates: [
    { id: 'approval-1', state: 'pending', superseded: false },
    { id: 'approval-0', state: 'approved', humanDecision: 'allow', janusDecision: 'allow', superseded: true },
  ],
  deliverables: [
    // A deliverable's missionId is a mission id, which in /v1/workspace equals a
    // task id (a task IS its mission in the projection). Using the goal id made
    // the live filter drop it; the real convention is the task/mission id.
    { id: 'd-1', kind: 'document', missionId: 'mission-demo', sourceRef: 'blob:abc', summary: 'Report v1' },
  ],
  edges: [
    {
      id: 'e-1',
      from: { type: 'mission', id: 'mission-demo' },
      to: { type: 'deliverable', id: 'd-1' },
      edgeKind: 'produces',
      actor: 'op',
      correlation: 'relay',
    },
  ],
  counts: { running: 1, needsYou: 0, blocked: 1 },
  attention: [{ kind: 'waiting_for_human', refId: 'mission-x', cause: 'mission waiting' }],
};

describe('adaptWorkspaceBody (Rhizome v1 wire)', () => {
  const p = adaptWorkspaceBody(wireBody);

  it('maps missions/tasks with kind discriminators, verbatim fields', () => {
    expect(p.missions[0]).toEqual({ kind: 'mission', id: 'goal-demo', name: 'demo', attention: false });
    expect(p.tasks[0]!.kind).toBe('task');
    expect(p.tasks[0]!.currentAction).toBe('writing draft');
    expect(p.tasks[1]!.blockedReason).toBe('upstream says so');
  });

  it('B3: progress exists only when hasProgress says so', () => {
    expect(p.tasks[0]!.progress).toBeUndefined();
    expect(p.tasks[1]!.progress).toBe(0.4);
  });

  it('gates: pending→waiting surfaces; decided/superseded gates dropped (live 필터); minimal fields', () => {
    const waiting = p.gates.find((g) => g.id === 'approval-1')!;
    expect(waiting.state).toBe('waiting');
    expect(waiting.name).toBe('approval-1'); // id shown, no invented title
    expect(waiting.request).toBeUndefined();
    expect(waiting.riskTier).toBeUndefined();
    // live 필터: 조종석은 waiting 게이트만 올림 — approval-0(superseded)은 미노출.
    expect(p.gates.find((g) => g.id === 'approval-0')).toBeUndefined();
  });

  it('deliverables: summary→title, kind→type, NO lifecycle state invented', () => {
    const d = p.deliverables[0]!;
    expect(d.title).toBe('Report v1');
    expect(d.deliverableType).toBe('document');
    expect(d.state).toBeUndefined();
  });

  it('edges: endpoint objects flatten to ids', () => {
    expect(p.edges[0]).toMatchObject({ from: 'mission-demo', to: 'd-1', edgeKind: 'produces' });
  });

  it('v1 has no capability projection → server-accepted writes enabled, privileged absent', () => {
    const caps = p.capabilities['mission-demo']!;
    expect(caps.pause).toBe('enabled');
    expect(caps.instruct).toBe('enabled');
    expect(caps.cancel).toBeUndefined(); // stays hidden by the missing-→hidden rule
    expect(p.gateCapabilities).toEqual({}); // gate decisions are hidden in the projection
  });

  it('counts pass through from upstream', () => {
    expect(p.counts).toEqual({ running: 1, needsYou: 0, blocked: 1 });
  });
});


describe('adaptWorkspaceBody (already-adapted passthrough)', () => {
  it('cockpit-shaped items pass through structurally unchanged', () => {
    const adapted = {
      revision: 4,
      missions: [{ kind: 'mission', id: 'm1', name: 'Ship', attention: false }],
      tasks: [{ kind: 'task', id: 't1', missionId: 'm1', name: 'Draft', state: 'running', attention: false, progress: 0.2 }],
      gates: [{ kind: 'gate', id: 'g1', missionId: 'm1', name: 'Publish?', gateType: 'publish', requestedAction: 'Publish', state: 'waiting' }],
      deliverables: [{ kind: 'deliverable', id: 'd1', missionId: 't1', title: 'Report', deliverableType: 'document' }],
      edges: [{ id: 'e1', from: 't1', to: 'g1', edgeKind: 'gate' }],
      capabilities: { t1: { pause: 'enabled' } },
    };
    const p = adaptWorkspaceBody(adapted);
    expect(p.missions).toEqual(adapted.missions);
    expect(p.tasks).toEqual(adapted.tasks);
    expect(p.gates).toEqual(adapted.gates);
    expect(p.deliverables).toEqual(adapted.deliverables);
    expect(p.edges).toEqual(adapted.edges);
    expect(p.capabilities).toEqual(adapted.capabilities);
  });
});
