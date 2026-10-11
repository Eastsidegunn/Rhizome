// Package schedule is the recurring-schedule aggregate (RHZ-136,
// FR-RHZ-180): a weekly rule and its display, nothing more — no wake is
// ever fired from here. Events are append-only on the schedule/<id> stream
// (schedule.created, schedule.cancelled) and Replay rebuilds the state; this
// Service is the single writer of that stream.
package schedule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"rhizome/internal/events"
)

const (
	CreatedType   = "schedule.created"
	CancelledType = "schedule.cancelled"

	Active    = "active"
	Cancelled = "cancelled"

	// DefaultTZ is the only zone accepted for now. Asia/Seoul has had a
	// fixed +09:00 offset without DST since 1988, so it is computed as a
	// fixed zone: no tzdata dependency, identical on every host.
	DefaultTZ = "Asia/Seoul"
)

var seoul = time.FixedZone("KST", 9*60*60)

// Rule is the recurrence rule. Weekday is 0-6 with Sunday = 0; Time is
// HH:MM (24h) in the schedule's zone.
type Rule struct {
	Freq    string `json:"freq"`
	Weekday int    `json:"weekday"`
	Time    string `json:"time"`
}

type Schedule struct {
	ID, Name, GoalID string
	Rule             Rule
	TZ               string
	State            string
	Revision         uint64
}

type Service struct{ Store events.Port }

// IDFor is the deterministic stream id of a schedule name.
func IDFor(name string) string { return "schedule-" + name }

