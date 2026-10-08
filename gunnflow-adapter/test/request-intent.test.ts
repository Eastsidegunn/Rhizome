// FR-RHZ-161: request capability relay mapping and the non-capability cancel.
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import Fastify from 'fastify';
import { adaptWithReport, contractRefusal, createRhizomeUpstream, nodesOf } from '../src/upstream.js';

const body = {
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: [{ id: 'task', missionId: 'goal', name: 'task', state: 'running', hasProgress: false, attention: false }],
  gates: [], deliverables: [], edges: [], attention: [], counts: { running: 1, needsYou: 1, blocked: 0 },
  capabilities: {}, gateCapabilities: {},
  requests: [{ id: 'r-1', name: 'hand task', state: 'waiting', missionId: 'task', createdAt: '2026-10-08T01:02:03Z' }],
  requestCapabilities: { 'r-1': { complete: 'enabled', unable: 'enabled' } },
};

describe('request intent FR-RHZ-161', () => {
  const nodes = nodesOf(adaptWithReport({ revision: 1, body }).envelope);
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
      if (sent.kind === 'request.unable' && (typeof sent.reason !== 'string' || sent.reason.trim() === '')) {
        return { Accepted: false, Reason: 'reason required' };
      }
      return { Accepted: true, Reason: '' };
    });
    await app.listen({ port: 0, host: '127.0.0.1' });
    const address = app.server.address();
    const url = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
    upstream = await createRhizomeUpstream(url);
  });

  afterAll(async () => {
    upstream.close();
    await app.close();
  });

  it('complete with decision.text → memo arrives at Rhizome', async () => {
    const result = await upstream.relayIntent(
      { nodeId: 'r-1', action: 'request.complete', decision: { text: 'finished' }, idempotencyKey: 'request-complete-text' },
      'fake-actor:request',
    );
    expect(result).toEqual({ accepted: true, reason: undefined });
    expect(received.at(-1)).toEqual({
      kind: 'request.complete', requestId: 'r-1', memo: 'finished', actor: 'fake-actor:request',
    });
  });

  it('complete without decision.text → memo key omitted (no empty string)', async () => {
    const result = await upstream.relayIntent(
      { nodeId: 'r-1', action: 'request.complete', idempotencyKey: 'request-complete-no-text' },
      'fake-actor:request',
    );
    expect(result).toEqual({ accepted: true, reason: undefined });
    expect(received.at(-1)).toEqual({
      kind: 'request.complete', requestId: 'r-1', actor: 'fake-actor:request',
    });
    expect(received.at(-1)).not.toHaveProperty('memo');
    expect(received.at(-1)).not.toHaveProperty('decision');
  });

  it('unable with decision.text → reason arrives', async () => {
    const result = await upstream.relayIntent(
      { nodeId: 'r-1', action: 'request.unable', decision: { text: 'locked' }, idempotencyKey: 'request-unable-text' },
      'fake-actor:request',
    );
    expect(result).toEqual({ accepted: true, reason: undefined });
    expect(received.at(-1)).toEqual({
      kind: 'request.unable', requestId: 'r-1', reason: 'locked', actor: 'fake-actor:request',
    });
    expect(received.at(-1)).not.toHaveProperty('decision');
  });

  it('unable with empty/whitespace decision.text → Rhizome fixed "reason required" propagates to the cockpit response', async () => {
    for (const [index, text] of ['', '   '].entries()) {
      const result = await upstream.relayIntent(
        { nodeId: 'r-1', action: 'request.unable', decision: { text }, idempotencyKey: `request-unable-blank-${index}` },
        'fake-actor:request',
      );
      expect(received.at(-1)).toEqual({
        kind: 'request.unable', requestId: 'r-1', reason: text, actor: 'fake-actor:request',
      });
      expect(result).toEqual({ accepted: false, reason: 'reason required' });
    }
  });

  it('missing decision object is refused by the contract check before reaching Rhizome', async () => {
    const before = received.length;
    const result = await upstream.relayIntent(
      { nodeId: 'r-1', action: 'request.unable', idempotencyKey: 'request-unable-missing-decision' },
      'fake-actor:request',
    );
    expect(result).toEqual({ accepted: false, reason: 'decision.text required by declared decision.input' });
    expect(received).toHaveLength(before);
  });

  it('refuses undeclared request.cancel at the contract boundary', () => {
    expect(contractRefusal(
      { nodeId: 'r-1', action: 'request.cancel', decision: { text: 'reason' }, idempotencyKey: 'request-cancel' },
      nodes,
    )).toBe("action 'request.cancel' is not declared on this node");
  });
});
