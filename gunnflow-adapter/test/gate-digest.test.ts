// FR-RHZ-163: gate decisions carry the requestDigest from the adapter's own
// snapshot; a cockpit-supplied digest is never trusted; no snapshot digest →
// fail-closed before anything reaches Rhizome.
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import Fastify from 'fastify';
import { DIGEST_UNAVAILABLE, INTENT_FIELDS, createRhizomeUpstream, snapshotDigest } from '../src/upstream.js';

const DIGEST = 'a'.repeat(64);
const gate = (id: string, extra: Record<string, unknown> = {}) => ({
  id, missionId: 'task', gateType: 'approval', requestedAction: 'ship', state: 'waiting', ...extra,
});
const body = {
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: [{ id: 'task', missionId: 'goal', name: 'task', state: 'waiting_for_human', hasProgress: false, attention: true }],
  gates: [gate('q-bound', { requestDigest: DIGEST }), gate('q-bare')],
  deliverables: [], edges: [], attention: [], counts: { running: 0, needsYou: 2, blocked: 0 },
  capabilities: {},
  gateCapabilities: {
    'q-bound': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
    'q-bare': { approve: 'enabled', reject: 'enabled', requestChanges: 'enabled' },
  },
};

describe('gate decision digest binding FR-RHZ-163', () => {
  const received: Array<Record<string, unknown>> = [];
  const app = Fastify({ logger: false });
  let upstream: Awaited<ReturnType<typeof createRhizomeUpstream>>;

  beforeAll(async () => {
    app.get('/v1/workspace', async () => ({ revision: 1, body }));
    app.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 1, body })}\n\n`);
    });
    app.post('/v1/intent', async (req) => {
      const sent = req.body as Record<string, unknown>;
      received.push(sent);
      if (String(sent.kind).startsWith('gate.') && sent.digest !== DIGEST) {
        return { Accepted: false, Reason: 'Not confirmed — digest required' };
      }
      return { Accepted: true, Reason: '' };
    });
    await app.listen({ port: 0, host: '127.0.0.1' });
    const address = app.server.address();
    upstream = await createRhizomeUpstream(`http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`);
  });
  afterAll(async () => {
    upstream.close();
    await app.close();
  });

  it('approve carries the snapshot requestDigest as digest', async () => {
    received.length = 0;
    const result = await upstream.relayIntent({ nodeId: 'q-bound', action: 'gate.approve', idempotencyKey: 'k1' }, 'h');
    expect(result).toEqual({ accepted: true });
    expect(received).toEqual([{ kind: 'gate.approve', actor: 'h', gateId: 'q-bound', digest: DIGEST }]);
  });

  it('reject and requestChanges carry the digest too, alongside their own fields', async () => {
    received.length = 0;
    const rejected = await upstream.relayIntent({ nodeId: 'q-bound', action: 'gate.reject', idempotencyKey: 'k2' }, 'h');
    expect(rejected).toEqual({ accepted: true });
    await upstream.relayIntent({ nodeId: 'q-bound', action: 'gate.requestChanges', idempotencyKey: 'k3', decision: { text: 'fix' } }, 'h');
    expect(received.map((r) => [r.kind, r.digest])).toEqual([['gate.reject', DIGEST], ['gate.requestChanges', DIGEST]]);
    expect(received[1]?.instruction).toBe('fix');
  });

  it('a cockpit-supplied digest is refused at the contract boundary and never forwarded', async () => {
    received.length = 0;
    const result = await upstream.relayIntent(
      { nodeId: 'q-bound', action: 'gate.approve', idempotencyKey: 'k4', digest: 'b'.repeat(64) } as Record<string, unknown>,
      'h',
    );
    expect(result.accepted).toBe(false);
    expect(received).toEqual([]);
    // And even past that boundary, 'digest' is not a forwarded intent field: only the snapshot value is set.
    expect(INTENT_FIELDS.has('digest')).toBe(false);
  });

  it('fails closed with a fixed reason when the snapshot gate has no requestDigest, sending nothing', async () => {
    received.length = 0;
    const result = await upstream.relayIntent({ nodeId: 'q-bare', action: 'gate.approve', idempotencyKey: 'k5' }, 'h');
    expect(result).toEqual({ accepted: false, reason: DIGEST_UNAVAILABLE });
    expect(received).toEqual([]);
  });

  it('non-gate intents are untouched (no digest key) and snapshotDigest is strict', async () => {
    received.length = 0;
    await upstream.relayIntent({ intent: 'session.pause', sessionId: 's-1' }, 'h');
    expect(received).toHaveLength(1);
    expect(received[0]?.kind).toBe('session.pause');
    expect(received[0]).not.toHaveProperty('digest');
    expect(snapshotDigest(upstream.rawSnapshot(), 'q-bound')).toBe(DIGEST);
    expect(snapshotDigest(upstream.rawSnapshot(), 'q-bare')).toBeUndefined();
    expect(snapshotDigest(upstream.rawSnapshot(), 'q-missing')).toBeUndefined();
    expect(snapshotDigest(upstream.rawSnapshot(), 42)).toBeUndefined();
  });
});
