package workspace

// RHZ-149 (FR-RHZ-193): lifecycle cards — done → mission_done_review, the
// opt-in auto-complete, the stale bundle with keep-alive, archived
// deliverables, and zero writes on read.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

func get193(t *testing.T, s events.Port, now time.Time, staleDays int) []byte {
	t.Helper()
	h := NewHTTP(s)
	h.Clock = func() time.Time { return now }
	h.StaleDays = staleDays
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	b, _ := io.ReadAll(rec.Body)
	return b
}

type wire193 struct {
	Body struct {
		Counts       struct{ NeedsYou int }
		Attention    []attention132
		Deliverables []struct {
			ID       string `json:"id"`
			Archived *bool  `json:"archived"`
		}
	}
}

func decode193(t *testing.T, b []byte) wire193 {
	t.Helper()
	var w wire193
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return w
}

// aged193 copies s into a new store with every envelope time moved back by d.
func aged193(t *testing.T, s *events.Store, d time.Duration) *events.Store {
	t.Helper()
	out := &events.Store{}
	for _, e := range s.All() {
		e.CreatedAt = e.CreatedAt.Add(-d)
		e.Sequence = 0
		if err := out.Append(e.Revision-1, e); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

const usage193 = "usage line\n```json\n{\"executionId\":\"exec-1\",\"totalCostUsd\":0.42}\n```"

// done note → one human card with the first line and the cost; terminal
// mission → card gone and its deliverable archived.
func TestDoneReviewCardFRRHZ193(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-193", domain.MissionReady, domain.MissionRunning)
	relay132(t, s, Intent{Kind: "deliverable.register", MissionID: "mission-193", DeliverableKind: "code", Summary: "patch 193"})
	now := time.Now()
	w := decode193(t, get193(t, s, now, 0))
	if len(ofKind132(w.Body.Attention, attentionMissionDoneReview)) != 0 || w.Body.Deliverables[0].Archived != nil {
		t.Fatalf("before done: %+v", w.Body)
	}
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "observation", MissionID: "mission-193", Tags: []string{"usage", "session-launcher"}, Content: usage193})
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "done", MissionID: "mission-193", Tags: []string{"done"}, Content: "결과 한 줄\n\n본문"})
	w = decode193(t, get193(t, s, now, 0))
	got := ofKind132(w.Body.Attention, attentionMissionDoneReview)
	if len(got) != 1 || got[0].RefID != "mission-193" || got[0].MissionID != "mission-193" || got[0].Cause != "결과 한 줄 · $0.42" || w.Body.Counts.NeedsYou != 1 {
		t.Fatalf("done card: needsYou=%d %+v", w.Body.Counts.NeedsYou, w.Body.Attention)
	}
	relay132(t, s, Intent{Kind: "mission.complete", MissionID: "mission-193"})
	w = decode193(t, get193(t, s, now, 0))
	if len(w.Body.Attention) != 0 || w.Body.Counts.NeedsYou != 0 {
		t.Fatalf("after complete: %+v", w.Body)
	}
	if a := w.Body.Deliverables[0].Archived; a == nil || !*a {
		t.Fatalf("deliverable of a terminal mission not archived: %+v", w.Body.Deliverables)
	}
}

// AutoCompleteMission writes nothing without a done note and closes the
// mission as system:lifecycle with the auto reason when one exists.
func TestAutoCompleteMissionFRRHZ193(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-193", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult)
	before := len(s.All())
	if ok, err := AutoCompleteMission(s, "mission-193"); ok || err != nil || len(s.All()) != before {
		t.Fatalf("no done note: ok=%v err=%v writes=%d", ok, err, len(s.All())-before)
	}
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "done", MissionID: "mission-193", Tags: []string{"done"}, Content: "끝"})
	if ok, err := AutoCompleteMission(s, "mission-193"); !ok || err != nil {
		t.Fatalf("auto-complete: ok=%v err=%v", ok, err)
	}
	log := s.List("mission", "mission-193")
	m, _ := projector.ReplayMission(log)
	last := log[len(log)-1]
	if m.State != domain.MissionSucceeded || last.CorrelationID != "relay:system:lifecycle" || !strings.Contains(string(last.Payload), `"Reason":"auto: done+succeeded"`) {
		t.Fatalf("state=%s corr=%q payload=%s", m.State, last.CorrelationID, last.Payload)
	}
	// Terminal: a second call is a rejection with zero writes.
	n := len(s.All())
	if ok, err := AutoCompleteMission(s, "mission-193"); ok || err == nil || len(s.All()) != n {
		t.Fatalf("second call: ok=%v err=%v", ok, err)
	}
}

