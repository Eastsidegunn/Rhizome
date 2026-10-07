/**
 * Rhizome workspace → the contract's NodeProjection[]. Translation only:
 * Rhizome's vocabulary (task/gate states, edge kinds, attention kinds) passes
 * through as opaque strings; nothing is scored or inferred. Where the wire
 * carries no value of its own, the adapter's fixed stand-ins below are used
 * and named as such.
 */
import { WORKSPACE_ROOT_KIND, type ArtifactRef, type Capability, type CapabilityLevel, type NodeProjection } from '@gunnflow/contract';
import type { WorkspaceProjection } from './workspaceWire.js';

/** State value for a node whose Rhizome shape carries no state (missions, deliverables, the root). */
export const UNSTATED = 'unstated';
/** Relation type for membership, which Rhizome carries as `missionId`. */
export const MEMBER_OF = 'member-of';
/** Attention cause for a task flagged without a matching attention entry (the flag carries no cause). */
export const FLAGGED = 'flagged';
/**
 * Attention cause for a gate awaiting a human decision. A gate in the waiting
 * state is, structurally, a pending human decision — the same acceptance-surface
 * fact the gate's approve/reject capability reflects, not an inferred judgment.
 */
export const NEEDS_HUMAN = 'needs_human';
/**
 * Id of the workspace root. Rhizome has no root object; the adapter
 * synthesizes one to carry workspace-level actions (mission creation). The
 * `~` prefix keeps it apart from Rhizome ids; should one still collide, the
 * root takes another `~` until it is unique.
 */
export const WORKSPACE_NODE_ID = '~workspace';

const TASK_ACTIONS = {
  pause: 'task.pause',
  resume: 'task.resume',
  instruct: 'task.instruct',
  cancel: 'task.cancel',
  forceReplan: 'task.forceReplan',
} as const;
/** Rhizome requires text for these (instruction; privileged actions carry a stated reason). */
const TEXT_REQUIRED = new Set<string>(['instruct', 'cancel', 'forceReplan']);
const GATE_ACTIONS = { approve: 'gate.approve', reject: 'gate.reject', requestChanges: 'gate.requestChanges' } as const;

/**
 * The documented /v1/intent acceptance surface: Rhizome accepts mission.create
 * from the operator. mission.create needs a name; the relay also sends it as
 * the success criterion when none is given.
 */
const ROOT_CAPABILITIES: Capability[] = [{ action: 'mission.create', level: 'enabled', decision: { input: { required: true } } }];

interface WireAttention {
  kind?: unknown;
  refId?: unknown;
}

/** Structural facts about the translation: what the wire carried that could not be projected. */
export interface RhizomeIntegrationReport {
  /** Attention entries that were not objects, had no kind, or referenced no node. */
  attentionDropped: number;
}

function levels<K extends string>(caps: unknown, actions: Record<K, string>, text: (k: K) => boolean): Capability[] {
  return Object.entries((caps ?? {}) as Record<string, unknown>).flatMap(([key, level]) =>
    key in actions && (level === 'enabled' || level === 'disabled' || level === 'hidden')
      ? [
          {
            action: actions[key as K],
            level: level as CapabilityLevel,
            ...(text(key as K) ? { decision: { input: { required: true } } } : {}),
          },
        ]
      : [],
  );
}

const BLOB_PREFIX = 'sha256:';
/**
 * RHZ-076 (FR-RHZ-104): cockpit kind of a Rhizome goal. Three cockpit layers
 * out of Rhizome's two shapes: a goal with NO incoming `contains` edge anywhere
 * in the raw snapshot → "goal" (top level); a goal that is the target of any
 * `contains` edge → "mission" (as before); Rhizome missions stay "task".
 *
 * Rule, precisely: kind = "goal" iff id ∉ containedIds, where containedIds is
 * the set of `to` ids of all non-superseded `contains` edges of the raw
 * snapshot (workspaceWire.ts) — including edges whose other end is hidden
 * (terminal parent) or which form a cycle, excluding edges a later edge
 * supersedes (edge.rewire, RHZ-066). Live edges alone would make an orphan
 * (parent resolved/cancelled, so the edge is dropped from the live projection)
 * jump to the top level; reading the raw edges keeps it "mission"
 * (conservative). Every member of a contains cycle has an incoming edge, so a
 * cycle yields no "goal" either. A function of state and edges only — no ids
 * are special-cased.
 */
export function goalKind(id: string, containedIds: ReadonlySet<string>): 'goal' | 'mission' {
  return containedIds.has(id) ? 'mission' : 'goal';
}

