import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { detailItems, signerLabels, type WireDetailBody } from '../src/details.js';
import { APPROVAL_UNVERIFIED, WORKSPACE_NODE_ID } from '../src/nodes.js';
import { adaptWithReport, nodesOf } from '../src/upstream.js';
import { isGateVerification, isUnverifiedDecidedGate } from '../src/workspaceWire.js';

const LABEL_ENV = 'RHIZOME_SIGNER_LABEL';
const originalLabel = process.env[LABEL_ENV];
const KEY_A = `sha256:${'01234567'.repeat(8)}`;
const KEY_B = `sha256:${'89abcdef'.repeat(8)}`;

beforeEach(() => delete process.env[LABEL_ENV]);
afterEach(() => {
  if (originalLabel === undefined) delete process.env[LABEL_ENV];
  else process.env[LABEL_ENV] = originalLabel;
});

const gate = (id: string, verification: unknown, extra: Record<string, unknown> = {}) => ({
  id,
  state: 'approved',
  superseded: false,
  body: 'request',
  decisionReason: 'approved',
  decidedBy: 'unverified-local-operator:signer',
  verification,
  ...extra,
});

const bodyOf = (gates: unknown[]): WireDetailBody & Record<string, unknown> => ({
  missions: [], tasks: [], gates: gates as NonNullable<WireDetailBody['gates']>, deliverables: [],
  edges: [], counts: { running: 0, needsYou: 0, blocked: 0 }, attention: [], capabilities: {}, gateCapabilities: {},
});

describe('RHZ-117 attested R-A wire rules', () => {
  it('accepts exact attested key provenance and clears terminal-decision attention', () => {
    const attested = { status: 'attested', assurance: 'key', keyId: KEY_A, keyRevokedNow: false };
    expect(isGateVerification(attested)).toBe(true);
    expect(isUnverifiedDecidedGate(gate('attested', attested))).toBe(false);
    const nodes = nodesOf(adaptWithReport({ revision: 1, body: bodyOf([gate('attested', attested)]) }).envelope);
    expect(nodes.map((node) => node.id)).toEqual([WORKSPACE_NODE_ID]);
  });

  it('retains malformed attested provenance with attention', () => {
    const malformed = [
      { status: 'attested', keyId: KEY_A },
      { status: 'attested', assurance: 'key', keyId: 'sha256:BAD' },
      { status: 'attested', assurance: 'key', keyId: KEY_A, claimKind: 'relayed' },
    ];
    for (const [index, verification] of malformed.entries()) {
      expect(isGateVerification(verification)).toBe(false);
      const id = `bad-${index}`;
      const raw = bodyOf([gate(id, verification)]);
      const node = nodesOf(adaptWithReport({ revision: 1, body: raw }).envelope).find((item) => item.id === id);
      expect(node?.attention).toEqual([{ cause: APPROVAL_UNVERIFIED }]);
    }
  });

  it('leaves verified validation and clearing unchanged', () => {
    const verified = { status: 'verified', assurance: 'key', keyId: KEY_A };
    expect(isGateVerification(verified)).toBe(true);
    expect(isUnverifiedDecidedGate(gate('verified', verified))).toBe(false);
    expect(isGateVerification({ status: 'verified', assurance: 'key', keyId: KEY_A, claimKind: 'relayed' })).toBe(false);
  });
});

describe('RHZ-117 signer identity display', () => {
  it('replaces verified decidedBy only on a full key-label match and preserves submittedBy', () => {
    process.env[LABEL_ENV] = `H=${KEY_A},backup=${KEY_B}`;
    expect(signerLabels().get(KEY_A)).toBe('H');
    const items = detailItems(bodyOf([gate('verified', { status: 'verified', assurance: 'key', keyId: KEY_A })]), 'verified')!;
    expect(items.filter((item) => ['decidedBy', 'submittedBy'].includes(item.label))).toEqual([
      { label: 'decidedBy', text: 'H · key 01234567' },
      { label: 'submittedBy', text: 'unverified-local-operator:signer' },
    ]);
  });

  it('does not apply a label mapped to a different full key id', () => {
    process.env[LABEL_ENV] = `H=${KEY_B}`;
    const items = detailItems(bodyOf([gate('verified', { status: 'verified', assurance: 'key', keyId: KEY_A })]), 'verified')!;
    expect(items.find((item) => item.label === 'decidedBy')).toEqual({ label: 'decidedBy', text: 'key 01234567' });
    expect(items.find((item) => item.label === 'submittedBy')).toEqual({
      label: 'submittedBy', text: 'unverified-local-operator:signer',
    });
  });

  it('keeps attested decidedBy, adds a separate attester item, and displays revoked status', () => {
    process.env[LABEL_ENV] = `H=${KEY_A}`;
    const items = detailItems(bodyOf([gate('attested', {
      status: 'attested', assurance: 'key', keyId: KEY_A, keyRevokedNow: true,
    })]), 'attested')!;
    expect(items.find((item) => item.label === '승인 상태')).toEqual({ label: '승인 상태', text: 'attested (key) · 키 폐기됨' });
    expect(items.find((item) => item.label === 'decidedBy')).toEqual({
      label: 'decidedBy', text: 'unverified-local-operator:signer',
    });
    expect(items.find((item) => item.label === 'attested by')).toEqual({ label: 'attested by', text: 'H · key 01234567' });
    expect(items.some((item) => item.label === 'submittedBy')).toBe(false);
  });
});
