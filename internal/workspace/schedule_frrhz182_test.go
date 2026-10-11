package workspace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
)

// dtoTestNow is the fixed clock tests hand to toDTO, which never reads the
// wall clock itself (FR-RHZ-182).
var dtoTestNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("KST", 9*3600))

var weeklyMon9 = json.RawMessage(`{"freq":"weekly","weekday":1,"time":"09:00"}`)

// FR-RHZ-182: dueAt is RFC3339 with an offset ("Z" included) in whole
// seconds; a fraction is rejected with zero writes.
func TestDueAtWholeSecondsFRRHZ182(t *testing.T) {
	s := &events.Store{}
	for _, due := range []string{"2026-10-12T09:00:00.5+09:00", "2026-10-12T09:00:00.000Z", "2026-10-12T09:00:00.123456789Z", "2026-10-12T09:00:00,5+09:00"} {
		res, _ := RelayIntent(s, Intent{Kind: "mission.create", Name: "frac", Prompt: "p", DueAt: due}, "op", noAuthority())
		if res.Accepted || !strings.Contains(res.Reason, "whole seconds") {
			t.Fatalf("dueAt %q: %+v", due, res)
		}
	}
	if n := len(s.All()); n != 0 {
		t.Fatalf("rejections wrote %d events", n)
	}
	for name, due := range map[string]string{"zulu": "2026-10-12T00:00:00Z", "utc": "2026-10-12T00:00:00+00:00", "kst": "2026-10-12T09:00:00+09:00"} {
		if res, _ := RelayIntent(s, Intent{Kind: "mission.create", Name: name, Prompt: "p", DueAt: due}, "op", noAuthority()); !res.Accepted {
			t.Fatalf("dueAt %q rejected: %s", due, res.Reason)
		}
	}
}

// FR-RHZ-182: schedule.create resolves a goal handle (g-…) to its id, and a
// cancelled schedule's name cannot be reused (zero writes, explicit reason).
func TestScheduleGoalHandleAndNameReuseFRRHZ182(t *testing.T) {
	s := &events.Store{}
	if res, _ := RelayIntent(s, Intent{Kind: "mission.create", Name: "g1", Prompt: "p"}, "op", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	handle := p.handles.of("g", "goal-g1")
	if !handleShape(handle) {
		t.Fatalf("goal handle %q", handle)
	}
	if res, _ := RelayIntent(s, Intent{Kind: "schedule.create", Name: "review", GoalID: handle, Rule: weeklyMon9}, "op", noAuthority()); !res.Accepted {
		t.Fatalf("handle goal rejected: %s", res.Reason)
	}
	if p, _ = Snapshot(s); len(p.Schedules) != 1 || p.Schedules[0].GoalID != "goal-g1" {
		t.Fatalf("schedules = %+v", p.Schedules)
	}
	if res, _ := RelayIntent(s, Intent{Kind: "schedule.create", Name: "review", Rule: weeklyMon9}, "op", noAuthority()); res.Accepted || res.Reason != "schedule already exists" {
		t.Fatalf("duplicate active: %+v", res)
	}
	if res, _ := RelayIntent(s, Intent{Kind: "schedule.cancel", ScheduleID: "schedule-review"}, "op", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	before := len(s.All())
	res, _ := RelayIntent(s, Intent{Kind: "schedule.create", Name: "review", Rule: weeklyMon9}, "op", noAuthority())
	if res.Accepted || !strings.Contains(res.Reason, "cancelled schedule; names cannot be reused") || len(s.All()) != before {
		t.Fatalf("reuse of cancelled name: %+v", res)
	}
}

// FR-RHZ-182: toDTO reads time only from its argument.
func TestToDTOClockArgumentFRRHZ182(t *testing.T) {
	s := &events.Store{}
	if res, _ := RelayIntent(s, Intent{Kind: "schedule.create", Name: "review", Rule: weeklyMon9}, "op", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	for now, want := range map[time.Time]string{dtoTestNow: "2026-10-12T09:00:00+09:00", dtoTestNow.AddDate(0, 0, 7): "2026-10-19T09:00:00+09:00"} {
		if got := toDTO(p, now).Schedules[0].NextAt; got != want {
			t.Fatalf("at %s nextAt = %s, want %s", now, got, want)
		}
	}
}

// timedPort overrides envelope times by sequence; a zero entry models a
// legacy event that carries no time.
type timedPort struct {
	*events.Store
	at map[uint64]time.Time
}

func (p timedPort) fix(es []events.Event) []events.Event {
	out := make([]events.Event, len(es))
	for i, e := range es {
		if at, ok := p.at[e.Sequence]; ok {
			e.CreatedAt = at
		}
		out[i] = e
	}
	return out
}
func (p timedPort) All() []events.Event { return p.fix(p.Store.All()) }
func (p timedPort) List(typ, id string) []events.Event {
	return p.fix(p.Store.List(typ, id))
}

// FR-RHZ-182: an entity whose events carry no time gets the journal clock at
// its creation revision as DTSTAMP (latest earlier envelope time), and the
// epoch only when nothing that early is timed.
func TestCalendarUntimedStampFRRHZ182(t *testing.T) {
	s := &events.Store{}
	for _, in := range []Intent{
		{Kind: "mission.create", Name: "first", Prompt: "p", DueAt: "2026-10-13T00:00:00Z"}, // seq 1 goal, 2 mission
		{Kind: "mission.create", Name: "later", Prompt: "p", DueAt: "2026-10-14T00:00:00Z"}, // seq 3 goal, 4 mission
		{Kind: "schedule.create", Name: "review", Rule: weeklyMon9},                         // seq 5
	} {
		if res, _ := RelayIntent(s, in, "op", noAuthority()); !res.Accepted {
			t.Fatal(res.Reason)
		}
	}
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	port := timedPort{s, map[uint64]time.Time{1: {}, 2: {}, 3: t0, 4: {}, 5: {}}}
	p, err := Snapshot(port)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := calendarEvents(port, p, dtoTestNow, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]time.Time{}
	for _, e := range evs {
		got[e.UID] = e.Stamp
	}
	want := map[string]time.Time{
		"mission-first@rhizome.local":   {},
		"mission-later@rhizome.local":   t0,
		"schedule-review@rhizome.local": t0,
	}
	for uid, w := range want {
		if g, ok := got[uid]; !ok || !g.Equal(w) {
			t.Fatalf("%s stamp = %v, want %v (all %v)", uid, g, w, got)
		}
	}
	if ics := string(renderICS(evs)); !strings.Contains(ics, "UID:mission-first@rhizome.local\r\nDTSTAMP:19700101T000000Z\r\n") ||
		!strings.Contains(ics, "UID:mission-later@rhizome.local\r\nDTSTAMP:20261001T030000Z\r\n") ||
		// The untimed schedule's DTSTART is its first occurrence after the
		// same journal clock (Thu 2026-10-01 → Mon 2026-10-05 09:00 KST).
		!strings.Contains(ics, "UID:schedule-review@rhizome.local\r\nDTSTAMP:20261001T030000Z\r\nDTSTART;TZID=Asia/Seoul:20261005T090000\r\n") {
		t.Fatalf("ics stamps:\n%s", ics)
	}
}
