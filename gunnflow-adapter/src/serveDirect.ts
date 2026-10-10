/**
 * Rhizome's direct-wire server implements the Gunnflow contract. It opens the
 * four wire endpoints and sources them from its own /v1/workspace surface via the reshape
 * in ./upstream.ts. There is no Gunnflow-loaded adapter module: the connection
 * is a URL (gunnflow.config.json → { upstream: "direct", url }). Intents are
 * checked with the contract's own validateIntent before Rhizome sees them, as a
 * native backend must. Stage 0 carries only what Rhizome already has —
 * id/kind/label/state/relations, revision, the actually-supported capabilities
 * (task pause/resume/instruct, mission.create); artifacts answer 404.
 *
 * Paths come from the contract's DIRECT_WIRE (npm @gunnflow/contract 0.3.2, RHZ-094). The
 * optional fifth endpoint, GET /detail/:nodeId, answers
 * from the snapshot already held (./details.ts); its only further fetch is the
 * goal/mission about notes from /v1/context (RHZ-086), failure-isolated.
 * The optional execution surface, GET /execution/:taskId (+ /stream), answers
 * from Rhizome /v1/execution/{missionId} via the upstream's per-task feed
 * (RHZ-094, FR-RHZ-121; projection in ./execution.ts). Terminal has no wire
 * route in the contract: /terminal/* is "not on the direct wire" (404) and the
 * port's terminalSnapshot stays its 501-worded unsupported.
 */
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { pathToFileURL } from 'node:url';
import {
  DIRECT_WIRE,
  WORKSPACE_ROOT_KIND,
  lookupCapability,
  validateIntent,
  validateNodeDetail,
  type ExecutionSnapshot,
  type NodeProjection,
} from '@gunnflow/contract';
import { blockedNoteEntry, detailItems, fetchContextNotes, noteDetail, noteQueryFor, withNotes, type WireDetailBody } from './details.js';
import { executionForWire } from './execution.js';
import { createRhizomeUpstream, nodesOf } from './upstream.js';
import type { SigningOptions } from './signing.js';

/** The direct wire's fixed paths and header (WIRE.md). */
const WIRE = DIRECT_WIRE;
const ARTIFACT_RE = /^\/artifact\/([^/]+)\/([^/]+)$/;
const DETAIL_RE = /^\/detail\/([^/]+)$/;
// RHZ-094 (FR-RHZ-121): WIRE.md §execution. The task id segment is
// percent-encoded on the wire (Rhizome ids may contain '/').
const EXECUTION_RE = /^\/execution\/([^/]+)$/;
const EXECUTION_STREAM_RE = /^\/execution\/([^/]+)\/stream$/;

/** Body of GET /nodes and data of every `snapshot` stream event. */
interface DirectSnapshot {
  revision: number;
  nodes: NodeProjection[];
}

