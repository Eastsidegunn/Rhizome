// RHZ-149 (FR-RHZ-193): lifecycle on the cockpit — mission_done_review and
// stale attention ride on their task / note nodes and are grouped as
// decisions ('완료 확인' · '오래 멈춤'); Rhizome's deliverable archived:true is
// the node state 'archived', which the 'deliverables' lens leaves out.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { attentionGroup, attentionMechanism, renderFor, validateWiringConfig, type WiringConfig } from '@gunnflow/contract/wiring';
import { adaptWithReport, nodesOf } from '../src/upstream.js';

const body = {
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: [
    { id: 'task', missionId: 'goal', name: 'task', state: 'waiting', hasProgress: false, attention: false },
    { id: 'task-old', missionId: 'goal', name: 'old', state: 'running', hasProgress: false, attention: false },
    { id: 'task-done', missionId: 'goal', name: 'done', state: 'completed', hasProgress: false, attention: false },
  ],
  gates: [],
  deliverables: [
    { id: 'd-live', kind: 'code', missionId: 'task', sourceRef: 'exec-1', summary: 'live' },
    { id: 'd-arch', kind: 'code', missionId: 'task-done', sourceRef: 'exec-2', summary: 'archived', archived: true },
  ],
  edges: [],
  counts: { running: 1, needsYou: 2, blocked: 0 },
  attention: [
    { kind: 'mission_done_review', refId: 'task', cause: '결과 한 줄 · $0.42', missionId: 'task' },
    { kind: 'stale', refId: 'task-old', cause: '10일 멈춤: old', missionId: 'task-old' },
  ],
  capabilities: {},
  gateCapabilities: {},
};

describe('lifecycle cards and archived deliverables FR-RHZ-193', () => {
  const { envelope, report } = adaptWithReport({ revision: 7, body });
  const nodes = nodesOf(envelope);
  const byId = (id: string) => nodes.find((n) => n.id === id);

  it('FR-RHZ-193 carries mission_done_review and stale on their nodes (nothing dropped)', () => {
    expect(report.attentionDropped).toBe(0);
    expect(byId('task')?.attention.map((a) => a.cause)).toContain('mission_done_review');
    expect(byId('task-old')?.attention.map((a) => a.cause)).toContain('stale');
  });

  it('FR-RHZ-193 archived:true is the deliverable state archived; others stay unstated', () => {
    expect(byId('d-arch')?.state.value).toBe('archived');
    expect(byId('d-live')?.state.value).toBe('unstated');
  });

  const draft = JSON.parse(readFileSync(join(import.meta.dirname, '..', 'wiring', 'rhizome.wiring.draft.json'), 'utf8')) as WiringConfig;

  it('FR-RHZ-193 wiring draft validates, groups the two causes and lenses out archived', () => {
    expect(validateWiringConfig(draft)).toEqual({ ok: true, config: draft });
    expect(attentionMechanism(draft, 'mission_done_review')).toBe('interrupt');
    expect(attentionGroup(draft, 'mission_done_review')).toBe('완료 확인');
    expect(attentionMechanism(draft, 'stale')).toBe('interrupt');
    expect(attentionGroup(draft, 'stale')).toBe('오래 멈춤');
    expect(renderFor(draft, 'archived')).toBeDefined();
    const lens = draft.lenses?.find((l) => l.id === 'deliverables');
    expect(lens?.label).toBe('Deliverables');
    expect(lens?.match.kinds).toEqual(['deliverable']);
    expect(lens?.match.states).toContain(byId('d-live')?.state.value);
    expect(lens?.match.states).not.toContain(byId('d-arch')?.state.value);
  });
});
