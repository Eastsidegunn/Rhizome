package workspace

import (
	"errors"
	"sort"
	"strings"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/schedule"
)

// RHZ-137 (FR-RHZ-181): GET /v1/calendar.ics — a read-only RFC 5545 feed of
// the RHZ-136 timing facts. Active weekly schedules become recurring
// VEVENTs; non-terminal missions with an effectiveDueAt become 15-minute
// VEVENTs with two display alarms. Every byte is derived from the journal:
// DTSTAMP is the envelope time of the entity's latest own event (its last
// change), schedule DTSTART is the first occurrence after the schedule was
// created, and nothing reads the wall clock except effectiveDueAt's anchor
// nextAt, which comes from the server's injectable Clock like
// /v1/workspace. Same journal and clock → same bytes.

var errCalendarGoal = errors.New("goal not found")

// calEvent is one VEVENT before serialization.
type calEvent struct {
	UID, Summary, Description string
	Stamp, Start              time.Time
	Duration                  string
	// RRule is set for recurring schedules; Alarms for mission deadlines.
	RRule  string
	Alarms bool
}

var icsDays = [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

var terminalTaskStates = map[string]bool{"completed": true, "cancelled": true, "failed": true}

// calendarEvents collects the feed's events from a snapshot taken at now;
// goalID != "" narrows to that goal's schedules and missions.
func calendarEvents(s events.Port, p Projection, now time.Time, goalID string) ([]calEvent, error) {
	goalName := map[string]string{}
	for _, m := range p.Missions {
		goalName[m.ID] = m.Name
	}
	if _, ok := goalName[goalID]; goalID != "" && !ok {
		return nil, errCalendarGoal
	}
	var out []calEvent
	next := map[string]time.Time{}
	for _, sc := range p.Schedules {
		if at, ok := sc.sched.NextAt(now); ok {
			next[sc.ID] = at
		}
		if sc.State != schedule.Active || (goalID != "" && sc.GoalID != goalID) {
			continue
		}
		log := s.List("schedule", sc.ID)
		if len(log) == 0 {
			continue
		}
		first, ok := sc.sched.NextAt(orEpoch(log[0].CreatedAt))
		if !ok {
			continue
		}
		out = append(out, calEvent{
			UID: sc.ID + "@rhizome.local", Summary: sc.Name, Description: describeEvent("", goalName[sc.GoalID], sc.GoalID),
			Stamp: log[len(log)-1].CreatedAt, Start: first, Duration: "PT1H",
			RRule: "FREQ=WEEKLY;BYDAY=" + icsDays[sc.Rule.Weekday],
		})
	}
	for _, t := range p.Tasks {
		if terminalTaskStates[t.State] || (goalID != "" && t.MissionID != goalID) {
			continue
		}
		due := effectiveDue(t, next)
		if due.IsZero() {
			continue
		}
		var stamp time.Time
		if t.LastActivityTs > 0 {
			stamp = time.UnixMilli(t.LastActivityTs)
		}
		out = append(out, calEvent{
			UID: t.ID + "@rhizome.local", Summary: t.Name, Description: describeEvent(t.Timing.WorkKind, goalName[t.MissionID], t.MissionID),
			Stamp: stamp, Start: due, Duration: "PT15M", Alarms: true,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// orEpoch maps an unknown (zero) envelope time to the Unix epoch so legacy
// events still render a fixed, valid value.
func orEpoch(t time.Time) time.Time {
	if t.IsZero() {
		return time.Unix(0, 0)
	}
	return t
}

func describeEvent(workKind, goalName, goalID string) string {
	var lines []string
	if workKind != "" {
		lines = append(lines, "workKind: "+workKind)
	}
	if goalID != "" {
		g := goalID
		if goalName != "" {
			g = goalName + " (" + goalID + ")"
		}
		lines = append(lines, "goal: "+g)
	}
	return strings.Join(lines, "\n")
}

// renderICS serializes events as one VCALENDAR: CRLF line ends, TEXT
// escaping and 75-octet folding per RFC 5545 §3.1 and §3.3.11.
func renderICS(evs []calEvent) []byte {
	var b strings.Builder
	line := func(s string) { b.WriteString(foldICS(s)) }
	for _, s := range []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Rhizome//Calendar//EN", "CALSCALE:GREGORIAN",
		"BEGIN:VTIMEZONE", "TZID:" + schedule.DefaultTZ, "BEGIN:STANDARD", "DTSTART:19700101T000000",
		"TZOFFSETFROM:+0900", "TZOFFSETTO:+0900", "TZNAME:KST", "END:STANDARD", "END:VTIMEZONE",
	} {
		line(s)
	}
	loc, _ := schedule.Location(schedule.DefaultTZ)
	for _, e := range evs {
		summary := escapeICSText(e.Summary)
		line("BEGIN:VEVENT")
		line("UID:" + e.UID)
		line("DTSTAMP:" + orEpoch(e.Stamp).UTC().Format("20060102T150405Z"))
		line("DTSTART;TZID=" + schedule.DefaultTZ + ":" + e.Start.In(loc).Format("20060102T150405"))
		line("DURATION:" + e.Duration)
		if e.RRule != "" {
			line("RRULE:" + e.RRule)
		}
		line("SUMMARY:" + summary)
		if e.Description != "" {
			line("DESCRIPTION:" + escapeICSText(e.Description))
		}
		if e.Alarms {
			for _, trigger := range []string{"-P1D", "-PT1H"} {
				line("BEGIN:VALARM")
				line("ACTION:DISPLAY")
				line("TRIGGER:" + trigger)
				line("DESCRIPTION:" + summary)
				line("END:VALARM")
			}
		}
		line("END:VEVENT")
	}
	line("END:VCALENDAR")
	return []byte(b.String())
}

// escapeICSText escapes a TEXT value: backslash, semicolon, comma and
// newline (CRLF, CR or LF → \n).
func escapeICSText(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

// foldICS ends a content line with CRLF, folding it so that no physical
// line exceeds 75 octets; continuation lines start with one space and a
// UTF-8 sequence is never split.
func foldICS(s string) string {
	var b strings.Builder
	limit := 75
	for len(s) > limit {
		cut := limit
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n ")
		s = s[cut:]
		limit = 74
	}
	b.WriteString(s)
	b.WriteString("\r\n")
	return b.String()
}
