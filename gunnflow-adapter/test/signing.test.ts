import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import {
  SIGNER_REFUSAL,
  SIGNER_STDOUT_MAX_BYTES,
  checkSignerStartup,
  prepareSigner,
  signingMode,
  type SignRequest,
  type SignResponse,
  type SignerStat,
  type SpawnedSigner,
  type SpawnSigner,
} from '../src/signing.js';
import { createRhizomeUpstream } from '../src/upstream.js';

const DIGEST = `rhz-question-v2:${'a'.repeat(64)}`;
const KEY_ID = `sha256:${'b'.repeat(64)}`;
const SIGNER_PATH = '/test/bin/rhizome-signer';

class MockChild extends EventEmitter implements SpawnedSigner {
  pid = 4242;
  stdin = new PassThrough();
  stdout = new PassThrough();
  kill = vi.fn(() => true);
}

type SignHandler = (child: MockChild, request: SignRequest) => void;

function spawnMock(handler: SignHandler, keyShowCode = 0) {
  const calls: Array<{ command: string; args: string[]; detached: boolean }> = [];
  const signChildren: MockChild[] = [];
  const spawn: SpawnSigner = (command, args, options) => {
    calls.push({ command, args, detached: options.detached });
    const child = new MockChild();
    if (args[0] === 'key') {
      queueMicrotask(() => child.emit('close', keyShowCode, null));
      return child;
    }
    signChildren.push(child);
    const chunks: Buffer[] = [];
    child.stdin.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    child.stdin.on('finish', () => handler(child, JSON.parse(Buffer.concat(chunks).toString('utf8')) as SignRequest));
    return child;
  };
  return { spawn, calls, signChildren };
}

function stat(kind: 'file' | 'directory', extra: Partial<{ uid: number; mode: number; symlink: boolean }> = {}): SignerStat {
  return {
    uid: extra.uid ?? 0,
    mode: extra.mode ?? 0o755,
    isFile: () => kind === 'file',
    isDirectory: () => kind === 'directory',
    isSymbolicLink: () => extra.symlink ?? false,
  };
}

const goodLstat = async (path: string) => stat(path === SIGNER_PATH ? 'file' : 'directory');

function response(reason = '', digest = DIGEST): SignResponse {
  return {
    digest,
    decision: 'approve',
    reason,
    keyId: KEY_ID,
    signature: { keyId: KEY_ID, signedAt: '2026-10-09T01:02:03Z', nonce: 'ab'.repeat(16), sig: 'c2ln' },
  };
}

function succeed(child: MockChild, body: unknown) {
  child.stdout.write(JSON.stringify(body));
  queueMicrotask(() => child.emit('close', 0, null));
}

const gate = (id: string, source: string) => ({
  id,
  source,
  missionId: 'task',
  gateType: 'approval',
  requestedAction: 'ship',
  state: 'pending',
  requestDigest: DIGEST,
});

const workspaceBody = {
  missions: [{ id: 'goal', name: 'goal', attention: false, state: 'active' }],
  tasks: [{ id: 'task', missionId: 'goal', name: 'task', state: 'waiting_for_human', hasProgress: false, attention: true }],
  gates: [gate('q-internal', 'internal'), gate('q-internal-2', 'internal'), gate('q-approval', 'janus')],
  deliverables: [], edges: [], attention: [], counts: { running: 0, needsYou: 3, blocked: 0 },
  capabilities: {},
  gateCapabilities: Object.fromEntries(['q-internal', 'q-internal-2', 'q-approval'].map((id) => [id, {
    approve: 'enabled', reject: 'enabled', requestChanges: 'enabled',
  }])),
};

