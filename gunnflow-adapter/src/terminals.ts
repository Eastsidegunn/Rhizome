/**
 * RHZ-141 (FR-RHZ-185): the direct wire's terminal routes (Gunnflow GF-T2),
 * relayed to Rhizome's /v1/terminals bridge (RHZ-140). A dumb relay: output
 * and input stay base64 strings end to end, never decoded or re-encoded here.
 *
 *   GET    /terminals               list: open attachable registry sessions
 *                                   (id = Rhizome session id) + live terminals
 *   POST   /terminals               {target:{attach:id}|{shell:{cwd}}, cols, rows}
 *                                   → {terminalId}; Rhizome refusals → 422 {reason}
 *   GET    /terminals/{id}/stream   SSE snapshot·data (data: = base64 itself)·exit{code}
 *   POST   /terminals/{id}/input    {data} → 204
 *   POST   /terminals/{id}/resize   {cols, rows} → 204
 *   DELETE /terminals/{id}          → 204
 *
 * RHZ-150: target {attach:'log:<executionId>'} opens a launcher run's log read
 * only; its input/resize 409 passes through with Rhizome's read-only reason.
 * Unknown id 404; a terminal this relay saw end (exit relayed or DELETE) is
 * 409 — Rhizome forgets an ended terminal, so the relay remembers it. Rhizome's
 * 501 (non-loopback bind) passes through. The contract package carries no
 * terminal types yet, so the wire shapes are local.
 */
import type { IncomingMessage, ServerResponse } from 'node:http';
import { SseDecoder } from './upstream.js';

const ITEM_RE = /^\/terminals\/([^/]+)$/;
const OP_RE = /^\/terminals\/([^/]+)\/(stream|input|resize)$/;
/** Ended ids kept for the 409 answer; the oldest go first past this. */
const ENDED_MAX = 1024;

/** One GET /terminals entry. */
export interface WireTerminal {
  terminalId: string;
  label: string;
  kind: 'attachable' | 'shell';
  state: string;
  nodeId?: string;
}

interface RhizomeTerminalList {
  sessionTargets?: boolean;
  // RHZ-142: attachable/kind/status; attachable false = a session claude
  // attach cannot reach (another terminal, a -p run). Absent = older Rhizome.
  sessions?: Array<{ sessionId: string; name?: string; missionId?: string; attachable?: boolean; kind?: string; status?: string }>;
  // RHZ-150: launcher -p runs openable as a read-only log view.
  logs?: Array<{ executionId: string; sessionId?: string; missionId?: string; name?: string; running?: boolean }>;
  terminals?: Array<{ terminalId: string; kind: string; sessionId?: string; executionId?: string; cwd?: string }>;
}

/** RHZ-150: a log target's wire id; opening it is Rhizome's execution target. */
export const LOG_PREFIX = 'log:';
const READ_ONLY = '보기 전용';

/**
 * GET /terminals body from Rhizome's list. Session targets are listed only
 * when Rhizome can attach (sessionTargets); a mission's node id is the
 * Rhizome mission id itself (the adapter projects missions under their id).
 * Sessions Rhizome marks attachable: false are left out (RHZ-142).
 * RHZ-150: a launcher run's log is an 'attachable' target `log:<executionId>`
 * labelled 보기 전용 (the wire has no read-only kind); a finished run stays
 * 'running' so the cockpit can still open it, its label says it ended.
 */