export function projectRhizomeNodes(
  raw: unknown,
  p: WorkspaceProjection,
  mediaTypeOf?: (blobId: string) => string | undefined,
): { nodes: NodeProjection[]; report: RhizomeIntegrationReport } {
  const edgesFrom = (id: string) => p.edges.filter((e) => e.from === id).map((e) => ({ type: e.edgeKind, target: e.to }));
  const membership = (missionId: string) => (missionId ? [{ type: MEMBER_OF, target: missionId }] : []);

  /**
   * A deliverable whose sourceRef is a content-addressed blob (`sha256:<hex>`)
   * carries one artifact: its id is the deliverable (so the viewer binds it to
   * this node), the digest is the sha256 hex of the original bytes (Rhizome's
   * blob id without the prefix). mediaType is learned from the blob's served
   * Content-Type (octet-stream until known); the bytes themselves are served
   * verbatim by the /artifact route. Non-blob refs (`exec-` …) carry no bytes.
   */
  const artifactsOf = (id: string, sourceRef?: string): ArtifactRef[] => {
    if (!sourceRef || !sourceRef.startsWith(BLOB_PREFIX)) return [];
    return [
      {
        id,
        mediaType: mediaTypeOf?.(sourceRef) ?? 'application/octet-stream',
        digest: sourceRef.slice(BLOB_PREFIX.length),
        access: { kind: 'snapshot' },
      },
    ];
  };

  const ids = new Set<string>([...p.missions, ...p.tasks, ...p.gates, ...p.deliverables].map((n) => n.id));
  let rootId = WORKSPACE_NODE_ID;
  while (ids.has(rootId)) rootId = `~${rootId}`;

  // Rhizome's attention list names a kind per referenced object; the kind is the cause.
  const rawAttention = (raw as { attention?: unknown } | null)?.attention;
  const listed = (Array.isArray(rawAttention) ? rawAttention : []) as unknown[];
  const valid = listed.filter(
    (a): a is WireAttention & { kind: string; refId: string } =>
      typeof a === 'object' && a !== null && typeof (a as WireAttention).kind === 'string' && (a as WireAttention).kind !== '' &&
      typeof (a as WireAttention).refId === 'string' && ids.has((a as WireAttention).refId as string),
  );
  const report: RhizomeIntegrationReport = { attentionDropped: listed.length - valid.length };
  // The list and the object's own flag are independent signals; both are projected.
  const attentionOf = (id: string, flag = false) => [
    ...valid.filter((a) => a.refId === id).map((a) => ({ cause: a.kind })),
    ...(flag ? [{ cause: FLAGGED }] : []),
  ];

  const root: NodeProjection = {
    id: rootId,
    kind: WORKSPACE_ROOT_KIND,
    label: 'Workspace',
    state: { value: UNSTATED },
    relations: [],
    capabilities: ROOT_CAPABILITIES,
    attention: [],
    artifacts: [],
  };
  const containedIds = new Set<string>(p.containedIds);
  const missions: NodeProjection[] = p.missions.map((m) => ({
    id: m.id,
    // RHZ-076 (FR-RHZ-104): top-level goal → "goal", contained goal → "mission" (see goalKind).
    kind: goalKind(m.id, containedIds),
    label: m.name,
    // Goal lifecycle state passes through (RHZ-061); terminal goals are already
    // dropped upstream, so a surviving mission is active/paused (or unstated).
    state: { value: m.state ?? UNSTATED },
    relations: edgesFrom(m.id),
    capabilities: [],
    attention: attentionOf(m.id, m.attention),
    artifacts: [],
  }));
  const tasks: NodeProjection[] = p.tasks.map((t) => ({
    id: t.id,
    kind: 'task',
    label: t.name,
    state: { value: t.state },
    relations: [...membership(t.missionId), ...edgesFrom(t.id)],
    capabilities: levels(p.capabilities[t.id], TASK_ACTIONS, (k) => TEXT_REQUIRED.has(k)),
    attention: attentionOf(t.id, t.attention),
    artifacts: [],
  }));
  const gates: NodeProjection[] = p.gates.map((g) => ({
    id: g.id,
    kind: 'gate',
    // v1 gates carry no name (the domain body shows the id); the label stays absent.
    ...(g.name && g.name !== g.id ? { label: g.name } : {}),
    state: { value: g.state },
    // RHZ-075 (FR-RHZ-108): a gate is bound to exactly one of mission / goal (Rhizome enforces
    // the xor); membership follows missionId, else goalId — never both, so one member-of at most.
    relations: [...membership(g.missionId || g.goalId), ...edgesFrom(g.id)],
    capabilities: levels(p.gateCapabilities[g.id], GATE_ACTIONS, (k) => k === 'requestChanges'),
    // A waiting gate pulls the human: it is a pending decision by definition. A
    // changes_requested gate (RHZ-078, FR-RHZ-109) passes through as its own state
    // value; the ball is with the worker, so no needs_human is added for it.
    attention: [...attentionOf(g.id), ...(g.state === 'waiting' ? [{ cause: NEEDS_HUMAN }] : [])],
    artifacts: [],
  }));
  const deliverables: NodeProjection[] = p.deliverables.map((d) => ({
    id: d.id,
    kind: 'deliverable',
    label: d.title,
    state: { value: d.state ?? UNSTATED },
    // RHZ-081 (FR-RHZ-112): bound to exactly one of mission / goal (Rhizome enforces the xor);
    // membership follows missionId, else goalId — one member-of at most (RHZ-075 gate pattern).
    // RHZ-085 (FR-RHZ-115): when the owning task is hidden (terminal) under a live goal, the wire
    // promotes membership to that goal (promotedGoalId) so member-of never names a hidden node.
    relations: [...membership(d.promotedGoalId || d.missionId || d.goalId || ''), ...edgesFrom(d.id)],
    capabilities: [],
    attention: attentionOf(d.id),
    artifacts: artifactsOf(d.id, d.sourceRef),
  }));
  return { nodes: [root, ...missions, ...tasks, ...gates, ...deliverables], report };
}
