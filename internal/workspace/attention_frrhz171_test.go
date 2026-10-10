package workspace

// RHZ-132 (FR-RHZ-171): human-owned attention kinds gate_pending,
// request_waiting and note_blocked, deduplicated counts.needsYou, and the
// coordination memory kinds. T1–T7 of the approved test plan.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/memory"
)

type attention132 struct {
	Kind      string `json:"kind"`
	RefID     string `json:"refId"`
	Cause     string `json:"cause"`
	MissionID string `json:"missionId"`
}

func wire132(t *testing.T, s events.Port) (needsYou int, items []attention132) {
	t.Helper()
	var env struct {
		Body struct {
			Counts    struct{ NeedsYou int }
			Attention []attention132
		}
	}
	raw := getWorkspace071(t, s)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	return env.Body.Counts.NeedsYou, env.Body.Attention
}

func ofKind132(items []attention132, kind string) []attention132 {
	var out []attention132
	for _, a := range items {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func relay132(t *testing.T, s events.Port, in Intent) {
	t.Helper()
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("%s: %v %+v", in.Kind, err, res)
	}
}

// noteID132 mirrors the note.create relay id: "note-" + sha256(content).
func noteID132(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "note-" + hex.EncodeToString(sum[:])
}

func blockedNote132(t *testing.T, s events.Port, missionID, goalID, kind string, tags []string, content string) string {
	t.Helper()
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: kind, MissionID: missionID, GoalID: goalID, Tags: tags, Content: content})
	return noteID132(content)
}

// T1: gate_pending appears for a pending internal question and is removed by
// approve or reject; needsYou follows.
func TestGatePendingAttentionLifecycleFRRHZ171(t *testing.T) {
	for _, kind := range []string{"gate.approve", "gate.reject"} {
		s := store078(t)
		id, digest := askBound078(t, s, "t1-"+kind)
		ny, items := wire132(t, s)
		got := ofKind132(items, "gate_pending")
		if ny != 1 || len(got) != 1 || got[0].RefID != id || got[0].Cause != "t1-"+kind || got[0].MissionID != "mission-078" {
			t.Fatalf("%s: pending needsYou=%d items=%+v", kind, ny, items)
		}
		relay132(t, s, Intent{Kind: kind, GateID: id, Digest: digest, Reason: "decided"})
		if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
			t.Fatalf("%s: after decision needsYou=%d items=%+v", kind, ny, items)
		}
	}
}

// T2: request_waiting appears for a waiting request and is removed by
// request.complete; a request closed as unable is never counted (D1).
func TestRequestWaitingAttentionLifecycleFRRHZ171(t *testing.T) {
	for _, close := range []Intent{{Kind: "request.complete", Memo: "done"}, {Kind: "request.unable", Reason: "cannot"}} {
		s := requestWorkspaceFixture(t)
		relay132(t, s, createRequestIntent())
		p, err := Snapshot(s)
		if err != nil || len(p.Requests) != 1 {
			t.Fatalf("snapshot %v %+v", err, p.Requests)
		}
		rid := p.Requests[0].ID
		ny, items := wire132(t, s)
		got := ofKind132(items, "request_waiting")
		if ny != 1 || len(items) != 1 || len(got) != 1 || got[0].RefID != rid || got[0].Cause != "Turn the key" || got[0].MissionID != "mission-request" {
			t.Fatalf("waiting needsYou=%d items=%+v", ny, items)
		}
		close.RequestID = rid
		relay132(t, s, close)
		if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
			t.Fatalf("%s: after close needsYou=%d items=%+v", close.Kind, ny, items)
		}
	}
}

