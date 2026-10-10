package workspace

// RHZ-133 (FR-RHZ-173): per-node liveness / version / provenance / progress
// facts for GET /v1/workspace, derived only from the journal.
//
//   - Activity (changedAtRevision, lastActivityTs) is the global Sequence and
//     envelope CreatedAt (Unix ms) of the latest event in the node's OWN
//     aggregate stream(s). Edges are deliberately not included: declaring or
//     rewiring a relationship does not change the node itself.
//   - Task.Active is true iff the domain mission state is running.
//   - Task.OriginNodeID is the single source of the task's incoming live
//     spawn edges (procedure run → step mission); none or ambiguous → "".
//   - Steps count spawn children (task) or tasks under a goal and every goal
//     it contains (goal), by domain mission state.

import (
	"rhizome/internal/domain"
	"rhizome/internal/edge"
	"rhizome/internal/events"
)

// Activity is the latest own-stream event of a node. Zero = unknown.
type Activity struct {
	ChangedAtRevision uint64
	LastActivityTs    int64
}

// Steps is a done/total progress count; Total == 0 means "no steps" and is
// omitted on the wire.
type Steps struct{ Done, Total int }

type aggKey struct{ typ, id string }

// activityIndex maps an aggregate stream to its latest event. It is built in
// Snapshot's single All() scan.
type activityIndex map[aggKey]Activity

// livenessStreams are the aggregate types whose activity a node may report.
var livenessStreams = map[string]bool{
	"goal": true, "mission": true, "surface": true, "approval": true,
	"approvalrequest": true, "question": true, "deliverable": true, "request": true,
}

func (x activityIndex) note(e events.Event) {
	if !livenessStreams[e.AggregateType] {
		return
	}
	k := aggKey{e.AggregateType, e.AggregateID}
	if e.Sequence <= x[k].ChangedAtRevision {
		return
	}
	var ts int64
	if !e.CreatedAt.IsZero() {
		if ms := e.CreatedAt.UnixMilli(); ms > 0 {
			ts = ms
		}
	}
	x[k] = Activity{ChangedAtRevision: e.Sequence, LastActivityTs: ts}
}

// of returns the latest activity across the given streams (typ, id pairs).
func (x activityIndex) of(streams ...aggKey) Activity {
	var out Activity
	for _, k := range streams {
		if a := x[k]; a.ChangedAtRevision > out.ChangedAtRevision {
			out = a
		}
	}
	return out
}

// applyStructureFacts fills Task.OriginNodeID, Task.Steps and Mission.Steps
// from the projected edges and the domain mission states. Superseded edges
// (another edge names them in Supersedes) are ignored, as everywhere else.
func applyStructureFacts(p *Projection, states map[string]domain.MissionState) {
	superseded := map[string]bool{}
	for _, x := range p.Edges {
		if x.Supersedes != "" {
			superseded[x.Supersedes] = true
		}
	}
	origins := map[string]map[string]bool{}  // child task id → spawn sources
	children := map[string]map[string]bool{} // task id → spawned child tasks
	contains := map[string][]string{}        // goal id → contained goal ids
	for _, x := range p.Edges {
		if superseded[x.ID] {
			continue
		}
		switch x.Kind {
		case edge.Spawn:
			if x.To.Type != "mission" {
				continue
			}
			if _, ok := states[x.To.ID]; !ok {
				continue
			}
			if x.From.ID != x.To.ID {
				if origins[x.To.ID] == nil {
					origins[x.To.ID] = map[string]bool{}
				}
				origins[x.To.ID][x.From.ID] = true
			}
			if x.From.Type == "mission" && x.From.ID != x.To.ID {
				if children[x.From.ID] == nil {
					children[x.From.ID] = map[string]bool{}
				}
				children[x.From.ID][x.To.ID] = true
			}
		case edge.Contains:
			if x.From.Type == "goal" && x.To.Type == "goal" {
				contains[x.From.ID] = append(contains[x.From.ID], x.To.ID)
			}
		}
	}
	count := func(ids map[string]bool) Steps {
		var s Steps
		for id := range ids {
			switch states[id] {
			case domain.MissionCancelled:
				continue
			case domain.MissionSucceeded:
				s.Done++
			}
			s.Total++
		}
		return s
	}
	tasksByGoal := map[string]map[string]bool{}
	for i := range p.Tasks {
		t := &p.Tasks[i]
		if src := origins[t.ID]; len(src) == 1 {
			for id := range src {
				t.OriginNodeID = id
			}
		}
		t.Steps = count(children[t.ID])
		if t.MissionID != "" {
			if tasksByGoal[t.MissionID] == nil {
				tasksByGoal[t.MissionID] = map[string]bool{}
			}
			tasksByGoal[t.MissionID][t.ID] = true
		}
	}
	for i := range p.Missions {
		g := &p.Missions[i]
		visited := map[string]bool{}
		queue := []string{g.ID}
		under := map[string]bool{}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if visited[cur] {
				continue
			}
			visited[cur] = true
			for id := range tasksByGoal[cur] {
				under[id] = true
			}
			queue = append(queue, contains[cur]...)
		}
		g.Steps = count(under)
	}
}
