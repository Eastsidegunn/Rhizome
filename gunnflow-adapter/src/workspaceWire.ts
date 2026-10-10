/**
 * Rhizome v1 workspace DTOs → the cockpit's projection shape
 * (workspace/http.go DTOs). Pure
 * shape/vocabulary adaptation — no new facts: omitted upstream values stay
 * omitted, never inferred. Items already in the cockpit's shape pass through.
 */
import type { CapabilityLevel } from '@gunnflow/contract';

/*
 * The cockpit's current projection shape, as far as this adapter produces it.
 * Declared here so the adapter depends on nothing but the contract and the port.
 */
type NodeState = 'queued' | 'running' | 'waiting' | 'paused' | 'blocked' | 'completed' | 'failed' | 'cancelled';
interface MissionProjection {
  kind: 'mission';
  id: string;
  name: string;
  attention: boolean;
  /** Goal lifecycle state (RHZ-061). Terminal goals are dropped upstream (조종석=진행 중); non-terminal goals stay even without a live task (RHZ-074/FR-RHZ-102). */
  state?: string;
}
interface TaskProjection {
  kind: 'task';
  id: string;
  missionId: string;
  name: string;
  state: NodeState;
  currentAction?: string;
  progress?: number;
  blockedReason?: string;
  attention: boolean;
}
interface GateProjection {
  kind: 'gate';
  id: string;
  missionId: string;
  /** RHZ-075 (FR-RHZ-108): binding goal when the gate is goal-bound (xor with missionId upstream); '' when absent. */
  goalId: string;
  name: string;
  gateType: string;
  requestedAction: string;
  /** 'changes_requested' (RHZ-078, FR-RHZ-109): the human sent the request back; the gate still awaits approve/reject. */
  state: 'waiting' | 'changes_requested' | 'approved' | 'rejected' | 'expired' | 'superseded' | 'canceled';
  /** Cockpit fields v1 never carries; left absent, never invented. */
  request?: unknown;
  riskTier?: 'logged' | 'privileged';
  /** Decision provenance as emitted by Rhizome; absent means unverified. */
  verification?: GateVerification;
  /** Decision-event envelope time as emitted by Rhizome; absent on older servers/open gates. */
  decidedAt?: string;
}

/** Additive gate-decision provenance on GET /v1/workspace and its SSE frames. */
export interface GateVerification {
  status: string;
  claimKind?: string;
  assurance?: string;
  keyId?: string;
  keyRevokedNow?: boolean;
}
interface WireTrustSummary {
  journalId: string;
  genesisKeyId: string;
}
interface WireWorkspaceBody extends Record<string, unknown> {
  /** Read-only trust-domain metadata is accepted but never projected to nodes. */
  trust?: WireTrustSummary;
}
interface DeliverableProjection {
  kind: 'deliverable';
  id: string;
  missionId: string;
  /** RHZ-081 (FR-RHZ-112): a deliverable is bound to exactly one of missionId / goalId; present only when goal-bound. */
  goalId?: string;
  title: string;
  deliverableType: string;
  /** v1 has no lifecycle state; left absent, never invented. */
  state?: 'draft' | 'completed';
  /**
   * Rhizome's source reference, carried through so nodes.ts can derive an
   * artifact (id+digest) when it is a content-addressed blob (`sha256:<hex>`).
   * `exec-` refs and others carry no blob bytes and are left as-is. (RHZ-057
   * stage 2 — first-class edges are a separate axis.)
   */
  sourceRef?: string;
  /**
   * RHZ-085 (FR-RHZ-115): set only when the owning task (missionId) is hidden
   * as terminal while its parent goal is still live. The deliverable then
   * survives under the goal and nodes.ts hangs its member-of on this goal so the
   * relation never names a hidden node. missionId stays as Rhizome stated it.
   */
  promotedGoalId?: string;
}
interface EdgeProjection {
  id: string;
  from: string;
  to: string;
  edgeKind: 'dependency' | 'spawn' | 'produces' | 'gate' | 'contains';
  /** Id of the edge this one replaces (edge.rewire, RHZ-066); the replaced edge is not a live relationship. */
  supersedes?: string;
}
export interface RequestProjection {
  kind: 'request';
  id: string;
  name: string;
  state: 'waiting' | 'done' | 'unable' | 'cancelled';
  missionId?: string;
  goalId?: string;
  /** Live parent only; omitted when a waiting request outlives its target. */
  membershipId?: string;
  createdAt: string;
}
/**
 * RHZ-132 (FR-RHZ-171): a blocked note Rhizome lists as a `note_blocked`
 * attention entry (refId = note id, cause = first content line, missionId =
 * the open mission it is bound to). Carried verbatim from the wire's
 * attention list; nodes.ts projects each as a `note` node.
 */