// T3: note_blocked for a blocked/h-request note on an open mission; removed
// when the mission terminates (complete, fail, cancel).
func TestNoteBlockedAttentionLifecycleFRRHZ171(t *testing.T) {
	cases := []struct {
		close string
		path  []domain.MissionState
		kind  string
		tags  []string
	}{
		{"mission.complete", []domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionBlocked}, "blocked", nil},
		{"mission.fail", []domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionBlocked}, "observation", []string{"h-request"}},
		{"mission.cancel", []domain.MissionState{domain.MissionReady}, "fact", []string{"x", "blocked"}},
	}
	for _, c := range cases {
		s := &events.Store{}
		missionIn062(t, s, "mission-132", c.path...)
		content := "\n   needs   the prod   key  \nsecond line " + c.close
		id := blockedNote132(t, s, "mission-132", "", c.kind, c.tags, content)
		ny, items := wire132(t, s)
		got := ofKind132(items, "note_blocked")
		if ny != 1 || len(items) != 1 || len(got) != 1 || got[0].RefID != id || got[0].Cause != "needs the prod key" || got[0].MissionID != "mission-132" {
			t.Fatalf("%s: blocked needsYou=%d items=%+v", c.close, ny, items)
		}
		relay132(t, s, Intent{Kind: c.close, MissionID: "mission-132", Reason: "closed"})
		if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
			t.Fatalf("%s: after terminal needsYou=%d items=%+v", c.close, ny, items)
		}
	}
}

// T3b: the cause is cut to 200 runes.
func TestNoteBlockedCauseCutFRRHZ171(t *testing.T) {
	long := strings.Repeat("가", 250)
	if got := noteCause("\n\t \n" + long + "\nrest"); got != strings.Repeat("가", 200) {
		t.Fatalf("cause runes=%d", len([]rune(got)))
	}
	if got := noteCause("  a \t b  "); got != "a b" {
		t.Fatalf("cause %q", got)
	}
}

// T4: WaitingHuman mission + gate + request + note all counted once each,
// deduplicated by RefID, attention sorted by (kind, refId).
func TestNeedsYouMixedDedupeFRRHZ171(t *testing.T) {
	s := store078(t)
	missionIn062(t, s, "mission-132w", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	askBound078(t, s, "t4")
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "note-t4", Kind: memory.Blocked, Content: "stuck", SourceType: "test", SourceID: "t4", Confidence: 1, MissionID: "mission-078"}); err != nil {
		t.Fatal(err)
	}
	// Same note tagged twice still yields one item.
	blockedNote132(t, s, "mission-132w", "", "blocked", []string{"blocked", "h-request"}, "waiting on human")
	in := createRequestIntent()
	in.MissionID = "mission-078"
	relay132(t, s, in)
	ny, items := wire132(t, s)
	if ny != 5 || len(items) != 5 {
		t.Fatalf("needsYou=%d items=%+v, want 5/5", ny, items)
	}
	for i := 1; i < len(items); i++ {
		a, b := items[i-1], items[i]
		if a.Kind > b.Kind || a.Kind == b.Kind && a.RefID >= b.RefID {
			t.Fatalf("attention not sorted: %+v", items)
		}
	}
	// Dedupe by RefID directly: a refId already counted is not counted twice.
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	p.Requests = append(p.Requests, p.Requests...)
	p.Attention = nil
	if err := applyHumanAttention(s, s.All(), &p); err != nil || p.Counts.NeedsYou != 5 {
		t.Fatalf("dedupe needsYou=%d err=%v", p.Counts.NeedsYou, err)
	}
}

