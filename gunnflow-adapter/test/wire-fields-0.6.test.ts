// RHZ-133 (FR-RHZ-173): contract-0.6 node fields (changedAtRevision,
// lastActivityTs, active, originNodeId, steps, shortName, summary) behind the
// wire-fields flag. OFF (default) = today's nodes byte-for-byte; ON = the
// validated fields, and nothing else changes.
import Fastify from 'fastify';
import { afterEach, describe, expect, it } from 'vitest';
import { nodeProblem, type NodeProjection } from '@gunnflow/contract';
import { adaptWithReport, createRhizomeUpstream, nodesOf } from '../src/upstream.js';
import { shortNameOf, summaryOf, wireFieldsOf } from '../src/nodes.js';

const NEW_KEYS = ['changedAtRevision', 'lastActivityTs', 'active', 'originNodeId', 'steps', 'shortName', 'summary'] as const;

/** A wire body as Rhizome emits it before RHZ-133. */
const legacyBody = {
  missions: [
    { id: 'goal-a', name: 'Ship v1 — the first public release', attention: false, state: 'active', success: 'ci green\n  and   review pass', handle: 'g-1' },
  ],
  tasks: [
    { id: 'run-1', missionId: 'goal-a', name: 'run', state: 'running', currentAction: '  writing\tdraft ', blockedReason: 'ignored while acting', hasProgress: false, attention: false, handle: 'm-1' },
    { id: 'step-1', missionId: 'goal-a', name: 'step one', state: 'queued', blockedReason: 'waiting for key', hasProgress: false, attention: false, handle: 'm-2' },
  ],
  gates: [
    { id: 'q-1', state: 'pending', superseded: false, name: 'deploy gate', source: 'internal', body: '\n\n  ## Why\nbecause', handle: 'q-1' },
  ],
  deliverables: [{ id: 'd-1', kind: 'code', missionId: 'run-1', sourceRef: 'exec-1', summary: 'patch v1', handle: 'd-1' }],
  edges: [
    { id: 'e-spawn', from: { type: 'mission', id: 'run-1' }, to: { type: 'mission', id: 'step-1' }, edgeKind: 'spawn', actor: 'a', correlation: 'c' },
  ],
  counts: { running: 1, needsYou: 2, blocked: 0 },
  attention: [{ kind: 'note_blocked', refId: 'note-1', cause: 'need a key', missionId: 'run-1' }],
  requests: [{ id: 'r-1', handle: 'r-1', name: 'Turn the key', state: 'waiting', missionId: 'run-1', why: 'manual\nlock', where: 'rack', commands: [], after: 'green', requestedBy: 'x', createdAt: '2026-10-08T01:02:03Z' }],
  requestCapabilities: { 'r-1': { complete: 'enabled', unable: 'enabled' } },
  capabilities: {
    'run-1': { pause: 'enabled', resume: 'disabled', instruct: 'enabled' },
    'step-1': { pause: 'disabled', resume: 'enabled', instruct: 'enabled' },
  },
  gateCapabilities: { 'q-1': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' } },
};

const REVISION = 40;
const TS = 1791430534000;

/** The same body with the RHZ-133 facts Rhizome now emits. */
const factBody = {
  ...legacyBody,
  missions: [{ ...legacyBody.missions[0], changedAtRevision: 3, lastActivityTs: TS, steps: { done: 1, total: 2 } }],
  tasks: [
    { ...legacyBody.tasks[0], changedAtRevision: 40, lastActivityTs: TS + 1, active: true, steps: { done: 0, total: 1 } },
    { ...legacyBody.tasks[1], changedAtRevision: 12, lastActivityTs: TS + 2, active: false, originNodeId: 'run-1' },
  ],
  gates: [{ ...legacyBody.gates[0], changedAtRevision: 20, lastActivityTs: TS + 3 }],
  deliverables: [{ ...legacyBody.deliverables[0], changedAtRevision: 21, lastActivityTs: TS + 4 }],
  requests: [{ ...legacyBody.requests[0], changedAtRevision: 22, lastActivityTs: TS + 5 }],
};

const nodes = (body: unknown, wireFields?: '0.5' | '0.6', revision = REVISION) =>
  nodesOf(adaptWithReport({ revision, body }, undefined, wireFields ? { wireFields } : {}).envelope);
const byId = (ns: NodeProjection[], id: string) => ns.find((n) => n.id === id) as unknown as Record<string, unknown>;
const strip = (n: NodeProjection) => {
  const copy = { ...(n as unknown as Record<string, unknown>) };
  for (const k of NEW_KEYS) delete copy[k];
  return copy;
};

describe('RHZ-133 (FR-RHZ-173): wire fields flag', () => {
  const saved = process.env.GUNNFLOW_WIRE_FIELDS;
  afterEach(() => {
    if (saved === undefined) delete process.env.GUNNFLOW_WIRE_FIELDS;
    else process.env.GUNNFLOW_WIRE_FIELDS = saved;
  });

  it('turns on only for the exact value 0.6', () => {
    expect(wireFieldsOf('0.6')).toBe('0.6');
    for (const v of [undefined, '', '0.5', ' 0.6', '0.6 ', '0.60', '06', 'true', '1']) expect(wireFieldsOf(v), String(v)).toBe('0.5');
    delete process.env.GUNNFLOW_WIRE_FIELDS;
    expect(wireFieldsOf()).toBe('0.5');
    process.env.GUNNFLOW_WIRE_FIELDS = '0.6';
    expect(wireFieldsOf()).toBe('0.6');
    process.env.GUNNFLOW_WIRE_FIELDS = 'on';
    expect(wireFieldsOf()).toBe('0.5');
  });

  it('OFF: nodes are byte-identical with or without the new upstream fields, and pass the contract', () => {
    const before = nodes(legacyBody);
    for (const off of [nodes(factBody), nodes(factBody, '0.5'), nodes(legacyBody, '0.5')]) {
      expect(off).toEqual(before);
      expect(JSON.stringify(off)).toBe(JSON.stringify(before));
    }
    for (const n of nodes(factBody)) {
      expect(nodeProblem(n), n.id).toBeNull();
      for (const k of NEW_KEYS) expect(k in n, `${n.id}.${k}`).toBe(false);
    }
  });

  it('ON: emits the exact validated values and omissions per node kind', () => {
    const on = nodes(factBody, '0.6');
    const pick = (id: string) => Object.fromEntries(NEW_KEYS.flatMap((k) => (k in byId(on, id) ? [[k, byId(on, id)[k]]] : [])));
    expect(pick('goal-a')).toEqual({ changedAtRevision: 3, lastActivityTs: TS, steps: { done: 1, total: 2 }, shortName: 'Ship v1', summary: 'ci green and review pass' });
    expect(pick('run-1')).toEqual({ changedAtRevision: 40, lastActivityTs: TS + 1, active: true, steps: { done: 0, total: 1 }, shortName: 'run', summary: 'writing draft' });
    expect(pick('step-1')).toEqual({ changedAtRevision: 12, lastActivityTs: TS + 2, active: false, originNodeId: 'run-1', shortName: 'step one', summary: 'waiting for key' });
    expect(pick('q-1')).toEqual({ changedAtRevision: 20, lastActivityTs: TS + 3, shortName: 'deploy gate', summary: '## Why' });
    expect(pick('d-1')).toEqual({ changedAtRevision: 21, lastActivityTs: TS + 4, shortName: 'patch v1', summary: 'patch v1' });
    expect(pick('r-1')).toEqual({ changedAtRevision: 22, lastActivityTs: TS + 5, shortName: 'Turn the key', summary: 'manual lock' });
    // Note and root: no facts, no summary; only the label-derived shortName.
    expect(pick('note-1')).toEqual({ shortName: 'need a key' });
    expect(pick('~workspace')).toEqual({ shortName: 'Workspace' });
  });

  it('ON: stripping the new keys yields the OFF node, which passes the 0.4 contract', () => {
    const off = nodes(factBody);
    const on = nodes(factBody, '0.6');
    expect(on.map((n) => n.id)).toEqual(off.map((n) => n.id));
    on.forEach((n, i) => {
      const stripped = strip(n);
      expect(stripped, n.id).toEqual(off[i]);
      expect(nodeProblem(stripped), n.id).toBeNull();
    });
  });

  it('ON: without upstream facts only shortName/summary appear', () => {
    const on = nodes(legacyBody, '0.6');
    for (const n of on) {
      for (const k of ['changedAtRevision', 'lastActivityTs', 'active', 'originNodeId', 'steps']) expect(k in n, `${n.id}.${k}`).toBe(false);
    }
  });

  it('ON: invalid values are omitted, never repaired', () => {
    const bad = (task: Record<string, unknown>, revision = REVISION) =>
      byId(nodes({ ...legacyBody, tasks: [{ ...legacyBody.tasks[1], ...task }] }, '0.6', revision), 'step-1');
    for (const v of [0, -1, 1.5, '3', REVISION + 1, null, Number.MAX_SAFE_INTEGER + 2]) {
      expect('changedAtRevision' in bad({ changedAtRevision: v }), String(v)).toBe(false);
    }
    expect(bad({ changedAtRevision: 1 }).changedAtRevision).toBe(1);
    expect(bad({ changedAtRevision: REVISION }).changedAtRevision).toBe(REVISION);
    // The bound is the envelope revision.
    expect('changedAtRevision' in bad({ changedAtRevision: 5 }, 4)).toBe(false);
    for (const v of [0, -5, 1.5, '1791430534000', null]) expect('lastActivityTs' in bad({ lastActivityTs: v }), String(v)).toBe(false);
    for (const v of ['true', 1, null]) expect('active' in bad({ active: v }), String(v)).toBe(false);
    for (const v of ['', 'step-1', 7]) expect('originNodeId' in bad({ originNodeId: v }), String(v)).toBe(false);
    for (const v of [{ done: 0, total: 0 }, { done: 3, total: 2 }, { done: -1, total: 2 }, { done: 0.5, total: 2 }, { done: 0 }, 'x', null, { done: 1, total: 2, extra: 1 }]) {
      const got = bad({ steps: v });
      if (v !== null && typeof v === 'object' && 'extra' in v) expect(got.steps).toEqual({ done: 1, total: 2 });
      else expect('steps' in got, JSON.stringify(v)).toBe(false);
    }
    // Facts never leak onto kinds that do not own them.
    const gateWithTaskFacts = byId(nodes({ ...legacyBody, gates: [{ ...legacyBody.gates[0], active: true, originNodeId: 'run-1', steps: { done: 0, total: 1 } }] }, '0.6'), 'q-1');
    for (const k of ['active', 'originNodeId', 'steps']) expect(k in gateWithTaskFacts, k).toBe(false);
    const goalWithTaskFacts = byId(nodes({ ...legacyBody, missions: [{ ...legacyBody.missions[0], active: true, originNodeId: 'x' }] }, '0.6'), 'goal-a');
    for (const k of ['active', 'originNodeId']) expect(k in goalWithTaskFacts, k).toBe(false);
  });
});

describe('RHZ-133 (FR-RHZ-173): shortName and summary', () => {
  it('shortName: part before " — ", trimmed and collapsed, 1..32 code points', () => {
    expect(shortNameOf('Alpha — beta — gamma')).toBe('Alpha');
    expect(shortNameOf('  a\n\tb  ')).toBe('a b');
    expect(shortNameOf('no-dash—here')).toBe('no-dash—here');
    expect(shortNameOf('x'.repeat(32))).toBe('x'.repeat(32));
    expect(shortNameOf('x'.repeat(33))).toBeUndefined();
    expect(shortNameOf('😀'.repeat(32))).toBe('😀'.repeat(32)); // code points, not UTF-16 units
    expect(shortNameOf('😀'.repeat(33))).toBeUndefined();
    expect(shortNameOf(' — tail')).toBeUndefined();
    expect(shortNameOf('   ')).toBeUndefined();
    expect(shortNameOf(undefined)).toBeUndefined();
    expect(shortNameOf('x'.repeat(40) + ' — short')).toBeUndefined();
  });

  it('summary: one line, collapsed, ≤200 code points with 199 + … when cut, absent when empty', () => {
    expect(summaryOf('a\n\nb\t c ')).toBe('a b c');
    expect(summaryOf('x'.repeat(200))).toBe('x'.repeat(200));
    expect(summaryOf('x'.repeat(201))).toBe(`${'x'.repeat(199)}…`);
    expect([...summaryOf('😀'.repeat(250))!].length).toBe(200);
    expect(summaryOf('😀'.repeat(250))).toBe(`${'😀'.repeat(199)}…`);
    expect(summaryOf(' \n ')).toBeUndefined();
    expect(summaryOf(undefined)).toBeUndefined();
    expect(summaryOf(3)).toBeUndefined();
  });

  it('per-kind sources: task falls back to blockedReason, gate uses the first non-empty body line', () => {
    const body = {
      ...legacyBody,
      tasks: [{ ...legacyBody.tasks[0], currentAction: '   ', blockedReason: 'stuck\non key' }],
      gates: [{ ...legacyBody.gates[0], body: '   \n\nfirst real line\nsecond' }],
      deliverables: [{ ...legacyBody.deliverables[0], summary: '' }],
      missions: [{ ...legacyBody.missions[0], success: undefined }],
    };
    const on = nodes(body, '0.6');
    expect(byId(on, 'run-1').summary).toBe('stuck on key');
    expect(byId(on, 'q-1').summary).toBe('first real line');
    expect('summary' in byId(on, 'd-1')).toBe(false);
    expect('summary' in byId(on, 'goal-a')).toBe(false);
  });
});

describe('RHZ-133 (FR-RHZ-173): runtime wiring', () => {
  const saved = process.env.GUNNFLOW_WIRE_FIELDS;
  afterEach(() => {
    if (saved === undefined) delete process.env.GUNNFLOW_WIRE_FIELDS;
    else process.env.GUNNFLOW_WIRE_FIELDS = saved;
  });

  const withMock = async (fn: (url: string) => Promise<void>) => {
    const mock = Fastify({ logger: false });
    mock.get('/v1/workspace', async () => ({ revision: REVISION, body: factBody }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: REVISION, body: factBody })}\n\n`);
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const address = mock.server.address();
    try {
      await fn(`http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`);
    } finally {
      await mock.close();
    }
  };

  it('createRhizomeUpstream reads GUNNFLOW_WIRE_FIELDS; unset/other = off, 0.6 = on, option wins', async () => {
    await withMock(async (url) => {
      delete process.env.GUNNFLOW_WIRE_FIELDS;
      const off = await createRhizomeUpstream(url);
      process.env.GUNNFLOW_WIRE_FIELDS = '0.6';
      const on = await createRhizomeUpstream(url);
      const forcedOff = await createRhizomeUpstream(url, { wireFields: '0.5' });
      try {
        const offNodes = nodesOf(off.snapshot());
        expect(offNodes).toEqual(nodes(factBody));
        expect(byId(nodesOf(on.snapshot()), 'run-1').changedAtRevision).toBe(40);
        expect(nodesOf(forcedOff.snapshot())).toEqual(offNodes);
      } finally {
        off.close();
        on.close();
        forcedOff.close();
      }
    });
  });
});
