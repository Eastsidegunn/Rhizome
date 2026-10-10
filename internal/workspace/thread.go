package workspace

// RHZ-134 (FR-RHZ-175): "카드가 대화한다". Notes of kind question/answer are
// linked to what they are about by the tag re:<id> (written by note.create
// from its re field). This file derives, on every projection and from the
// event list only (no persistent cache):
//
//   - the gate thread: for each internal gate, the question/answer notes
//     reachable by re: links from the gate (notes tagged re:<gateId>, then
//     notes tagged re:<one of those notes>, transitively), superseded
//     originals excluded, ordered by journal sequence ascending, capped at
//     threadCap entries (the latest threadCap are kept), each entry's
//     content cut at threadContentMaxRunes runes;
//   - the answered set: blocked notes that carry a non-superseded answer
//     note tagged re:<blocked note id> (attention.go resolution).

import (
	"sort"
	"strings"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/question"
)

// threadCap bounds one gate's thread. When more entries are reachable the
// oldest are dropped (the latest threadCap, still ascending, are kept).
const threadCap = 50

// threadContentMaxRunes bounds one thread entry's content on the wire (a note
// may be 16 KiB and the projection is re-sent after every accepted intent);
// longer content is cut and marked with threadContentCut. The full note stays
// readable through /v1/knowledge.
const (
	threadContentMaxRunes = 2000
	threadContentCut      = "…"
)

func threadContent(c string) string {
	if r := []rune(c); len(r) > threadContentMaxRunes {
		return string(r[:threadContentMaxRunes]) + threadContentCut
	}
	return c
}

// rePrefix is the tag prefix of a re: link.
const rePrefix = "re:"

// ThreadEntry is one question/answer note in a gate thread. At is the note
// event envelope CreatedAt (RFC3339Nano UTC, "" when zero/legacy).
type ThreadEntry struct{ ID, Kind, Content, At string }

// noteRecord is one replayed note with its first event's facts.
type noteRecord struct {
	memory.Memory
	seq uint64
	at  time.Time
}

// noteIndex is the replayed memory notes of one snapshot.
type noteIndex struct {
	notes      []noteRecord // ordered by note id
	superseded map[string]bool
	// byRe maps a re: target id to the live (non-superseded) notes tagged
	// re:<target>, in journal sequence order.
	byRe map[string][]noteRecord
}

// buildNoteIndex replays every memory stream in all. A stream this binary
// cannot replay is left out (it must not take /v1/workspace down —
// /v1/knowledge still reports it).
func buildNoteIndex(all []events.Event) noteIndex {
	idx := noteIndex{superseded: map[string]bool{}, byRe: map[string][]noteRecord{}}
	streams := map[string][]events.Event{}
	for _, e := range all {
		if e.AggregateType == "memory" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	if len(streams) == 0 {
		return idx
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m, err := memory.Replay(streams[id])
		if err != nil {
			continue
		}
		first := streams[id][0]
		idx.notes = append(idx.notes, noteRecord{Memory: m, seq: first.Sequence, at: first.CreatedAt})
		if m.Supersedes != "" {
			idx.superseded[m.Supersedes] = true
		}
	}
	bySeq := make([]noteRecord, 0, len(idx.notes))
	for _, n := range idx.notes {
		if !idx.superseded[n.ID] {
			bySeq = append(bySeq, n)
		}
	}
	sort.SliceStable(bySeq, func(i, j int) bool { return bySeq[i].seq < bySeq[j].seq })
	for _, n := range bySeq {
		seen := map[string]bool{}
		for _, t := range n.Tags {
			target := strings.TrimPrefix(t, rePrefix)
			if !strings.HasPrefix(t, rePrefix) || target == "" || seen[target] {
				continue
			}
			seen[target] = true
			idx.byRe[target] = append(idx.byRe[target], n)
		}
	}
	return idx
}

// answered reports whether a live answer note is tagged re:<id>.
func (idx noteIndex) answered(id string) bool {
	for _, n := range idx.byRe[id] {
		if n.Kind == memory.Answer {
			return true
		}
	}
	return false
}

func threadKind(k memory.Kind) bool { return k == memory.Question || k == memory.Answer }

// thread returns the gate thread of gateID (see the file comment). Only
// question/answer notes are entries and only entries extend the closure;
// the visited set guards against re: cycles.
func (idx noteIndex) thread(gateID string) []ThreadEntry {
	visited := map[string]bool{gateID: true}
	queue := []string{gateID}
	var found []noteRecord
	for len(queue) > 0 {
		target := queue[0]
		queue = queue[1:]
		for _, n := range idx.byRe[target] {
			if visited[n.ID] || !threadKind(n.Kind) {
				continue
			}
			visited[n.ID] = true
			found = append(found, n)
			queue = append(queue, n.ID)
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].seq < found[j].seq })
	if len(found) > threadCap {
		found = found[len(found)-threadCap:]
	}
	out := make([]ThreadEntry, 0, len(found))
	for _, n := range found {
		at := ""
		if !n.at.IsZero() && n.at.UnixMilli() > 0 {
			at = n.at.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, ThreadEntry{ID: n.ID, Kind: string(n.Kind), Content: threadContent(n.Content), At: at})
	}
	return out
}

// applyThreads sets Thread on every internal gate of p.
func applyThreads(idx noteIndex, p *Projection) {
	for i := range p.Gates {
		if p.Gates[i].Source == "internal" {
			p.Gates[i].Thread = idx.thread(p.Gates[i].ID)
		}
	}
}

// reTargetExists reports whether id names an existing question gate or a
// memory note that replays (RHZ-134, FR-RHZ-175). Only these two streams
// are eligible as re: targets.
func reTargetExists(s events.Port, id string) bool {
	if log := s.List("question", id); len(log) > 0 {
		if _, err := question.Replay(log); err == nil {
			return true
		}
	}
	if log := s.List("memory", id); len(log) > 0 {
		if _, err := memory.Replay(log); err == nil {
			return true
		}
	}
	return false
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
