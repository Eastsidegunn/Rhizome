package workspace

import (
	"sort"
	"strings"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/projector"
)

// RHZ-132 (FR-RHZ-171): attention kinds for work whose ball is in the
// human's court, and the deduplicated counts.needsYou.
const (
	attentionGatePending    = "gate_pending"
	attentionRequestWaiting = "request_waiting"
	attentionNoteBlocked    = "note_blocked"
	// noteCauseMaxRunes caps the note_blocked cause line.
	noteCauseMaxRunes = 200
)

// applyHumanAttention appends gate_pending, request_waiting and note_blocked
// attention items to p and sets counts.needsYou to the number of distinct
// RefIDs over: missions waiting_for_human ∪ pending internal gates ∪ waiting
// requests ∪ blocked notes.
//
// Rules (approved test plan D1–D3):
//   - gate_pending: internal gates (questions) in state pending only. JANUS
//     gates (approval/approvalrequest) are JANUS's to resolve (RHZ-085) and
//     changes_requested hands the ball back to the worker.
//   - request_waiting: requests in state waiting only; done/unable are closed.
//   - note_blocked: memory notes of kind "blocked" or tagged "blocked" /
//     "h-request" that are bound to an existing, non-terminal mission. Notes
//     bound only to a goal, unbound notes, and originals replaced by a
//     superseding note are excluded. Resolution = mission terminal.
//
// It reads p (already projected from the same snapshot) and the memory
// streams in all; it never writes to the journal.
func applyHumanAttention(s events.Port, all []events.Event, p *Projection) error {
	needs := map[string]bool{}
	for _, t := range p.Tasks {
		if t.Attention { // Attention ⇔ MissionWaitingHuman (Snapshot)
			needs[t.ID] = true
		}
	}
	for _, g := range p.Gates {
		if g.Source != "internal" || g.State != "pending" {
			continue
		}
		needs[g.ID] = true
		p.Attention = append(p.Attention, AttentionItem{Kind: attentionGatePending, RefID: g.ID, Cause: g.Name, MissionID: g.MissionID})
	}
	for _, r := range p.Requests {
		if r.State != "waiting" {
			continue
		}
		needs[r.ID] = true
		p.Attention = append(p.Attention, AttentionItem{Kind: attentionRequestWaiting, RefID: r.ID, Cause: r.Name, MissionID: r.MissionID})
	}
	notes, err := blockedNotes(s, all)
	if err != nil {
		return err
	}
	for _, n := range notes {
		needs[n.ID] = true
		p.Attention = append(p.Attention, AttentionItem{Kind: attentionNoteBlocked, RefID: n.ID, Cause: noteCause(n.Content), MissionID: n.MissionID})
	}
	p.Counts.NeedsYou = len(needs)
	return nil
}

// blockedNotes returns the note_blocked candidates ordered by note id.
func blockedNotes(s events.Port, all []events.Event) ([]memory.Memory, error) {
	streams := map[string][]events.Event{}
	for _, e := range all {
		if e.AggregateType == "memory" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	if len(streams) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	mems := make([]memory.Memory, 0, len(ids))
	superseded := map[string]bool{}
	for _, id := range ids {
		m, err := memory.Replay(streams[id])
		if err != nil {
			// A note stream this binary cannot replay (corrupt, or a kind
			// from a newer binary) is left out of attention only; it must not
			// take /v1/workspace down — /v1/knowledge still reports it.
			continue
		}
		mems = append(mems, m)
		if m.Supersedes != "" {
			superseded[m.Supersedes] = true
		}
	}
	open := map[string]bool{}
	checked := map[string]bool{}
	var out []memory.Memory
	for _, m := range mems {
		if superseded[m.ID] || m.MissionID == "" || !isBlockedNote(m) {
			continue
		}
		if !checked[m.MissionID] {
			checked[m.MissionID] = true
			if log := s.List("mission", m.MissionID); len(log) > 0 {
				if ms, err := projector.ReplayMission(log); err == nil {
					switch ms.State {
					case domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled:
					default:
						open[m.MissionID] = true
					}
				}
			}
		}
		if open[m.MissionID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func isBlockedNote(m memory.Memory) bool {
	if m.Kind == memory.Blocked {
		return true
	}
	for _, t := range m.Tags {
		if t == "blocked" || t == "h-request" {
			return true
		}
	}
	return false
}

// noteCause is the first non-blank line of content with whitespace runs
// collapsed, cut to noteCauseMaxRunes runes.
func noteCause(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > noteCauseMaxRunes {
			line = string(r[:noteCauseMaxRunes])
		}
		return line
	}
	return ""
}
