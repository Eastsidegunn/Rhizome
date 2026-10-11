// FR-RHZ-192 (RHZ-148): the cockpit starts a mission. The adapter declares
// mission.start on startable tasks from serve's GET /v1/launcher/options,
// translates the chosen option to Rhizome mission.start, and passes Rhizome's
// refusal through verbatim.
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { adaptWithReport, createRhizomeUpstream, missionStartIntent, nodesOf, startChoices } from '../src/index.js';

const launchOptions = {
  sessionModes: ['oneshot', 'interactive'],
  defaultSessionMode: 'oneshot',
  workdirs: ['gunnflow', 'rhizome'],
  defaultWorkdir: 'rhizome',
  models: ['claude-opus-5-5'],
  defaultModel: 'claude-opus-5-5',
};
const DEFAULT = 'oneshot · rhizome · claude-opus-5-5';
const INTERACTIVE_GUNNFLOW = 'interactive · gunnflow · claude-opus-5-5';

// The wire states serve sends (workspace.go mapState: planned/ready → queued, waiting_for_* → waiting).
const STATES = ['queued', 'paused', 'running', 'waiting', 'blocked'];
const body = () => ({
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: STATES.map((s) => ({ id: `m-${s}`, missionId: 'goal', name: s, state: s, hasProgress: false, attention: false })),
  gates: [],
  deliverables: [],
  edges: [],
  counts: { running: 0, needsYou: 0, blocked: 0 },
  attention: [],
});

const startCap = (nodes: ReturnType<typeof nodesOf>, id: string) => nodes.find((n) => n.id === id)?.capabilities.find((c) => c.action === 'mission.start');

describe('mission.start declaration FR-RHZ-192', () => {
  const nodes = nodesOf(adaptWithReport({ revision: 1, body: body() }, undefined, { launchOptions }).envelope);

  it('declared on queued / paused tasks only, with every choice as an option (defaults first)', () => {
    for (const s of ['queued', 'paused']) {
      expect(startCap(nodes, `m-${s}`), s).toEqual({
        action: 'mission.start',
        level: 'enabled',
        decision: {
          options: [
            DEFAULT,
            'oneshot · gunnflow · claude-opus-5-5',
            'interactive · rhizome · claude-opus-5-5',
            INTERACTIVE_GUNNFLOW,
          ],
        },
      });
    }
    for (const s of ['running', 'waiting', 'blocked']) expect(startCap(nodes, `m-${s}`), s).toBeUndefined();
  });

  it('not declared anywhere without launch options (serve has no session launcher)', () => {
    const bare = nodesOf(adaptWithReport({ revision: 1, body: body() }).envelope);
    expect(bare.flatMap((n) => n.capabilities.map((c) => c.action))).not.toContain('mission.start');
  });

  it('not declared on goals, gates or the root', () => {
    for (const n of nodes.filter((x) => x.kind !== 'task')) expect(n.capabilities.map((c) => c.action)).not.toContain('mission.start');
  });
});

describe('mission.start translation FR-RHZ-192', () => {
  it('option → sessionMode + workdir of that choice; no model field', () => {
    expect(missionStartIntent(launchOptions, 'm-paused', { option: INTERACTIVE_GUNNFLOW })).toEqual({
      ok: true,
      fields: { kind: 'mission.start', missionId: 'm-paused', sessionMode: 'interactive', workdir: 'gunnflow' },
    });
  });

  it('no option = the ledger defaults', () => {
    expect(missionStartIntent(launchOptions, 'm-queued', undefined)).toEqual({
      ok: true,
      fields: { kind: 'mission.start', missionId: 'm-queued', sessionMode: 'oneshot', workdir: 'rhizome' },
    });
  });

  it('an option not offered, or no options, is refused before Rhizome', () => {
    expect(missionStartIntent(launchOptions, 'm-queued', { option: 'interactive · /etc · x' })).toEqual({ ok: false, reason: 'start option not offered' });
    expect(missionStartIntent(undefined, 'm-queued', { option: DEFAULT })).toEqual({ ok: false, reason: 'launch options unavailable in adapter' });
  });

  it('labels are looked up, never parsed: a workdir key with the separator stays one choice', () => {
    const odd = { ...launchOptions, workdirs: ['a · b'], defaultWorkdir: 'a · b' };
    expect(startChoices(odd).get('oneshot · a · b · claude-opus-5-5')).toEqual({ sessionMode: 'oneshot', workdir: 'a · b', model: 'claude-opus-5-5' });
  });
});

describe('mission.start through the upstream FR-RHZ-192', () => {
  const received: Array<Record<string, unknown>> = [];
  const app = Fastify({ logger: false });
  const env = () => ({ revision: 1, body: body() });
  app.get('/v1/workspace', async () => env());
  app.get('/v1/launcher/options', async () => launchOptions);
  app.get('/v1/workspace/stream', (_req, reply) => {
    reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
    reply.raw.write(`event: snapshot\ndata: ${JSON.stringify(env())}\n\n`);
  });
  app.post('/v1/intent', async (req) => {
    const b = req.body as Record<string, unknown>;
    received.push(b);
    if (b.missionId === 'm-queued') return { Accepted: false, Reason: 'POLICY_DENIED: workdir "gunnflow" not in ledger' };
    return { Accepted: true, Reason: '' };
  });
  let url = '';
  beforeAll(async () => {
    url = await app.listen({ port: 0, host: '127.0.0.1' });
  });
  afterAll(async () => {
    await app.close();
  });

  it('relays the chosen start to Rhizome mission.start with the actor', async () => {
    const up = await createRhizomeUpstream(url);
    try {
      expect(startCap(nodesOf(up.snapshot()), 'm-paused')?.level).toBe('enabled');
      const r = await up.relayIntent({ nodeId: 'm-paused', action: 'mission.start', decision: { option: INTERACTIVE_GUNNFLOW }, idempotencyKey: 'k-1' }, 'op');
      expect(r).toEqual({ accepted: true, reason: undefined });
      expect(received.at(-1)).toEqual({ kind: 'mission.start', missionId: 'm-paused', sessionMode: 'interactive', workdir: 'gunnflow', actor: 'op' });
    } finally {
      up.close();
    }
  });

  it("Rhizome's refusal reaches the card verbatim", async () => {
    const up = await createRhizomeUpstream(url);
    try {
      const r = await up.relayIntent({ nodeId: 'm-queued', action: 'mission.start', decision: { option: INTERACTIVE_GUNNFLOW }, idempotencyKey: 'k-2' }, 'op');
      expect(r).toEqual({ accepted: false, reason: 'POLICY_DENIED: workdir "gunnflow" not in ledger' });
    } finally {
      up.close();
    }
  });

  it('a running mission refuses at the contract check, nothing reaches Rhizome', async () => {
    const up = await createRhizomeUpstream(url);
    try {
      const before = received.length;
      const r = await up.relayIntent({ nodeId: 'm-running', action: 'mission.start', idempotencyKey: 'k-3' }, 'op');
      expect(r.accepted).toBe(false);
      expect(received.length).toBe(before);
    } finally {
      up.close();
    }
  });
});