describe('RHZ-117 signer startup checks', () => {
  it('accepts off/unset, rejects unknown and empty values', () => {
    expect(signingMode(undefined)).toBe('off');
    expect(signingMode('off')).toBe('off');
    expect(signingMode('required')).toBe('required');
    expect(() => signingMode('optional')).toThrow('RHIZOME_SIGNING');
    expect(() => signingMode('')).toThrow('RHIZOME_SIGNING');
  });

  it('required checks the injected absolute path and runs key show', async () => {
    const mock = spawnMock(() => undefined);
    await expect(prepareSigner({ mode: 'required', signerPath: SIGNER_PATH, lstat: goodLstat, spawn: mock.spawn })).resolves.toBeDefined();
    expect(mock.calls).toEqual([{ command: SIGNER_PATH, args: ['key', 'show'], detached: false }]);
  });

  it('fails closed for a symlink, non-root executable, writable parent, and key-show failure', async () => {
    const base = { signerPath: SIGNER_PATH, spawn: spawnMock(() => undefined).spawn };
    await expect(checkSignerStartup({ ...base, lstat: async (path) => stat(path === SIGNER_PATH ? 'file' : 'directory', { symlink: path === SIGNER_PATH }) }))
      .rejects.toThrow('symbolic link');
    await expect(checkSignerStartup({ ...base, lstat: async (path) => stat(path === SIGNER_PATH ? 'file' : 'directory', { uid: path === SIGNER_PATH ? 501 : 0 }) }))
      .rejects.toThrow('owned by root');
    await expect(checkSignerStartup({ ...base, lstat: async (path) => stat(path === SIGNER_PATH ? 'file' : 'directory', { mode: path === '/test/bin' ? 0o775 : 0o755 }) }))
      .rejects.toThrow('parent must not be group/other writable');
    const badKey = spawnMock(() => undefined, 5);
    await expect(checkSignerStartup({ signerPath: SIGNER_PATH, lstat: goodLstat, spawn: badKey.spawn }))
      .rejects.toThrow('key show failed (exit 5)');
  });
});

