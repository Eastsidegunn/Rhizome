import { spawn as nodeSpawn } from 'node:child_process';
import { lstat as nodeLstat } from 'node:fs/promises';
import { dirname } from 'node:path';

export const DEFAULT_SIGNER_PATH = '/usr/local/bin/rhizome-signer';
export const SIGNER_KEY_SHOW_TIMEOUT_MS = 10_000;
export const SIGNER_TIMEOUT_MS = 60_000;
export const SIGNER_STDOUT_MAX_BYTES = 64 * 1024;

export type SigningMode = 'off' | 'required';

export interface SignerStat {
  uid: number;
  mode: number;
  isFile(): boolean;
  isDirectory(): boolean;
  isSymbolicLink(): boolean;
}

export interface SpawnedSigner {
  pid?: number;
  stdin: {
    end(data?: string): void;
    on(event: 'error', listener: (error: Error) => void): unknown;
  } | null;
  stdout: {
    on(event: 'data', listener: (chunk: Buffer | string) => void): unknown;
    on(event: 'error', listener: (error: Error) => void): unknown;
  } | null;
  once(event: 'error', listener: (error: Error) => void): unknown;
  once(event: 'close', listener: (code: number | null, signal: NodeJS.Signals | null) => void): unknown;
  kill(signal?: NodeJS.Signals): boolean;
}

export type SpawnSigner = (
  command: string,
  args: string[],
  options: {
    detached: boolean;
    stdio: ['pipe' | 'ignore', 'pipe' | 'ignore', 'ignore'];
    env: NodeJS.ProcessEnv;
  },
) => SpawnedSigner;

