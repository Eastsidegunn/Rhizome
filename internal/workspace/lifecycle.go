package workspace

// RHZ-149 (FR-RHZ-193): lifecycle management. Rhizome raises what needs a
// closing decision as attention cards; it changes no state by itself except
// the opt-in auto-complete (AutoCompleteMission, called by the composition
// root after a succeeded launcher run). Everything here is recomputed from
// the journal plus an injected now — reading the workspace never writes.
//
//   - mission_done_review: a non-terminal mission with a live done note. One
//     human card per mission (refId = mission id): the done note's first line,
//     the latest usage cost and the latest deliverable sourceRef.
//   - stale: running/waiting missions whose lastActivityTs is older than
//     staleDays, and blocked notes (note_blocked) created that long ago, as
//     entries of the single kind "stale" (refId = the mission / note, so each
//     entry hangs on its node). A live keep-alive note (tag keep-alive, bound
//     to the mission or tagged re:<target>) younger than staleDays hides it.
//     Decisions go through existing intents (mission.complete / cancel /
//     task.resume); "keep" = that note.
//   - archived: a deliverable of a terminal mission carries archived:true.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/trust"
)

const (
	attentionMissionDoneReview = "mission_done_review"
	attentionStale             = "stale"
	// DefaultStaleDays is staleDays when the ledger omits it.
	DefaultStaleDays = 7
	// keepAliveTag marks a "keep" decision on a stale card.
	keepAliveTag = "keep-alive"
	// AutoCompleteActor / AutoCompleteReason are journaled on the opt-in
	// auto-complete (correlation relay:<actor>, decision reason).
	AutoCompleteActor  = "system:lifecycle"
	AutoCompleteReason = "auto: done+succeeded"
)

// lifecycleFacts are the journal facts the now-dependent stale card needs,
// derived in Snapshot and consumed by withLifecycle; never serialized.
type lifecycleFacts struct {
	// needs is the distinct RefID set behind counts.needsYou.
	needs map[string]bool
	// blockedAt is each note_blocked note's creation time.
	blockedAt map[string]time.Time
	// keepAlive is the latest live keep-alive note time per target id.
	keepAlive map[string]time.Time
}

