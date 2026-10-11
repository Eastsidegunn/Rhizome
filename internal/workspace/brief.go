package workspace

// RHZ-146 (FR-RHZ-190): GET /v1/ops/brief — the one-call boot summary a new
// ops session reads to learn "what is going on now". It is a rule-only
// rendering of the journal and the workspace projection (no LLM, no wall
// clock except the server's injectable Clock): the same journal and the same
// now give the same bytes. Sections come in a fixed order, each capped at a
// fixed number of lines; what does not fit is counted as "…외 N건". Every
// time is an absolute KST instant, never a relative "2시간 전".

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/memory"
	"rhizome/internal/schedule"
	"rhizome/internal/session"
)

// briefLineMaxRunes caps one rendered item; with the section caps below it
// keeps the whole brief near 1500 tokens.
const briefLineMaxRunes = 110

// Section keys in render order, with their line caps.
var briefSections = []struct {
	Key, Title string
	Cap        int
}{
	{"waiting_h", "H 대기", 10},
	{"running", "실행 중·막힘", 8},
	{"recent_runs", "최근 끝난 실행", 5},
	{"next", "다음", 6},
	{"due", "일정·마감 7일", 6},
	{"decisions", "최근 결정 48시간", 5},
	{"threads", "열린 실타래", 6},
	{"release", "최근 배포", 1},
}

// attentionKindOrder fixes the grouping order of section ①.
var attentionKindOrder = map[string]int{"waiting_for_human": 0, attentionGatePending: 1, attentionRequestWaiting: 2, attentionNoteBlocked: 3, attentionSessionStale: 4}

// BriefSection is one rendered section: Total items exist, Items holds at
// most the section cap, Omitted = Total - len(Items).
type BriefSection struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Total   int      `json:"total"`
	Items   []string `json:"items"`
	Omitted int      `json:"omitted"`
}

// Brief is the whole summary at one revision and one now.
type Brief struct {
	Revision uint64         `json:"revision"`
	Now      string         `json:"now"`
	Sections []BriefSection `json:"sections"`
}

var errBriefFormat = errors.New("invalid brief query: want ?format=md|json")

// briefItem is one candidate line, already in section order.
type briefItem struct{ text string }

// streamFacts is the first/last journal position and time of one aggregate.
type streamFacts struct {
	firstSeq, lastSeq uint64
	firstAt, lastAt   time.Time
}