async function readJson(req: IncomingMessage): Promise<unknown> {
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

export interface RhizomeDirectServer {
  url: string;
  close(): Promise<void>;
}

export async function startRhizomeDirectServer(options: {
  rhizomeUrl: string;
  port?: number;
  host?: string;
  /** Defaults to RHIZOME_SIGNING; OS seams are injectable for startup tests. */
  signing?: SigningOptions;
}): Promise<RhizomeDirectServer> {
  // Reshapes Rhizome's /v1/workspace (+ SSE) into contract NodeProjection[].
  const upstream = await createRhizomeUpstream(options.rhizomeUrl, {
    signing: { ...options.signing, mode: options.signing?.mode ?? process.env.RHIZOME_SIGNING },
  });
  const snapshot = (): DirectSnapshot => {
    const e = upstream.snapshot();
    return { revision: e.revision, nodes: nodesOf(e) };
  };

  const server = createServer((req, res) => {
    void (async () => {
      const path = (req.url ?? '/').split('?')[0]!;
      if (req.method === 'GET' && path === WIRE.nodes) return json(res, 200, snapshot());
      if (req.method === 'GET' && path === WIRE.stream) {
        res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
        // Every frame is a full snapshot; a reconnect's first frame is the resync.
        const send = () => res.write(`event: ${WIRE.snapshotEvent}\ndata: ${JSON.stringify(snapshot())}\n\n`);
        send();
        const unsubscribe = upstream.subscribe(send);
        const keepalive = setInterval(() => res.write(': keepalive\n\n'), 15_000);
        req.on('close', () => {
          unsubscribe();
          clearInterval(keepalive);
        });
        return;
      }
      if (req.method === 'POST' && path === WIRE.intent) {
        const intent = await readJson(req);
        const actor = String(req.headers[WIRE.actorHeader] ?? '');
        const i = intent as { nodeId?: unknown; action?: unknown } | undefined;
        const node = snapshot().nodes.find((n) => n.id === i?.nodeId);
        const check = validateIntent(intent, lookupCapability(node, typeof i?.action === 'string' ? i.action : ''));
        if (!check.ok) return json(res, 200, { accepted: false, reason: check.reason });
        // Authority is the next snapshot, not this response.
        const result = await upstream.relayIntent(intent, actor);
        return json(res, 200, result.reason === undefined ? { accepted: result.accepted } : result);
      }
      const detailMatch = req.method === 'GET' ? DETAIL_RE.exec(path) : null;
      if (detailMatch) {
        // RHZ-072 (FR-RHZ-101): on-demand detail from the snapshot already
        // held upstream. The node must be on the wire (the cockpit's live
        // scope) — otherwise, or when it has nothing to say, 404; never an
        // empty 200. revision is the snapshot's own, so the cockpit can say
        // "as of revision N" against the same generation it displays.
        const nodeId = decodeURIComponent(detailMatch[1]!);
        const node = snapshot().nodes.find((n) => n.id === nodeId);
        if (!node) return json(res, 404, { reason: 'no such node' });
        const raw = upstream.rawSnapshot();
        const base = detailItems((raw.body ?? {}) as WireDetailBody, nodeId, node.kind === WORKSPACE_ROOT_KIND ? node.id : undefined);
        if (!base) return json(res, 404, { reason: 'no detail for this node' });
        // RHZ-086 (FR-RHZ-116): goal/mission details also carry the about
        // notes, read from /v1/context once per request (no cache). The one
        // exception to "never a further fetch": it is isolated — on any
        // failure the detail is served without note items.
        const query = noteQueryFor(node.kind);
        // RHZ-132 (FR-RHZ-171): a blocked note's detail is its full text from
        // its mission's /v1/context bundle; on failure the cause line stays.
        const noteMission = node.kind === 'note' ? blockedNoteEntry((raw.body ?? {}) as WireDetailBody, nodeId)?.missionId : undefined;
        const detail = query
          ? await withNotes({ revision: raw.revision, items: base }, () => fetchContextNotes(options.rhizomeUrl, query, nodeId))
          : noteMission
            ? await noteDetail({ revision: raw.revision, items: base }, nodeId, () => fetchContextNotes(options.rhizomeUrl, 'mission', noteMission))
            : { revision: raw.revision, items: base };
        if (detail.items.length === 0) return json(res, 404, { reason: 'no detail for this node' });
        // Never put a non-conforming body on the wire (the BFF would 502 it anonymously).
        const check = validateNodeDetail(detail);
        if (!check.ok) return json(res, 500, { reason: `detail fails the contract: ${check.problems.join('; ')}` });
        return json(res, 200, check.detail);
      }
      const executionStreamMatch = req.method === 'GET' ? EXECUTION_STREAM_RE.exec(path) : null;
      if (executionStreamMatch) {
        // RHZ-094 (FR-RHZ-121): SSE of `snapshot` events, each a FULL
        // ExecutionSnapshot (the /stream republish grammar): the first frame
        // is the current snapshot, then a re-emit on every change Rhizome's
        // own tail reports. 404 is answered at route entry as plain HTTP (the
        // contract: "404/501 rules same as GET"); the BFF turns that into its
        // own stated end. A frame that fails the contract is never sent: the
        // response is ENDED instead (a silently stale tail would look live
        // downstream) — the BFF reconnects, and its GET/first-frame path then
        // gets the reasoned 500 that GET gives for the same body.
        const taskId = decodeURIComponent(executionStreamMatch[1]!);
        const body = await upstream.executionWire(taskId);
        if (body === undefined) return json(res, 404, { reason: 'no execution for this task' });
        const first = executionForWire(body, taskId);
        if (!first.ok) return json(res, 500, { reason: first.reason });
        res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
        const send = (snapshot: ExecutionSnapshot) => res.write(`event: ${WIRE.snapshotEvent}\ndata: ${JSON.stringify(snapshot)}\n\n`);
        send(first.snapshot);
        const unsubscribe = upstream.subscribeExecutionBody(taskId, (next) => {
          const checked = executionForWire(next, taskId);
          if (checked.ok) send(checked.snapshot);
          else res.end(); // fault signal, not a stale LIVE; 'close' below unsubscribes
        });
        const keepalive = setInterval(() => res.write(': keepalive\n\n'), 15_000);
        req.on('close', () => {
          unsubscribe();
          clearInterval(keepalive);
        });
        return;
      }
      const executionMatch = req.method === 'GET' ? EXECUTION_RE.exec(path) : null;
      if (executionMatch) {
        // RHZ-094 (FR-RHZ-121): 200 ExecutionSnapshot | 404 (Rhizome has no
        // execution for that task: no mission log). Rhizome serves the surface
        // for every known mission (200 with empty sessions/events when nothing
        // is recorded or the session-log source is not wired), so this wire
        // never answers 501 for execution — an empty snapshot is Rhizome's
        // honest "nothing yet", not a missing surface.
        const taskId = decodeURIComponent(executionMatch[1]!);
        const body = await upstream.executionWire(taskId);
        if (body === undefined) return json(res, 404, { reason: 'no execution for this task' });
        const checked = executionForWire(body, taskId);
        if (!checked.ok) return json(res, 500, { reason: checked.reason });
        return json(res, 200, checked.snapshot);
      }
      const artifactMatch = req.method === 'GET' ? ARTIFACT_RE.exec(path) : null;
      if (artifactMatch) {
        // Stage 2: proxy content-addressed bytes from Rhizome /v1/blob. The wire
        // digest is the sha256 hex of the original bytes; Rhizome addresses the
        // same blob as "sha256:<hex>". Bytes pass through VERBATIM — the BFF's
        // isolated origin re-hashes them against the digest, so any re-encoding,
        // newline or charset change would read as a 404. The logical id is not
        // needed to fetch (the digest is the address); any non-200 → 404.
        const digest = artifactMatch[2]!;
        const upstreamRes = await fetch(`${options.rhizomeUrl}/v1/blob/sha256:${digest}`);
        if (!upstreamRes.ok) return json(res, 404, { reason: 'no bytes for this artifact version' });
        const bytes = Buffer.from(await upstreamRes.arrayBuffer());
        res.writeHead(200, { 'content-type': upstreamRes.headers.get('content-type') ?? 'application/octet-stream' });
        res.end(bytes);
        return;
      }
      json(res, 404, { reason: 'not on the direct wire' });
    })().catch((err: unknown) => json(res, 500, { reason: String(err) }));
  });

  await new Promise<void>((resolve) => server.listen(options.port ?? 8792, options.host ?? '127.0.0.1', resolve));
  const address = server.address();
  const port = typeof address === 'object' && address ? address.port : 0;
  return {
    url: `http://${options.host ?? '127.0.0.1'}:${port}`,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
        upstream.close();
      }),
  };
}

// Entry point: RHIZOME_URL (default :8790) is the backend; PORT (default 8792)
// is the wire. Only auto-start when run directly (npx tsx src/serveDirect.ts),
// never on import — importing for tests must not bind a port.
const runDirectly = process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;
if (runDirectly) {
  const rhizomeUrl = process.env.RHIZOME_URL ?? 'http://127.0.0.1:8790';
  const port = Number(process.env.PORT ?? 8792);
  void startRhizomeDirectServer({ rhizomeUrl, port })
    .then((s) => console.log(`rhizome direct-wire server  ${s.url}  →  ${rhizomeUrl}`))
    .catch((error: unknown) => {
      console.error((error instanceof Error ? error.message : String(error)).replace(/[\r\n]+/g, ' '));
      process.exitCode = 1;
    });
}
