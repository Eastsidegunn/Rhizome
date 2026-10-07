package workspace

// RHZ-073 (FR-RHZ-103): short deterministic node handles. A handle is a
// read-side projection of the journal (zero new events, no schema change): it is
// computed from the node ID and, only on collision, from journal order.
// IDs stay the only thing the journal and edges ever store; handles are
// resolved back to IDs at the surface entrance (GET /v1/context?task= and
// POST /v1/intent) and nowhere deeper.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"rhizome/internal/events"
)

// handlePrefixLen is the number of sha256 hex chars a handle starts with.
// It is a var (not a const) solely so rhz073_test can lower it to force a
// prefix collision; production never writes it.
var handlePrefixLen = 8

// Handle type tags: one letter per aggregate stream so handles of different
// node types can never collide and the resolver knows which stream the
// handle names. JANUS gates (approval / approvalrequest, which share one id
// per RHZ-047) are tagged "a" so every gate in /v1/workspace has a handle.
var handleTags = map[string]string{
	"goal": "g", "mission": "m", "question": "q", "deliverable": "d",
	"approval": "a", "approvalrequest": "a",
}

// handleFor computes the collision-free form of a node handle:
//
//	<tag>-<hex(sha256(id))[:n]>
//
// with n == handlePrefixLen. DESIGN DECISION (a) sha256 prefix over
// (b) journal ordinal: any client can derive the handle from the ID alone,
// with no lookup and no journal, and the handle is stable across journals
// (the same mission replayed into a fresh store keeps its handle). An ordinal
// would depend on everything appended before the node. The exact rule:
//
//  1. handle = tag + "-" + first handlePrefixLen hex chars of sha256(id).
//  2. Nodes are assigned in journal order (by the Sequence of the aggregate's
//     first event). If the handle is already held by an earlier node, the
//     LATER node's hex prefix is extended one char at a time until free; the
//     earlier node's handle never changes, so new nodes never rename old ones.
//  3. Resolution is exact-match against the index built by the same rule, so
//     a handle names at most one node by construction (two nodes can only
//     hold the same handle if they have the same full sha256, i.e. an actual
//     sha256 collision, which is outside the model). A caller that derives
//     the short form offline for a node that was extended would name the
//     earlier node — the cost of (a), accepted: at 8 hex chars the collision
//     probability for N nodes is ~N²/2^33.
func handleFor(tag, id string, n int) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return tag + "-" + h[:n]
}

// handleIndex is the bidirectional handle↔ID map for one journal state.
type handleIndex struct {
	toID map[string]string // handle → id
	ofID map[string]string // tag + ":" + id → handle
	ids  map[string]bool   // every node id that has a handle (any type)
}

func (hi handleIndex) of(tag, id string) string { return hi.ofID[tag+":"+id] }

// resolve maps a handle to its ID. A literal node ID, an input that is no
// known handle, or an empty string passes through unchanged — existing
// callers that send IDs see no behaviour change. A known ID is preferred
// over a handle of the same spelling so an ID can never be mistaken for a
// handle. This matters in practice: question IDs are "q-" + 24 hex chars
// (question.IDForDigest), i.e. handle-shaped. They are always looked up as
// IDs first; a question handle is 8 hex chars (24 only after 16 collision
// extensions on the same digest prefix, outside the model).
func (hi handleIndex) resolve(x string) string {
	if x == "" || hi.ids[x] {
		return x
	}
	if id, ok := hi.toID[x]; ok {
		return id
	}
	return x
}

// buildHandleIndex is the one journal scan the rule needs: first-appearance
// Sequence per (tag, id), then assignment in that order with collision
// extension (see handleFor).
func buildHandleIndex(all []events.Event) handleIndex {
	type node struct {
		tag, id string
		first   uint64
	}
	seen := map[string]bool{}
	nodes := []node{}
	for _, e := range all {
		tag, ok := handleTags[e.AggregateType]
		if !ok {
			continue
		}
		k := tag + ":" + e.AggregateID
		if seen[k] {
			continue
		}
		seen[k] = true
		nodes = append(nodes, node{tag, e.AggregateID, e.Sequence})
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].first < nodes[j].first })
	hi := handleIndex{toID: map[string]string{}, ofID: map[string]string{}, ids: map[string]bool{}}
	for _, nd := range nodes {
		h := handleFor(nd.tag, nd.id, handlePrefixLen)
		for n := handlePrefixLen + 1; ; n++ {
			if prev, taken := hi.toID[h]; !taken || prev == nd.id {
				break
			}
			if n > sha256.Size*2 {
				break // full-digest collision: outside the model (rule 3).
			}
			h = handleFor(nd.tag, nd.id, n)
		}
		hi.toID[h] = nd.id
		hi.ofID[nd.tag+":"+nd.id] = h
		hi.ids[nd.id] = true
	}
	return hi
}

// handleShape reports whether x could be a handle (<tag>-<hex>), so the
// relay and context paths only pay for the journal scan when a caller may
// actually have sent one.
func handleShape(x string) bool {
	if len(x) < 3 || x[1] != '-' {
		return false
	}
	tagOK := false
	for _, t := range handleTags {
		if t == x[:1] {
			tagOK = true
			break
		}
	}
	if !tagOK {
		return false
	}
	for _, c := range x[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// resolveHandles resolves every handle-shaped field of an intent to its ID
// at the relay entrance (one scan, only when some field looks like a
// handle). The returned intent is a copy; the journal still only ever sees
// IDs.
func resolveHandles(s events.Port, in Intent) Intent {
	// ParentGoalID joins the set for goal.create (RHZ-077, FR-RHZ-105).
	if !(handleShape(in.MissionID) || handleShape(in.GoalID) || handleShape(in.GateID) || handleShape(in.TaskID) || handleShape(in.ParentGoalID)) {
		return in
	}
	hi := buildHandleIndex(s.All())
	in.MissionID, in.GoalID, in.GateID, in.TaskID = hi.resolve(in.MissionID), hi.resolve(in.GoalID), hi.resolve(in.GateID), hi.resolve(in.TaskID)
	in.ParentGoalID = hi.resolve(in.ParentGoalID)
	return in
}
