import { spawn as nodeSpawn } from 'node:child_process';
import { lstat as nodeLstat } from 'node:fs/promises';
import { dirname } from 'node:path';

export const DEFAULT_SIGNER_PATH = '/usr/local/bin/rhizome-signer';
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
  stdin: { end(data?: string): void; on?(event: 'error', listener: (error: Error) => void): unknown } | null;
  stdout: { on(event: 'data', listener: (chunk: Buffer | string) => void): unknown } | null;
  once(event: 'error', listener: (error: Error) => void): unknown;
  once(event: 'close', listener: (code: number | null, signal: NodeJS.Signals | null) => void): unknown;
  kill(signal?: NodeJS.Signals): boolean;
}

export type SpawnSigner = (
  command: string,
  args: string[],
  options: { detached: boolean; stdio: ['pipe' | 'ignore', 'pipe' | 'ignore', 'ignore'] },
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

function resolved(options: SigningOptions) {
  const signerPath = options.signerPath ?? DEFAULT_SIGNER_PATH;
  return {
    signerPath,
    parentPaths: options.parentPaths ?? [dirname(signerPath), dirname(dirname(signerPath))],
    lstat: options.lstat ?? (nodeLstat as (path: string) => Promise<SignerStat>),
    spawn: options.spawn ?? defaultSpawn,
    kill: options.kill ?? ((pid: number, signal: NodeJS.Signals) => process.kill(pid, signal)),
    timers: options.timers ?? REAL_SIGNING_TIMERS,
    timeoutMs: options.timeoutMs ?? SIGNER_TIMEOUT_MS,
    stdoutMaxBytes: options.stdoutMaxBytes ?? SIGNER_STDOUT_MAX_BYTES,
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

  const code = await spawnForExit(runtime.spawn, runtime.signerPath, ['key', 'show']);
  if (code !== 0) throw new Error(`rhizome signer key show failed (exit ${code === null ? 'unknown' : code})`);
}

function spawnForExit(spawn: SpawnSigner, command: string, args: string[]): Promise<number | null> {
  return new Promise((resolve, reject) => {
    let child: SpawnedSigner;
    try {
      child = spawn(command, args, { detached: false, stdio: ['ignore', 'ignore', 'ignore'] });
    } catch (error) {
      reject(new Error(`rhizome signer key show failed: ${oneLine(error)}`));
      return;
    }
    let settled = false;
    child.once('error', (error) => {
      if (settled) return;
      settled = true;
      reject(new Error(`rhizome signer key show failed: ${oneLine(error)}`));
    });
    child.once('close', (code) => {
      if (settled) return;
      settled = true;
      resolve(code);
    });
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
    typeof sig.nonce !== 'string' || typeof sig.sig !== 'string' ||
    response.keyId !== sig.keyId
  ) return undefined;
  return {
    digest: response.digest,
    decision: response.decision,
    reason: response.reason,
    keyId: response.keyId,
    signature: { keyId: sig.keyId, signedAt: sig.signedAt, nonce: sig.nonce, sig: sig.sig },
  };
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
      child.once('error', () => finish({ ok: false, reason: SIGNER_REFUSAL.failed }));
      child.once('close', (code) => {
        if (settled) return;
        if (code !== 0) {
          finish({ ok: false, reason: code === null ? SIGNER_REFUSAL.failed : (EXIT_REFUSALS[code] ?? SIGNER_REFUSAL.failed) });
          return;
        }
        const response = parseResponse(Buffer.concat(chunks).toString('utf8'));
        finish(response ? { ok: true, response } : { ok: false, reason: SIGNER_REFUSAL.malformedOutput });
      });
      child.stdin?.on?.('error', () => finish({ ok: false, reason: SIGNER_REFUSAL.failed }));
      try {
        child.stdin?.end(JSON.stringify(request));
      } catch {
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