export function terminalList(body: RhizomeTerminalList): WireTerminal[] {
  const sessions = body.sessions ?? [];
  const logs = body.logs ?? [];
  const byId = new Map(sessions.map((s) => [s.sessionId, s]));
  const logById = new Map(logs.map((l) => [l.executionId, l]));
  const node = (missionId?: string) => (missionId ? { nodeId: missionId } : {});
  const logLabel = (l: { executionId: string; name?: string; running?: boolean }) =>
    `${l.name || l.executionId} · ${l.running === false ? `끝남 · ${READ_ONLY}` : READ_ONLY}`;
  const targets: WireTerminal[] = body.sessionTargets
    ? sessions.filter((s) => s.attachable !== false).map((s): WireTerminal => ({ terminalId: s.sessionId, label: s.name || s.sessionId, kind: 'attachable', state: 'running', ...node(s.missionId) }))
    : [];
  const logTargets: WireTerminal[] = logs.map((l): WireTerminal => ({ terminalId: LOG_PREFIX + l.executionId, label: logLabel(l), kind: 'attachable', state: 'running', ...node(l.missionId) }));
  const open: WireTerminal[] = (body.terminals ?? []).map((t): WireTerminal => {
    if (t.kind === 'log') {
      const l = t.executionId ? logById.get(t.executionId) : undefined;
      return { terminalId: t.terminalId, label: logLabel(l ?? { executionId: t.executionId || t.terminalId }), kind: 'attachable', state: 'running', ...node(l?.missionId) };
    }
    const s = t.sessionId ? byId.get(t.sessionId) : undefined;
    return t.kind === 'shell'
      ? { terminalId: t.terminalId, label: t.cwd || t.terminalId, kind: 'shell', state: 'running' }
      : { terminalId: t.terminalId, label: s?.name || t.sessionId || t.terminalId, kind: 'attachable', state: 'running', ...node(s?.missionId) };
  });
  return [...targets, ...logTargets, ...open];
}

/** Rhizome's open body for a wire target, or the reason it is malformed. */
export function openBody(input: unknown): { body: unknown } | { reason: string } {
  const i = (input ?? {}) as { target?: { attach?: unknown; shell?: { cwd?: unknown } }; cols?: unknown; rows?: unknown };
  const t = i.target;
  if (!t || typeof t !== 'object') return { reason: 'target required' };
  if ((t.attach === undefined) === (t.shell === undefined)) return { reason: 'target needs exactly one of attach or shell' };
  const { cols, rows } = i;
  if (typeof cols !== 'number' || typeof rows !== 'number') return { reason: 'cols and rows required' };
  if (t.attach !== undefined) {
    if (typeof t.attach !== 'string' || !t.attach) return { reason: 'target.attach must be a session id' };
    if (t.attach.startsWith(LOG_PREFIX)) {
      const executionId = t.attach.slice(LOG_PREFIX.length);
      if (!executionId) return { reason: 'target.attach log: needs an execution id' };
      return { body: { target: { execution: { executionId } }, cols, rows } };
    }
    return { body: { target: { session: { sessionId: t.attach } }, cols, rows } };
  }
  const cwd = t.shell?.cwd;
  if (typeof cwd !== 'string' || !cwd) return { reason: 'target.shell.cwd required' };
  return { body: { target: { shell: { cwd } }, cols, rows } };
}

/**
 * RHZ-147 (FR-RHZ-191): one Rhizome stream event as a wire frame, or null for
 * an event name the wire does not carry. Rhizome sends snapshot·output as JSON
 * {data: base64} and exit as {exitCode}; the wire's snapshot·data `data:` line
 * is the base64 string itself (empty output = empty line), exit's is {code}.
 * Unparseable JSON throws, which ends the stream (the cockpit resyncs).
 */
export function wireFrame(e: { event: string; data: string }): { event: 'snapshot' | 'data' | 'exit'; data: string } | null {
  if (e.event !== 'snapshot' && e.event !== 'output' && e.event !== 'exit') return null;
  const d = JSON.parse(e.data) as { data?: unknown; exitCode?: unknown };
  if (e.event === 'exit') return { event: 'exit', data: JSON.stringify({ code: Number.isInteger(d.exitCode) ? d.exitCode : null }) };
  return { event: e.event === 'output' ? 'data' : 'snapshot', data: typeof d.data === 'string' ? d.data : '' };
}

async function readBody(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    return undefined;
  }
}

function json(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { 'content-type': 'application/json' });
  res.end(JSON.stringify(body));
}

async function reasonOf(up: Response): Promise<string> {
  return (await up.text()).trim() || `rhizome answered ${up.status}`;
}