export interface BlockedNoteProjection {
  kind: 'note';
  id: string;
  cause: string;
  missionId?: string;
}
/** Attention kind Rhizome emits for a blocked note on an open mission (FR-RHZ-171). */
export const NOTE_BLOCKED = 'note_blocked';

/** The note_blocked entries of a raw /v1/workspace body; malformed entries are skipped, first refId wins. */
export function blockedNotesOf(raw: unknown): BlockedNoteProjection[] {
  const listed = (raw as { attention?: unknown } | null)?.attention;
  const seen = new Set<string>();
  const out: BlockedNoteProjection[] = [];
  for (const a of Array.isArray(listed) ? listed : []) {
    const x = a as { kind?: unknown; refId?: unknown; cause?: unknown; missionId?: unknown } | null;
    if (typeof x !== 'object' || x === null || x.kind !== NOTE_BLOCKED) continue;
    if (typeof x.refId !== 'string' || x.refId === '' || seen.has(x.refId)) continue;
    seen.add(x.refId);
    out.push({
      kind: 'note',
      id: x.refId,
      cause: typeof x.cause === 'string' ? x.cause : '',
      ...(typeof x.missionId === 'string' && x.missionId !== '' ? { missionId: x.missionId } : {}),
    });
  }
  return out;
}
type TaskCapabilities = Partial<Record<'pause' | 'resume' | 'instruct' | 'cancel' | 'forceReplan', CapabilityLevel>>;
type RequestCapabilities = Partial<Record<'complete' | 'unable', CapabilityLevel>>;
export interface WorkspaceProjection {
  revision: number;
  missions: MissionProjection[];
  tasks: TaskProjection[];
  gates: GateProjection[];
  deliverables: DeliverableProjection[];
  edges: EdgeProjection[];
  /** Absent when Rhizome omitted requests, preserving request-free bodies. */
  requests?: RequestProjection[];
  /** RHZ-132 (FR-RHZ-171): present only when the wire lists note_blocked attention. */
  blockedNotes?: BlockedNoteProjection[];
  /**
   * RHZ-076 (FR-RHZ-104): ids that are the target of a `contains` edge anywhere
   * in the raw snapshot, whether or not both ends survive the cockpit's live
   * filter, EXCLUDING edges that a later edge supersedes (edge.rewire,
   * RHZ-066: a superseded contains edge is no relationship at all). nodes.ts
   * reads it to tell a top-level goal (cockpit kind "goal") from a contained
   * one ("mission"). Hidden ends are kept on purpose: a goal whose parent is
   * terminal still has a parent in Rhizome's truth and must not jump to the
   * top level. Required: it is the only source of the kind decision.
   */
  containedIds: string[];
  counts: { running: number; needsYou: number; blocked: number };
  activities: unknown[];
  sessions: unknown[];
  capabilities: Record<string, TaskCapabilities>;
  gateCapabilities: Record<string, unknown>;
  requestCapabilities?: Record<string, RequestCapabilities>;
  effects: unknown[];
}

interface WireTask {
  id: string;
  missionId: string;
  name: string;
  state: TaskProjection['state'];
  currentAction?: string;
  progress?: number;
  hasProgress: boolean;
  blockedReason?: string;
  attention: boolean;
}
interface WireGate {
  id: string;
  // 'changes_requested' (RHZ-078, FR-RHZ-109) is non-terminal: approve/reject may still follow.
  state: 'pending' | 'changes_requested' | 'approved' | 'rejected';
  // Rhizome now carries the gate's own name (RHZ-047/050); v1 did not, so it stays optional.
  name?: string;
  // Rhizome now carries the gate's binding mission (RHZ-074/FR-RHZ-102); old journal entries may omit it.
  missionId?: string;
  // RHZ-075 (FR-RHZ-108): a gate is bound to exactly one of missionId / goalId; both stay optional on the wire.
  goalId?: string;
  humanDecision?: string;
  janusDecision?: string;
  superseded: boolean;
  verification?: GateVerification;
  decidedAt?: string;
}
interface WireDeliverable {
  id: string;
  kind: string;
  missionId: string;
  // RHZ-081 (FR-RHZ-112): goal-bound deliverables carry goalId (omitempty on the wire) and an empty missionId.
  goalId?: string;
  sourceRef: string;
  summary: string;
}
interface WireEdge {
  id: string;
  from: { type: string; id: string };
  to: { type: string; id: string };
  edgeKind: string;
  supersedes?: string;
  actor?: string;
  correlation?: string;
}
interface WireRequest {
  id: string;
  name: string;
  state: RequestProjection['state'];
  missionId?: string;
  goalId?: string;
  createdAt: string;
}

