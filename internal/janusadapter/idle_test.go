package janusadapter

// RHZ-093 (FR-RHZ-119): idle-timeout stop on the tail path. Fixtures
// only: scripted tail pages, fake stop socket, injected clock.

import (
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
)

const idleTimeout = 10 * time.Minute

// idleLoop wires a tail fixture whose session is running with the given
// last_activity_ts, a stop socket that records submissions, and a fixed clock.
func idleLoop(t *testing.T, s *events.Store, lastActivity int64, now time.Time, stops *int) (*Loop, *loopErrs) {
	t.Helper()
	return idleLoopExpecting(t, s, lastActivity, now, stops, IdleStopReason)
}

// idleLoopExpecting is idleLoop with the stop reason the fake socket must see.
func idleLoopExpecting(t *testing.T, s *events.Store, lastActivity int64, now time.Time, stops *int, wantReason string) (*Loop, *loopErrs) {
	t.Helper()
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"stop": func(q map[string]any) map[string]any {
			*stops++
			if q["reason"] != wantReason {
				t.Errorf("stop reason: %v want %s", q["reason"], wantReason)
			}
			return stopEcho(q)
		},
	})
	l, errs := newTestLoop(s, noReplay(t), dial)
	l.Tail = func(sid string, from int64) (TailResult, error) {
		p := tailPage(from, nil, false, "running")
		p.Session.LastActivityTS = lastActivity
		return p, nil
	}
	l.IdleTimeout = idleTimeout
	l.Now = func() time.Time { return now }
	return l, errs
}

