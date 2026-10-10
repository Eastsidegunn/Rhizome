/**
 * Rhizome workspace → the contract's NodeProjection[]. Translation only:
 * Rhizome's vocabulary (task/gate states, edge kinds, attention kinds) passes
 * through as opaque strings; nothing is scored or inferred. Where the wire
 * carries no value of its own, the adapter's fixed stand-ins below are used
 * and named as such.
 */
import { WORKSPACE_ROOT_KIND, type ArtifactRef, type Capability, type CapabilityLevel, type NodeProjection } from '@gunnflow/contract';
import { isUnverifiedDecidedGate, type WorkspaceProjection } from './workspaceWire.js';

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
/** A request waiting for the operator to perform an external hand task. */
export const NEEDS_HUMAN_ACTION = 'needs_human_action';
/** Attention cause for a retained terminal decision without verified provenance. */
export const APPROVAL_UNVERIFIED = 'approval_unverified';
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
const REQUEST_ACTIONS = { complete: 'request.complete', unable: 'request.unable' } as const;
/**
 * RHZ-132 (FR-RHZ-171): answering a blocked note. The adapter translates it to
 * Rhizome note.create {memoryKind: answer} (upstream.ts); text is required.
 */
export const NOTE_ANSWER = 'note.answer';
/** State value of a projected blocked note (the only notes on the cockpit). */
export const NOTE_BLOCKED_STATE = 'blocked';

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