// BuildBrief derives the brief from the journal and its projection at now.
func BuildBrief(s events.Port, p Projection, now time.Time) Brief {
	loc, _ := schedule.Location(schedule.DefaultTZ)
	kst := func(t time.Time) string {
		if t.IsZero() || t.Unix() <= 0 {
			return "시각 없음"
		}
		return t.In(loc).Format("2006-01-02 15:04 KST")
	}
	all := s.All()
	facts := map[aggKey]*streamFacts{}
	byID := map[string]*streamFacts{} // first stream seen under an id, any type
	for _, e := range all {
		k := aggKey{e.AggregateType, e.AggregateID}
		f := facts[k]
		if f == nil {
			f = &streamFacts{firstSeq: e.Sequence, firstAt: e.CreatedAt}
			facts[k] = f
			if byID[e.AggregateID] == nil {
				byID[e.AggregateID] = f
			}
		}
		f.lastSeq, f.lastAt = e.Sequence, e.CreatedAt
	}
	firstOf := func(id string) (uint64, time.Time) {
		if f := byID[id]; f != nil {
			return f.firstSeq, f.firstAt
		}
		return 0, time.Time{}
	}
	mh := func(id string) string { return orID(p.handles.of("m", id), id) }
	notes := buildNoteIndex(all)
	b := Brief{Revision: p.Revision, Now: kst(now)}
	sections := map[string][]briefItem{}
	headers := map[string]string{}

	// ① H 대기: grouped by kind (fixed order), oldest first within a kind.
	att := append([]AttentionItem(nil), p.Attention...)
	sort.SliceStable(att, func(i, j int) bool {
		ki, kj := attentionKindOrder[att[i].Kind], attentionKindOrder[att[j].Kind]
		if ki != kj {
			return ki < kj
		}
		si, _ := firstOf(att[i].RefID)
		sj, _ := firstOf(att[j].RefID)
		if si != sj {
			return si < sj
		}
		return att[i].RefID < att[j].RefID
	})
	kindCount := map[string]int{}
	var kinds []string
	for _, a := range att {
		if kindCount[a.Kind] == 0 {
			kinds = append(kinds, a.Kind)
		}
		kindCount[a.Kind]++
		_, at := firstOf(a.RefID)
		line := fmt.Sprintf("[%s] %s %s", a.Kind, refHandle(p, a.Kind, a.RefID), a.Cause)
		if a.MissionID != "" && a.Kind != "waiting_for_human" {
			line += " · " + mh(a.MissionID)
		}
		sections["waiting_h"] = append(sections["waiting_h"], briefItem{line + " · " + kst(at)})
	}
	var parts []string
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", k, kindCount[k]))
	}
	headers["waiting_h"] = strings.Join(parts, " · ")

	// ② 실행 중·막힘: live missions with their open executions and sessions,
	// then the five most recently ended executions.
	execs := replayExecutions(s, facts)
	openExec := map[string][]execution.Ref{}
	for _, x := range execs {
		if !execTerminal(x.ref.State) {
			openExec[x.ref.MissionID] = append(openExec[x.ref.MissionID], x.ref)
		}
	}
	openSess := map[string][]Session{}
	for _, x := range p.Sessions {
		if x.State == session.Open && x.MissionID != "" {
			openSess[x.MissionID] = append(openSess[x.MissionID], x)
		}
	}
	tasks := append([]Task(nil), p.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool {
		si, _ := firstOf(tasks[i].ID)
		sj, _ := firstOf(tasks[j].ID)
		if si != sj {
			return si < sj
		}
		return tasks[i].ID < tasks[j].ID
	})
	for _, t := range tasks {
		switch t.State {
		case "running", "waiting", "blocked", "paused":
		default:
			continue
		}
		line := fmt.Sprintf("%s [%s] %s", mh(t.ID), t.State, t.Name)
		if t.BlockedReason != "" {
			line += " · 막힘: " + t.BlockedReason
		} else if t.CurrentAction != "" {
			line += " · " + t.CurrentAction
		}
		for _, r := range openExec[t.ID] {
			line += fmt.Sprintf(" · exec %s %s", r.ID, r.State)
		}
		for _, x := range openSess[t.ID] {
			line += fmt.Sprintf(" · session %s %s", x.SessionID, x.Mode)
		}
		sections["running"] = append(sections["running"], briefItem{line})
	}
	cost := usageCosts(notes)
	ended := make([]execRecord, 0, len(execs))
	for _, x := range execs {
		if execTerminal(x.ref.State) {
			ended = append(ended, x)
		}
	}
	sort.SliceStable(ended, func(i, j int) bool { return ended[i].last > ended[j].last })
	for _, x := range ended {
		c := "비용 없음"
		if v, ok := cost[x.ref.ID]; ok {
			c = "$" + v
		}
		sections["recent_runs"] = append(sections["recent_runs"], briefItem{fmt.Sprintf("%s %s · %s · %s · %s", x.ref.ID, x.ref.State, mh(x.ref.MissionID), c, kst(x.lastAt))})
	}

	// ③ 다음: queued missions in creation order.
	for _, t := range tasks {
		if t.State == "queued" {
			sections["next"] = append(sections["next"], briefItem{fmt.Sprintf("%s %s", mh(t.ID), t.Name)})
		}
	}

	// ④ 일정·마감 7일: schedule occurrences and effective due times up to
	// now+7d (overdue deadlines included), earliest first.
	type dated struct {
		at   time.Time
		id   string
		text string
	}
	var due []dated
	horizon := now.Add(7 * 24 * time.Hour)
	next := map[string]time.Time{}
	for _, sc := range p.Schedules {
		at, ok := sc.sched.NextAt(now)
		if !ok {
			continue
		}
		next[sc.ID] = at
		if sc.State == schedule.Active && !at.After(horizon) {
			due = append(due, dated{at, sc.ID, fmt.Sprintf("%s 일정 %s (%s)", kst(at), sc.Name, sc.ID)})
		}
	}
	for _, t := range p.Tasks {
		if terminalTaskStates[t.State] {
			continue
		}
		if at := effectiveDue(t, next); !at.IsZero() && !at.After(horizon) {
			mark := "마감"
			if at.Before(now) {
				mark = "마감 지남"
			}
			due = append(due, dated{at, t.ID, fmt.Sprintf("%s %s %s %s", kst(at), mark, mh(t.ID), t.Name)})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].at.Equal(due[j].at) {
			return due[i].at.Before(due[j].at)
		}
		return due[i].id < due[j].id
	})
	for _, d := range due {
		sections["due"] = append(sections["due"], briefItem{d.text})
	}

	// ⑤ 최근 결정 48시간: live H-decision notes, newest first.
	live := make([]noteRecord, 0, len(notes.notes))
	for _, n := range notes.notes {
		if !notes.superseded[n.ID] {
			live = append(live, n)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].seq > live[j].seq })
	since := now.Add(-48 * time.Hour)
	for _, n := range live {
		if hasTag(n.Tags, "h-decision") && !n.at.Before(since) && !n.at.After(now) {
			sections["decisions"] = append(sections["decisions"], briefItem{fmt.Sprintf("%s %s · %s", kst(n.at), noteHandle(n.ID), noteCause(n.Content))})
		}
	}

	// ⑥ 열린 실타래: commitment/open-thread notes without a done re: them,
	// and question notes without an answer; oldest first.
	for i := len(live) - 1; i >= 0; i-- {
		n := live[i]
		var kind string
		switch {
		case (hasTag(n.Tags, "commitment") || hasTag(n.Tags, "open-thread")) && !notes.doneFor(n):
			kind = "약속"
		case n.Kind == memory.Question && !notes.answered(n.ID):
			kind = "질문"
		default:
			continue
		}
		line := fmt.Sprintf("[%s] %s %s", kind, noteHandle(n.ID), noteCause(n.Content))
		if n.MissionID != "" {
			line += " · " + mh(n.MissionID)
		}
		sections["threads"] = append(sections["threads"], briefItem{line + " · " + kst(n.at)})
	}

	// ⑦ 최근 배포: the latest live release note.
	for _, n := range live {
		if n.Kind == memory.Release || hasTag(n.Tags, "release") {
			sections["release"] = append(sections["release"], briefItem{fmt.Sprintf("%s %s · %s", kst(n.at), noteHandle(n.ID), noteCause(n.Content))})
		}
	}

	for _, sec := range briefSections {
		items := sections[sec.Key]
		out := BriefSection{Key: sec.Key, Title: sec.Title, Total: len(items), Items: []string{}}
		if headers[sec.Key] != "" {
			out.Title += " — " + headers[sec.Key]
		}
		for i, it := range items {
			if i == sec.Cap {
				break
			}
			out.Items = append(out.Items, briefLine(it.text))
		}
		out.Omitted = out.Total - len(out.Items)
		b.Sections = append(b.Sections, out)
	}
	return b
}

