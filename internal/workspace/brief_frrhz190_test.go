package workspace

// RHZ-146 (FR-RHZ-190): GET /v1/ops/brief — fixed section order, per-section
// caps with "…외 N건", byte determinism for the same journal and now, and the
// empty board.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/policy"
	"rhizome/internal/schedule"
)

func getBrief190(t *testing.T, s events.Port, now time.Time, query string) (int, string) {
	t.Helper()
	h := NewHTTP(s)
	h.Clock = func() time.Time { return now }
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ops/brief"+query, nil))
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, string(b)
}

var brief190Titles = []string{"H 대기", "실행 중·막힘", "최근 끝난 실행", "다음", "일정·마감 7일", "최근 결정 48시간", "열린 실타래", "최근 배포"}

// The empty board renders every section, in order, as "없음".
func TestOpsBriefEmptyBoardFRRHZ190(t *testing.T) {
	now := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)
	code, md := getBrief190(t, &events.Store{}, now, "")
	want := "# Rhizome ops brief · 2026-10-11 12:00 KST · rev 0\n"
	for i, title := range brief190Titles {
		want += fmt.Sprintf("\n## %d. %s (0)\n- 없음\n", i+1, title)
	}
	if code != 200 || md != want {
		t.Fatalf("empty brief %d:\n%s\nwant\n%s", code, md, want)
	}
	code, js := getBrief190(t, &events.Store{}, now, "?format=json")
	var b Brief
	if code != 200 || json.Unmarshal([]byte(js), &b) != nil || len(b.Sections) != len(brief190Titles) {
		t.Fatalf("empty json %d: %s", code, js)
	}
	for i, sec := range b.Sections {
		if sec.Title != brief190Titles[i] || sec.Total != 0 || sec.Items == nil || len(sec.Items) != 0 {
			t.Fatalf("section %d: %+v", i, sec)
		}
	}
	for _, q := range []string{"?format=html", "?format=md&format=json", "?x=1"} {
		if code, _ := getBrief190(t, &events.Store{}, now, q); code != 400 {
			t.Fatalf("%s: status %d, want 400", q, code)
		}
	}
}

func board190(t *testing.T) *events.Store {
	t.Helper()
	s := store078(t)
	for i := 0; i < 12; i++ {
		askBound078(t, s, fmt.Sprintf("gate %02d", i))
	}
	for i := 0; i < 8; i++ {
		missionIn062(t, s, fmt.Sprintf("mission-q%d", i))
	}
	note := func(kind, re string, tags []string, content string) string {
		relay132(t, s, Intent{Kind: "note.create", MemoryKind: kind, Re: re, Tags: tags, Content: content})
		return noteID132(content)
	}
	note("decision", "", []string{"H-decision"}, "결정: brief는 규칙으로만 만든다\n둘째 줄은 요약에서 빠진다")
	note("observation", "", []string{"commitment"}, "약속: 내일까지 README 절")
	closed := note("observation", "", []string{"open-thread"}, "실타래: 닫힐 것")
	note("done", "", []string{"re:" + closed}, "닫았다")
	// Sharing a re: target with a done note also closes a thread.
	note("observation", "", []string{"commitment", "re:mission-078"}, "약속: 미션 끝내기")
	note("done", "", []string{"re:mission-078"}, "미션 끝")
	note("question", closed, nil, "질문: 답 없는 것")
	answered := note("question", closed, nil, "질문: 답 있는 것")
	note("answer", answered, nil, "답")
	note("release", "", nil, "release v0.2.0 main abc1234")
	return s
}