function levels<K extends string>(caps: unknown, actions: Record<K, string>, text: (k: K) => boolean | undefined): Capability[] {
  return Object.entries((caps ?? {}) as Record<string, unknown>).flatMap(([key, level]) =>
    key in actions && (level === 'enabled' || level === 'disabled' || level === 'hidden')
      ? [
          {
            action: actions[key as K],
            level: level as CapabilityLevel,
            ...(text(key as K) === undefined ? {} : { decision: { input: { required: text(key as K)! } } }),
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

/**
 * RHZ-133 (FR-RHZ-173): which node field set the adapter emits. '0.5' (the
 * default) is today's node shape, byte-identical; '0.6' adds the optional
 * liveness / provenance / progress fields of contract 0.6 (changedAtRevision,
 * lastActivityTs, active, originNodeId, steps, shortName, summary). The
 * installed contract (0.4) and the live BFF (0.5) reject unknown node keys,
 * so 0.6 must stay off until the viewer's contract ships.
 */
export type WireFields = '0.5' | '0.6';

/** Only the exact value `0.6` turns the 0.6 fields on; anything else (or unset) is 0.5. */
export function wireFieldsOf(value: string | undefined = process.env.GUNNFLOW_WIRE_FIELDS): WireFields {
  return value === '0.6' ? '0.6' : '0.5';
}

export interface ProjectOptions {
  /** Default '0.5' (off). */
  wireFields?: WireFields;
  /** Envelope revision bounding changedAtRevision; defaults to the projection's revision. */
  revision?: number;
}

/** shortName / summary bounds (code points). */
export const SHORT_NAME_MAX = 32;
export const SUMMARY_MAX = 200;
const EM_DASH_SEPARATOR = ' — ';

const collapse = (s: string) => s.replace(/\s+/g, ' ').trim();
const codePoints = (s: string) => [...s];

/** Label → shortName: the part before ' — ' (else the whole label), collapsed; 1..32 code points or absent. */
export function shortNameOf(label: string | undefined): string | undefined {
  if (typeof label !== 'string') return undefined;
  const cut = label.indexOf(EM_DASH_SEPARATOR);
  const name = collapse(cut >= 0 ? label.slice(0, cut) : label);
  const n = codePoints(name).length;
  return n >= 1 && n <= SHORT_NAME_MAX ? name : undefined;
}

/** One line, whitespace collapsed, at most 200 code points (199 + '…' when cut); absent when empty. */
export function summaryOf(text: unknown): string | undefined {
  if (typeof text !== 'string') return undefined;
  const line = collapse(text);
  if (line === '') return undefined;
  const cps = codePoints(line);
  return cps.length > SUMMARY_MAX ? `${cps.slice(0, SUMMARY_MAX - 1).join('')}…` : line;
}

/** First non-empty line of a gate body. */
const firstLine = (body: unknown): string | undefined =>
  typeof body === 'string' ? body.split(/\r?\n/).find((l) => l.trim() !== '') : undefined;

const isInt = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v);

/** Raw wire items of one section, by id (for the 0.6 summary sources the projection does not carry). */
function rawById(raw: unknown, section: string): Map<string, Record<string, unknown>> {
  const list = (raw as Record<string, unknown> | null)?.[section];
  const out = new Map<string, Record<string, unknown>>();
  for (const x of Array.isArray(list) ? list : []) {
    if (typeof x === 'object' && x !== null && typeof (x as { id?: unknown }).id === 'string') {
      out.set((x as { id: string }).id, x as Record<string, unknown>);
    }
  }
  return out;
}

interface Facts06 {
  changedAtRevision?: unknown;
  lastActivityTs?: unknown;
  active?: unknown;
  originNodeId?: unknown;
  steps?: unknown;
  summary?: unknown;
}

/** The validated 0.6 fields of one node; an invalid or absent value omits its key. */
function fields06(node: NodeProjection, f: Facts06, revision: number): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (isInt(f.changedAtRevision) && f.changedAtRevision >= 1 && f.changedAtRevision <= revision) {
    out.changedAtRevision = f.changedAtRevision;
  }
  if (isInt(f.lastActivityTs) && f.lastActivityTs > 0) out.lastActivityTs = f.lastActivityTs;
  if (typeof f.active === 'boolean') out.active = f.active;
  if (typeof f.originNodeId === 'string' && f.originNodeId !== '' && f.originNodeId !== node.id) {
    out.originNodeId = f.originNodeId;
  }
  const st = f.steps as { done?: unknown; total?: unknown } | undefined;
  if (typeof st === 'object' && st !== null && isInt(st.done) && isInt(st.total) && st.total >= 1 && st.done >= 0 && st.done <= st.total) {
    out.steps = { done: st.done, total: st.total };
  }
  const shortName = shortNameOf(node.label);
  if (shortName !== undefined) out.shortName = shortName;
  const summary = summaryOf(f.summary);
  if (summary !== undefined) out.summary = summary;
  return out;
}

export function projectRhizomeNodes(
  raw: unknown,
  p: WorkspaceProjection,
  mediaTypeOf?: (blobId: string) => string | undefined,
  options: ProjectOptions = {},
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

  // RHZ-132 (FR-RHZ-171): blocked notes are nodes too, so their note_blocked attention is not dropped.
  const ids = new Set<string>([...p.missions, ...p.tasks, ...p.gates, ...p.deliverables, ...(p.requests ?? [])].map((n) => n.id));
  const notesToProject = (p.blockedNotes ?? []).filter((n) => !ids.has(n.id));
  for (const n of notesToProject) ids.add(n.id);
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
    capabilities: levels(p.capabilities[t.id], TASK_ACTIONS, (k) => TEXT_REQUIRED.has(k) ? true : undefined),
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
    // Approved/rejected gates are retained only for provenance review and are no longer actionable.
    capabilities: isUnverifiedDecidedGate(g)
      ? []
      : levels(p.gateCapabilities[g.id], GATE_ACTIONS, (k) => k === 'reject' || k === 'requestChanges' ? true : undefined),
    // A waiting gate pulls the human: it is a pending decision by definition. A
    // changes_requested gate (RHZ-078, FR-RHZ-109) passes through as its own state
    // value; the ball is with the worker, so no needs_human is added for it.
    attention: [
      ...attentionOf(g.id).filter((a) => a.cause !== APPROVAL_UNVERIFIED),
      ...(g.state === 'waiting' ? [{ cause: NEEDS_HUMAN }] : []),
      ...(isUnverifiedDecidedGate(g)
        ? [{ cause: APPROVAL_UNVERIFIED, ...(g.decidedAt ? { since: g.decidedAt } : {}) }]
        : []),
    ],
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
  const requests: NodeProjection[] = (p.requests ?? []).map((r) => ({
    id: r.id,
    kind: 'request',
    label: r.name,
    state: { value: r.state },
    relations: membership(r.membershipId ?? ''),
    capabilities: levels(p.requestCapabilities?.[r.id], REQUEST_ACTIONS, (k) => k === 'unable'),
    // RHZ-132 (FR-RHZ-171): Rhizome's own request_waiting entry rides along with the adapter's cause.
    attention: [...attentionOf(r.id), ...(r.state === 'waiting' ? [{ cause: NEEDS_HUMAN_ACTION, since: r.createdAt }] : [])],
    artifacts: [],
  }));
  // RHZ-132 (FR-RHZ-171): a blocked note hangs on its mission only while that node is on the wire.
  const visibleParents = new Set<string>([...p.missions, ...p.tasks].map((n) => n.id));
  const notes: NodeProjection[] = notesToProject.map((n) => ({
    id: n.id,
    kind: 'note',
    label: n.cause || n.id,
    state: { value: NOTE_BLOCKED_STATE },
    relations: n.missionId && visibleParents.has(n.missionId) ? membership(n.missionId) : [],
    capabilities: [{ action: NOTE_ANSWER, level: 'enabled', decision: { input: { required: true } } }],
    attention: attentionOf(n.id),
    artifacts: [],
  }));
  const nodes = [root, ...missions, ...tasks, ...gates, ...deliverables, ...requests, ...notes];
  if ((options.wireFields ?? '0.5') !== '0.6') return { nodes, report };

  // RHZ-133 (FR-RHZ-173): wire fields 0.6 — per-node facts from the projection
  // (validated) plus shortName (from the label) and a one-line summary.
  const revision = options.revision ?? p.revision;
  const rawMissions = rawById(raw, 'missions');
  const rawGates = rawById(raw, 'gates');
  const rawDeliverables = rawById(raw, 'deliverables');
  const rawRequests = rawById(raw, 'requests');
  const facts = new Map<string, Facts06>();
  for (const m of p.missions) facts.set(m.id, { ...m, summary: rawMissions.get(m.id)?.success });
  for (const t of p.tasks) facts.set(t.id, { ...t, summary: summaryOf(t.currentAction) ?? t.blockedReason });
  for (const g of p.gates) facts.set(g.id, { ...g, summary: firstLine(rawGates.get(g.id)?.body) });
  for (const d of p.deliverables) facts.set(d.id, { ...d, summary: rawDeliverables.get(d.id)?.summary });
  for (const r of p.requests ?? []) facts.set(r.id, { ...r, summary: rawRequests.get(r.id)?.why });
  const kindOf = new Map<string, string>([
    ...p.missions.map((m) => [m.id, 'mission'] as const),
    ...p.tasks.map((t) => [t.id, 'task'] as const),
    ...p.gates.map((g) => [g.id, 'gate'] as const),
    ...p.deliverables.map((d) => [d.id, 'deliverable'] as const),
    ...(p.requests ?? []).map((r) => [r.id, 'request'] as const),
  ]);
  const with06 = nodes.map((n) => {
    // Root and notes carry no per-node facts; only their label-derived shortName.
    const f = n === root || !kindOf.has(n.id) ? {} : (facts.get(n.id) ?? {});
    // active / originNodeId / steps are task facts (steps also goal); never on other kinds.
    const k = kindOf.get(n.id);
    const scoped: Facts06 = {
      changedAtRevision: f.changedAtRevision,
      lastActivityTs: f.lastActivityTs,
      ...(k === 'task' ? { active: f.active, originNodeId: f.originNodeId } : {}),
      ...(k === 'task' || k === 'mission' ? { steps: f.steps } : {}),
      summary: f.summary,
    };
    return { ...n, ...fields06(n, scoped, revision) } as NodeProjection;
  });
  return { nodes: with06, report };
}