func TestTickIdleRequestsStopOnceFRRHZ119(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	for name, tc := range map[string]struct {
		idle time.Duration
		stop bool
	}{
		"below_threshold": {idle: idleTimeout - time.Millisecond, stop: false},
		"at_threshold":    {idle: idleTimeout, stop: true},
		"stale":           {idle: 3 * idleTimeout, stop: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := loopStore(t)
			stops := 0
			l, errs := idleLoop(t, s, now.Add(-tc.idle).UnixMilli(), now, &stops)
			l.Tick()
			got := execRef(t, s, r.ID)
			if got.StopRequested != tc.stop || countType(s, "execution.stop_requested") != b2i(tc.stop) || stops != b2i(tc.stop) {
				t.Fatalf("stop_requested=%v events=%d submits=%d want %v", got.StopRequested, countType(s, "execution.stop_requested"), stops, tc.stop)
			}
			if tc.stop && (got.StopReason != IdleStopReason || got.StopActor != IdleStopActor || got.StopEvidenceSeq != 0) {
				t.Fatalf("stop request content: %+v", got)
			}
			// Second tick: no re-request, no second submission, no write.
			before := len(s.All())
			l.Tick()
			if len(s.All()) != before || stops != b2i(tc.stop) || errs.count() != 0 {
				t.Fatalf("second tick: events %d->%d submits=%d errs=%v", before, len(s.All()), stops, errs.all())
			}
		})
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A restarted serve (new Loop) finds StopRequested durable and never appends
// a second request; the socket resubmission of the same stop_id is the
// existing, deterministic contract behavior.
func TestTickIdleNoDuplicateStopAcrossRestartFRRHZ119(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	s, r := loopStore(t)
	stops := 0
	a, _ := idleLoop(t, s, now.Add(-2*idleTimeout).UnixMilli(), now, &stops)
	a.Tick()
	if countType(s, "execution.stop_requested") != 1 || stops != 1 {
		t.Fatalf("seed: %d %d", countType(s, "execution.stop_requested"), stops)
	}
	stops = 0
	b, errsB := idleLoop(t, s, now.Add(-2*idleTimeout).UnixMilli(), now.Add(time.Hour), &stops)
	before := len(s.All())
	b.Tick()
	b.Tick()
	if countType(s, "execution.stop_requested") != 1 || len(s.All()) != before {
		t.Fatalf("restart re-requested: %d events %d->%d", countType(s, "execution.stop_requested"), before, len(s.All()))
	}
	if stops != 1 || errsB.count() != 0 {
		t.Fatalf("restart submits=%d errs=%v", stops, errsB.all())
	}
	if got := execRef(t, s, r.ID); got.StopActor != IdleStopActor {
		t.Fatalf("%+v", got)
	}
}

// A stop already requested by someone else (operator, reason user) is never
// shadowed or conflicted by the idle judgment: the durable StopRequested
// makes idleStop a no-op, the journal keeps exactly the operator's request,
// and the existing path submits it as the operator's.
func TestTickIdleSkipsWhenOtherStopRequestedFRRHZ119(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	s, r := loopStore(t)
	if _, err := (execution.Service{Store: s}).RequestStop(r.ID, "user", 0, "operator"); err != nil {
		t.Fatal(err)
	}
	stops := 0
	l, errs := idleLoopExpecting(t, s, now.Add(-3*idleTimeout).UnixMilli(), now, &stops, "user")
	l.Tick()
	l.Tick()
	if errs.count() != 0 {
		t.Fatalf("idle conflicted with the operator stop: %v", errs.all())
	}
	if n := countType(s, "execution.stop_requested"); n != 1 {
		t.Fatalf("stop_requested events: %d", n)
	}
	got := execRef(t, s, r.ID)
	if !got.StopRequested || got.StopReason != "user" || got.StopActor != "operator" {
		t.Fatalf("operator request altered: %+v", got)
	}
	if stops != 1 {
		t.Fatalf("stop submissions: %d", stops)
	}
}

func TestTickIdleDisabledWhenZeroFRRHZ119(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	s, r := loopStore(t)
	stops := 0
	l, errs := idleLoop(t, s, now.Add(-100*idleTimeout).UnixMilli(), now, &stops)
	l.IdleTimeout = 0
	l.Tick()
	l.Tick()
	if execRef(t, s, r.ID).StopRequested || countType(s, "execution.stop_requested") != 0 || stops != 0 || errs.count() != 0 {
		t.Fatalf("idle stop while disabled: %+v submits=%d %v", execRef(t, s, r.ID), stops, errs.all())
	}
}

// The replay path (no tail, or a tail tick that fell back) never judges
// idle: row ts in the replay stream are ancient, the clock is far ahead, and
// still no stop is requested.
func TestTickIdleNotJudgedOnReplayPathFRRHZ119(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	stream := replayLine(1, "subagent/spawn", `{}`) + replayLine(2, "other", `{}`) // ts:1 — decades idle
	for name, tail := range map[string]TailSource{
		"legacy_no_tail": nil,
		"fallback_unavailable": func(sid string, from int64) (TailResult, error) {
			return TailResult{}, ErrUnavailable
		},
		"fallback_payload_kind": func(sid string, from int64) (TailResult, error) {
			p := tailPage(from, []string{"subagent/spawn", "policy/decision"}, false, "running") // payload kind → replay path
			p.Session.LastActivityTS = 1
			return p, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := loopStore(t)
			l, errs := newTestLoop(s, fixedReplay(stream), noDial(t))
			l.Tail, l.IdleTimeout, l.Now = tail, idleTimeout, func() time.Time { return now }
			l.Tick()
			got := execRef(t, s, r.ID)
			if got.StopRequested || countType(s, "execution.stop_requested") != 0 {
				t.Fatalf("idle judged on replay path: %+v", got)
			}
			if got.Cursor != c19(t, 2) || errs.count() != 0 {
				t.Fatalf("observation: %+v %v", got, errs.all())
			}
		})
	}
}

// Same fixtures and clock twice → identical journals, and the clock is the
// only time input (no wall-clock leak).
func TestTickIdleNowInjectionDeterministicFRRHZ119(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	run := func() []string {
		s, _ := loopStore(t)
		stops := 0
		l, errs := idleLoop(t, s, now.Add(-idleTimeout).UnixMilli(), now, &stops)
		l.Tick()
		l.Tick()
		if errs.count() != 0 {
			t.Fatal(errs.all())
		}
		out := []string{}
		for _, e := range s.All() {
			out = append(out, e.AggregateType+"/"+e.AggregateID+"/"+e.Type+"/"+string(e.Payload))
		}
		return out
	}
	a, b := run(), run()
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("non-deterministic:\n%s\n---\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if countStr(a, "execution.stop_requested") != 1 {
		t.Fatalf("journal: %v", a)
	}
}

func countStr(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

var _ = execution.Observing