// Markdown renders the brief; an empty section says "없음".
func (b Brief) Markdown() []byte {
	var w strings.Builder
	fmt.Fprintf(&w, "# Rhizome ops brief · %s · rev %d\n", b.Now, b.Revision)
	for i, sec := range b.Sections {
		fmt.Fprintf(&w, "\n## %d. %s (%d)\n", i+1, sec.Title, sec.Total)
		if sec.Total == 0 {
			w.WriteString("- 없음\n")
			continue
		}
		for _, it := range sec.Items {
			w.WriteString("- " + it + "\n")
		}
		if sec.Omitted > 0 {
			fmt.Fprintf(&w, "- …외 %d건\n", sec.Omitted)
		}
	}
	return []byte(w.String())
}

// serveOpsBrief answers GET /v1/ops/brief. The only query is an optional
// single ?format=md|json (default md); anything else is 400. GET never writes.
func (h *HTTPServer) serveOpsBrief(w http.ResponseWriter, r *http.Request) {
	format := "md"
	for k, v := range r.URL.Query() {
		if k != "format" || len(v) != 1 || (v[0] != "md" && v[0] != "json") {
			http.Error(w, errBriefFormat.Error(), http.StatusBadRequest)
			return
		}
		format = v[0]
	}
	p, e := Snapshot(h.Store, h.Trust)
	if e != nil {
		http.Error(w, e.Error(), 500)
		return
	}
	b := BuildBrief(h.Store, p, h.now())
	w.Header().Set("Cache-Control", "no-store")
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(b.Markdown())
}

