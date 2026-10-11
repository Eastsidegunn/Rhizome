package workspace

import (
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
	// attentionSessionStale (RHZ-139, FR-RHZ-183): an open session of a
	// terminal mission — a human cleanup card.
	attentionSessionStale = "session_stale"
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
//     changes_requested hands the ball back to the worker. A gate bound to a
//     terminal mission (completed/failed/cancelled) is left out (RHZ-135,
//     FR-RHZ-177): the gate itself stays pending, only attention drops it.
//   - request_waiting: requests in state waiting only; done/unable are closed.
//   - note_blocked: memory notes of kind "blocked" or tagged "blocked" /
//     "h-request" that are bound to an existing, non-terminal mission. Notes
//     bound only to a goal, unbound notes, and originals replaced by a
//     superseding note are excluded. Resolution = mission terminal OR an
//     answer re: the note (RHZ-134, FR-RHZ-175): a live (non-superseded)
//     memory note of kind answer tagged re:<blocked note id>.
//
// It reads p (already projected from the same snapshot) and the memory
// streams in all; it never writes to the journal.
func applyHumanAttention(s events.Port, all []events.Event, p *Projection) error {
	return applyHumanAttentionNotes(s, buildNoteIndex(all), p)
}

// applyHumanAttentionNotes is applyHumanAttention over memory streams already
// replayed (Snapshot shares one noteIndex with the gate threads).
func applyHumanAttentionNotes(s events.Port, notes noteIndex, p *Projection) error {
	needs := map[string]bool{}
	closed := map[string]bool{}
	for _, t := range p.Tasks {
		if t.Attention { // Attention ⇔ MissionWaitingHuman (Snapshot)
			needs[t.ID] = true
		}
		switch t.State {
		case "completed", "failed", "cancelled":
			closed[t.ID] = true
		}
	}
	for _, g := range p.Gates {
		if g.Source != "internal" || g.State != "pending" || closed[g.MissionID] {
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
	blocked, err := blockedNotes(s, notes)
	if err != nil {
		return err
	}
	for _, n := range blocked {
		needs[n.ID] = true
		p.Attention = append(p.Attention, AttentionItem{Kind: attentionNoteBlocked, RefID: n.ID, Cause: noteCause(n.Content), MissionID: n.MissionID})
	}
	for _, x := range p.Sessions {
		if x.Stale {
			needs[x.SessionID] = true
			p.Attention = append(p.Attention, AttentionItem{Kind: attentionSessionStale, RefID: x.SessionID, Cause: x.Name, MissionID: x.MissionID})
		}
	}
	p.Counts.NeedsYou = len(needs)
	return nil
}

// blockedNotes returns the note_blocked candidates ordered by note id.
// Unreplayable note streams are already left out of notes (buildNoteIndex).
func blockedNotes(s events.Port, notes noteIndex) ([]memory.Memory, error) {
	open := map[string]bool{}
	checked := map[string]bool{}
	var out []memory.Memory
	for _, n := range notes.notes {
		m := n.Memory
		if notes.superseded[m.ID] || m.MissionID == "" || !isBlockedNote(m) || notes.answered(m.ID) {
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
