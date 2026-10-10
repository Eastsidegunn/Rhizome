// RHZ-132 (FR-RHZ-171): human-owned attention on the cockpit — blocked notes
// projected as `note` nodes, gate_pending / request_waiting carried on their
// existing nodes (attentionDropped 0), note detail (full text + fallback),
// note.answer → Rhizome note.create {memoryKind: answer}, and the wiring draft.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { DIRECT_WIRE, nodeProblem, validateNodeDetail } from '@gunnflow/contract';
import {
  attentionGroup,
  attentionMechanism,
  actionLabel,
  coveredActions,
  detailCollapsed,
  renderFor,
  validateWiringConfig,
  type WiringConfig,
} from '@gunnflow/contract/wiring';
import { adaptWithReport, createRhizomeUpstream, nodesOf, noteAnswerIntent, NOTE_ANSWER_TEXT_REQUIRED, NOTE_NOT_BLOCKED } from '../src/upstream.js';
import { detailItems } from '../src/details.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';

const FULL = '첫 줄 막힘: 키가 필요\n\n둘째 줄 상세 설명\n- 항목';
const body = {
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: [
    { id: 'task', missionId: 'goal', name: 'task', state: 'running', hasProgress: false, attention: false },
    { id: 'task-gone', missionId: 'goal', name: 'gone', state: 'completed', hasProgress: false, attention: false },
  ],
  gates: [{ id: 'q-1', state: 'pending', superseded: false, name: 'ship?', missionId: 'task', source: 'internal', requestDigest: 'rhz-question-v1:x' }],
  deliverables: [],
  edges: [],
  counts: { running: 1, needsYou: 4, blocked: 0 },
  attention: [
    { kind: 'gate_pending', refId: 'q-1', cause: 'ship?', missionId: 'task' },
    { kind: 'note_blocked', refId: 'note-a', cause: '첫 줄 막힘: 키가 필요', missionId: 'task' },
    // Its mission is not on the wire (hidden): the note stays, without member-of.
    { kind: 'note_blocked', refId: 'note-b', cause: 'orphan block', missionId: 'task-hidden' },
    { kind: 'request_waiting', refId: 'r-1', cause: 'hand task', missionId: 'task' },
  ],
  capabilities: {},
  gateCapabilities: { 'q-1': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' } },
  requests: [{ id: 'r-1', name: 'hand task', state: 'waiting' as const, missionId: 'task', createdAt: '2026-10-10T01:02:03Z' }],
  requestCapabilities: { 'r-1': { complete: 'enabled', unable: 'enabled' } },
};

describe('note_blocked projection FR-RHZ-171', () => {
  const { envelope, report } = adaptWithReport({ revision: 3, body });
  const nodes = nodesOf(envelope);
  const byId = (id: string) => nodes.find((n) => n.id === id);

  it('FR-RHZ-171 projects each blocked note as a contract-valid note node', () => {
    const a = byId('note-a')!;
    expect(a).toEqual({
      id: 'note-a',
      kind: 'note',
      label: '첫 줄 막힘: 키가 필요',
      state: { value: 'blocked' },
      relations: [{ type: 'member-of', target: 'task' }],
      capabilities: [{ action: 'note.answer', level: 'enabled', decision: { input: { required: true } } }],
      attention: [{ cause: 'note_blocked' }],
      artifacts: [],
    });
    expect(nodeProblem(a)).toBeNull();
    // Mission not visible → no relation naming a hidden node.
    expect(byId('note-b')!.relations).toEqual([]);
  });

  it('FR-RHZ-171 attentionDropped is 0: gate_pending and request_waiting ride on existing nodes', () => {
    expect(report.attentionDropped).toBe(0);
    expect(byId('q-1')!.attention).toEqual([{ cause: 'gate_pending' }, { cause: 'needs_human' }]);
    expect(byId('r-1')!.attention).toEqual([{ cause: 'request_waiting' }, { cause: 'needs_human_action', since: '2026-10-10T01:02:03Z' }]);
  });

  it('FR-RHZ-171 a body without note_blocked keeps the projection free of blockedNotes', () => {
    const plain = adaptWithReport({ revision: 1, body: { ...body, attention: [] } }).envelope;
    expect(plain.body).not.toHaveProperty('blockedNotes');
    expect(nodesOf(plain).some((n) => n.kind === 'note')).toBe(false);
  });

  it('FR-RHZ-171 snapshot detail of a note is its cause line', () => {
    expect(detailItems(body, 'note-a')).toEqual([{ label: 'note', text: '첫 줄 막힘: 키가 필요' }]);
    expect(detailItems(body, 'note-zz')).toBeUndefined();
  });
});

describe('note.answer translation FR-RHZ-171', () => {
  const wire = { revision: 3, body };

  it('FR-RHZ-171 maps decision.text to note.create {memoryKind: answer} on the note mission', () => {
    expect(noteAnswerIntent(wire, 'note-a', { text: '키는 금고에' })).toEqual({
      ok: true,
      fields: { kind: 'note.create', memoryKind: 'answer', missionId: 'task', tags: ['answer', 're:note-a'], content: 're: note-a\n\n키는 금고에' },
    });
  });

  it('FR-RHZ-171 the same answer text to two notes yields distinct contents (content-addressed ids never collide)', () => {
    const two = { revision: 3, body: { ...body, attention: [...(body.attention as unknown[]), { kind: 'note_blocked', refId: 'note-c', cause: 'c', missionId: 'task' }] } };
    const a = noteAnswerIntent(two, 'note-a', { text: '확인' });
    const c = noteAnswerIntent(two, 'note-c', { text: '확인' });
    expect(a.ok && c.ok).toBe(true);
    if (a.ok && c.ok) expect(a.fields.content).not.toEqual(c.fields.content);
  });

  it('FR-RHZ-171 rejects empty text and notes the snapshot does not list', () => {
    expect(noteAnswerIntent(wire, 'note-a', { text: '   ' })).toEqual({ ok: false, reason: NOTE_ANSWER_TEXT_REQUIRED });
    expect(noteAnswerIntent(wire, 'note-a', undefined)).toEqual({ ok: false, reason: NOTE_ANSWER_TEXT_REQUIRED });
    expect(noteAnswerIntent(wire, 'q-1', { text: 'x' })).toEqual({ ok: false, reason: NOTE_NOT_BLOCKED });
  });
});

describe('note detail and answer over the wire FR-RHZ-171', () => {
  const mock = Fastify({ logger: false });
  const received: Array<Record<string, unknown>> = [];
  let contextFails = false;
  let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;
  let upstream: Awaited<ReturnType<typeof createRhizomeUpstream>>;

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => ({ revision: 3, body }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 3, body })}\n\n`);
    });
    mock.get('/v1/context', async (req, reply) => {
      const q = req.query as Record<string, string>;
      if (contextFails) return reply.code(500).send('boom');
      if (q.mission !== 'task') return reply.code(404).send('unknown');
      return { mission: { id: 'task', name: 'task', state: 'running' }, memories: [
        { id: 'note-other', kind: 'fact', content: 'unrelated', sourceId: 's', tags: [], seq: 2 },
        { id: 'note-a', kind: 'blocked', content: FULL, sourceId: 's', tags: ['blocked'], seq: 1 },
      ] };
    });
    mock.post('/v1/intent', async (req) => {
      received.push(req.body as Record<string, unknown>);
      return { Accepted: true, Reason: '' };
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const address = mock.server.address();
    const url = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
    server = await startRhizomeDirectServer({ rhizomeUrl: url, port: 0 });
    upstream = await createRhizomeUpstream(url);
  });
  afterAll(async () => {
    upstream.close();
    await server.close();
    await mock.close();
  });

  it('FR-RHZ-171 GET /detail/<note> answers the full note text from /v1/context', async () => {
    contextFails = false;
    const res = await fetch(`${server.url}${DIRECT_WIRE.detail}/note-a`);
    expect(res.status).toBe(200);
    const detail = await res.json();
    expect(validateNodeDetail(detail).ok).toBe(true);
    expect(detail).toEqual({ revision: 3, items: [{ label: 'note', text: FULL }] });
  });

  it('FR-RHZ-171 falls back to the cause line when /v1/context fails or lacks the note', async () => {
    contextFails = true;
    const res = await fetch(`${server.url}${DIRECT_WIRE.detail}/note-a`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ revision: 3, items: [{ label: 'note', text: '첫 줄 막힘: 키가 필요' }] });
    contextFails = false;
    const missing = await fetch(`${server.url}${DIRECT_WIRE.detail}/note-b`);
    expect(missing.status).toBe(200);
    expect(await missing.json()).toEqual({ revision: 3, items: [{ label: 'note', text: 'orphan block' }] });
  });

  it('FR-RHZ-171 relayIntent note.answer posts note.create answer; empty text never reaches Rhizome', async () => {
    const ok = await upstream.relayIntent(
      { nodeId: 'note-a', action: 'note.answer', decision: { text: '키는 금고에' }, idempotencyKey: 'answer-1' },
      'fake-actor:h',
    );
    expect(ok).toEqual({ accepted: true, reason: undefined });
    expect(received.at(-1)).toEqual({
      kind: 'note.create', memoryKind: 'answer', missionId: 'task', tags: ['answer', 're:note-a'], content: 're: note-a\n\n키는 금고에', actor: 'fake-actor:h',
    });
    const before = received.length;
    const empty = await upstream.relayIntent(
      { nodeId: 'note-a', action: 'note.answer', decision: { text: '' }, idempotencyKey: 'answer-2' },
      'fake-actor:h',
    );
    expect(empty).toEqual({ accepted: false, reason: NOTE_ANSWER_TEXT_REQUIRED });
    const missing = await upstream.relayIntent({ nodeId: 'note-a', action: 'note.answer', idempotencyKey: 'answer-3' }, 'fake-actor:h');
    expect(missing.accepted).toBe(false);
    expect(received.length).toBe(before);
  });
});

describe('wiring draft FR-RHZ-171', () => {
  const draft = JSON.parse(readFileSync(join(import.meta.dirname, '..', 'wiring', 'rhizome.wiring.draft.json'), 'utf8')) as WiringConfig;

  it('FR-RHZ-171 validates and groups the human-owned causes', () => {
    expect(validateWiringConfig(draft)).toEqual({ ok: true, config: draft });
    for (const [cause, group] of [
      ['needs_human', '결정'], ['gate_pending', '결정'],
      ['needs_human_action', '할 일'], ['request_waiting', '할 일'],
      ['note_blocked', '차단'],
    ] as const) {
      expect(attentionMechanism(draft, cause), cause).toBe('interrupt');
      expect(attentionGroup(draft, cause), cause).toBe(group);
    }
    expect(attentionMechanism(draft, 'waiting_for_human')).toBe('interrupt');
    expect(attentionMechanism(draft, 'flagged')).toBe('ambient');
  });

  it('FR-RHZ-171 note kind answers via note.answer, labelled and collapsible', () => {
    expect(coveredActions(draft, 'note')).toEqual(['note.answer']);
    expect(actionLabel(draft, 'note.answer')).toBe('답하기');
    expect(detailCollapsed(draft)).toContain('note');
    expect(renderFor(draft, 'blocked')).toBeDefined();
  });

  it('FR-RHZ-171 glyphs are printable characters, not icon words', () => {
    for (const [state, r] of Object.entries(draft.render ?? {})) {
      expect(/^[a-z]+$/.test(r.glyph), `${state}: ${r.glyph}`).toBe(false);
    }
  });
});
