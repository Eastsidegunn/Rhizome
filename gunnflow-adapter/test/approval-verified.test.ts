import { describe, expect, it } from 'vitest';
import { nodeProblem } from '@gunnflow/contract';
import { detailItems, type WireDetailBody } from '../src/details.js';
import { APPROVAL_UNVERIFIED, WORKSPACE_NODE_ID } from '../src/nodes.js';
import { adaptWithReport, nodesOf } from '../src/upstream.js';
import { adaptWorkspaceBody, isGateVerification } from '../src/workspaceWire.js';

const keyId = `sha256:${'a'.repeat(64)}`;
const base = (gates: unknown[], trust?: unknown) => ({
  missions: [], tasks: [], gates, deliverables: [], edges: [],
  counts: { running: 0, needsYou: 0, blocked: 0 }, attention: [],
  capabilities: {}, gateCapabilities: {}, ...(trust === undefined ? {} : { trust }),
});
const gate = (id: string, verification?: unknown) => ({
  id, state: 'approved', superseded: false, ...(verification === undefined ? {} : { verification }),
});
const validateNodes = (nodes: ReturnType<typeof nodesOf>) => {
  for (const node of nodes) expect(nodeProblem(node), node.id).toBeNull();
};

describe('FR-RHZ-150 verified approval adapter', () => {
  it('retains missing, legacy, unknown, and malformed provenance but clears attention for claimed and verified', () => {
    const raw = base([
      gate('missing'),
      gate('legacy', { status: 'legacy-asserted' }),
      gate('unknown', { status: 'future-status' }),
      gate('malformed', { status: 'verified', assurance: 'key' }),
      gate('claimed', { status: 'claimed', claimKind: 'relayed' }),
      gate('verified', { status: 'verified', assurance: 'key', keyId }),
    ]);
    const nodes = nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope);
    validateNodes(nodes);
    expect(nodes.map((node) => node.id)).toEqual([WORKSPACE_NODE_ID, 'missing', 'legacy', 'unknown', 'malformed']);
    for (const id of ['missing', 'legacy', 'unknown', 'malformed']) {
      expect(nodes.find((node) => node.id === id)?.attention).toEqual([{ cause: APPROVAL_UNVERIFIED }]);
    }
  });

  it('applies the exact status-dependent verification shape rules', () => {
    expect(isGateVerification({ status: 'verified', assurance: 'key', keyId })).toBe(true);
    const malformed = [
      { status: 'verified', keyId },
      { status: 'verified', assurance: 'presence', keyId },
      { status: 'verified', assurance: 'key' },
      { status: 'verified', assurance: 'key', keyId: 'sha256:BAD' },
      { status: 'verified', assurance: 'key', keyId, claimKind: 'relayed' },
      { status: 'claimed', claimKind: 'relayed', keyRevokedNow: true },
      { status: 'claimed', claimKind: 'relayed', keyId },
      { status: 'claimed', claimKind: 'relayed', assurance: 'key' },
      { status: 'verified', assurance: 'key', keyId, extra: true },
    ];
    for (const value of malformed) expect(isGateVerification(value), JSON.stringify(value)).toBe(false);
  });

  it('renders verified key assurance and current revocation warning', () => {
    const body = base([
      gate('active', { status: 'verified', assurance: 'key', keyId }),
      gate('revoked', { status: 'verified', assurance: 'key', keyId, keyRevokedNow: true }),
    ]);
    expect(detailItems(body as WireDetailBody, 'active')?.find((item) => item.label === '승인 상태')?.text).toBe('verified (key)');
    expect(detailItems(body as WireDetailBody, 'revoked')?.find((item) => item.label === '승인 상태')?.text).toBe('verified (key) · 키 폐기됨');
  });

  it('accepts top-level trust metadata without projecting it and preserves legacy adapted bytes', () => {
    const legacy = base([{ id: 'pending', state: 'pending', superseded: false }]);
    const before = JSON.stringify(adaptWorkspaceBody(legacy));
    const withTrust = adaptWorkspaceBody({ ...legacy, trust: { journalId: '0'.repeat(32), genesisKeyId: keyId } });
    expect(JSON.stringify(adaptWorkspaceBody(legacy))).toBe(before);
    expect(JSON.stringify(withTrust)).toBe(before);
    expect(withTrust).not.toHaveProperty('trust');
  });
});