// Sections keep their order; over-cap sections end with "…외 N건".
func TestOpsBriefSectionsCapsFRRHZ190(t *testing.T) {
	s := board190(t)
	now := time.Now().Add(time.Minute)
	code, md := getBrief190(t, s, now, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, md)
	}
	last := -1
	for i, title := range brief190Titles {
		at := strings.Index(md, fmt.Sprintf("## %d. %s", i+1, title))
		if at <= last {
			t.Fatalf("section %q out of order:\n%s", title, md)
		}
		last = at
	}
	var b Brief
	_, js := getBrief190(t, s, now, "?format=json")
	if err := json.Unmarshal([]byte(js), &b); err != nil {
		t.Fatal(err)
	}
	sec := map[string]BriefSection{}
	for _, x := range b.Sections {
		sec[x.Key] = x
	}
	if w := sec["waiting_h"]; w.Total != 12 || len(w.Items) != 10 || w.Omitted != 2 || !strings.HasPrefix(w.Items[0], "[gate_pending] q-") || !strings.Contains(w.Items[0], "gate 00") {
		t.Fatalf("waiting_h: %+v", w)
	}
	if !strings.Contains(md, "## 1. H 대기 — gate_pending 12 (12)\n") || !strings.Contains(md, "- …외 2건\n\n## 2.") {
		t.Fatalf("waiting_h markdown:\n%s", md)
	}
	if n := sec["next"]; n.Total != 8 || len(n.Items) != 6 || n.Omitted != 2 || !strings.HasPrefix(n.Items[0], "m-") {
		t.Fatalf("next: %+v", n)
	}
	if r := sec["running"]; r.Total != 1 || !strings.Contains(r.Items[0], "[running]") {
		t.Fatalf("running: %+v", r)
	}
	if d := sec["decisions"]; d.Total != 1 || !strings.HasSuffix(d.Items[0], "결정: brief는 규칙으로만 만든다") || !strings.Contains(d.Items[0], " KST note-") {
		t.Fatalf("decisions: %+v", d)
	}
	th := sec["threads"]
	if all := strings.Join(th.Items, "\n"); th.Total != 2 || !strings.Contains(all, "[약속] note-") || !strings.Contains(all, "[질문] note-") || strings.Contains(all, "닫힐 것") || strings.Contains(all, "미션 끝내기") || strings.Contains(all, "답 있는 것") {
		t.Fatalf("threads: %+v", th)
	}
	if r := sec["release"]; r.Total != 1 || !strings.Contains(r.Items[0], "release v0.2.0") {
		t.Fatalf("release: %+v", r)
	}
	// Decisions older than 48h fall out.
	_, js = getBrief190(t, s, now.Add(49*time.Hour), "?format=json")
	b = Brief{}
	if err := json.Unmarshal([]byte(js), &b); err != nil || b.Sections[5].Total != 0 {
		t.Fatalf("decisions after 49h: %+v", b.Sections[5])
	}
	// Caps and line cuts bound the whole brief (~1500 tokens).
	if n := utf8.RuneCountInString(md); n > 6000 {
		t.Fatalf("brief is %d runes", n)
	}
}

// Launcher runs (open on the mission line, ended with their usage cost) and
// the 7-day horizon of schedules and deadlines, in KST.
func TestOpsBriefRunsAndDueFRRHZ190(t *testing.T) {
	s := store078(t)
	ex := execution.Service{Store: s}
	pol := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	done, err := ex.IntentWithPolicy("mission-078", "k-done", pol, pol)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() (execution.Ref, error){
		func() (execution.Ref, error) { return ex.ClaimDispatch(done.ID, "claude-local", "c1") },
		func() (execution.Ref, error) { return ex.Accept(done.ID, "ext-1") },
		func() (execution.Ref, error) { return ex.Observe(done.ID, fmt.Sprintf("%019d", 1), "ok", "src", false) },
	} {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	open, _ := ex.IntentWithPolicy("mission-078", "k-open", pol, pol)
	ex.ClaimDispatch(open.ID, "claude-local", "c2")
	if _, err := ex.Accept(open.ID, "ext-2"); err != nil {
		t.Fatal(err)
	}
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "observation", MissionID: "mission-078", Tags: []string{"usage", "session-launcher"},
		Content: "usage line\n```json\n{\"executionId\":\"" + done.ID + "\",\"totalCostUsd\":0.082}\n```"})
	if _, err := (schedule.Service{Store: s}).Create("weekly review", "", schedule.Rule{Freq: "weekly", Weekday: 1, Time: "09:00"}, ""); err != nil {
		t.Fatal(err)
	}
	for name, due := range map[string]string{"soon": "2026-10-12T18:00:00+09:00", "late": "2026-10-30T18:00:00+09:00", "past": "2026-10-10T08:00:00+09:00"} {
		relay132(t, s, Intent{Kind: "mission.create", Name: name, Prompt: "p", DueAt: due})
	}
	now := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC) // 12:00 KST, Sunday
	_, js := getBrief190(t, s, now, "?format=json")
	var b Brief
	if err := json.Unmarshal([]byte(js), &b); err != nil {
		t.Fatal(err)
	}
	if r := b.Sections[1]; r.Total != 1 || !strings.Contains(r.Items[0], "exec "+open.ID+" accepted") {
		t.Fatalf("running: %+v", r)
	}
	if r := b.Sections[2]; r.Total != 1 || !strings.HasPrefix(r.Items[0], done.ID+" succeeded · m-") || !strings.Contains(r.Items[0], "$0.082") {
		t.Fatalf("recent runs: %+v", r)
	}
	d := b.Sections[4]
	if d.Total != 3 || !strings.HasPrefix(d.Items[0], "2026-10-10 08:00 KST 마감 지남") || !strings.Contains(d.Items[0], "past") ||
		!strings.HasPrefix(d.Items[1], "2026-10-12 09:00 KST 일정 weekly review") || !strings.HasPrefix(d.Items[2], "2026-10-12 18:00 KST 마감 m-") {
		t.Fatalf("due: %+v", d)
	}
}

// Same journal and same now → byte-identical output.
func TestOpsBriefDeterministicFRRHZ190(t *testing.T) {
	s := board190(t)
	now := time.Now().Add(time.Minute)
	for _, q := range []string{"", "?format=json"} {
		_, a := getBrief190(t, s, now, q)
		_, b := getBrief190(t, s, now, q)
		if a != b {
			t.Fatalf("%q differs:\n%s\n---\n%s", q, a, b)
		}
	}
}
