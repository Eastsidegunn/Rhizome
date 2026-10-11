package workspace

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

// FR-RHZ-181: the VCALENDAR bytes are fixed — VTIMEZONE, RRULE, DURATION,
// two VALARMs, TEXT escaping and 75-octet folding, CRLF everywhere.
func TestCalendarGoldenFRRHZ181(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	got := string(renderICS([]calEvent{
		{
			UID: "mission-due@rhizome.local", Summary: "a\\b;c,d\r\ne",
			Description: "workKind: understanding\ngoal: 0123456789012345678901234567890123456789",
			Stamp:       time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC), Start: time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC),
			Duration: "PT15M", Alarms: true,
		},
		{
			UID: "schedule-weekly-review@rhizome.local", Summary: "weekly review", Description: "goal: Life (goal-life)",
			Stamp: time.Date(2026, 10, 11, 12, 0, 0, 0, kst), Start: time.Date(2026, 10, 12, 9, 0, 0, 0, kst),
			Duration: "PT1H", RRule: "FREQ=WEEKLY;BYDAY=MO",
		},
		{UID: "mission-legacy@rhizome.local", Summary: "legacy", Start: time.Date(2026, 10, 13, 0, 0, 0, 0, kst), Duration: "PT15M"},
	}))
	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//Rhizome//Calendar//EN\r\n" +
		"CALSCALE:GREGORIAN\r\n" +
		"BEGIN:VTIMEZONE\r\n" +
		"TZID:Asia/Seoul\r\n" +
		"BEGIN:STANDARD\r\n" +
		"DTSTART:19700101T000000\r\n" +
		"TZOFFSETFROM:+0900\r\n" +
		"TZOFFSETTO:+0900\r\n" +
		"TZNAME:KST\r\n" +
		"END:STANDARD\r\n" +
		"END:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:mission-due@rhizome.local\r\n" +
		"DTSTAMP:20261011T040000Z\r\n" +
		"DTSTART;TZID=Asia/Seoul:20261012T080000\r\n" +
		"DURATION:PT15M\r\n" +
		"SUMMARY:a\\\\b\\;c\\,d\\ne\r\n" +
		"DESCRIPTION:workKind: understanding\\ngoal: 01234567890123456789012345678901\r\n" +
		" 23456789\r\n" +
		"BEGIN:VALARM\r\n" +
		"ACTION:DISPLAY\r\n" +
		"TRIGGER:-P1D\r\n" +
		"DESCRIPTION:a\\\\b\\;c\\,d\\ne\r\n" +
		"END:VALARM\r\n" +
		"BEGIN:VALARM\r\n" +
		"ACTION:DISPLAY\r\n" +
		"TRIGGER:-PT1H\r\n" +
		"DESCRIPTION:a\\\\b\\;c\\,d\\ne\r\n" +
		"END:VALARM\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:schedule-weekly-review@rhizome.local\r\n" +
		"DTSTAMP:20261011T030000Z\r\n" +
		"DTSTART;TZID=Asia/Seoul:20261012T090000\r\n" +
		"DURATION:PT1H\r\n" +
		"RRULE:FREQ=WEEKLY;BYDAY=MO\r\n" +
		"SUMMARY:weekly review\r\n" +
		"DESCRIPTION:goal: Life (goal-life)\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:mission-legacy@rhizome.local\r\n" +
		"DTSTAMP:19700101T000000Z\r\n" +
		"DTSTART;TZID=Asia/Seoul:20261013T000000\r\n" +
		"DURATION:PT15M\r\n" +
		"SUMMARY:legacy\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	if got != want {
		t.Fatalf("calendar bytes:\n%q\nwant:\n%q", got, want)
	}
}

// FR-RHZ-181: folding never exceeds 75 octets, never splits a UTF-8
// sequence, and unfolds back to the original line.
func TestCalendarFoldFRRHZ181(t *testing.T) {
	for _, in := range []string{
		"SUMMARY:" + strings.Repeat("가", 40),
		"SUMMARY:" + strings.Repeat("x", 67),
		"SUMMARY:" + strings.Repeat("x", 68),
		"SUMMARY:a" + strings.Repeat("😀", 30),
	} {
		folded := foldICS(in)
		if !strings.HasSuffix(folded, "\r\n") || strings.Count(folded, "\n") != strings.Count(folded, "\r\n") {
			t.Fatalf("bad line ends: %q", folded)
		}
		for i, l := range strings.Split(strings.TrimSuffix(folded, "\r\n"), "\r\n") {
			if len(l) > 75 || !utf8.ValidString(l) || (i > 0 && !strings.HasPrefix(l, " ")) {
				t.Fatalf("bad physical line %d (%d octets): %q", i, len(l), l)
			}
		}
		if back := strings.ReplaceAll(strings.TrimSuffix(folded, "\r\n"), "\r\n ", ""); back != in {
			t.Fatalf("unfold = %q, want %q", back, in)
		}
	}
	if got := foldICS("SUMMARY:" + strings.Repeat("x", 67)); strings.Contains(got, "\r\n ") {
		t.Fatalf("75-octet line folded: %q", got)
	}
}