/**
 * Terminal task states. The cockpit is awareness of work in progress, not an
 * archive of finished work; terminal items stay in Rhizome's truth but are not
 * forwarded upstream. Terminal-ness is Rhizome's own emitted state, so this
 * scopes the cockpit over a backend fact — it invents nothing.
 */
const TERMINAL_TASK_STATES = new Set<string>(['completed', 'failed', 'cancelled']);
/** Gate states that keep a gate on the cockpit: a decision is still open (RHZ-078, FR-RHZ-109). */
const LIVE_GATE_STATES = new Set<GateProjection['state']>(['waiting', 'changes_requested']);
/** Terminal gate decisions; changes_requested remains live and actionable. */
const DECIDED_GATE_STATES = new Set<GateProjection['state']>(['approved', 'rejected']);
/** Default number of unverified terminal decisions kept on the cockpit. */
export const DEFAULT_UNVERIFIED_DECIDED_MAX = 10;
/** Terminal goal states (RHZ-061): a closed/cancelled goal leaves the cockpit. */
const TERMINAL_GOAL_STATES = new Set<string>(['achieved', 'failed', 'cancelled']);

/** Items that already carry the cockpit's discriminator pass through untouched. */
const isAdapted = (x: unknown, kind: string) =>
  (x as { kind?: string }).kind === kind && (kind !== 'deliverable' || 'title' in (x as object));

/** Runtime shape check for the additive wire field; malformed values fail closed. */
export function isGateVerification(value: unknown): value is GateVerification {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const v = value as Record<string, unknown>;
  const allowed = new Set(['status', 'claimKind', 'assurance', 'keyId', 'keyRevokedNow']);
  if (Object.keys(v).some((key) => !allowed.has(key))) return false;
  if (typeof v.status !== 'string' || v.status === '') return false;
  if (v.claimKind !== undefined && typeof v.claimKind !== 'string') return false;
  if (v.assurance !== undefined && typeof v.assurance !== 'string') return false;
  if (v.keyId !== undefined && typeof v.keyId !== 'string') return false;
  if (v.keyRevokedNow !== undefined && typeof v.keyRevokedNow !== 'boolean') return false;
  if (v.status === 'verified' || v.status === 'attested') {
    return v.claimKind === undefined && v.assurance === 'key' &&
      typeof v.keyId === 'string' && /^sha256:[0-9a-f]{64}$/.test(v.keyId);
  }
  return v.assurance === undefined && v.keyId === undefined && v.keyRevokedNow === undefined;
}

/**
 * A terminal decision stays visible only when provenance is absent
 * (unverified), malformed, legacy asserted, or unknown. Only a valid claimed
 * verified, or attested status clears approval_unverified.
 */
export function isUnverifiedDecidedGate(gate: {
  state?: string;
  superseded?: boolean;
  verification?: unknown;
}): boolean {
  if (gate.superseded || !DECIDED_GATE_STATES.has((gate.state ?? '') as GateProjection['state'])) return false;
  if (!isGateVerification(gate.verification)) return true;
  return gate.verification.status !== 'claimed' && gate.verification.status !== 'verified' && gate.verification.status !== 'attested';
}

/**
 * The uncapped set eligible for decided-gate retention. A bound gate is kept
 * only while its projected parent is live, matching the RHZ-085 liveness rule
 * and preventing a member-of relation from naming a hidden node.
 */
