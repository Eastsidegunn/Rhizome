package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
)

func post180(t *testing.T, h http.Handler, body string) RelayResult {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var res RelayResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// FR-RHZ-180: schedule.create/cancel over HTTP are strict (unknown key, bad
// rule, bad tz, unknown goal → rejected with zero writes); mission.create
// timing fields are validated before any write; /v1/workspace projects
// schedules[] with nextAt and the mission timing with effectiveDueAt at the
// injected clock.
func TestScheduleAndMissionTimingFRRHZ180(t *testing.T) {
	s := &events.Store{}
	kst := time.FixedZone("KST", 9*3600)
	srv := NewHTTP(s)
	srv.Clock = func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, kst) } // Sunday noon
	h := srv.Handler()
	for _, bad := range []string{
		`{"kind":"schedule.create","name":"a","rule":{"freq":"weekly","weekday":1,"time":"09:00"},"extra":1}`,
		`{"kind":"schedule.create","name":"a","rule":{"freq":"weekly","weekday":1,"time":"25:00"}}`,
		`{"kind":"schedule.create","name":"a","rule":{"freq":"weekly","weekday":1,"time":"09:00","x":1}}`,
		`{"kind":"schedule.create","name":"a","rule":{"freq":"weekly","weekday":1,"time":"09:00"},"tz":"UTC"}`,
		`{"kind":"schedule.create","name":"a","rule":{"freq":"weekly","weekday":1,"time":"09:00"},"goalId":"goal-none"}`,
		`{"kind":"schedule.create","name":"","rule":{"freq":"weekly","weekday":1,"time":"09:00"}}`,
		`{"kind":"schedule.cancel","scheduleId":"schedule-a"}`,
		`{"kind":"mission.create","name":"m","prompt":"p","dueAt":"2026-10-12 09:00"}`,
		`{"kind":"mission.create","name":"m","prompt":"p","dueAt":"2026-10-12T09:00:00"}`,
		`{"kind":"mission.create","name":"m","prompt":"p","anchor":"schedule-none"}`,
		`{"kind":"mission.create","name":"m","prompt":"p","workKind":"other"}`,
	} {
		if res := post180(t, h, bad); res.Accepted {
			t.Fatalf("accepted %s", bad)
		}
	}
	if n := len(s.All()); n != 0 {
		t.Fatalf("rejections wrote %d events", n)
	}
	for _, ok := range []string{
		`{"kind":"schedule.create","actor":"op","name":"weekly-review","rule":{"freq":"weekly","weekday":1,"time":"09:00"},"tz":"Asia/Seoul"}`,
		`{"kind":"schedule.create","name":"old","rule":{"freq":"weekly","weekday":3,"time":"18:00"}}`,
		`{"kind":"schedule.cancel","scheduleId":"schedule-old","reason":"retired"}`,
		`{"kind":"mission.create","name":"plain","prompt":"p"}`,
		`{"kind":"mission.create","name":"anchored","prompt":"p","anchor":"schedule-weekly-review","dueAt":"2026-10-13T00:00:00+09:00","workKind":"output"}`,
		`{"kind":"mission.create","name":"due","prompt":"p","dueAt":"2026-10-11T23:00:00Z","workKind":"understanding"}`,
	} {
		if res := post180(t, h, ok); !res.Accepted {
			t.Fatalf("rejected %s: %s", ok, res.Reason)
		}
	}
	// A cancelled schedule cannot anchor; resubmitting a cancel is rejected.
	if res := post180(t, h, `{"kind":"mission.create","name":"late","prompt":"p","anchor":"schedule-old"}`); res.Accepted {
		t.Fatal("cancelled anchor accepted")
	}
	if res := post180(t, h, `{"kind":"schedule.cancel","scheduleId":"schedule-old"}`); res.Accepted {
		t.Fatal("second cancel accepted")
	}
	// Without timing, mission.created keeps the pre-RHZ-136 payload bytes.
	if got := string(s.List("mission", "mission-plain")[0].Payload); got != `{"ID":"mission-plain","GoalID":"goal-plain","Description":"plain","Success":"p"}` {
		t.Fatalf("plain payload changed: %s", got)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	var env struct {
		Body struct {
			Schedules []json.RawMessage `json:"schedules"`
			Tasks     []json.RawMessage `json:"tasks"`
		} `json:"body"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	wantSchedules := []string{
		`{"id":"schedule-old","name":"old","rule":{"freq":"weekly","weekday":3,"time":"18:00"},"tz":"Asia/Seoul","state":"cancelled"}`,
		`{"id":"schedule-weekly-review","name":"weekly-review","rule":{"freq":"weekly","weekday":1,"time":"09:00"},"tz":"Asia/Seoul","state":"active","nextAt":"2026-10-12T09:00:00+09:00"}`,
	}
	if len(env.Body.Schedules) != len(wantSchedules) {
		t.Fatalf("schedules = %s", env.Body.Schedules)
	}
	for i, w := range wantSchedules {
		if string(env.Body.Schedules[i]) != w {
			t.Fatalf("schedule %d:\n got %s\nwant %s", i, env.Body.Schedules[i], w)
		}
	}
	timing := map[string]string{}
	for _, raw := range env.Body.Tasks {
		var x struct{ ID, DueAt, Anchor, WorkKind, EffectiveDueAt string }
		_ = json.Unmarshal(raw, &x)
		timing[x.ID] = x.DueAt + "|" + x.Anchor + "|" + x.WorkKind + "|" + x.EffectiveDueAt
		if x.ID == "mission-plain" && strings.Contains(string(raw), "ueAt") {
			t.Fatalf("plain task carries timing keys: %s", raw)
		}
	}
	// anchored: the anchor nextAt (Mon 09:00) is earlier than dueAt (Tue 00:00).
	want := map[string]string{
		"mission-plain":    "|||",
		"mission-anchored": "2026-10-13T00:00:00+09:00|schedule-weekly-review|output|2026-10-12T09:00:00+09:00",
		"mission-due":      "2026-10-11T23:00:00Z||understanding|2026-10-11T23:00:00Z",
	}
	for id, w := range want {
		if timing[id] != w {
			t.Fatalf("%s timing = %q, want %q", id, timing[id], w)
		}
	}
	// The clock is the only time input: a week later nextAt moves a week.
	srv.Clock = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, kst) }
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	if !strings.Contains(rr.Body.String(), `"nextAt":"2026-10-19T09:00:00+09:00"`) {
		t.Fatalf("nextAt did not follow the clock: %s", rr.Body.String())
	}
}

// FR-RHZ-180: a journal without schedules or timing serializes no new key.
func TestWorkspaceWithoutSchedulesHasNoNewKeysFRRHZ180(t *testing.T) {
	s := requestWorkspaceFixture(t)
	body := string(workspaceBody118(t, s, "/v1/workspace"))
	for _, k := range []string{`"schedules"`, `"dueAt"`, `"anchor"`, `"workKind"`, `"effectiveDueAt"`} {
		if strings.Contains(body, k) {
			t.Fatalf("new key %s present: %s", k, body)
		}
	}
}
