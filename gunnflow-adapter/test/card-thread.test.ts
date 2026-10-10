// RHZ-134 (FR-RHZ-175) "카드가 대화한다": the question card and gate thread
// in the gate detail, the gate.ask capability and its note.create mapping,
// note.answer carrying `re`, and the 0.6 gate summary from the card risk.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { nodeProblem, validateNodeDetail } from '@gunnflow/contract';
import { adaptWithReport, createRhizomeUpstream, gateAskIntent, GATE_ASK_TEXT_REQUIRED, GATE_NOT_INTERNAL, nodesOf } from '../src/upstream.js';
import { detailItems, detailOf } from '../src/details.js';
import { gateCardOf } from '../src/workspaceWire.js';

const card = {
  risk: { level: 'high', line: 'prod data may be lost' },
  ifSkipped: 'release slips a day',
  whyHuman: 'touches billing',
  options: [
    { key: 'go', label: 'Ship it', meaning: 'deploy now', consequence: 'billing migrates' },
    { key: 'wait', label: 'Wait' },
  ],
  rollback: 'revert the migration',
  context: { project: 'rhizome', what: 'migration', state: 'staged' },
  thread: [
    { id: 'note-q', kind: 'question', content: 're: q-1\n\nwhich env?', at: '2026-10-11T01:02:03.5Z' },
    { id: 'note-a', kind: 'answer', content: 'staging first' },
  ],
};
const plainGate = { id: 'q-1', state: 'pending', superseded: false, name: 'deploy?', missionId: 'task', source: 'internal', requestDigest: 'rhz-question-v2:x', body: '## 권고\napprove it\n\nrest text', recommendation: 'approve' };
const body = (gates: Array<{ id: string } & Record<string, unknown>>) => ({
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: [{ id: 'task', missionId: 'goal', name: 'task', state: 'running', hasProgress: false, attention: false }],
  gates,
  deliverables: [],
  edges: [],
  counts: { running: 1, needsYou: 1, blocked: 0 },
  attention: [],
  capabilities: {},
  gateCapabilities: Object.fromEntries(
    gates.filter((g) => g.source === 'internal').map((g) => [g.id, { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' }]),
  ),
});

describe('gate card detail FR-RHZ-175', () => {
  it('FR-RHZ-175 card items come first, then the body sections, then the thread', () => {
    const plain = detailItems(body([plainGate]), 'q-1')!;
    const carded = detailItems(body([{ ...plainGate, ...card }]), 'q-1')!;
    expect(carded.slice(0, 8)).toEqual([
      { label: '위험', text: 'high — prod data may be lost' },
      { label: '안 하면', text: 'release slips a day' },
      { label: '왜 사람이', text: 'touches billing' },
      { label: '선택지', text: 'Ship it\n의미: deploy now\n결과: billing migrates' },
      { label: '선택지', text: 'Wait' },
      { label: '되돌림', text: 'revert the migration' },
      { label: '맥락', text: 'rhizome · migration · staged' },
      plain[0],
    ]);
    // The existing items are untouched and in place; the thread trails them.
    expect(carded.slice(7, 7 + plain.length)).toEqual(plain);
    expect(carded.slice(7 + plain.length)).toEqual([
      { label: '질문 · 2026-10-11T01:02:03.5Z', text: 're: q-1\n\nwhich env?' },
      { label: '답', text: 'staging first' },
    ]);
    expect(validateNodeDetail(detailOf({ revision: 1, body: body([{ ...plainGate, ...card }]) }, 'q-1')).ok).toBe(true);
  });

  it('FR-RHZ-175 a gate without card keys has the same detail as before', () => {
    const withEmpties = { ...plainGate, options: [], thread: [], context: {} };
    expect(detailItems(body([withEmpties]), 'q-1')).toEqual(detailItems(body([plainGate]), 'q-1'));
    expect(JSON.stringify(detailItems(body([plainGate]), 'q-1'))).not.toContain('위험');
  });

  it('FR-RHZ-175 malformed card values are dropped, not fatal', () => {
    expect(gateCardOf({
      risk: { level: 'high' },
      ifSkipped: 7,
      whyHuman: '  ',
      options: [{ key: 'a' }, 'x', { key: 'b', label: 'B', meaning: 3 }],
      rollback: null,
      context: { project: 1, what: 'w' },
      thread: [{ id: 'n', kind: 'observation', content: 'x' }, { id: 'm', kind: 'answer', content: 'ok', at: 5 }],
    })).toEqual({ options: [{ key: 'b', label: 'B' }], context: { what: 'w' }, thread: [{ id: 'm', kind: 'answer', content: 'ok' }] });
    expect(gateCardOf(undefined)).toEqual({});
    expect(gateCardOf({ options: 'nope', thread: {} })).toEqual({});
  });
});

describe('gate.ask capability FR-RHZ-175', () => {
  const ask = { action: 'gate.ask', level: 'enabled', decision: { input: { required: true } } };
  const nodes = nodesOf(adaptWithReport({ revision: 2, body: body([
    plainGate,
    { ...plainGate, id: 'q-cr', state: 'changes_requested' },
    { ...plainGate, id: 'q-done', state: 'approved' },
    { id: 'a-1', state: 'pending', superseded: false, source: 'janus', missionId: 'task' },
  ]) }).envelope);
  const caps = (id: string) => nodes.find((n) => n.id === id)?.capabilities ?? [];

  it('FR-RHZ-175 offered on pending and changes_requested internal gates only', () => {
    expect(caps('q-1')).toContainEqual(ask);
    expect(caps('q-cr')).toContainEqual(ask);
    expect(caps('q-done').some((c) => c.action === 'gate.ask')).toBe(false);
    expect(caps('a-1').some((c) => c.action === 'gate.ask')).toBe(false);
    for (const n of nodes) expect(nodeProblem(n), n.id).toBeNull();
  });

  const wire = { revision: 2, body: body([plainGate, { ...plainGate, id: 'q-goal', missionId: undefined, goalId: 'goal' }, { id: 'a-1', state: 'pending', superseded: false, source: 'janus', missionId: 'task' }]) };

  it('FR-RHZ-175 maps to note.create {memoryKind: question, re} on the gate binding', () => {
    expect(gateAskIntent(wire, 'q-1', { text: 'which env?' })).toEqual({
      ok: true,
      fields: { kind: 'note.create', memoryKind: 'question', re: 'q-1', missionId: 'task', tags: ['question'], content: 're: q-1\n\nwhich env?' },
    });
    const goal = gateAskIntent(wire, 'q-goal', { text: 'x' });
    expect(goal.ok && goal.fields.goalId).toBe('goal');
    expect(goal.ok && 'missionId' in goal.fields).toBe(false);
  });

  it('FR-RHZ-175 refuses blank text and gates not internal in the snapshot', () => {
    expect(gateAskIntent(wire, 'q-1', { text: '  ' })).toEqual({ ok: false, reason: GATE_ASK_TEXT_REQUIRED });
    expect(gateAskIntent(wire, 'q-1', undefined)).toEqual({ ok: false, reason: GATE_ASK_TEXT_REQUIRED });
    expect(gateAskIntent(wire, 'q-missing', { text: 'x' })).toEqual({ ok: false, reason: GATE_NOT_INTERNAL });
    expect(gateAskIntent(wire, 'a-1', { text: 'x' })).toEqual({ ok: false, reason: GATE_NOT_INTERNAL });
  });
});

describe('gate.ask and note.answer over the upstream FR-RHZ-175', () => {
  const mock = Fastify({ logger: false });
  const received: Array<Record<string, unknown>> = [];
  const wireBody = { ...body([plainGate]), attention: [{ kind: 'note_blocked', refId: 'note-b', cause: 'stuck', missionId: 'task' }] };
  let upstream: Awaited<ReturnType<typeof createRhizomeUpstream>>;

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => ({ revision: 3, body: wireBody }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 3, body: wireBody })}\n\n`);
    });
    mock.post('/v1/intent', async (req) => {
      received.push(req.body as Record<string, unknown>);
      return { Accepted: true, Reason: '' };
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const address = mock.server.address();
    upstream = await createRhizomeUpstream(`http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`);
  });
  afterAll(async () => {
    upstream.close();
    await mock.close();
  });

  it('FR-RHZ-175 relayIntent gate.ask posts note.create question; blank text never reaches Rhizome', async () => {
    const ok = await upstream.relayIntent({ nodeId: 'q-1', action: 'gate.ask', decision: { text: 'which env?' }, idempotencyKey: 'ask-1' }, 'fake-actor:h');
    expect(ok).toEqual({ accepted: true, reason: undefined });
    expect(received.at(-1)).toEqual({ kind: 'note.create', memoryKind: 'question', re: 'q-1', missionId: 'task', tags: ['question'], content: 're: q-1\n\nwhich env?', actor: 'fake-actor:h' });
    const before = received.length;
    expect(await upstream.relayIntent({ nodeId: 'q-1', action: 'gate.ask', decision: { text: ' ' }, idempotencyKey: 'ask-2' }, 'fake-actor:h')).toEqual({ accepted: false, reason: GATE_ASK_TEXT_REQUIRED });
    expect(received.length).toBe(before);
  });

  it('FR-RHZ-175 note.answer carries re as a field and keeps the re: tag', async () => {
    await upstream.relayIntent({ nodeId: 'note-b', action: 'note.answer', decision: { text: 'done' }, idempotencyKey: 'ans-1' }, 'fake-actor:h');
    expect(received.at(-1)).toMatchObject({ kind: 'note.create', memoryKind: 'answer', re: 'note-b', tags: ['answer', 're:note-b'] });
  });
});

describe('0.6 gate summary from the card FR-RHZ-175', () => {
  const nodeOf = (gate: { id: string } & Record<string, unknown>, wireFields?: '0.5' | '0.6') =>
    nodesOf(adaptWithReport({ revision: 5, body: body([gate]) }, undefined, wireFields ? { wireFields } : {}).envelope).find((n) => n.id === 'q-1') as unknown as Record<string, unknown>;

  it('FR-RHZ-175 summary = level · ifSkipped (else risk.line), one line, ≤200', () => {
    expect(nodeOf({ ...plainGate, ...card }, '0.6').summary).toBe('high · release slips a day');
    expect(nodeOf({ ...plainGate, risk: { level: 'low', line: 'safe' } }, '0.6').summary).toBe('low · safe');
    const long = nodeOf({ ...plainGate, risk: { level: 'medium', line: 'x' }, ifSkipped: `a\nb ${'가'.repeat(400)}` }, '0.6').summary as string;
    expect(long.includes('\n')).toBe(false);
    expect([...long].length).toBe(200);
    expect(long.startsWith('medium · a b ')).toBe(true);
    // Without a risk the 0.6 summary is the first body line, as before.
    expect(nodeOf(plainGate, '0.6').summary).toBe('## 권고');
  });

  it('FR-RHZ-175 under 0.5 a carded gate node equals the card-less node', () => {
    expect(nodeOf({ ...plainGate, ...card })).toEqual(nodeOf(plainGate));
    expect(nodeOf({ ...plainGate, ...card })).not.toHaveProperty('summary');
  });
});
