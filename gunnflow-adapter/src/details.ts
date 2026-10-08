/**
 * RHZ-072 (FR-RHZ-101): on-demand node detail for the direct wire's
 * GET /detail/:nodeId (Gunnflow contract 0.2.0, WIRE.md §detail). Pure
 * translation from the Rhizome /v1/workspace snapshot the upstream already
 * holds — no extra fetch, no new facts: every item restates a field Rhizome
 * emitted, in Rhizome's own vocabulary (labels are opaque to the cockpit).
 * Empty fields are omitted; a node with nothing to say has no detail (404) —
 * an empty 200 is never produced (contract invariant: "none → 404").
 *
 * Bodies are always `text`: a gate's body is a plain field of the
 * snapshot, not a content-addressed blob, and the contract's `artifact` body
 * is an ArtifactRef (id + digest + bytes behind /artifact). Recognised gate
 * sections are stripped to plain text; legacy unstructured bodies remain
 * byte-identical. Deliverable artifacts already live in the projection
 * (RHZ-057), so deliverables carry no detail here (no duplicate).
 */
import type { DetailItem, NodeDetail } from '@gunnflow/contract';
import { parseGateBodySections } from './gateBody.js';
import { WORKSPACE_NODE_ID } from './nodes.js';
import { isGateVerification, unverifiedDecidedGates, type GateVerification } from './workspaceWire.js';

/** The slice of Rhizome's /v1/workspace body (workspace/http.go DTOs) the detail reads. */
export interface WireDetailBody {
  missions?: Array<{ id: string; name?: string; state?: string; success?: string }>;
  tasks?: Array<{
    id: string;
    missionId?: string;
    state?: string;
    currentAction?: string;
    progress?: number;
    hasProgress?: boolean;
    blockedReason?: string;
    assignee?: string;
  }>;
  gates?: Array<{
    id: string;
    state?: string;
    requestDigest?: string;
    body?: string;
    recommendation?: string;
    decisionReason?: string;
    decidedBy?: string;
    decidedAt?: string;
    superseded?: boolean;
    verification?: GateVerification;
    missionId?: string;
    goalId?: string;
  }>;
  deliverables?: Array<{ id: string }>;
  counts?: { running?: number; needsYou?: number; blocked?: number };
}

/** A Rhizome workspace envelope as GET /v1/workspace (and its SSE frames) emit it. */
export interface WireWorkspaceEnvelope {
  revision: number;
  body: unknown;
}

/**
 * Gate states in which Rhizome's decisionReason is a decision (not a draft).
 * changes_requested (RHZ-078, FR-RHZ-109) is a recorded decision too — its
 * reason is the change request — so it shows under the same `decision` label.
 */
const DECISION_GATE_STATES = new Set(['approved', 'rejected', 'changes_requested']);
/** Only terminal approval outcomes carry approval provenance detail. */
const APPROVAL_STATUS_GATE_STATES = new Set(['approved', 'rejected']);
/** The workspace root's count labels, in a fixed order (same vocabulary as Gunnflow's fake-contracts). */
const COUNT_LABELS = ['running', 'needsYou', 'blocked'] as const;

const text = (label: string, value: unknown): DetailItem[] =>
  typeof value === 'string' && value !== '' ? [{ label, text: value }] : [];

/** Exact display vocabulary for the additive provenance DTO. */
const verificationText = (verification: unknown): string => {
  if (!isGateVerification(verification)) return 'unverified';
  const base =
    verification.status === 'claimed' && (verification.claimKind === 'relayed' || verification.claimKind === 'session-direct')
      ? `claimed (${verification.claimKind})`
      : verification.status;
  return verification.assurance ? `${base} (${verification.assurance})` : base;
};

/**
 * The detail of one node, or undefined when the node has none (the 404 path).
 * `rootId` is the id the projection gave the synthesized workspace root
 * (nodes.ts may add `~` on collision); it defaults to the usual one.
 */