// Old running mission and old blocked note → kind stale entries; a younger
// staleDays window or a keep-alive note hides them.
func TestStaleBundleKeepAliveFRRHZ193(t *testing.T) {
	src := &events.Store{}
	missionIn062(t, src, "mission-a", domain.MissionReady, domain.MissionRunning)
	missionIn062(t, src, "mission-b", domain.MissionReady, domain.MissionRunning)
	relay132(t, src, Intent{Kind: "note.create", MemoryKind: "blocked", MissionID: "mission-b", Tags: []string{"blocked"}, Content: "막힌 이유"})
	s := aged193(t, src, 10*24*time.Hour)
	missionIn062(t, s, "mission-fresh", domain.MissionReady, domain.MissionRunning)
	now := time.Now()
	w := decode193(t, get193(t, s, now, 0))
	stale := ofKind132(w.Body.Attention, attentionStale)
	note := noteID132("막힌 이유")
	if len(stale) != 3 || w.Body.Counts.NeedsYou != 3 { // a, b, note (shared with its note_blocked ref)
		t.Fatalf("stale: needsYou=%d %+v", w.Body.Counts.NeedsYou, w.Body.Attention)
	}
	refs := map[string]string{}
	for _, a := range stale {
		refs[a.RefID] = a.Cause
	}
	if !strings.HasPrefix(refs["mission-a"], "10일 멈춤") || !strings.HasPrefix(refs[note], "10일 막힘: 막힌 이유") || refs["mission-fresh"] != "" {
		t.Fatalf("stale causes: %+v", refs)
	}
	if w := decode193(t, get193(t, s, now, 30)); len(ofKind132(w.Body.Attention, attentionStale)) != 0 {
		t.Fatalf("staleDays=30 still stale: %+v", w.Body.Attention)
	}
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "observation", MissionID: "mission-a", Tags: []string{"keep-alive"}, Content: "유지 a"})
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "observation", MissionID: "mission-b", Tags: []string{"keep-alive", "re:" + note}, Content: "유지 note"})
	w = decode193(t, get193(t, s, now, 0))
	stale = ofKind132(w.Body.Attention, attentionStale)
	if len(stale) != 1 || stale[0].RefID != "mission-b" {
		t.Fatalf("keep-alive: %+v", stale)
	}
	// The keep-alive expires after another staleDays.
	if w := decode193(t, get193(t, s, now.Add(8*24*time.Hour), 0)); len(ofKind132(w.Body.Attention, attentionStale)) != 4 {
		t.Fatalf("expired keep-alive: %+v", w.Body.Attention)
	}
}

// Reading the workspace at the same state and now writes nothing and gives
// the same bytes.
func TestLifecycleReadWritesNothingFRRHZ193(t *testing.T) {
	src := &events.Store{}
	missionIn062(t, src, "mission-a", domain.MissionReady, domain.MissionRunning)
	relay132(t, src, Intent{Kind: "note.create", MemoryKind: "done", MissionID: "mission-a", Tags: []string{"done"}, Content: "끝"})
	s := aged193(t, src, 10*24*time.Hour)
	now := time.Now()
	n := len(s.All())
	a, b := get193(t, s, now, 0), get193(t, s, now, 0)
	if len(s.All()) != n || !bytes.Equal(a, b) {
		t.Fatalf("writes=%d equal=%v", len(s.All())-n, bytes.Equal(a, b))
	}
	if w := decode193(t, a); len(ofKind132(w.Body.Attention, attentionStale)) != 1 || len(ofKind132(w.Body.Attention, attentionMissionDoneReview)) != 1 {
		t.Fatalf("cards: %+v", w.Body.Attention)
	}
}
