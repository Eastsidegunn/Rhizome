/**
 * RHZ-141 (FR-RHZ-185): the direct wire's terminal routes (Gunnflow GF-T2),
 * relayed to Rhizome's /v1/terminals bridge (RHZ-140). A dumb relay: output
 * and input stay base64 strings end to end, never decoded or re-encoded here.
 *
 *   GET    /terminals               list: open registry sessions (attachable,
 *                                   id = Rhizome session id) + live terminals
 *   POST   /terminals               {target:{attach:id}|{shell:{cwd}}, cols, rows}
 *                                   → {terminalId}; Rhizome refusals → 422 {reason}
 *   GET    /terminals/{id}/stream   SSE snapshot·data·exit{code}
 *   POST   /terminals/{id}/input    {data} → 204
 *   POST   /terminals/{id}/resize   {cols, rows} → 204
 *   DELETE /terminals/{id}          → 204
 *
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
  sessions?: Array<{ sessionId: string; name?: string; missionId?: string }>;
  terminals?: Array<{ terminalId: string; kind: string; sessionId?: string; cwd?: string }>;
}

/**
 * GET /terminals body from Rhizome's list. Session targets are listed only
 * when Rhizome can attach (sessionTargets); a mission's node id is the
 * Rhizome mission id itself (the adapter projects missions under their id).
 */
export function terminalList(body: RhizomeTerminalList): WireTerminal[] {
  const sessions = body.sessions ?? [];
  const byId = new Map(sessions.map((s) => [s.sessionId, s]));
  const node = (missionId?: string) => (missionId ? { nodeId: missionId } : {});
  const targets: WireTerminal[] = body.sessionTargets
    ? sessions.map((s): WireTerminal => ({ terminalId: s.sessionId, label: s.name || s.sessionId, kind: 'attachable', state: 'open', ...node(s.missionId) }))
    : [];
  const open: WireTerminal[] = (body.terminals ?? []).map((t): WireTerminal => {
    const s = t.sessionId ? byId.get(t.sessionId) : undefined;
    return t.kind === 'shell'
      ? { terminalId: t.terminalId, label: t.cwd || t.terminalId, kind: 'shell', state: 'running' }
      : { terminalId: t.terminalId, label: s?.name || t.sessionId || t.terminalId, kind: 'attachable', state: 'running', ...node(s?.missionId) };
  });
  return [...targets, ...open];
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
    return { body: { target: { session: { sessionId: t.attach } }, cols, rows } };
  }
  const cwd = t.shell?.cwd;
  if (typeof cwd !== 'string' || !cwd) return { reason: 'target.shell.cwd required' };
  return { body: { target: { shell: { cwd } }, cols, rows } };
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
      const send = (event: string, data: unknown) => res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
      const reader = up.body.getReader();
      const decoder = new SseDecoder();
      const text = new TextDecoder();
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const e of decoder.push(text.decode(value, { stream: true }))) {
            const d = JSON.parse(e.data) as { data?: string; exitCode?: number };
            if (e.event === 'snapshot') send('snapshot', { data: d.data });
            else if (e.event === 'output') send('data', { data: d.data });
            else if (e.event === 'exit') {
              markEnded(id);
              send('exit', { code: d.exitCode });
            }
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
