import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_SIGNER_PATH,
  SIGNER_KEY_SHOW_TIMEOUT_MS,
  SIGNER_REFUSAL,
  SIGNER_STDOUT_MAX_BYTES,
  checkSignerStartup,
  prepareSigner,
  signingMode,
  type SignRequest,
  type SignResponse,
  type SignerStat,
  type SigningTimers,
  type SpawnedSigner,
  type SpawnSigner,
} from '../src/signing.js';
import { createUpstream } from '../src/index.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';
import { createRhizomeUpstream, effectiveReason } from '../src/upstream.js';

const DIGEST = `rhz-question-v2:${'a'.repeat(64)}`;
const KEY_ID = `sha256:${'b'.repeat(64)}`;
const SIGNER_PATH = '/test/bin/rhizome-signer';
const MINIMAL_ENV = {
  PATH: '/usr/bin:/bin:/usr/sbin:/sbin',
  HOME: process.env.HOME,
  LANG: process.env.LANG ?? 'en_US.UTF-8',
};

class MockChild extends EventEmitter implements SpawnedSigner {
  pid = 4242;
  stdin = new PassThrough();
  stdout = new PassThrough();
  kill = vi.fn(() => true);
}

type SignHandler = (child: MockChild, request: SignRequest) => void;

function spawnMock(handler: SignHandler, keyShowCode = 0) {
  const calls: Array<{
    command: string;
    args: string[];
    detached: boolean;
    stdio: ['pipe' | 'ignore', 'pipe' | 'ignore', 'ignore'];
    env: NodeJS.ProcessEnv;
  }> = [];
  const signChildren: MockChild[] = [];
  const spawn: SpawnSigner = (command, args, options) => {
    calls.push({ command, args, detached: options.detached, stdio: options.stdio, env: { ...options.env } });
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

function response(
  reason = '',
  digest = DIGEST,
  decision = 'approve',
  keyId = KEY_ID,
  signatureKeyId = keyId,
): SignResponse {
  return {
    digest,
    decision,
    reason,
    keyId,
    signature: { keyId: signatureKeyId, signedAt: '2026-10-09T01:02:03Z', nonce: 'ab'.repeat(16), sig: 'c2ln' },
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
    expect(mock.calls).toEqual([{
      command: SIGNER_PATH,
      args: ['key', 'show'],
      detached: false,
      stdio: ['ignore', 'ignore', 'ignore'],
      env: MINIMAL_ENV,
    }]);
    expect(mock.calls[0]?.env).not.toHaveProperty('HTTP_PROXY');
    expect(mock.calls[0]?.env).not.toHaveProperty('HTTPS_PROXY');
    expect(mock.calls[0]?.env).not.toHaveProperty('RHIZOME_URL');
    expect(Object.keys(mock.calls[0]!.env).some((key) => key.startsWith('DYLD_'))).toBe(false);
  });

  it('fails closed for a symlink, non-file, non-root, or group/other-writable executable', async () => {
    const base = { signerPath: SIGNER_PATH, spawn: spawnMock(() => undefined).spawn };
    await expect(checkSignerStartup({ ...base, lstat: async (path) => stat(path === SIGNER_PATH ? 'file' : 'directory', { symlink: path === SIGNER_PATH }) }))
      .rejects.toThrow('symbolic link');
    await expect(checkSignerStartup({ ...base, lstat: async () => stat('directory') }))
      .rejects.toThrow('regular file');
    await expect(checkSignerStartup({ ...base, lstat: async (path) => stat(path === SIGNER_PATH ? 'file' : 'directory', { uid: path === SIGNER_PATH ? 501 : 0 }) }))
      .rejects.toThrow('owned by root');
    await expect(checkSignerStartup({ ...base, lstat: async (path) => stat(path === SIGNER_PATH ? 'file' : 'directory', { mode: path === SIGNER_PATH ? 0o775 : 0o755 }) }))
      .rejects.toThrow('must not be group/other writable');
  });

  it('checks the default /usr/local parents and rejects non-root, writable, or symlink parents', async () => {
    const checked: string[] = [];
    const good = spawnMock(() => undefined);
    await checkSignerStartup({
      lstat: async (path) => {
        checked.push(path);
        return stat(path === DEFAULT_SIGNER_PATH ? 'file' : 'directory');
      },
      spawn: good.spawn,
    });
    expect(checked).toEqual([DEFAULT_SIGNER_PATH, '/usr/local/bin', '/usr/local']);

    const base = { spawn: spawnMock(() => undefined).spawn };
    await expect(checkSignerStartup({
      ...base,
      lstat: async (path) => stat(path === DEFAULT_SIGNER_PATH ? 'file' : 'directory', { uid: path === '/usr/local' ? 501 : 0 }),
    })).rejects.toThrow('parent must be owned by root (/usr/local)');
    await expect(checkSignerStartup({
      ...base,
      lstat: async (path) => stat(path === DEFAULT_SIGNER_PATH ? 'file' : 'directory', { mode: path === '/usr/local' ? 0o775 : 0o755 }),
    })).rejects.toThrow('parent must not be group/other writable (/usr/local)');
    await expect(checkSignerStartup({
      ...base,
      lstat: async (path) => stat(path === DEFAULT_SIGNER_PATH ? 'file' : 'directory', { symlink: path === '/usr/local/bin' }),
    })).rejects.toThrow('parent must be a directory (/usr/local/bin)');
  });

  it('fails key show on a nonzero exit and kills it after the 10 s startup timeout', async () => {
    const badKey = spawnMock(() => undefined, 5);
    await expect(checkSignerStartup({ signerPath: SIGNER_PATH, lstat: goodLstat, spawn: badKey.spawn }))
      .rejects.toThrow('key show failed (exit 5)');

    const child = new MockChild();
    let fireTimeout: (() => void) | undefined;
    const timers: SigningTimers = {
      setTimeout(fn, ms) {
        expect(ms).toBe(SIGNER_KEY_SHOW_TIMEOUT_MS);
        fireTimeout = fn;
        return 'key-show-timeout';
      },
      clearTimeout: vi.fn(),
    };
    const spawn: SpawnSigner = () => child;
    const pending = checkSignerStartup({ signerPath: SIGNER_PATH, lstat: goodLstat, spawn, timers });
    await vi.waitFor(() => expect(fireTimeout).toBeTypeOf('function'));
    fireTimeout!();
    await expect(pending).rejects.toThrow('rhizome signer key show timed out');
    expect(child.kill).toHaveBeenCalledWith('SIGKILL');
  });

  it('rejects an unknown RHIZOME_SIGNING value through both public startup entries', async () => {
    const previous = process.env.RHIZOME_SIGNING;
    process.env.RHIZOME_SIGNING = 'unknown';
    try {
      await expect(createUpstream({ url: 'http://127.0.0.1:1' })).rejects.toThrow('RHIZOME_SIGNING');
      await expect(startRhizomeDirectServer({ rhizomeUrl: 'http://127.0.0.1:1', port: 0 })).rejects.toThrow('RHIZOME_SIGNING');
    } finally {
      if (previous === undefined) delete process.env.RHIZOME_SIGNING;
      else process.env.RHIZOME_SIGNING = previous;
    }
  });

  it('matches Rhizome effective-reason selection without trimming stored text', () => {
    expect(effectiveReason('gate.approve', { reason: '  yes  ', instruction: 'ignored' })).toBe('  yes  ');
    expect(effectiveReason('gate.approve', { instruction: 'ignored' })).toBe('');
    expect(effectiveReason('gate.reject', { reason: '  no  ', instruction: 'ignored' })).toBe('  no  ');
    expect(effectiveReason('gate.requestChanges', { reason: '  explicit  ', instruction: 'ignored' })).toBe('  explicit  ');
    expect(effectiveReason('gate.requestChanges', { reason: '   ', instruction: '  fallback  ' })).toBe('  fallback  ');
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

  it('attaches the signature, uses actor signer, and sends only Rhizome\'s effective reason for signed requestChanges', async () => {
    received.length = 0;
    let request: SignRequest | undefined;
    const mock = spawnMock((child, value) => {
      request = value;
      succeed(child, response('  fix it  ', DIGEST, 'requestChanges'));
    });
    const upstream = await upstreamWith(mock);
    try {
      const result = await upstream.relayIntent({
        nodeId: 'q-internal', action: 'gate.requestChanges', idempotencyKey: 'idem-1', decision: { text: '  fix it  ' },
      }, 'cockpit-actor');
      expect(result).toEqual({ accepted: true });
      expect(request).toEqual({
        gateId: 'q-internal', kind: 'gate.requestChanges', reason: '  fix it  ', correlationId: 'cockpit:idem-1',
      });
      expect(received).toEqual([{
        gateId: 'q-internal', digest: DIGEST, reason: '  fix it  ',
        verification: { signature: response().signature }, kind: 'gate.requestChanges', actor: 'signer',
      }]);
      expect(received[0]).not.toHaveProperty('instruction');
      expect(mock.calls).toHaveLength(2);
      for (const call of mock.calls) {
        expect(call.env).toEqual(MINIMAL_ENV);
        expect(call.env).not.toHaveProperty('HTTP_PROXY');
        expect(call.env).not.toHaveProperty('HTTPS_PROXY');
        expect(call.env).not.toHaveProperty('RHIZOME_URL');
        expect(Object.keys(call.env).some((key) => key.startsWith('DYLD_'))).toBe(false);
      }
    } finally {
      upstream.close();
    }
  });

  it('leaves approval requestChanges unsigned and otherwise byte-identical', async () => {
    received.length = 0;
    const off = await createRhizomeUpstream(baseUrl, { signing: { mode: 'off' } });
    try {
      await off.relayIntent({
        nodeId: 'q-approval', action: 'gate.requestChanges', idempotencyKey: 'idem-2', decision: { text: 'revise' },
      }, 'original');
    } finally {
      off.close();
    }
    const offBody = JSON.stringify(received[0]);
    received.length = 0;
    const mock = spawnMock(() => { throw new Error('must not sign'); });
    const upstream = await upstreamWith(mock);
    try {
      await upstream.relayIntent({
        nodeId: 'q-approval', action: 'gate.requestChanges', idempotencyKey: 'idem-2', decision: { text: 'revise' },
      }, 'original');
      expect(mock.calls).toHaveLength(1); // startup key show only
      const expected = {
        instruction: 'revise', gateId: 'q-approval', digest: DIGEST, kind: 'gate.requestChanges', actor: 'original',
      };
      expect(received).toEqual([expected]);
      expect(JSON.stringify(received[0])).toBe(offBody);
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

  it.each([
    [4, SIGNER_REFUSAL.cancelled],
    [5, SIGNER_REFUSAL.noKey],
  ])('lets signer exit %i win over an EPIPE while writing stdin', async (code, reason) => {
    received.length = 0;
    const mock = spawnMock((child) => queueMicrotask(() => {
      child.stdin.emit('error', Object.assign(new Error('broken pipe'), { code: 'EPIPE' }));
      child.emit('close', code, null);
    }));
    const upstream = await upstreamWith(mock);
    try {
      await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: `epipe-${code}` }, 'h'))
        .resolves.toEqual({ accepted: false, reason });
      expect(received).toEqual([]);
    } finally {
      upstream.close();
    }
  });

  it('handles signer stdout and non-EPIPE stdin errors without sending', async () => {
    for (const emitError of [
      (child: MockChild) => child.stdout.emit('error', new Error('stdout failed')),
      (child: MockChild) => child.stdin.emit('error', Object.assign(new Error('stdin failed'), { code: 'EIO' })),
    ]) {
      received.length = 0;
      const upstream = await upstreamWith(spawnMock((child) => queueMicrotask(() => emitError(child))));
      try {
        await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: 'stream-error' }, 'h'))
          .resolves.toEqual({ accepted: false, reason: SIGNER_REFUSAL.failed });
        expect(received).toEqual([]);
      } finally {
        upstream.close();
      }
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

  it('refuses signer responses whose decision, reason, or key identity does not match the request', async () => {
    const otherKey = `sha256:${'c'.repeat(64)}`;
    const mismatches: SignHandler[] = [
      (child) => succeed(child, response('', DIGEST, 'reject')),
      (child) => succeed(child, response('changed by signer')),
      (child) => succeed(child, response('', DIGEST, 'approve', `sha256:${'B'.repeat(64)}`)),
      (child) => succeed(child, response('', DIGEST, 'approve', KEY_ID, otherKey)),
    ];
    for (const handler of mismatches) {
      received.length = 0;
      const upstream = await upstreamWith(spawnMock(handler));
      try {
        await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: 'mismatch' }, 'h'))
          .resolves.toEqual({ accepted: false, reason: SIGNER_REFUSAL.responseMismatch });
        expect(received).toEqual([]);
      } finally {
        upstream.close();
      }
    }
  });

  it('accepts the kind-specific approve/allow decision alias', async () => {
    received.length = 0;
    const upstream = await upstreamWith(spawnMock((child) => succeed(child, response('', DIGEST, 'allow'))));
    try {
      await expect(upstream.relayIntent({ nodeId: 'q-internal', action: 'gate.approve', idempotencyKey: 'allow' }, 'h'))
        .resolves.toEqual({ accepted: true });
      expect(received).toHaveLength(1);
      expect(received[0]).toMatchObject({ kind: 'gate.approve', actor: 'signer' });
    } finally {
      upstream.close();
    }
  });

  it('sends a signed reject reason from decision.text and accepts the reject/deny decision alias', async () => {
    received.length = 0;
    let request: SignRequest | undefined;
    const upstream = await upstreamWith(spawnMock((child, value) => {
      request = value;
      succeed(child, response('do not ship', DIGEST, 'deny'));
    }));
    try {
      await expect(upstream.relayIntent({
        nodeId: 'q-internal', action: 'gate.reject', idempotencyKey: 'deny', decision: { text: 'do not ship' },
      }, 'h')).resolves.toEqual({ accepted: true });
      expect(request).toEqual({
        gateId: 'q-internal', kind: 'gate.reject', reason: 'do not ship', correlationId: 'cockpit:deny',
      });
      expect(received).toEqual([{
        gateId: 'q-internal', digest: DIGEST, reason: 'do not ship',
        verification: { signature: response().signature }, kind: 'gate.reject', actor: 'signer',
      }]);
    } finally {
      upstream.close();
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
      expect(JSON.stringify(received[0])).toBe(JSON.stringify({
        gateId: 'q-internal', digest: DIGEST, kind: 'gate.approve', actor: 'operator',
      }));
    } finally {
      upstream.close();
    }
  });
});