export function detailOf(wire: WireWorkspaceEnvelope, nodeId: string, rootId: string = WORKSPACE_NODE_ID): NodeDetail | undefined {
  const items = detailItems((wire.body ?? {}) as WireDetailBody, nodeId, rootId);
  return items && items.length > 0 ? { revision: wire.revision, items } : undefined;
}

/** The items for a node, by kind; undefined for unknown nodes and deliverables. */
export function detailItems(body: WireDetailBody, nodeId: string, rootId: string = WORKSPACE_NODE_ID): DetailItem[] | undefined {
  // Workspace root: the status counts, each as a string, fixed order.
  if (nodeId === rootId) {
    const counts = body.counts;
    const unverifiedDecisions = unverifiedDecidedGates(
      body.gates ?? [],
      body.missions ?? [],
      body.tasks ?? [],
    ).length;
    const countItems = counts
      ? COUNT_LABELS.flatMap((label) => (typeof counts[label] === 'number' ? [{ label, text: String(counts[label]) }] : []))
      : [];
    return [
      ...countItems,
      ...(unverifiedDecisions > 0 ? [{ label: '승인 미확인 결정', text: String(unverifiedDecisions) }] : []),
    ];
  }

  // Gate (internal question): the request body, the recommendation, the
  // decision (only once decided), who decided, and the request digest.
  const gate = body.gates?.find((g) => g.id === nodeId);
  if (gate) {
    const hasDecision = DECISION_GATE_STATES.has(gate.state ?? '');
    const hasApprovalStatus = APPROVAL_STATUS_GATE_STATES.has(gate.state ?? '');
    const parsedBody = typeof gate.body === 'string' ? parseGateBodySections(gate.body) : undefined;
    const structuredBody = parsedBody && parsedBody.sections.length > 0;
    const hasStructuredRecommendation = parsedBody?.sections.some(({ label }) => label === '권고') ?? false;
    return [
      ...(structuredBody
        ? [
            ...parsedBody.sections.map(({ label, text: sectionText }) => ({ label, text: truncateDetailText(sectionText) })),
            ...text('request', parsedBody.rest),
          ]
        : text('request', gate.body)),
      ...(hasStructuredRecommendation ? [] : text('recommendation', gate.recommendation)),
      ...(hasApprovalStatus ? [{ label: '승인 상태', text: verificationText(gate.verification) }] : []),
      ...(hasDecision ? text('decision', gate.decisionReason) : []),
      ...text('decidedBy', gate.decidedBy),
      ...text('digest', gate.requestDigest),
    ];
  }

  // Task (Rhizome mission): progress facts the projection carries only partly.
  // Progress exists only when Rhizome says so (hasProgress) — never inferred from 0.
  const task = body.tasks?.find((t) => t.id === nodeId);
  if (task) {
    return [
      ...text('currentAction', task.currentAction),
      ...(task.hasProgress && typeof task.progress === 'number' ? [{ label: 'progress', text: String(task.progress) }] : []),
      ...text('blockedReason', task.blockedReason),
      ...text('assignee', task.assignee), // RHZ-080 (FR-RHZ-111)
      ...text('state', task.state),
    ];
  }

  // Mission (Rhizome goal): the success criterion (RHZ-067). Rhizome's goal
  // DTO carries only `name` (description ≡ name, RHZ-067), and the node
  // label already shows it — repeating it as a `description` item would be
  // noise, so a goal without a success criterion has no detail (404).
  const mission = body.missions?.find((m) => m.id === nodeId);
  if (mission) {
    return text('success', mission.success);
  }

  // Deliverables: their artifacts are already in the projection — no detail.
  // Anything else: no such node.
  return undefined;
}

/* ---- RHZ-086 (FR-RHZ-116): about notes on goal/mission detail ---- */

/** One memory of a Rhizome /v1/context bundle (workspace/contexthttp.go contextMemoryDTO). */
export interface ContextNote {
  id: string;
  kind: string;
  content: string;
  tags?: string[];
  /** Journal Sequence of the note's first event — the recency key. */
  seq?: number;
}

