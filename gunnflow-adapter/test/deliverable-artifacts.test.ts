// RHZ-057 stage 2 (adapter half): deliverable artifacts[] derivation and the
// /artifact byte proxy. Unit-level, independent of the live-filter path so the
// adapter's artifact logic is pinned on its own.
import { createHash } from 'node:crypto';
import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { projectRhizomeNodes } from '../src/nodes.js';
import { startRhizomeDirectServer } from '../src/serveDirect.js';
import type { WorkspaceProjection } from '../src/workspaceWire.js';

const CONTENT = Buffer.from('# deliverable body\n\x00\x01binary tail', 'latin1');
const HEX = createHash('sha256').update(CONTENT).digest('hex');
const BLOB_ID = `sha256:${HEX}`;

function projectionWith(sourceRef: string | undefined): WorkspaceProjection {
  return {
    revision: 1,
    missions: [{ kind: 'mission', id: 'm1', name: 'M', attention: false }],
    tasks: [],
    gates: [],
    deliverables: [{ kind: 'deliverable', id: 'd-1', missionId: 'm1', title: 'Report', deliverableType: 'document', sourceRef }],
    edges: [],
    containedIds: [],
    counts: { running: 0, needsYou: 0, blocked: 0 },
    activities: [],
    sessions: [],
    capabilities: {},
    gateCapabilities: {},
    effects: [],
  };
}

describe('deliverable artifacts[] derivation (FR-RHZ-087 stage 2)', () => {
  const deliverableNode = (p: WorkspaceProjection, mediaTypeOf?: (b: string) => string | undefined) =>
    projectRhizomeNodes({}, p, mediaTypeOf).nodes.find((n) => n.id === 'd-1')!;

  it('a sha256 sourceRef yields one snapshot artifact whose digest is the hex (no prefix)', () => {
    const d = deliverableNode(projectionWith(BLOB_ID));
    expect(d.artifacts).toEqual([
      { id: 'd-1', mediaType: 'application/octet-stream', digest: HEX, access: { kind: 'snapshot' } },
    ]);
    expect(nodeProblem(d)).toBeNull();
  });

  it('mediaTypeOf supplies the media type when known', () => {
    const d = deliverableNode(projectionWith(BLOB_ID), (b) => (b === BLOB_ID ? 'text/markdown' : undefined));
    expect(d.artifacts[0]!.mediaType).toBe('text/markdown');
    expect(d.artifacts[0]!.digest).toBe(HEX);
  });

  it('a non-blob sourceRef (exec-…, blob:…, absent) carries no artifact — no invention', () => {
    expect(deliverableNode(projectionWith('exec-123')).artifacts).toEqual([]);
    expect(deliverableNode(projectionWith('blob:abc')).artifacts).toEqual([]);
    expect(deliverableNode(projectionWith(undefined)).artifacts).toEqual([]);
  });
});

/* ---- /artifact byte proxy against a mock Rhizome ---- */
describe('/artifact proxies Rhizome /v1/blob verbatim (FR-RHZ-086 reuse)', () => {
  const mock = Fastify({ logger: false });
  let server: Awaited<ReturnType<typeof startRhizomeDirectServer>>;

  beforeAll(async () => {
    mock.get('/v1/workspace', async () => ({ revision: 1, body: { missions: [], tasks: [], gates: [], deliverables: [], edges: [] } }));
    mock.get('/v1/workspace/stream', (_req, reply) => {
      reply.raw.writeHead(200, { 'content-type': 'text/event-stream' });
      reply.raw.write('event: snapshot\ndata: {"revision":1,"body":{}}\n\n');
    });
    // Content-addressed blob: only the exact id serves bytes; anything else 404.
    mock.get('/v1/blob/:id', async (req, reply) => {
      if ((req.params as { id: string }).id !== BLOB_ID) return reply.code(404).send('not found');
      return reply.header('content-type', 'text/markdown').send(CONTENT);
    });
    await mock.listen({ port: 0, host: '127.0.0.1' });
    const a = mock.server.address();
    const rhizomeUrl = `http://127.0.0.1:${typeof a === 'object' && a ? a.port : 0}`;
    server = await startRhizomeDirectServer({ rhizomeUrl, port: 0 });
  });
  afterAll(async () => {
    await server.close();
    await mock.close();
  });

  it('serves the exact bytes + media type for a held digest; byte-identical (BFF re-hash would pass)', async () => {
    const res = await fetch(`${server.url}/artifact/d-1/${HEX}`);
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe('text/markdown');
    const got = Buffer.from(await res.arrayBuffer());
    expect(got.equals(CONTENT)).toBe(true);
    expect(createHash('sha256').update(got).digest('hex')).toBe(HEX); // digest survives the hop
  });

  it('a digest with no bytes answers 404', async () => {
    const res = await fetch(`${server.url}/artifact/d-1/${'0'.repeat(64)}`);
    expect(res.status).toBe(404);
  });
});