// T5: D1 unable (covered in T2), D2 JANUS pending gates are not counted,
// D3 goal-only, mission-less, terminal-mission and superseded notes are not
// counted; non-blocked notes on open missions are not counted.
func TestNeedsYouExclusionsFRRHZ171(t *testing.T) {
	s, _ := pendingGateFixture(t)
	if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
		t.Fatalf("JANUS pending: needsYou=%d items=%+v", ny, items)
	}
	blockedNote132(t, s, "", "g", "blocked", []string{"h-request"}, "goal only")
	blockedNote132(t, s, "", "", "blocked", nil, "unbound")
	blockedNote132(t, s, "m", "", "observation", []string{"other"}, "not blocked")
	missionIn062(t, s, "mission-done", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	blockedNote132(t, s, "mission-done", "", "blocked", nil, "on done mission")
	relay132(t, s, Intent{Kind: "mission.complete", MissionID: "mission-done"})
	// A superseded original no longer counts; its replacement is judged alone.
	mem := memory.Service{Store: s}
	orig, err := mem.Create(memory.Memory{ID: "note-orig", Kind: memory.Blocked, Content: "old", SourceType: "test", SourceID: "o", Confidence: 1, MissionID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Supersede(orig, memory.Memory{ID: "note-new", Kind: memory.Done, Content: "resolved", SourceType: "test", SourceID: "o", Confidence: 1, MissionID: "m"}); err != nil {
		t.Fatal(err)
	}
	if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
		t.Fatalf("exclusions: needsYou=%d items=%+v", ny, items)
	}
}

// T6: the six coordination memory kinds are accepted by note.create; existing
// kinds unchanged; unknown kinds still rejected with zero writes.
func TestCoordinationMemoryKindsFRRHZ171(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-k", domain.MissionReady)
	for _, k := range []string{"handoff", "blocked", "done", "usage", "release", "answer", "fact", "decision", "preference", "observation", "hypothesis", "reference"} {
		relay132(t, s, Intent{Kind: "note.create", MemoryKind: k, MissionID: "mission-k", Content: "kind " + k})
		m, err := memory.Replay(s.List("memory", noteID132("kind "+k)))
		if err != nil || string(m.Kind) != k {
			t.Fatalf("%s: %v %+v", k, err, m)
		}
	}
	for _, k := range []string{"blockers", "Answer", "todo"} {
		n := len(s.All())
		res, err := RelayIntent(s, Intent{Kind: "note.create", MemoryKind: k, Content: "bad " + k}, "tester", noAuthority())
		if err != nil || res.Accepted || !strings.Contains(res.Reason, "unknown memory kind") || len(s.All()) != n {
			t.Fatalf("%s: %+v %v writes=%d", k, res, err, len(s.All())-n)
		}
	}
	if _, err := (memory.Service{Store: &events.Store{}}).Create(memory.Memory{ID: "x", Kind: "nope", Content: "c", SourceType: "t", SourceID: "s"}); err == nil {
		t.Fatal("memory service accepted unknown kind")
	}
}

// T7: re-projection is byte-identical and projection writes nothing.
func TestHumanAttentionDeterministicReadOnlyFRRHZ171(t *testing.T) {
	s := store078(t)
	missionIn062(t, s, "mission-132w", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	askBound078(t, s, "t7")
	blockedNote132(t, s, "mission-078", "", "blocked", nil, "t7 blocked")
	in := createRequestIntent()
	in.MissionID = "mission-078"
	relay132(t, s, in)
	before, n := journalBytes075(t, s), len(s.All())
	w1, w2 := getWorkspace071(t, s), getWorkspace071(t, s)
	if !bytes.Equal(w1, w2) {
		t.Fatalf("wire diverged:\n%s\n%s", w1, w2)
	}
	p1, err1 := Snapshot(s)
	p2, err2 := Snapshot(s)
	if err1 != nil || err2 != nil || p1.Counts != p2.Counts || p1.Counts.NeedsYou != 4 || len(p1.Attention) != 4 {
		t.Fatalf("snapshots %+v %+v %v %v", p1.Counts, p2.Counts, err1, err2)
	}
	if len(s.All()) != n || !bytes.Equal(journalBytes075(t, s), before) {
		t.Fatalf("journal changed by read")
	}
}

// Review follow-up: a memory stream this binary cannot replay (here an unknown
// kind, as a newer binary might write) is left out of attention only; the
// workspace still serves 200 and the readable blocked note still counts.
func TestUnreplayableMemoryDoesNotBreakWorkspaceFRRHZ171(t *testing.T) {
	s := store078(t)
	res, err := RelayIntent(s, Intent{Kind: "note.create", MemoryKind: "blocked", MissionID: "mission-078", Content: "readable block"}, "dev", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("note.create: %v %+v", err, res)
	}
	bad := events.Event{AggregateType: "memory", AggregateID: "note-future", Revision: 1, Type: "memory.created", Payload: json.RawMessage(`{"id":"note-future","kind":"future-kind","content":"x","missionId":"mission-078","tags":["blocked"]}`)}
	if err := s.Append(0, bad); err != nil {
		t.Fatalf("append: %v", err)
	}
	ny, items := wire132(t, s)
	if ny != 1 || len(items) != 1 || items[0].Kind != "note_blocked" || items[0].Cause != "readable block" {
		t.Fatalf("needsYou=%d attention=%+v, want the readable note only", ny, items)
	}
}