// DecodeRule decodes a wire rule strictly: exactly freq, weekday and time,
// no other key, correct types. Values are checked by ValidateRule.
func DecodeRule(raw json.RawMessage) (Rule, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Rule{}, errors.New("rule required")
	}
	var w struct {
		Freq    *string `json:"freq"`
		Weekday *int    `json:"weekday"`
		Time    *string `json:"time"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&w); err != nil {
		return Rule{}, errors.New("invalid rule")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return Rule{}, errors.New("invalid rule")
	}
	for k := range object { // DisallowUnknownFields is case-insensitive
		if k != "freq" && k != "weekday" && k != "time" {
			return Rule{}, errors.New("invalid rule")
		}
	}
	if w.Freq == nil || w.Weekday == nil || w.Time == nil {
		return Rule{}, errors.New("rule needs freq, weekday and time")
	}
	r := Rule{Freq: *w.Freq, Weekday: *w.Weekday, Time: *w.Time}
	return r, ValidateRule(r)
}

func ValidateRule(r Rule) error {
	if r.Freq != "weekly" {
		return fmt.Errorf("unknown freq %q", r.Freq)
	}
	if r.Weekday < 0 || r.Weekday > 6 {
		return fmt.Errorf("invalid weekday %d", r.Weekday)
	}
	if _, _, err := clock(r.Time); err != nil {
		return err
	}
	return nil
}

func clock(s string) (int, int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("invalid time %q", s)
	}
	h, m := 0, 0
	for i, c := range s {
		if i == 2 {
			continue
		}
		if c < '0' || c > '9' {
			return 0, 0, fmt.Errorf("invalid time %q", s)
		}
		if i < 2 {
			h = h*10 + int(c-'0')
		} else {
			m = m*10 + int(c-'0')
		}
	}
	if h > 23 || m > 59 {
		return 0, 0, fmt.Errorf("invalid time %q", s)
	}
	return h, m, nil
}

// Location resolves a schedule zone; only DefaultTZ is known.
func Location(tz string) (*time.Location, error) {
	if tz == DefaultTZ {
		return seoul, nil
	}
	return nil, fmt.Errorf("unknown tz %q", tz)
}

// NextAt is the first occurrence strictly after now, in the schedule zone.
func (s Schedule) NextAt(now time.Time) (time.Time, bool) {
	loc, err := Location(s.TZ)
	if err != nil || s.State != Active {
		return time.Time{}, false
	}
	h, m, err := clock(s.Rule.Time)
	if err != nil {
		return time.Time{}, false
	}
	local := now.In(loc)
	days := (s.Rule.Weekday - int(local.Weekday()) + 7) % 7
	at := time.Date(local.Year(), local.Month(), local.Day()+days, h, m, 0, 0, loc)
	if !at.After(now) {
		at = at.AddDate(0, 0, 7)
	}
	return at, true
}

type createdPayload struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	GoalID string `json:"goalId,omitempty"`
	Rule   Rule   `json:"rule"`
	TZ     string `json:"tz"`
}

type cancelledPayload struct {
	Reason string `json:"reason,omitempty"`
}

func validCreated(p createdPayload, agg string) error {
	if strings.TrimSpace(p.Name) == "" || p.ID != IDFor(p.Name) || p.ID != agg {
		return errors.New("invalid schedule")
	}
	if _, err := Location(p.TZ); err != nil {
		return err
	}
	return ValidateRule(p.Rule)
}

// Create appends schedule.created after every check; a rejection writes
// nothing. The goal reference is the caller's (relay's) check.
func (s Service) Create(name, goalID string, r Rule, tz string) (Schedule, error) {
	if s.Store == nil {
		return Schedule{}, errors.New("nil store")
	}
	if tz == "" {
		tz = DefaultTZ
	}
	p := createdPayload{ID: IDFor(name), Name: name, GoalID: goalID, Rule: r, TZ: tz}
	if strings.TrimSpace(name) == "" {
		return Schedule{}, errors.New("schedule name required")
	}
	if err := validCreated(p, p.ID); err != nil {
		return Schedule{}, err
	}
	if log := s.Store.List("schedule", p.ID); len(log) > 0 {
		// The id is the name and the stream is append-only, so a cancelled
		// schedule's name stays taken (RHZ-138, FR-RHZ-182).
		if old, err := Replay(log); err == nil && old.State != Active {
			return Schedule{}, fmt.Errorf("schedule name %q belongs to a cancelled schedule; names cannot be reused", name)
		}
		return Schedule{}, errors.New("schedule already exists")
	}
	b, _ := json.Marshal(p)
	if err := s.Store.Append(0, events.Event{AggregateType: "schedule", AggregateID: p.ID, Revision: 1, Type: CreatedType, Payload: b}); err != nil {
		return Schedule{}, err
	}
	return Replay(s.Store.List("schedule", p.ID))
}

// Cancel appends schedule.cancelled; cancelling a cancelled schedule is
// rejected with zero writes.
func (s Service) Cancel(id, reason string) (Schedule, error) {
	if s.Store == nil {
		return Schedule{}, errors.New("nil store")
	}
	if strings.TrimSpace(id) == "" {
		return Schedule{}, errors.New("scheduleId required")
	}
	log := s.Store.List("schedule", id)
	if len(log) == 0 {
		return Schedule{}, errors.New("schedule not found")
	}
	cur, err := Replay(log)
	if err != nil {
		return Schedule{}, err
	}
	if cur.State != Active {
		return Schedule{}, errors.New("schedule already cancelled")
	}
	b, _ := json.Marshal(cancelledPayload{Reason: reason})
	if err := s.Store.Append(cur.Revision, events.Event{AggregateType: "schedule", AggregateID: id, Revision: cur.Revision + 1, Type: CancelledType, Payload: b}); err != nil {
		return Schedule{}, err
	}
	return Replay(s.Store.List("schedule", id))
}

// Replay rebuilds one schedule stream.
func Replay(log []events.Event) (Schedule, error) {
	var out Schedule
	if len(log) == 0 {
		return out, errors.New("schedule not found")
	}
	for i, e := range log {
		if e.AggregateType != "schedule" || e.Revision != uint64(i+1) {
			return Schedule{}, errors.New("invalid schedule stream")
		}
		switch {
		case i == 0 && e.Type == CreatedType:
			var p createdPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return Schedule{}, err
			}
			if err := validCreated(p, e.AggregateID); err != nil {
				return Schedule{}, err
			}
			out = Schedule{ID: p.ID, Name: p.Name, GoalID: p.GoalID, Rule: p.Rule, TZ: p.TZ, State: Active}
		case i > 0 && e.Type == CancelledType && out.State == Active:
			out.State = Cancelled
		default:
			return Schedule{}, errors.New("invalid schedule event")
		}
		out.Revision = e.Revision
	}
	return out, nil
}