export function unverifiedDecidedGates<T extends {
  state?: string;
  superseded?: boolean;
  verification?: unknown;
  missionId?: string;
  goalId?: string;
}>(
  gates: readonly T[],
  missions: readonly { id: string; state?: string }[],
  tasks: readonly { id: string; missionId?: string; state?: string }[],
): T[] {
  const terminalGoalIds = new Set(
    missions.filter((mission) => TERMINAL_GOAL_STATES.has(mission.state ?? '')).map((mission) => mission.id),
  );
  const liveTasks = tasks.filter(
    (task) => !TERMINAL_TASK_STATES.has(task.state ?? '') && !terminalGoalIds.has(task.missionId ?? ''),
  );
  const liveGoalIds = new Set([
    ...missions.filter((mission) => !terminalGoalIds.has(mission.id)).map((mission) => mission.id),
    ...liveTasks.flatMap((task) => task.missionId ? [task.missionId] : []),
  ]);
  const liveTaskIds = new Set(liveTasks.map((task) => task.id));

  return gates.filter((gate) => {
    if (!isUnverifiedDecidedGate(gate)) return false;
    if (gate.missionId) return liveTaskIds.has(gate.missionId);
    if (gate.goalId) return liveGoalIds.has(gate.goalId);
    return true;
  });
}

/**
 * Rank terminal decisions newest-first. Known decision times precede missing
 * legacy values; gate id descending preserves the former cap selection as the
 * deterministic tiebreak and as the complete fallback for old servers.
 */
function rankUnverifiedDecidedGates<T extends { id?: string; decidedAt?: string }>(gates: readonly T[]): T[] {
  const timeOf = (gate: T): string | undefined => {
    if (!gate.decidedAt) return undefined;
    // The core emits UTC RFC3339Nano. Normalize its optional fraction to nine
    // digits so lexical comparison preserves the full envelope precision.
    const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z$/.exec(gate.decidedAt);
    return match ? `${match[1]}.${(match[2] ?? '').padEnd(9, '0')}Z` : undefined;
  };
  return [...gates].sort((a, b) => {
    const aTime = timeOf(a);
    const bTime = timeOf(b);
    if (aTime !== undefined && bTime !== undefined && aTime !== bTime) return aTime < bTime ? 1 : -1;
    if (aTime !== undefined && bTime === undefined) return -1;
    if (aTime === undefined && bTime !== undefined) return 1;
    const aID = String(a.id ?? '');
    const bID = String(b.id ?? '');
    return aID < bID ? 1 : aID > bID ? -1 : 0;
  });
}

/** Invalid and negative overrides fall back to the documented default; zero is a valid cap. */
export function unverifiedDecidedMax(value: string | undefined = process.env.RHIZOME_UNVERIFIED_DECIDED_MAX): number {
  if (value === undefined || !/^\d+$/.test(value)) return DEFAULT_UNVERIFIED_DECIDED_MAX;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : DEFAULT_UNVERIFIED_DECIDED_MAX;
}

