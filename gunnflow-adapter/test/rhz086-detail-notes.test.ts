// RHZ-086 (FR-RHZ-116): goal/mission detail items carry the about notes read
// from Rhizome /v1/context (goal → ?goal=, task → ?task=), newest-first,
// capped, truncated, and failure-isolated. N1–N5 exercise the pure layer
// (noteItems/withNotes); H runs the direct wire's /detail route against a
// stubbed Rhizome whose /v1/context can be made to fail.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { DIRECT_WIRE, validateNodeDetail } from '@gunnflow/contract';
import { NOTE_LIMIT, NOTE_TEXT_MAX, NOTE_TRUNCATED_SUFFIX, noteFetchTimeout, noteItems, noteQueryFor, withNotes, type ContextNote } from '../src/details.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';

const base = { revision: 7, items: [{ label: 'success', text: 'v3 live' }] };
// Served oldest-first on purpose (the task bundle is by id): the adapter must impose seq desc.
const notes: ContextNote[] = [
  { id: 'n1', kind: 'decision', content: 'first decision', tags: ['a', 'b'], seq: 11 },
  { id: 'n2', kind: 'observation', content: 'second obs', tags: [], seq: 15 },
  { id: 'n3', kind: 'reference', content: 'third ref', tags: ['z'], seq: 19 },
];
const expectedNoteItems = [
  { label: 'note:reference [z]', text: 'third ref' },
  { label: 'note:observation', text: 'second obs' },
  { label: 'note:decision [a,b]', text: 'first decision' },
];

describe('FR-RHZ-116 note items (pure layer)', () => {
  it('N1: 3 notes → 3 items after the existing ones, newest-first, label note:<kind> [tags], text = content', async () => {
    const d = await withNotes(base, async () => notes);
    expect(d.revision).toBe(7);
    expect(d.items).toEqual([...base.items, ...expectedNoteItems]);
    expect(validateNodeDetail(d).ok).toBe(true);
    expect(base.items).toHaveLength(1); // input untouched
  });

  it('N2: 0 notes → the existing items only', async () => {
    const d = await withNotes(base, async () => []);
    expect(d).toEqual(base);
  });

  it('N3: content over the cap is cut to the first 2000 chars plus the /v1/context marker', () => {
    const long = 'x'.repeat(NOTE_TEXT_MAX + 1);
    const exact = 'y'.repeat(NOTE_TEXT_MAX);
    const items = noteItems([
      { id: 'a', kind: 'decision', content: long, seq: 2 },
      { id: 'b', kind: 'decision', content: exact, seq: 1 },
    ]);
    expect(NOTE_TEXT_MAX).toBe(2000);
    expect(items[0]!.text).toBe('x'.repeat(2000) + NOTE_TRUNCATED_SUFFIX);
    expect(items[0]!.text!.endsWith('…(전문 /v1/context)')).toBe(true);
    expect(items[1]!.text).toBe(exact); // at the cap: verbatim
  });

  it('N4: a failing fetch (throw) → the detail is returned unchanged', async () => {
    const d = await withNotes(base, async () => {
      throw new Error('rhizome context failed (500)');
    });
    expect(d).toEqual(base);
  });

  it('N5: at most 10 notes, the newest ones', () => {
    const many: ContextNote[] = Array.from({ length: 25 }, (_, i) => ({ id: `n${i}`, kind: 'fact', content: `c${i}`, seq: i + 1 }));
    const items = noteItems(many);
    expect(NOTE_LIMIT).toBe(10);
    expect(items).toHaveLength(10);
    expect(items.map((i) => i.text)).toEqual(['c24', 'c23', 'c22', 'c21', 'c20', 'c19', 'c18', 'c17', 'c16', 'c15']);
  });

  it('N6: a malformed element (no content / no kind / bad tags) is dropped, the valid ones are shown', async () => {
    const d = await withNotes(base, async () => [
      { id: 'bad-1', kind: 'decision', seq: 99 }, // no content
      { id: 'bad-2', content: 'no kind', seq: 98 },
      { id: 'bad-3', kind: 'fact', content: 'bad tags', tags: 'x', seq: 97 },
      null,
      ...notes,
    ]);
    expect(d.items).toEqual([...base.items, ...expectedNoteItems]);
    expect(validateNodeDetail(d).ok).toBe(true);
  });

  it('routes: goal and contained mission read ?goal=, task reads ?mission= (never the trace-writing ?task=), others none', () => {
    expect(noteQueryFor('goal')).toBe('goal');
    expect(noteQueryFor('mission')).toBe('goal');
    expect(noteQueryFor('task')).toBe('mission');
    for (const k of ['gate', 'deliverable', 'workspace', '']) expect(noteQueryFor(k)).toBeUndefined();
  });
});

const wireBody = {
  missions: [
    { id: 'goal-a', name: 'ship v3', attention: false, state: 'active', success: 'v3 live' },
    { id: 'goal-bare', name: 'no criterion', attention: false, state: 'active' },
  ],
  tasks: [{ id: 'm-build', missionId: 'goal-a', name: 'build', state: 'running', currentAction: 'compiling', hasProgress: false, attention: false }],
  gates: [{ id: 'q-pending', state: 'pending', superseded: false, body: 'Publish?' }],
  deliverables: [],
  edges: [],
  counts: { running: 1, needsYou: 1, blocked: 0 },
  attention: [],
};
const REVISION = 9;