// applyLifecycleNotes appends mission_done_review items (adding their refs
// to needs) and records the stale-card facts on p.
func applyLifecycleNotes(notes noteIndex, needs map[string]bool, p *Projection) {
	open := map[string]Task{}
	for _, t := range p.Tasks {
		switch t.State {
		case "completed", "failed", "cancelled":
		default:
			open[t.ID] = t
		}
	}
	type doneRec struct {
		line string
		seq  uint64
	}
	done := map[string]doneRec{}
	cost := map[string]struct {
		usd string
		seq uint64
	}{}
	keep := map[string]time.Time{}
	for _, n := range notes.notes {
		if notes.superseded[n.ID] {
			continue
		}
		if n.MissionID != "" && n.Kind == memory.Done {
			if d, ok := done[n.MissionID]; !ok || n.seq > d.seq {
				done[n.MissionID] = doneRec{noteCause(n.Content), n.seq}
			}
		}
		if n.MissionID != "" && hasTag(n.Tags, "usage") {
			if usd := usageCostOf(n.Content); usd != "" && n.seq >= cost[n.MissionID].seq {
				cost[n.MissionID] = struct {
					usd string
					seq uint64
				}{usd, n.seq}
			}
		}
		if hasTag(n.Tags, keepAliveTag) {
			targets := []string{}
			for _, t := range n.Tags {
				if strings.HasPrefix(t, rePrefix) && len(t) > len(rePrefix) {
					targets = append(targets, t[len(rePrefix):])
				}
			}
			if len(targets) == 0 && n.MissionID != "" {
				targets = append(targets, n.MissionID)
			}
			for _, id := range targets {
				if n.at.After(keep[id]) {
					keep[id] = n.at
				}
			}
		}
	}
	link := map[string]string{}
	seqOf := map[string]uint64{}
	for _, d := range p.Deliverables {
		if d.MissionID == "" || d.SourceRef == "" {
			continue
		}
		if s := p.deliverableSeq[d.ID]; link[d.MissionID] == "" || s >= seqOf[d.MissionID] {
			link[d.MissionID], seqOf[d.MissionID] = d.SourceRef, s
		}
	}
	ids := make([]string, 0, len(done))
	for id := range done {
		if _, ok := open[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		cause := done[id].line
		if c := cost[id].usd; c != "" {
			cause += " · $" + c
		}
		needs[id] = true
		p.Attention = append(p.Attention, AttentionItem{Kind: attentionMissionDoneReview, RefID: id, Cause: cause, SourceRef: link[id], MissionID: id})
	}
	blockedAt := map[string]time.Time{}
	for _, a := range p.Attention {
		if a.Kind == attentionNoteBlocked {
			blockedAt[a.RefID] = time.Time{}
		}
	}
	for _, n := range notes.notes {
		if _, ok := blockedAt[n.ID]; ok {
			blockedAt[n.ID] = n.at
		}
	}
	p.lifecycle = lifecycleFacts{needs: needs, blockedAt: blockedAt, keepAlive: keep}
}

// usageCostOf is totalCostUsd of a launcher usage note ("" when absent).
func usageCostOf(content string) string {
	i, j := strings.Index(content, "{"), strings.LastIndex(content, "}")
	if i < 0 || j < i {
		return ""
	}
	var body struct {
		TotalCostUSD *json.Number `json:"totalCostUsd"`
	}
	if json.Unmarshal([]byte(content[i:j+1]), &body) != nil || body.TotalCostUSD == nil {
		return ""
	}
	return body.TotalCostUSD.String()
}

// withLifecycle returns p with the stale entries at now added to its
// attention and counts.needsYou. staleDays <= 0 = DefaultStaleDays. p's own
// slices and maps are never mutated (projections are shared with SSE).
func withLifecycle(p Projection, now time.Time, staleDays int) Projection {
	if staleDays <= 0 {
		staleDays = DefaultStaleDays
	}
	window := time.Duration(staleDays) * 24 * time.Hour
	cutoff := now.Add(-window)
	lf := p.lifecycle
	kept := func(id string) bool {
		at, ok := lf.keepAlive[id]
		return ok && at.After(cutoff)
	}
	days := func(at time.Time) int { return int(now.Sub(at) / (24 * time.Hour)) }
	var add []AttentionItem
	for _, t := range p.Tasks {
		if (t.State != "running" && t.State != "waiting") || t.LastActivityTs <= 0 {
			continue
		}
		at := time.UnixMilli(t.LastActivityTs)
		if !at.Before(cutoff) || kept(t.ID) {
			continue
		}
		add = append(add, AttentionItem{Kind: attentionStale, RefID: t.ID, Cause: fmt.Sprintf("%d일 멈춤: %s", days(at), t.Name), MissionID: t.ID})
	}
	for _, a := range p.Attention {
		if a.Kind != attentionNoteBlocked {
			continue
		}
		at := lf.blockedAt[a.RefID]
		if at.IsZero() || !at.Before(cutoff) || kept(a.RefID) {
			continue
		}
		add = append(add, AttentionItem{Kind: attentionStale, RefID: a.RefID, Cause: fmt.Sprintf("%d일 막힘: %s", days(at), a.Cause), MissionID: a.MissionID})
	}
	if len(add) == 0 {
		return p
	}
	sort.Slice(add, func(i, j int) bool { return add[i].RefID < add[j].RefID })
	att := make([]AttentionItem, 0, len(p.Attention)+len(add))
	for _, a := range p.Attention {
		if a.Kind > attentionStale && len(add) > 0 {
			att, add = append(att, add...), nil
		}
		att = append(att, a)
	}
	att = append(att, add...)
	needs := len(lf.needs)
	for _, a := range att {
		if a.Kind == attentionStale && !lf.needs[a.RefID] {
			needs++
		}
	}
	p.Attention, p.Counts.NeedsYou = att, needs
	return p
}

// archivedMissions is the set of terminal mission ids whose deliverables
// carry archived:true.
func archivedMissions(p Projection) map[string]bool {
	out := map[string]bool{}
	for _, t := range p.Tasks {
		switch t.State {
		case "completed", "failed", "cancelled":
			out[t.ID] = true
		}
	}
	return out
}

// AutoCompleteMission is the opt-in auto-complete (ledger
// autoCompleteOnDone): the composition root calls it after a launcher run of
// missionID ended succeeded. When the mission carries a live done note it
// relays mission.complete as AutoCompleteActor with AutoCompleteReason;
// otherwise it writes nothing. ok reports whether the mission was closed.
func AutoCompleteMission(s events.Port, missionID string) (ok bool, err error) {
	notes := buildNoteIndex(s.All())
	found := false
	for _, n := range notes.notes {
		if n.MissionID == missionID && n.Kind == memory.Done && !notes.superseded[n.ID] {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	res, err := RelayIntentHooks(s, Intent{Kind: "mission.complete", MissionID: missionID, Reason: AutoCompleteReason, CorrelationID: "relay:" + AutoCompleteActor}, AutoCompleteActor, trust.Authority{}, RelayHooks{})
	if err != nil {
		return false, err
	}
	if !res.Accepted {
		return false, fmt.Errorf("auto-complete rejected: %s", res.Reason)
	}
	return true, nil
}