/** Newest-first cap on note items per detail. */
export const NOTE_LIMIT = 10;
/** Longer contents are cut to this many characters plus NOTE_TRUNCATED_SUFFIX. */
export const NOTE_TEXT_MAX = 2000;
export const NOTE_TRUNCATED_SUFFIX = '…(전문 /v1/context)';

/** Apply the adapter's established long-detail cut and full-text marker. */
const truncateDetailText = (value: string): string =>
  value.length > NOTE_TEXT_MAX ? value.slice(0, NOTE_TEXT_MAX) + NOTE_TRUNCATED_SUFFIX : value;

/** Upper bound on one /v1/context read; a hung Rhizome must not hang the detail. Tests may lower it. */
export const noteFetchTimeout = { ms: 2000 };

/**
 * Which /v1/context query a node kind reads its notes from: a Rhizome goal
 * (wire kind `goal` or contained `mission`, RHZ-076) → `goal`, a Rhizome
 * mission (wire kind `task`) → `mission`. Never `task`: that bundle records a
 * KnowledgeUseTrace per read, and a cockpit click is not knowledge use.
 * Gates, deliverables, root: none.
 */
export function noteQueryFor(kind: string): 'goal' | 'mission' | undefined {
  if (kind === 'goal' || kind === 'mission') return 'goal';
  if (kind === 'task') return 'mission';
  return undefined;
}

/** A well-formed note element: content and kind are strings, tags (if any) an array of strings. */
const isContextNote = (x: unknown): x is ContextNote => {
  const n = x as Partial<ContextNote> | null;
  return (
    typeof n === 'object' && n !== null && typeof n.content === 'string' && typeof n.kind === 'string' &&
    (n.tags === undefined || (Array.isArray(n.tags) && n.tags.every((t) => typeof t === 'string')))
  );
};

/**
 * Note items, newest-first (seq desc; the task bundle is served by id, so the
 * order is imposed here for both), capped at `limit`. label = `note:<kind>`
 * plus ` [tag1,tag2]` when tagged; text = content, cut at NOTE_TEXT_MAX with
 * the marker pointing at the full text on /v1/context.
 */
export function noteItems(notes: readonly unknown[], limit: number = NOTE_LIMIT): DetailItem[] {
  return notes
    .filter(isContextNote)
    .sort((a, b) => (b.seq ?? 0) - (a.seq ?? 0) || String(a.id).localeCompare(String(b.id)))
    .slice(0, limit)
    .map((n) => ({
      label: n.tags && n.tags.length > 0 ? `note:${n.kind} [${n.tags.join(',')}]` : `note:${n.kind}`,
      text: truncateDetailText(n.content),
    }));
}

/**
 * The async layer over the pure detail: appends note items after the existing
 * ones. Failure isolation — a rejected fetch, a timeout or an unusable body
 * yields the detail unchanged (malformed elements are dropped one by one), so
 * a Rhizome /v1/context problem never costs the cockpit the detail it had.
 */
export async function withNotes(detail: NodeDetail, fetchNotes: () => Promise<readonly unknown[]>): Promise<NodeDetail> {
  try {
    const items = noteItems(await fetchNotes());
    return { revision: detail.revision, items: [...detail.items, ...items] };
  } catch {
    return detail;
  }
}

/** One GET of Rhizome's context bundle, bounded by noteFetchTimeout; non-200 or a malformed body throws (withNotes isolates it). */
export async function fetchContextNotes(rhizomeUrl: string, query: 'goal' | 'mission', id: string): Promise<unknown[]> {
  const res = await fetch(`${rhizomeUrl}/v1/context?${query}=${encodeURIComponent(id)}`, { signal: AbortSignal.timeout(noteFetchTimeout.ms) });
  if (!res.ok) throw new Error(`rhizome context failed (${res.status})`);
  const body = (await res.json()) as { memories?: unknown };
  if (!Array.isArray(body.memories)) throw new Error('rhizome context: no memories array');
  return body.memories;
}