describe('FR-RHZ-116 H: GET /detail/:nodeId fetches notes from a stubbed Rhizome /v1/context', () => {
  const mock = Fastify({ logger: false });
  let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;
  const contextCalls: string[] = [];
  let contextMode: 'ok' | '500' | '404' | 'hang-up' | 'never' = 'ok';
  const pendingReplies: Array<() => void> = [];
  let closeConnections: () => void = () => undefined;

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => ({ revision: REVISION, body: wireBody }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: REVISION, body: wireBody })}\n\n`);
    });
    mock.get('/v1/context', async (req, reply) => {
      contextCalls.push(req.url);
      if (contextMode === '500') return reply.code(500).send('context assembly failed');
      if (contextMode === '404') return reply.code(404).send('404 page not found');
      if (contextMode === 'hang-up') return reply.raw.destroy(); // network error on the adapter side
      if (contextMode === 'never') return new Promise<void>((resolve) => pendingReplies.push(resolve)); // N7: never answers
      const q = req.query as { goal?: string; task?: string; mission?: string };
      if (q.task !== undefined) return reply.code(500).send('probe: ?task= writes a trace — the adapter must not call it');
      if (q.goal === 'goal-a') return { goal: { id: 'goal-a' }, memories: notes };
      if (q.goal === 'goal-bare') return { goal: { id: 'goal-bare' }, memories: [notes[0]] };
      if (q.mission === 'm-build') return { mission: { id: 'm-build', name: 'build', state: 'running' }, memories: [notes[0], notes[2]] };
      return reply.code(404).send('404 page not found');
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    closeConnections = () => mock.server.closeAllConnections();
    const address = mock.server.address();
    const port = typeof address === 'object' && address ? address.port : 0;
    server = await startRhizomeDirectServer({ rhizomeUrl: `http://127.0.0.1:${port}`, port: 0 });
  });
  afterAll(async () => {
    for (const r of pendingReplies) r();
    await server.close();
    closeConnections();
    await mock.close();
  });

  const detail = async (id: string) => {
    const res = await fetch(`${server.url}${DIRECT_WIRE.detail}/${id}`);
    return { status: res.status, body: (await res.json()) as { revision?: number; items?: unknown[] } };
  };

  it('goal node: ?goal=<id> once per request, notes appended newest-first, contract-valid', async () => {
    contextMode = 'ok';
    const before = contextCalls.length;
    const { status, body } = await detail('goal-a');
    expect(status).toBe(200);
    expect(validateNodeDetail(body).ok).toBe(true);
    expect(body.revision).toBe(REVISION);
    expect(body.items).toEqual([{ label: 'success', text: 'v3 live' }, ...expectedNoteItems]);
    expect(contextCalls.slice(before)).toEqual(['/v1/context?goal=goal-a']);
    // No cache: a second request fetches again.
    await detail('goal-a');
    expect(contextCalls.slice(before)).toHaveLength(2);
  });

  it('task node: ?mission=<id> (trace-free), re-ordered newest-first after the existing items', async () => {
    contextMode = 'ok';
    const { status, body } = await detail('m-build');
    expect(status).toBe(200);
    expect(validateNodeDetail(body).ok).toBe(true);
    expect(body.items).toEqual([
      { label: 'currentAction', text: 'compiling' },
      { label: 'state', text: 'running' },
      { label: 'note:reference [z]', text: 'third ref' },
      { label: 'note:decision [a,b]', text: 'first decision' },
    ]);
    expect(contextCalls.at(-1)).toBe('/v1/context?mission=m-build');
  });

  it('a goal with no success criterion but a note now has a detail (its notes)', async () => {
    contextMode = 'ok';
    const { status, body } = await detail('goal-bare');
    expect(status).toBe(200);
    expect(body.items).toEqual([{ label: 'note:decision [a,b]', text: 'first decision' }]);
  });

  it('gates never touch /v1/context', async () => {
    const before = contextCalls.length;
    expect((await detail('q-pending')).status).toBe(200);
    expect(contextCalls).toHaveLength(before);
  });

  it('N4 on the wire: upstream 500 / 404 / connection drop → note items omitted, detail still 200 with its own items', async () => {
    for (const mode of ['500', '404', 'hang-up'] as const) {
      contextMode = mode;
      const { status, body } = await detail('goal-a');
      expect(status, mode).toBe(200);
      expect(body.items, mode).toEqual([{ label: 'success', text: 'v3 live' }]);
      expect(validateNodeDetail(body).ok).toBe(true);
      // ...and a goal with nothing of its own stays 404 (never an empty 200).
      expect((await detail('goal-bare')).status, mode).toBe(404);
    }
    contextMode = 'ok';
  });

  it('N7: a never-responding upstream → detail 200 without note items, within the timeout', async () => {
    contextMode = 'never';
    const saved = noteFetchTimeout.ms;
    noteFetchTimeout.ms = 300;
    try {
      const t0 = Date.now();
      const { status, body } = await detail('goal-a');
      const elapsed = Date.now() - t0;
      expect(status).toBe(200);
      expect(body.items).toEqual([{ label: 'success', text: 'v3 live' }]);
      expect(elapsed).toBeGreaterThanOrEqual(250);
      expect(elapsed).toBeLessThan(2500);
    } finally {
      noteFetchTimeout.ms = saved;
      contextMode = 'ok';
    }
  }, 5000);
});