func getCalendar(t *testing.T, h http.Handler, query string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/calendar.ics"+query, nil))
	if rr.Code == 200 && rr.Header().Get("Content-Type") != "text/calendar; charset=utf-8" {
		t.Fatalf("content type %q", rr.Header().Get("Content-Type"))
	}
	return rr.Code, rr.Body.String()
}

// FR-RHZ-181: over HTTP the feed is deterministic (same state → same
// bytes), excludes cancelled schedules and terminal missions, narrows by
// ?goal=, rejects other queries, and never writes.
func TestCalendarFeedFRRHZ181(t *testing.T) {
	s := &events.Store{}
	kst := time.FixedZone("KST", 9*3600)
	srv := NewHTTP(s)
	srv.Clock = func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, kst) }
	h := srv.Handler()
	for _, ok := range []string{
		`{"kind":"mission.create","name":"due","prompt":"p","dueAt":"2026-10-11T23:00:00Z","workKind":"understanding"}`,
		`{"kind":"mission.create","name":"done","prompt":"p","dueAt":"2026-10-12T09:00:00+09:00"}`,
		`{"kind":"mission.create","name":"plain","prompt":"p"}`,
		`{"kind":"schedule.create","name":"weekly-review","goalId":"goal-due","rule":{"freq":"weekly","weekday":1,"time":"09:00"}}`,
		`{"kind":"schedule.create","name":"old","rule":{"freq":"weekly","weekday":3,"time":"18:00"}}`,
		`{"kind":"schedule.cancel","scheduleId":"schedule-old"}`,
		`{"kind":"mission.create","name":"anchored","prompt":"p","anchor":"schedule-weekly-review"}`,
	} {
		if res := post180(t, h, ok); !res.Accepted {
			t.Fatalf("rejected %s: %s", ok, res.Reason)
		}
	}
	m, err := projector.ReplayMission(s.List("mission", "mission-done"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: s}).Transition(m.ID, m.Revision, domain.MissionCancelled); err != nil {
		t.Fatal(err)
	}
	writes := len(s.All())

	code, first := getCalendar(t, h, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, first)
	}
	if _, again := getCalendar(t, h, ""); again != first {
		t.Fatalf("not deterministic:\n%q\n%q", first, again)
	}
	for _, want := range []string{
		"UID:schedule-weekly-review@rhizome.local\r\nDTSTAMP:",
		"DTSTART;TZID=Asia/Seoul:20261012T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;BYDAY=MO\r\nSUMMARY:weekly-review\r\nDESCRIPTION:goal: due (goal-due)\r\n",
		"UID:mission-due@rhizome.local\r\n",
		"DTSTART;TZID=Asia/Seoul:20261012T080000\r\nDURATION:PT15M\r\nSUMMARY:due\r\nDESCRIPTION:workKind: understanding\\ngoal: due (goal-due)\r\nBEGIN:VALARM\r\n",
		// The anchored mission's effectiveDueAt is the anchor's nextAt.
		"UID:mission-anchored@rhizome.local\r\n",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("feed lacks %q:\n%s", want, first)
		}
	}
	for _, absent := range []string{"schedule-old", "mission-done", "mission-plain"} {
		if strings.Contains(first, absent) {
			t.Fatalf("feed includes %s:\n%s", absent, first)
		}
	}
	if !strings.HasPrefix(first, "BEGIN:VCALENDAR\r\n") || !strings.HasSuffix(first, "END:VCALENDAR\r\n") || strings.Count(first, "\n") != strings.Count(first, "\r\n") {
		t.Fatalf("not a CRLF VCALENDAR:\n%q", first)
	}
	if strings.Index(first, "mission-anchored@") > strings.Index(first, "mission-due@") || strings.Index(first, "mission-due@") > strings.Index(first, "schedule-weekly-review@") {
		t.Fatal("events not ordered by UID")
	}

	code, scoped := getCalendar(t, h, "?goal=goal-due")
	if code != 200 || !strings.Contains(scoped, "mission-due@") || !strings.Contains(scoped, "schedule-weekly-review@") || strings.Contains(scoped, "mission-anchored@") {
		t.Fatalf("goal scope %d:\n%s", code, scoped)
	}
	if code, _ := getCalendar(t, h, "?goal=goal-none"); code != 404 {
		t.Fatalf("unknown goal status %d", code)
	}
	for _, bad := range []string{"?goal=", "?goal=a&goal=b", "?x=1"} {
		if code, _ := getCalendar(t, h, bad); code != 400 {
			t.Fatalf("%s status %d", bad, code)
		}
	}
	if n := len(s.All()); n != writes {
		t.Fatalf("GET wrote %d events", n-writes)
	}

	// The same journal behind a fresh server answers the same bytes.
	again := NewHTTP(s)
	again.Clock = srv.Clock
	if _, b := getCalendar(t, again.Handler(), ""); !bytes.Equal([]byte(b), []byte(first)) {
		t.Fatal("fresh server bytes differ")
	}
}