// execRecord is one replayed execution with its last journal position.
type execRecord struct {
	ref    execution.Ref
	last   uint64
	lastAt time.Time
}

// replayExecutions replays every execution stream in id order; a stream
// this binary cannot replay is left out (the brief must not go down).
func replayExecutions(s events.Port, facts map[aggKey]*streamFacts) []execRecord {
	var ids []string
	for k := range facts {
		if k.typ == "execution" {
			ids = append(ids, k.id)
		}
	}
	sort.Strings(ids)
	var out []execRecord
	for _, id := range ids {
		r, err := execution.Replay(s.List("execution", id))
		if err != nil {
			continue
		}
		f := facts[aggKey{"execution", id}]
		out = append(out, execRecord{ref: r, last: f.lastSeq, lastAt: f.lastAt})
	}
	return out
}

func execTerminal(st execution.State) bool {
	switch st {
	case execution.Succeeded, execution.Failed, execution.Cancelled, execution.Unknown:
		return true
	}
	return false
}

// usageCosts maps execution id → totalCostUsd from the session launcher's
// usage notes (the JSON block of NoteContent); unparseable notes are skipped.
func usageCosts(notes noteIndex) map[string]string {
	out := map[string]string{}
	for _, n := range notes.notes {
		if !hasTag(n.Tags, "usage") {
			continue
		}
		i, j := strings.Index(n.Content, "{"), strings.LastIndex(n.Content, "}")
		if i < 0 || j < i {
			continue
		}
		var body struct {
			ExecutionID  string       `json:"executionId"`
			TotalCostUSD *json.Number `json:"totalCostUsd"`
		}
		if json.Unmarshal([]byte(n.Content[i:j+1]), &body) != nil || body.ExecutionID == "" || body.TotalCostUSD == nil {
			continue
		}
		out[body.ExecutionID] = body.TotalCostUSD.String()
	}
	return out
}

// doneFor reports whether a live done note closes the thread n: one tagged
// re:<n's id>, or one sharing a re:<target> tag with n. Done notes cannot use
// the relay's re field, so they carry the re: tag in tags.
func (idx noteIndex) doneFor(n noteRecord) bool {
	targets := []string{n.ID}
	for _, t := range n.Tags {
		if strings.HasPrefix(t, rePrefix) && len(t) > len(rePrefix) {
			targets = append(targets, t[len(rePrefix):])
		}
	}
	for _, target := range targets {
		for _, x := range idx.byRe[target] {
			if x.Kind == memory.Done {
				return true
			}
		}
	}
	return false
}

// hasTag matches a tag case-insensitively (H-decision = h-decision).
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return false
}

// refHandle is the short handle of an attention item's ref.
func refHandle(p Projection, kind, id string) string {
	switch kind {
	case "waiting_for_human":
		return orID(p.handles.of("m", id), id)
	case attentionGatePending:
		return orID(p.handles.of("q", id), id)
	case attentionRequestWaiting:
		return orID(p.handles.of("r", id), id)
	case attentionNoteBlocked:
		return noteHandle(id)
	}
	return id
}

// noteHandle shortens a content-addressed note id to note-<8 hex>.
func noteHandle(id string) string {
	if strings.HasPrefix(id, "note-") && len(id) > 13 {
		return id[:13]
	}
	return id
}

func orID(handle, id string) string {
	if handle == "" {
		return id
	}
	return handle
}

// briefLine collapses whitespace and cuts a line to briefLineMaxRunes.
func briefLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > briefLineMaxRunes {
		return string(r[:briefLineMaxRunes-1]) + "…"
	}
	return s
}