export interface SigningTimers {
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

const REAL_SIGNING_TIMERS: SigningTimers = {
  setTimeout(fn, ms) {
    const timer = setTimeout(fn, ms);
    timer.unref?.();
    return timer;
  },
  clearTimeout(handle) {
    clearTimeout(handle as ReturnType<typeof setTimeout>);
  },
};

/** All OS-facing operations are seams so tests never need a root-owned binary or a real child. */
export interface SigningOptions {
  mode?: string;
  signerPath?: string;
  parentPaths?: readonly string[];
  lstat?: (path: string) => Promise<SignerStat>;
  spawn?: SpawnSigner;
  kill?: (pid: number, signal: NodeJS.Signals) => void;
  timers?: SigningTimers;
  keyShowTimeoutMs?: number;
  timeoutMs?: number;
  stdoutMaxBytes?: number;
}

export interface SignRequest {
  gateId: string;
  kind: string;
  reason: string;
  correlationId: string;
}

export interface SignerSignature {
  keyId: string;
  signedAt: string;
  nonce: string;
  sig: string;
}

export interface SignResponse {
  digest: string;
  decision: string;
  reason: string;
  keyId: string;
  signature: SignerSignature;
}

export type SignResult =
  | { ok: true; response: SignResponse }
  | { ok: false; reason: string };

export const SIGNER_REFUSAL = {
  refused: 'signer refused',
  rhizomeReadFailed: 'signer Rhizome read failed',
  cancelled: 'signing cancelled',
  noKey: 'signer key unavailable',
  notSignable: 'gate decision is not signable',
  timeout: 'signer timed out',
  malformedOutput: 'signer returned malformed output',
  outputTooLarge: 'signer output exceeded 64 KiB',
  responseMismatch: 'signer response does not match request',
  digestMismatch: 'signer digest does not match adapter snapshot',
  failed: 'signer failed',
} as const;

const EXIT_REFUSALS: Readonly<Record<number, string>> = {
  2: SIGNER_REFUSAL.refused,
  3: SIGNER_REFUSAL.rhizomeReadFailed,
  4: SIGNER_REFUSAL.cancelled,
  5: SIGNER_REFUSAL.noKey,
  6: SIGNER_REFUSAL.notSignable,
};

export function signingMode(value: string | undefined): SigningMode {
  if (value === undefined || value === 'off') return 'off';
  if (value === 'required') return 'required';
  throw new Error('RHIZOME_SIGNING must be "required" or "off"');
}

const oneLine = (error: unknown): string =>
  (error instanceof Error ? error.message : String(error)).replace(/[\r\n]+/g, ' ').trim();

const defaultSpawn: SpawnSigner = (command, args, options) =>
  nodeSpawn(command, args, options) as unknown as SpawnedSigner;

function signerEnvironment(): NodeJS.ProcessEnv {
  return {
    PATH: '/usr/bin:/bin:/usr/sbin:/sbin',
    HOME: process.env.HOME,
    LANG: process.env.LANG ?? 'en_US.UTF-8',
  };
}

function resolved(options: SigningOptions) {
  const signerPath = options.signerPath ?? DEFAULT_SIGNER_PATH;
  return {
    signerPath,
    parentPaths: options.parentPaths ?? [dirname(signerPath), dirname(dirname(signerPath))],
    lstat: options.lstat ?? (nodeLstat as (path: string) => Promise<SignerStat>),
    spawn: options.spawn ?? defaultSpawn,
    kill: options.kill ?? ((pid: number, signal: NodeJS.Signals) => process.kill(pid, signal)),
    timers: options.timers ?? REAL_SIGNING_TIMERS,
    keyShowTimeoutMs: options.keyShowTimeoutMs ?? SIGNER_KEY_SHOW_TIMEOUT_MS,
    timeoutMs: options.timeoutMs ?? SIGNER_TIMEOUT_MS,
    stdoutMaxBytes: options.stdoutMaxBytes ?? SIGNER_STDOUT_MAX_BYTES,
    env: signerEnvironment(),
  };
}

function insecureMode(stat: SignerStat): boolean {
  return (stat.mode & 0o022) !== 0;
}

/** Validate the immutable/root-owned executable boundary, then prove that a key is available. */
export async function checkSignerStartup(options: SigningOptions = {}): Promise<void> {
  const runtime = resolved(options);
  let executable: SignerStat;
  try {
    executable = await runtime.lstat(runtime.signerPath);
  } catch (error) {
    throw new Error(`rhizome signer check failed: ${oneLine(error)}`);
  }
  if (executable.isSymbolicLink()) throw new Error('rhizome signer must not be a symbolic link');
  if (!executable.isFile()) throw new Error('rhizome signer must be a regular file');
  if (executable.uid !== 0) throw new Error('rhizome signer must be owned by root');
  if (insecureMode(executable)) throw new Error('rhizome signer must not be group/other writable');

  for (const parentPath of runtime.parentPaths) {
    let parent: SignerStat;
    try {
      parent = await runtime.lstat(parentPath);
    } catch (error) {
      throw new Error(`rhizome signer parent check failed (${parentPath}): ${oneLine(error)}`);
    }
    if (parent.isSymbolicLink() || !parent.isDirectory()) {
      throw new Error(`rhizome signer parent must be a directory (${parentPath})`);
    }
    if (parent.uid !== 0) throw new Error(`rhizome signer parent must be owned by root (${parentPath})`);
    if (insecureMode(parent)) throw new Error(`rhizome signer parent must not be group/other writable (${parentPath})`);
  }

  const code = await spawnForExit(runtime.spawn, runtime.signerPath, ['key', 'show'], runtime.env, runtime.timers, runtime.keyShowTimeoutMs);
  if (code !== 0) throw new Error(`rhizome signer key show failed (exit ${code === null ? 'unknown' : code})`);
}

function spawnForExit(
  spawn: SpawnSigner,
  command: string,
  args: string[],
  env: NodeJS.ProcessEnv,
  timers: SigningTimers,
  timeoutMs: number,
): Promise<number | null> {
  return new Promise((resolve, reject) => {
    let child: SpawnedSigner;
    try {
      child = spawn(command, args, { detached: false, stdio: ['ignore', 'ignore', 'ignore'], env });
    } catch (error) {
      reject(new Error(`rhizome signer key show failed: ${oneLine(error)}`));
      return;
    }
    let settled = false;
    let timeout: unknown;
    const finish = (result: { code: number | null } | { error: Error }) => {
      if (settled) return;
      settled = true;
      timers.clearTimeout(timeout);
      if ('error' in result) reject(result.error);
      else resolve(result.code);
    };
    child.once('error', (error) => {
      finish({ error: new Error(`rhizome signer key show failed: ${oneLine(error)}`) });
    });
    child.once('close', (code) => {
      finish({ code });
    });
    timeout = timers.setTimeout(() => {
      finish({ error: new Error('rhizome signer key show timed out') });
      try { child.kill('SIGKILL'); } catch { /* already gone */ }
    }, timeoutMs);
  });
}

const exactKeys = (value: Record<string, unknown>, keys: readonly string[]): boolean => {
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
};

function parseResponse(stdout: string): SignResponse | undefined {
  let value: unknown;
  try {
    value = JSON.parse(stdout);
  } catch {
    return undefined;
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined;
  const response = value as Record<string, unknown>;
  if (!exactKeys(response, ['digest', 'decision', 'reason', 'keyId', 'signature'])) return undefined;
  const signature = response.signature;
  if (typeof signature !== 'object' || signature === null || Array.isArray(signature)) return undefined;
  const sig = signature as Record<string, unknown>;
  if (!exactKeys(sig, ['keyId', 'signedAt', 'nonce', 'sig'])) return undefined;
  if (
    typeof response.digest !== 'string' || typeof response.decision !== 'string' ||
    typeof response.reason !== 'string' || typeof response.keyId !== 'string' ||
    typeof sig.keyId !== 'string' || typeof sig.signedAt !== 'string' ||
    typeof sig.nonce !== 'string' || typeof sig.sig !== 'string'
  ) return undefined;
  return {
    digest: response.digest,
    decision: response.decision,
    reason: response.reason,
    keyId: response.keyId,
    signature: { keyId: sig.keyId, signedAt: sig.signedAt, nonce: sig.nonce, sig: sig.sig },
  };
}

const SIGNER_KEY_ID_RE = /^sha256:[0-9a-f]{64}$/;
const RESPONSE_DECISIONS: Readonly<Record<string, ReadonlySet<string>>> = {
  'gate.approve': new Set(['approve', 'allow']),
  'gate.reject': new Set(['reject', 'deny']),
  'gate.requestChanges': new Set(['requestChanges']),
};

function responseMatchesRequest(response: SignResponse, request: SignRequest): boolean {
  return RESPONSE_DECISIONS[request.kind]?.has(response.decision) === true &&
    response.reason === request.reason &&
    SIGNER_KEY_ID_RE.test(response.keyId) &&
    response.keyId === response.signature.keyId;
}

function isEpipe(error: unknown): boolean {
  return typeof error === 'object' && error !== null && 'code' in error && (error as NodeJS.ErrnoException).code === 'EPIPE';
}

/** One serialized client: a prompt is never concurrent with another cockpit prompt. */
export class SignerClient {
  private tail: Promise<void> = Promise.resolve();
  private readonly runtime: ReturnType<typeof resolved>;

  constructor(options: SigningOptions = {}) {
    this.runtime = resolved(options);
  }

  sign(request: SignRequest): Promise<SignResult> {
    const previous = this.tail;
    let release!: () => void;
    this.tail = new Promise<void>((resolve) => { release = resolve; });
    return (async () => {
      await previous;
      try {
        return await this.run(request);
      } finally {
        release();
      }
    })();
  }

  private run(request: SignRequest): Promise<SignResult> {
    return new Promise((resolve) => {
      let child: SpawnedSigner;
      try {
        child = this.runtime.spawn(this.runtime.signerPath, ['sign-stdin'], {
          detached: true,
          stdio: ['pipe', 'pipe', 'ignore'],
          env: this.runtime.env,
        });
      } catch {
        resolve({ ok: false, reason: SIGNER_REFUSAL.failed });
        return;
      }

      let settled = false;
      let stdoutBytes = 0;
      const chunks: Buffer[] = [];
      const finish = (result: SignResult) => {
        if (settled) return;
        settled = true;
        this.runtime.timers.clearTimeout(timeout);
        resolve(result);
      };
      const killGroup = () => {
        try {
          if (typeof child.pid === 'number' && child.pid > 0) this.runtime.kill(-child.pid, 'SIGKILL');
          else child.kill('SIGKILL');
        } catch {
          try { child.kill('SIGKILL'); } catch { /* already gone */ }
        }
      };
      const timeout = this.runtime.timers.setTimeout(() => {
        killGroup();
        finish({ ok: false, reason: SIGNER_REFUSAL.timeout });
      }, this.runtime.timeoutMs);

      child.stdout?.on('data', (chunk) => {
        if (settled) return;
        const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
        stdoutBytes += bytes.length;
        if (stdoutBytes > this.runtime.stdoutMaxBytes) {
          killGroup();
          finish({ ok: false, reason: SIGNER_REFUSAL.outputTooLarge });
          return;
        }
        chunks.push(bytes);
      });
      child.stdout?.on('error', () => {
        if (settled) return;
        killGroup();
        finish({ ok: false, reason: SIGNER_REFUSAL.failed });
      });
      child.once('error', () => finish({ ok: false, reason: SIGNER_REFUSAL.failed }));
      child.once('close', (code) => {
        if (settled) return;
        if (code !== 0) {
          finish({ ok: false, reason: code === null ? SIGNER_REFUSAL.failed : (EXIT_REFUSALS[code] ?? SIGNER_REFUSAL.failed) });
          return;
        }
        const response = parseResponse(Buffer.concat(chunks).toString('utf8'));
        if (!response) {
          finish({ ok: false, reason: SIGNER_REFUSAL.malformedOutput });
          return;
        }
        finish(responseMatchesRequest(response, request)
          ? { ok: true, response }
          : { ok: false, reason: SIGNER_REFUSAL.responseMismatch });
      });
      child.stdin?.on('error', (error) => {
        if (settled) return;
        if (isEpipe(error)) return; // the close code carries the useful refusal (notably 4/5)
        killGroup();
        finish({ ok: false, reason: SIGNER_REFUSAL.failed });
      });
      try {
        child.stdin?.end(JSON.stringify(request));
      } catch (error) {
        if (isEpipe(error)) return; // wait for close so its fixed exit-code mapping wins
        killGroup();
        finish({ ok: false, reason: SIGNER_REFUSAL.failed });
      }
    });
  }
}

/** Parse mode and perform the required startup checks before exposing a signing client. */
export async function prepareSigner(options: SigningOptions = {}): Promise<SignerClient | undefined> {
  if (signingMode(options.mode) === 'off') return undefined;
  await checkSignerStartup(options);
  return new SignerClient(options);
}