describe('RHZ-117 relay signing', () => {
  const received: Array<Record<string, unknown>> = [];
  const app = Fastify({ logger: false });
  let baseUrl = '';

  beforeAll(async () => {
    app.get('/v1/workspace', async () => ({ revision: 1, body: workspaceBody }));
    app.get('/v1/workspace/stream', (_request, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write(`event: snapshot\ndata: ${JSON.stringify({ revision: 1, body: workspaceBody })}\n\n`);
    });
    app.post('/v1/intent', async (request) => {
      received.push(request.body as Record<string, unknown>);
      return { Accepted: true, Reason: '' };
    });
    await app.listen({ port: 0, host: '127.0.0.1' });
    const address = app.server.address();
    baseUrl = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`;
  });

  afterAll(async () => app.close());

  async function upstreamWith(mock: ReturnType<typeof spawnMock>, extra: Record<string, unknown> = {}) {
    return createRhizomeUpstream(baseUrl, {
      signing: { mode: 'required', signerPath: SIGNER_PATH, lstat: goodLstat, spawn: mock.spawn, ...extra },
    });
  }

  it('attaches the signature, uses actor signer, and sends only normalized effective reason for signed requestChanges', async () => {
    received.length = 0;
    let request: SignRequest | undefined;
    const mock = spawnMock((child, value) => {
      request = value;
      succeed(child, response('fix it'));
    });
    const upstream = await upstreamWith(mock);
    try {
      const result = await upstream.relayIntent({
        nodeId: 'q-internal', action: 'gate.requestChanges', idempotencyKey: 'idem-1', decision: { text: '  fix it  ' },
      }, 'cockpit-actor');
      expect(result).toEqual({ accepted: true });
      expect(request).toEqual({
        gateId: 'q-internal', kind: 'gate.requestChanges', reason: 'fix it', correlationId: 'cockpit:idem-1',
      });
      expect(received).toEqual([{
        gateId: 'q-internal', digest: DIGEST, reason: 'fix it',
        verification: { signature: response().signature }, kind: 'gate.requestChanges', actor: 'signer',
      }]);
      expect(received[0]).not.toHaveProperty('instruction');
    } finally {
      upstream.close();
    }
  });

  it('leaves approval requestChanges unsigned and otherwise byte-identical', async () => {
    received.length = 0;
    const mock = spawnMock(() => { throw new Error('must not sign'); });
    const upstream = await upstreamWith(mock);
    try {
      await upstream.relayIntent({
        nodeId: 'q-approval', action: 'gate.requestChanges', idempotencyKey: 'idem-2', decision: { text: 'revise' },
      }, 'original');
      expect(mock.calls).toHaveLength(1); // startup key show only
      expect(received).toEqual([{
        instruction: 'revise', gateId: 'q-approval', digest: DIGEST, kind: 'gate.requestChanges', actor: 'original',
      }]);
    } finally {
      upstream.close();
    }
  });

  it.each([
    [2, SIGNER_REFUSAL.refused],
    [3, SIGNER_REFUSAL.rhizomeReadFailed],
    [4, SIGNER_REFUSAL.cancelled],
    [5, SIGNER_REFUSAL.noKey],
    [6, SIGNER_REFUSAL.notSignable],
  ])('maps signer exit %i to a fixed refusal and sends nothing', async (code, reason) => {
    received.length = 0;
    const mock = spawnMock((child) => queueMicrotask(() => child.emit('close', code, null)));
    const upstream = await upstreamWith(mock);
    try {
      await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: `exit-${code}` }, 'h'))
        .resolves.toEqual({ accepted: false, reason });
      expect(received).toEqual([]);
    } finally {
      upstream.close();
    }
  });

  it('kills the detached process group on timeout and sends nothing', async () => {
    received.length = 0;
    const kill = vi.fn();
    const mock = spawnMock(() => undefined);
    const upstream = await upstreamWith(mock, { timeoutMs: 5, kill });
    try {
      await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: 'timeout' }, 'h'))
        .resolves.toEqual({ accepted: false, reason: SIGNER_REFUSAL.timeout });
      expect(kill).toHaveBeenCalledWith(-4242, 'SIGKILL');
      expect(received).toEqual([]);
    } finally {
      upstream.close();
    }
  });

  it('caps stdout, rejects malformed JSON, and rejects a digest mismatch without sending', async () => {
    for (const [handler, reason] of [
      [((child: MockChild) => { child.stdout.write(Buffer.alloc(SIGNER_STDOUT_MAX_BYTES + 1)); }) as SignHandler, SIGNER_REFUSAL.outputTooLarge],
      [((child: MockChild) => { child.stdout.write('{bad'); queueMicrotask(() => child.emit('close', 0, null)); }) as SignHandler, SIGNER_REFUSAL.malformedOutput],
      [((child: MockChild) => succeed(child, response('', `rhz-question-v2:${'f'.repeat(64)}`))) as SignHandler, SIGNER_REFUSAL.digestMismatch],
    ] as const) {
      received.length = 0;
      const upstream = await upstreamWith(spawnMock(handler));
      try {
        await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: reason }, 'h'))
          .resolves.toEqual({ accepted: false, reason });
        expect(received).toEqual([]);
      } finally {
        upstream.close();
      }
    }
  });

  it('serializes two concurrent gate signing requests without dropping either', async () => {
    received.length = 0;
    const waiting: Array<{ child: MockChild; request: SignRequest }> = [];
    const mock = spawnMock((child, request) => waiting.push({ child, request }));
    const upstream = await upstreamWith(mock);
    try {
      const first = upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: 'one' }, 'h');
      const second = upstream.relayIntent({ nodeId: 'q-internal-2', action: 'gate.approve', idempotencyKey: 'two' }, 'h');
      await vi.waitFor(() => expect(waiting).toHaveLength(1));
      expect(waiting[0]?.request.correlationId).toBe('cockpit:one');
      succeed(waiting[0]!.child, response(''));
      await vi.waitFor(() => expect(waiting).toHaveLength(2));
      expect(waiting[1]?.request.correlationId).toBe('cockpit:two');
      succeed(waiting[1]!.child, response(''));
      await expect(Promise.all([first, second])).resolves.toEqual([{ accepted: true }, { accepted: true }]);
      expect(received).toHaveLength(2);
    } finally {
      upstream.close();
    }
  });

  it('off performs no checks or spawns and preserves the old relay body', async () => {
    received.length = 0;
    const lstat = vi.fn(async () => { throw new Error('must not check'); });
    const spawn = vi.fn(() => { throw new Error('must not spawn'); }) as unknown as SpawnSigner;
    const upstream = await createRhizomeUpstream(baseUrl, { signing: { mode: 'off', lstat, spawn } });
    try {
      await upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: 'off' }, 'operator');
      expect(lstat).not.toHaveBeenCalled();
      expect(spawn).not.toHaveBeenCalled();
      expect(received).toEqual([{ gateId: 'q-internal', digest: DIGEST, kind: 'gate.approve', actor: 'operator' }]);
    } finally {
      upstream.close();
    }
  });
});