export function adaptWorkspaceBody(raw: unknown): WorkspaceProjection {
  const body = (raw ?? {}) as WireWorkspaceBody;
  const list = (k: string) => (body[k] as unknown[] | undefined) ?? [];

  const tasks: TaskProjection[] = list('tasks').map((t) => {
    if (isAdapted(t, 'task')) return t as TaskProjection;
    const w = t as WireTask;
    return {
      kind: 'task',
      id: w.id,
      missionId: w.missionId,
      name: w.name,
      state: w.state,
      currentAction: w.currentAction,
      // Progress exists only when upstream says so (hasProgress) — B3.
      progress: w.hasProgress ? (w.progress ?? 0) : undefined,
      blockedReason: w.blockedReason,
      attention: w.attention ?? false,
    };
  });

  /**
   * v1 ships no capability projection; the server itself accepts
   * pause/resume/instruct from the (unverified) operator. Mirroring that
   * acceptance surface is not a widening — privileged actions stay hidden.
   */
  const capabilities =
    (body.capabilities as Record<string, TaskCapabilities> | undefined) ??
    Object.fromEntries(
      tasks.map((t) => [t.id, { pause: 'enabled', resume: 'enabled', instruct: 'enabled' } as TaskCapabilities]),
    );

  const allMissions: MissionProjection[] = list('missions').map((m) =>
    isAdapted(m, 'mission')
      ? (m as MissionProjection)
      : ({
          kind: 'mission',
          id: (m as { id: string }).id,
          name: (m as { name: string }).name,
          attention: (m as { attention?: boolean }).attention ?? false,
          state: (m as { state?: string }).state,
        } satisfies MissionProjection),
  );
  const allGates: GateProjection[] = list('gates').map((g) => {
    if (isAdapted(g, 'gate')) return g as GateProjection;
    const w = g as WireGate;
    const verification = isGateVerification(w.verification) ? w.verification : undefined;
    return {
      kind: 'gate',
      id: w.id,
      // Name/missionId are carried when Rhizome states them (RHZ-074/FR-RHZ-102: a bound gate
      // joins its mission instead of floating); type/request stay absent (v1).
      missionId: w.missionId ?? '',
      goalId: w.goalId ?? '',
      name: w.name || w.id,
      gateType: '',
      requestedAction: '',
      state: w.superseded ? 'superseded' : w.state === 'pending' ? 'waiting' : w.state,
      ...(verification ? { verification } : {}),
      ...(w.decidedAt ? { decidedAt: w.decidedAt } : {}),
    } satisfies GateProjection;
  });
  const allDeliverables: DeliverableProjection[] = list('deliverables').map((d) => {
    if (isAdapted(d, 'deliverable')) return d as DeliverableProjection;
    const w = d as WireDeliverable;
    return {
      kind: 'deliverable',
      id: w.id,
      missionId: w.missionId,
      ...(w.goalId ? { goalId: w.goalId } : {}),
      title: w.summary || w.sourceRef || w.id,
      deliverableType: w.kind,
      sourceRef: w.sourceRef,
      // No lifecycle state in v1 → omitted, never invented.
    } satisfies DeliverableProjection;
  });
  const allEdges: EdgeProjection[] = list('edges').map((e) => {
    const any = e as WireEdge | EdgeProjection;
    if (typeof (any as EdgeProjection).from === 'string') return any as EdgeProjection;
    const w = any as WireEdge;
    return {
      id: w.id,
      from: w.from.id,
      to: w.to.id,
      edgeKind: w.edgeKind as EdgeProjection['edgeKind'],
      ...(w.supersedes ? { supersedes: w.supersedes } : {}),
    };
  });

  // Cockpit scope = work in progress. Drop terminal goals, terminal tasks (and
  // the tasks of terminal goals), decided gates, and the deliverables whose
  // owning goal is terminal (RHZ-085). (A deliverable's missionId carries the
  // task id it was produced under.)
  // 조종석 = 진행 중. 종료된 goal(achieved/failed/cancelled)은 상류로 안 올린다 — RHZ-061
  // 운영자 생명주기 intent로 닫힌 이슈·쓸어낸 잔재가 조종석에서 사라지게(어댑터 숨김).
  // RHZ-074 (FR-RHZ-102): goal=결과이므로 live task가 없어도 non-terminal goal은 남긴다(contains
  // 계층의 뿌리가 쌍 mission 정리로 사라지지 않게). 반대로 부모 goal이 terminal인
  // task는 함께 숨긴다 — goal.resolve 뒤 queued 짝 mission이 고아로 뜨지 않게.
  const terminalGoalIds = new Set(
    allMissions.filter((m) => TERMINAL_GOAL_STATES.has(m.state ?? '')).map((m) => m.id),
  );
  const liveMissions = allMissions.filter((m) => !terminalGoalIds.has(m.id));
  const liveTasks = tasks.filter((t) => !TERMINAL_TASK_STATES.has(t.state) && !terminalGoalIds.has(t.missionId));
  // A gate is live while it awaits a human decision: waiting, or changes_requested (RHZ-078,
  // FR-RHZ-109 — the request went back for revision but approve/reject are still open on it).
  // RHZ-105/FR-RHZ-142: unverified and legacy-asserted terminal decisions remain visible,
  // capped to the N most recent by decidedAt. Gate-id order breaks ties and is the fallback
  // for older servers that omit decidedAt. Retained gates preserve their projection order.
  const retainedDecided = unverifiedDecidedGates(allGates, allMissions, tasks);
  const maxRetainedDecided = unverifiedDecidedMax();
  const retainedDecidedIds = new Set(
    rankUnverifiedDecidedGates(retainedDecided).slice(0, maxRetainedDecided).map((g) => g.id),
  );
  const liveGates = allGates.filter((g) => LIVE_GATE_STATES.has(g.state) || retainedDecidedIds.has(g.id));
  const liveMissionIds = new Set([...liveMissions.map((m) => m.id), ...liveTasks.map((t) => t.missionId)]);
  const liveTaskIds = new Set(liveTasks.map((t) => t.id));
  // FR-RHZ-159: waiting requests survive a terminal/hidden target as
  // top-level nodes; closed requests survive only while their target is live.
  const allRequests: RequestProjection[] = list('requests').map((r) => {
    if (isAdapted(r, 'request')) return r as RequestProjection;
    const w = r as WireRequest;
    return {
      kind: 'request', id: w.id, name: w.name, state: w.state,
      ...(w.missionId ? { missionId: w.missionId } : {}),
      ...(w.goalId ? { goalId: w.goalId } : {}),
      createdAt: w.createdAt,
    };
  });
  const liveRequests = allRequests.flatMap((r): RequestProjection[] => {
    const target = r.missionId || r.goalId || '';
    const targetLive = r.missionId ? liveTaskIds.has(r.missionId) : !!r.goalId && liveMissionIds.has(r.goalId);
    if (r.state !== 'waiting' && !targetLive) return [];
    return [{ ...r, ...(targetLive && target ? { membershipId: target } : {}) }];
  });
  // RHZ-081 (FR-RHZ-112): a goal-bound deliverable (missionId "") stays while its goal is live.
  // RHZ-085 (FR-RHZ-115): a deliverable is live iff its owning GOAL is live (goal = result,
  // RHZ-074). A mission-bound deliverable whose task is terminal (hidden) resolves the task's
  // parent goal; while that goal is live the deliverable stays and is promoted (member-of →
  // goal) so its membership never dangles on the hidden task. The goal must be known live
  // here (listed non-terminal, or the parent of a live task) — nothing is inferred otherwise.
  // 완료된 mission의 산출물이 조종석에서 사라지지 않게: goal이 non-terminal이면 goal 아래로 승격 노출.
  const goalOfTask = new Map(tasks.map((t) => [t.id, t.missionId]));
  const liveDeliverables = allDeliverables.flatMap((d): DeliverableProjection[] => {
    if (liveTaskIds.has(d.missionId)) return [d];
    if (d.goalId) return liveMissionIds.has(d.goalId) ? [d] : [];
    const goalId = goalOfTask.get(d.missionId);
    return goalId && liveMissionIds.has(goalId) ? [{ ...d, promotedGoalId: goalId }] : [];
  });
  const liveIds = new Set<string>([
    ...liveMissionIds,
    ...liveTaskIds,
    ...liveGates.map((g) => g.id),
    ...liveDeliverables.map((d) => d.id),
    ...liveRequests.map((r) => r.id),
  ]);
  // RHZ-066 edge.rewire: an edge another edge supersedes is no live relationship.
  const supersededEdgeIds = new Set(allEdges.flatMap((e) => (e.supersedes ? [e.supersedes] : [])));
  // FR-RHZ-106: superseded edges leave the wire too, so a parent's relations stop naming the old target.
  const liveEdges = allEdges.filter((e) => liveIds.has(e.from) && liveIds.has(e.to) && !supersededEdgeIds.has(e.id));
  // RHZ-076 (FR-RHZ-104): containment is read from every non-superseded edge, not only live
  // ones (see the field's doc).
  const containedIds = [
    ...new Set(allEdges.filter((e) => e.edgeKind === 'contains' && !supersededEdgeIds.has(e.id)).map((e) => e.to)),
  ];

  const blockedNotes = blockedNotesOf(body);

  return {
    revision: (body.revision as number | undefined) ?? 0,
    missions: liveMissions,
    tasks: liveTasks,
    gates: liveGates,
    deliverables: liveDeliverables,
    edges: liveEdges,
    ...(Object.prototype.hasOwnProperty.call(body, 'requests') ? { requests: liveRequests } : {}),
    ...(blockedNotes.length > 0 ? { blockedNotes } : {}),
    containedIds,
    counts: (body.counts as WorkspaceProjection['counts'] | undefined) ?? { running: 0, needsYou: 0, blocked: 0 },
    activities: (body.activities as WorkspaceProjection['activities'] | undefined) ?? [],
    sessions: (body.sessions as WorkspaceProjection['sessions'] | undefined) ?? [],
    capabilities,
    gateCapabilities: (body.gateCapabilities as WorkspaceProjection['gateCapabilities'] | undefined) ?? {},
    ...(Object.prototype.hasOwnProperty.call(body, 'requestCapabilities')
      ? { requestCapabilities: body.requestCapabilities as Record<string, RequestCapabilities> }
      : {}),
    effects: (body.effects as WorkspaceProjection['effects'] | undefined) ?? [],
  };
}
