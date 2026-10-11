package schedule

import (
	"encoding/json"
	"testing"
	"time"

	"rhizome/internal/events"
)

// FR-RHZ-180: strict rule decode — exact keys, types and values.
func TestDecodeRuleStrictFRRHZ180(t *testing.T) {
	if r, err := DecodeRule(json.RawMessage(`{"freq":"weekly","weekday":0,"time":"09:30"}`)); err != nil || r != (Rule{"weekly", 0, "09:30"}) {
		t.Fatalf("valid rule: %+v %v", r, err)
	}
	for _, raw := range []string{
		``, `null`, `[]`, `{}`,
		`{"freq":"weekly","weekday":1}`,
		`{"freq":"daily","weekday":1,"time":"09:00"}`,
		`{"freq":"weekly","weekday":7,"time":"09:00"}`,
		`{"freq":"weekly","weekday":-1,"time":"09:00"}`,
		`{"freq":"weekly","weekday":1,"time":"24:00"}`,
		`{"freq":"weekly","weekday":1,"time":"9:00"}`,
		`{"freq":"weekly","weekday":1,"time":"09:60"}`,
		`{"freq":"weekly","weekday":"1","time":"09:00"}`,
		`{"freq":"weekly","weekday":1,"time":"09:00","x":1}`,
		`{"Freq":"weekly","weekday":1,"time":"09:00"}`,
	} {
		if _, err := DecodeRule(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

// FR-RHZ-180: nextAt is the first occurrence strictly after now (KST).
func TestNextAtFRRHZ180(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	// Monday 09:00; 2026-10-11 is a Sunday.
	sc := Schedule{Rule: Rule{"weekly", 1, "09:00"}, TZ: DefaultTZ, State: Active}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, kst)
	cases := []struct{ now, want time.Time }{
		{now, time.Date(2026, 10, 12, 9, 0, 0, 0, kst)},
		{time.Date(2026, 10, 12, 8, 59, 0, 0, kst), time.Date(2026, 10, 12, 9, 0, 0, 0, kst)},
		{time.Date(2026, 10, 12, 9, 0, 0, 0, kst), time.Date(2026, 10, 19, 9, 0, 0, 0, kst)},
		// 00:30 UTC = 09:30 KST Monday: this week's slot has passed.
		{time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 19, 9, 0, 0, 0, kst)},
	}
	for _, c := range cases {
		got, ok := sc.NextAt(c.now)
		if !ok || !got.Equal(c.want) || got.Format(time.RFC3339)[19:] != "+09:00" {
			t.Fatalf("NextAt(%v) = %v, want %v", c.now, got, c.want)
		}
	}
	sc.State = Cancelled
	if _, ok := sc.NextAt(now); ok {
		t.Fatal("cancelled schedule has a nextAt")
	}
}

// FR-RHZ-180: create/cancel are append-only, replayable, and every
// rejection writes nothing.
func TestServiceCreateCancelReplayFRRHZ180(t *testing.T) {
	s := &events.Store{}
	svc := Service{Store: s}
	for _, bad := range []struct {
		name, tz string
		r        Rule
	}{
		{"", "", Rule{"weekly", 1, "09:00"}},
		{"x", "UTC", Rule{"weekly", 1, "09:00"}},
		{"x", "", Rule{"weekly", 9, "09:00"}},
	} {
		if _, err := svc.Create(bad.name, "", bad.r, bad.tz); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if n := len(s.All()); n != 0 {
		t.Fatalf("rejections wrote %d events", n)
	}
	sc, err := svc.Create("standup", "goal-g", Rule{"weekly", 1, "09:00"}, "")
	if err != nil || sc.ID != "schedule-standup" || sc.TZ != DefaultTZ || sc.State != Active || sc.Revision != 1 {
		t.Fatalf("create: %+v %v", sc, err)
	}
	if _, err := svc.Create("standup", "", Rule{"weekly", 2, "10:00"}, ""); err == nil {
		t.Fatal("duplicate accepted")
	}
	if sc, err = svc.Cancel("schedule-standup", "done"); err != nil || sc.State != Cancelled || sc.Revision != 2 {
		t.Fatalf("cancel: %+v %v", sc, err)
	}
	if _, err := svc.Cancel("schedule-standup", ""); err == nil {
		t.Fatal("second cancel accepted")
	}
	if _, err := svc.Cancel("schedule-missing", ""); err == nil {
		t.Fatal("unknown cancel accepted")
	}
	if n := len(s.All()); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
	re, err := Replay(s.List("schedule", "schedule-standup"))
	if err != nil || re != sc {
		t.Fatalf("replay %+v %v, want %+v", re, err, sc)
	}
}