/** Terminal routes on the direct wire; returns false when the path is not one. */
export function createTerminalRelay(rhizomeUrl: string) {
  const ended = new Set<string>();
  const markEnded = (id: string) => {
    ended.add(id);
    if (ended.size > ENDED_MAX) ended.delete(ended.values().next().value!);
  };
  const base = `${rhizomeUrl}/v1/terminals`;
  const item = (id: string) => `${base}/${encodeURIComponent(id)}`;
  // A per-terminal failure: 404 of an id seen ending is 409; others pass through.
  const fail = async (res: ServerResponse, id: string, up: Response) => {
    const status = up.status === 404 && ended.has(id) ? 409 : up.status;
    json(res, status, { reason: status === 409 && up.status === 404 ? 'terminal has ended' : await reasonOf(up) });
  };
  const post = (url: string, body: unknown) =>
    fetch(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });

  return async function handle(req: IncomingMessage, res: ServerResponse, path: string): Promise<boolean> {
    if (path === '/terminals') {
      if (req.method === 'GET') {
        const up = await fetch(base);
        if (!up.ok) json(res, up.status, { reason: await reasonOf(up) });
        else json(res, 200, terminalList((await up.json()) as RhizomeTerminalList));
        return true;
      }
      if (req.method === 'POST') {
        const open = openBody(await readBody(req));
        if ('reason' in open) {
          json(res, 422, { reason: open.reason });
          return true;
        }
        const up = await post(base, open.body);
        if (up.ok) json(res, 200, { terminalId: ((await up.json()) as { terminalId: string }).terminalId });
        // Rhizome's refusals (bad target or size 400, session not open 404,
        // no claudePath 409) are the wire's 422; 501 and the rest pass through.
        else json(res, [400, 404, 409].includes(up.status) ? 422 : up.status, { reason: await reasonOf(up) });
        return true;
      }
      return false;
    }
    const itemMatch = req.method === 'DELETE' ? ITEM_RE.exec(path) : null;
    if (itemMatch) {
      const id = decodeURIComponent(itemMatch[1]!);
      const up = await fetch(item(id), { method: 'DELETE' });
      if (up.ok) {
        markEnded(id);
        res.writeHead(204).end();
      } else await fail(res, id, up);
      return true;
    }
    const opMatch = OP_RE.exec(path);
    if (!opMatch) return false;
    const id = decodeURIComponent(opMatch[1]!);
    const op = opMatch[2]!;
    if (op === 'stream' && req.method === 'GET') {
      const abort = new AbortController();
      // the cockpit leaving ends the Rhizome stream too (its subscriber goes)
      res.on('close', () => abort.abort());
      const up = await fetch(`${item(id)}/stream`, { headers: { accept: 'text/event-stream' }, signal: abort.signal });
      if (!up.ok || !up.body) {
        await fail(res, id, up);
        return true;
      }
      res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
      const send = (event: string, data: string) => res.write(`event: ${event}\ndata: ${data}\n\n`);
      const reader = up.body.getReader();
      const decoder = new SseDecoder();
      const text = new TextDecoder();
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const e of decoder.push(text.decode(value, { stream: true }))) {
            const frame = wireFrame(e);
            if (!frame) continue;
            if (frame.event === 'exit') markEnded(id);
            send(frame.event, frame.data);
          }
        }
      } catch {
        // the cockpit left, or Rhizome's stream broke: headers are out, so end below
      }
      // Rhizome's end (exit, a slow reader cut off, a break) ends this stream
      // too; the cockpit reconnects for a fresh snapshot as against Rhizome.
      res.end();
      return true;
    }
    if ((op === 'input' || op === 'resize') && req.method === 'POST') {
      const b = (await readBody(req)) as Record<string, unknown> | undefined;
      const body = op === 'input' ? { data: b?.data } : { cols: b?.cols, rows: b?.rows };
      const up = await post(`${item(id)}/${op}`, body);
      if (up.ok) res.writeHead(204).end();
      else await fail(res, id, up);
      return true;
    }
    return false;
  };
}
