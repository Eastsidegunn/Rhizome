package janusadapter

// RHZ-093 tests (FR-RHZ-119): events_tail client op, the hybrid
// tail/replay observation loop and the D22 cursor gate (B′). Every JANUS
// contact is a fixture: net.Pipe fake sockets and io.Reader replay streams.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/execution"
)

func tailEvent(seq int64, kind string) TailEvent {
	return TailEvent{Seq: seq, TS: seq, Kind: kind, Actor: "parent", SpanID: replaySpan, TraceID: replayTrace}
}

// tailPage builds an ok page of events from+1..through with the given kinds.
func tailPage(from int64, kinds []string, more bool, state string) TailResult {
	r := TailResult{Status: "ok", SessionID: replayTrace, Events: []TailEvent{}, NextFromSeq: from, More: more, Session: TailSession{State: state, SessionMode: "oneshot"}}
	for i, k := range kinds {
		r.Events = append(r.Events, tailEvent(from+int64(i)+1, k))
		r.NextFromSeq = from + int64(i) + 1
	}
	r.Session.LastSeq = r.NextFromSeq
	return r
}

func noReplay(t *testing.T) ReplaySource {
	return func(cfg RunConfig, db, tr string) (io.Reader, error) {
		t.Errorf("unexpected hx replay")
		return nil, errors.New("unexpected replay")
	}
}

func fixedReplay(stream string) ReplaySource {
	return func(cfg RunConfig, db, tr string) (io.Reader, error) { return strings.NewReader(stream), nil }
}

func observedEvents(s events.Port, id string) []events.Event {
	out := []events.Event{}
	for _, e := range s.List("execution", id) {
		if e.Type == "execution.observed" {
			out = append(out, e)
		}
	}
	return out
}

func policyLineFor(seq int, requestID string) string {
	return replayLine(seq, "policy/decision", `{"decision":"allow","profile_id":"manual","request_id":"`+requestID+`","response_id":"resp","reason":""}`)
}

// --- (a) client -----------------------------------------------------------

func TestClientEventsTailWireExactFRRHZ119(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		assertWireKeys(t, q, "op", "session_id", "from_seq")
		if q["op"] != "events_tail" || q["session_id"] != replayTrace || q["from_seq"] != float64(7) {
			t.Errorf("wire: %v", q)
		}
		parent := "aaaaaaaaaaaaaaaa"
		return map[string]any{"status": "ok", "session_id": replayTrace,
			"events": []map[string]any{
				{"seq": 8, "ts": 100, "kind": "other", "actor": "parent", "span_id": replaySpan, "trace_id": replayTrace, "parent_span_id": parent, "usage_in": 3, "usage_out": 4},
				{"seq": 9, "ts": 101, "kind": "user/message", "actor": "human", "span_id": replaySpan, "trace_id": replayTrace},
			},
			"next_from_seq": 9, "more": true,
			"session": map[string]any{"state": "running", "done_status": "", "terminal_seq": 0, "session_mode": "multiturn", "last_seq": 12, "last_activity_ts": 101, "usage_in_total": 30, "usage_out_total": 40}}
	})}
	r, err := c.EventsTail(replayTrace, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := TailResult{Status: "ok", SessionID: replayTrace, NextFromSeq: 9, More: true,
		Events: []TailEvent{
			{Seq: 8, TS: 100, Kind: "other", Actor: "parent", SpanID: replaySpan, TraceID: replayTrace, ParentSpanID: "aaaaaaaaaaaaaaaa", UsageIn: 3, UsageOut: 4},
			{Seq: 9, TS: 101, Kind: "user/message", Actor: "human", SpanID: replaySpan, TraceID: replayTrace},
		},
		Session: TailSession{State: "running", SessionMode: "multiturn", LastSeq: 12, LastActivityTS: 101, UsageInTotal: 30, UsageOutTotal: 40}}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("parsed:\n%+v\nwant\n%+v", r, want)
	}
	if _, err = c.EventsTail("", 0); err == nil {
		t.Fatal("empty session accepted")
	}
	if _, err = c.EventsTail(replayTrace, -1); err == nil {
		t.Fatal("negative from_seq accepted")
	}
}

func TestClientEventsTailRejectsUnknownFieldFRRHZ119(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"status": "ok", "session_id": replayTrace, "events": []map[string]any{}, "next_from_seq": 0, "more": false,
			"session": map[string]any{"state": "pending", "session_mode": "oneshot", "last_seq": 0, "last_activity_ts": 0, "usage_in_total": 0, "usage_out_total": 0}}
	}
	for name, mut := range map[string]func(map[string]any){
		"top":     func(m map[string]any) { m["payload"] = "x" },
		"session": func(m map[string]any) { m["session"].(map[string]any)["exit_code"] = 0 },
		"event": func(m map[string]any) {
			ev := map[string]any{"seq": 1, "ts": 1, "kind": "k", "actor": "a", "span_id": replaySpan, "trace_id": replayTrace, "payload": map[string]any{}}
			m["events"] = []map[string]any{ev}
			m["next_from_seq"] = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp := base()
			mut(resp)
			c := Client{Dial: fake(func(q map[string]any) map[string]any { return resp })}
			if _, err := c.EventsTail(replayTrace, 0); err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatalf("unknown field accepted: %v", err)
			}
		})
	}
	// Baseline: the unmutated response parses.
	c := Client{Dial: fake(func(q map[string]any) map[string]any { return base() })}
	if r, err := c.EventsTail(replayTrace, 0); err != nil || len(r.Events) != 0 || r.More {
		t.Fatal(r, err)
	}
}

func TestClientEventsTailErrorReasonsFRRHZ119(t *testing.T) {
	for _, reason := range []string{"UNKNOWN_SESSION", "LOG_UNAVAILABLE", "LOG_INVALID", "REQUEST_MISMATCH"} {
		c := Client{Dial: fake(func(q map[string]any) map[string]any {
			return map[string]any{"status": "error", "reason": reason, "events": []map[string]any{}, "next_from_seq": 0, "more": false, "session": map[string]any{"state": "", "session_mode": "", "last_seq": 0, "last_activity_ts": 0, "usage_in_total": 0, "usage_out_total": 0}}
		})}
		_, err := c.EventsTail(replayTrace, 0)
		var te ErrTail
		if !errors.As(err, &te) || te.Reason != reason || err.Error() != reason {
			t.Fatalf("%s: %v", reason, err)
		}
	}
	// error without reason and an unknown status are both invalid responses.
	for _, resp := range []map[string]any{{"status": "error"}, {"status": "pending", "events": []map[string]any{}}} {
		c := Client{Dial: fake(func(q map[string]any) map[string]any { return resp })}
		_, err := c.EventsTail(replayTrace, 0)
		var te ErrTail
		if err == nil || errors.As(err, &te) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("%v: %v", resp, err)
		}
	}
	down := Client{Dial: func() (net.Conn, error) { return nil, errors.New("down") }}
	if _, err := down.EventsTail(replayTrace, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dial failure: %v", err)
	}
}

// --- (b) loop / tail -------------------------------------------------------

// A tail-only page advances the cursor with exactly one existing
// execution.observed event and no hx replay contact.
func TestTickTailAdvancesCursorWithoutReplayFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	l, errs := newTestLoop(s, noReplay(t), noDial(t))
	l.Tail = func(sid string, from int64) (TailResult, error) {
		if sid != replayTrace || from != 0 {
			t.Errorf("tail args: %s %d", sid, from)
		}
		return tailPage(0, []string{"subagent/spawn", "other"}, false, "running"), nil
	}
	l.Tick()
	got := execRef(t, s, r.ID)
	if got.State != execution.Observing || got.Cursor != c19(t, 2) || got.SourceRef != replaySession || got.Summary != "JANUS tail: observing" {
		t.Fatalf("observation: %+v", got)
	}
	if obs := observedEvents(s, r.ID); len(obs) != 1 || obs[0].Type != "execution.observed" {
		t.Fatalf("observed events: %d", len(obs))
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
	// Nothing new: no write.
	l.Tail = func(sid string, from int64) (TailResult, error) { return tailPage(from, nil, false, "running"), nil }
	before := len(s.All())
	l.Tick()
	if len(s.All()) != before || errs.count() != 0 {
		t.Fatalf("empty page wrote: %d->%d %v", before, len(s.All()), errs.all())
	}
}

// `more` paging is bounded by MaxTailPages per tick; the next tick continues
// from the durable cursor. A fixture that always says more=true must not loop.
func TestTickTailMorePagesBoundedFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	l, errs := newTestLoop(s, noReplay(t), noDial(t))
	calls := []int64{}
	l.Tail = func(sid string, from int64) (TailResult, error) {
		calls = append(calls, from)
		return tailPage(from, []string{"other"}, true, "running"), nil
	}
	l.Tick()
	if len(calls) != MaxTailPages {
		t.Fatalf("pages in one tick: %d (bound %d)", len(calls), MaxTailPages)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, int64(MaxTailPages)) || got.State != execution.Observing {
		t.Fatalf("cursor after bounded tick: %+v", got)
	}
	calls = nil
	l.Tick()
	if len(calls) != MaxTailPages || calls[0] != int64(MaxTailPages) {
		t.Fatalf("second tick pages: %v", calls)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 2*int64(MaxTailPages)) {
		t.Fatalf("cursor after second tick: %+v", got)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// A seq jump against the cursor (or inside a page, or a lying next_from_seq)
// is OBSERVATION_CORRUPT: reported only, no durable write, no replay fallback.
func TestTickTailGapIsObservationCorruptNoWriteFRRHZ119(t *testing.T) {
	for name, page := range map[string]func(from int64) TailResult{
		"jump_from_cursor": func(from int64) TailResult {
			p := tailPage(from, []string{"other"}, false, "running")
			p.Events[0].Seq = from + 2
			p.NextFromSeq = from + 2
			return p
		},
		"gap_inside_page": func(from int64) TailResult {
			p := tailPage(from, []string{"other", "other", "other"}, false, "running")
			p.Events[2].Seq = from + 5
			p.NextFromSeq = from + 5
			return p
		},
		"regression": func(from int64) TailResult {
			p := tailPage(from, []string{"other", "other"}, false, "running")
			p.Events[1].Seq = from + 1
			return p
		},
		"next_from_seq_mismatch": func(from int64) TailResult {
			p := tailPage(from, []string{"other"}, false, "running")
			p.NextFromSeq = from + 3
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := loopStore(t)
			l, errs := newTestLoop(s, noReplay(t), noDial(t))
			l.Tail = func(sid string, from int64) (TailResult, error) { return page(from), nil }
			before := len(s.All())
			l.Tick()
			if len(s.All()) != before {
				t.Fatalf("corrupt tail wrote: %d->%d", before, len(s.All()))
			}
			if got := execRef(t, s, r.ID); got.Cursor != "" || got.State != execution.Accepted {
				t.Fatalf("cursor moved: %+v", got)
			}
			if errs.count() != 1 || !strings.HasPrefix(errs.all()[0], "execution|"+r.ID+"|"+ErrObservationCorrupt.Error()) {
				t.Fatalf("corrupt not reported: %v", errs.all())
			}
			l.Tick() // Stays alive, keeps reporting.
			if errs.count() != 2 {
				t.Fatal(errs.all())
			}
		})
	}
}

func TestTickTailForeignTraceSessionReplacedFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	l, errs := newTestLoop(s, noReplay(t), noDial(t))
	l.Tail = func(sid string, from int64) (TailResult, error) {
		p := tailPage(from, []string{"other"}, false, "running")
		p.Events[0].TraceID = runTrace
		return p, nil
	}
	before := len(s.All())
	l.Tick()
	if len(s.All()) != before || execRef(t, s, r.ID).Cursor != "" {
		t.Fatal("foreign trace wrote")
	}
	if errs.count() != 1 || !strings.HasSuffix(errs.all()[0], ErrSessionReplaced.Error()) {
		t.Fatalf("not reported as SESSION_REPLACED: %v", errs.all())
	}
}

// A page carrying a payload kind routes the tick to hx replay (fake hx); the
// request is surfaced through the existing path and the cursor advances.
func TestTickTailApprovalKindFallsBackToReplayFRRHZ119(t *testing.T) {
	for _, kind := range []string{"subagent/approval_request", "policy/decision", "subagent/done", "session/end"} {
		t.Run(kind, func(t *testing.T) {
			s, r := loopStore(t)
			id := approval.IDFor(gateKey("r1"))
			replays := 0
			dial := opDial(t, map[string]func(map[string]any) map[string]any{
				"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
			})
			l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
				replays++
				return strings.NewReader(requestLine(1, "r1")), nil
			}, dial)
			l.Tail = func(sid string, from int64) (TailResult, error) {
				return tailPage(from, []string{"other", kind}, false, "running"), nil
			}
			l.Tick()
			if replays != 1 {
				t.Fatalf("replay calls: %d", replays)
			}
			if w, d, g := surfaceRecords(s, id); w != 1 || d != 1 || g != 1 {
				t.Fatalf("not surfaced: %d %d %d", w, d, g)
			}
			if got := execRef(t, s, r.ID); got.Cursor != c19(t, 1) || got.State != execution.Observing || got.Summary != "JANUS replay: observing" {
				t.Fatalf("observation: %+v", got)
			}
			if errs.count() != 0 {
				t.Fatal(errs.all())
			}
		})
	}
}

// Socket unavailable / UNKNOWN_SESSION / LOG_UNAVAILABLE fall back to replay,
// which concludes the terminal state. LOG_INVALID and REQUEST_MISMATCH are
// report-only (no replay, no write).
func TestTickTailUnavailableFallsBackToReplayFRRHZ119(t *testing.T) {
	done := replayLine(1, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(2, "session/end", `{}`)
	for name, tailErr := range map[string]error{"unavailable": ErrUnavailable, "unknown_session": ErrTail{Reason: "UNKNOWN_SESSION"}, "log_unavailable": ErrTail{Reason: "LOG_UNAVAILABLE"}} {
		t.Run(name, func(t *testing.T) {
			s, r := loopStore(t)
			l, errs := newTestLoop(s, fixedReplay(done), noDial(t))
			l.Tail = func(sid string, from int64) (TailResult, error) { return TailResult{}, tailErr }
			l.Tick()
			if got := execRef(t, s, r.ID); got.State != execution.Succeeded || got.Cursor != c19(t, 2) {
				t.Fatalf("terminal not concluded via replay: %+v", got)
			}
			if errs.count() != 0 {
				t.Fatal(errs.all())
			}
		})
	}
	for _, reason := range []string{"LOG_INVALID", "REQUEST_MISMATCH"} {
		t.Run(reason, func(t *testing.T) {
			s, r := loopStore(t)
			l, errs := newTestLoop(s, noReplay(t), noDial(t))
			l.Tail = func(sid string, from int64) (TailResult, error) { return TailResult{}, ErrTail{Reason: reason} }
			before := len(s.All())
			l.Tick()
			if len(s.All()) != before || execRef(t, s, r.ID).Cursor != "" {
				t.Fatal("report-only reason wrote")
			}
			if errs.count() != 1 || !strings.HasSuffix(errs.all()[0], reason) {
				t.Fatalf("not reported: %v", errs.all())
			}
		})
	}
}

// A tail parse failure (unknown envelope field) or an unlisted rejection
// reason (UNAUTHENTICATED) falls back to hx replay, so the execution still
// concludes; tail is never the sole path to a terminal state.
func TestTickTailParseFailureFallsBackToReplayFRRHZ119(t *testing.T) {
	done := replayLine(1, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(2, "session/end", `{}`)
	badEnvelope := func(q map[string]any) map[string]any {
		return map[string]any{"status": "ok", "session_id": replayTrace,
			"events":        []map[string]any{{"seq": 1, "ts": 1, "kind": "other", "actor": "a", "span_id": replaySpan, "trace_id": replayTrace, "payload": map[string]any{}}},
			"next_from_seq": 1, "more": false,
			"session": map[string]any{"state": "running", "session_mode": "oneshot", "last_seq": 1, "last_activity_ts": 1, "usage_in_total": 0, "usage_out_total": 0}}
	}
	unauthenticated := func(q map[string]any) map[string]any {
		return map[string]any{"status": "error", "reason": "UNAUTHENTICATED", "events": []map[string]any{}, "next_from_seq": 0, "more": false,
			"session": map[string]any{"state": "", "session_mode": "", "last_seq": 0, "last_activity_ts": 0, "usage_in_total": 0, "usage_out_total": 0}}
	}
	for name, script := range map[string]func(map[string]any) map[string]any{"parse_failure": badEnvelope, "unauthenticated": unauthenticated} {
		t.Run(name, func(t *testing.T) {
			s, r := loopStore(t)
			replays := 0
			l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
				replays++
				return strings.NewReader(done), nil
			}, fake(script))
			l.Tail = l.Client.EventsTail
			l.Tick()
			if replays != 1 {
				t.Fatalf("replay fallback not taken: %d", replays)
			}
			if got := execRef(t, s, r.ID); got.State != execution.Succeeded || got.Cursor != c19(t, 2) {
				t.Fatalf("execution not concluded: %+v", got)
			}
			if errs.count() != 0 {
				t.Fatal(errs.all())
			}
		})
	}
}

// session.state=exited without a terminal kind on the page (page bound)
// still goes through replay: terminal facts are never concluded from tail.
func TestTickTailExitedSessionFallsBackToReplayFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	done := replayLine(1, "other", `{}`) + replayLine(2, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(3, "session/end", `{}`)
	l, errs := newTestLoop(s, fixedReplay(done), noDial(t))
	l.Tail = func(sid string, from int64) (TailResult, error) {
		p := tailPage(from, []string{"other"}, true, "exited")
		p.Session.DoneStatus, p.Session.TerminalSeq = "ok", 3
		return p, nil
	}
	l.Tick()
	if got := execRef(t, s, r.ID); got.State != execution.Succeeded || got.Cursor != c19(t, 3) {
		t.Fatalf("exited session not concluded via replay: %+v", got)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// A fresh Loop instance (restart) resumes tail from the durable cursor; no
// process memory is involved.
func TestTickTailResumesFromDurableCursorAfterRestartFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	a, errsA := newTestLoop(s, noReplay(t), noDial(t))
	a.Tail = func(sid string, from int64) (TailResult, error) {
		return tailPage(from, []string{"other", "other"}, false, "running"), nil
	}
	a.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 2) {
		t.Fatalf("seed: %+v", got)
	}
	b, errsB := newTestLoop(s, noReplay(t), noDial(t))
	froms := []int64{}
	b.Tail = func(sid string, from int64) (TailResult, error) {
		froms = append(froms, from)
		return tailPage(from, []string{"other"}, false, "running"), nil
	}
	b.Tick()
	if !reflect.DeepEqual(froms, []int64{2}) {
		t.Fatalf("restart did not resume from durable cursor: %v", froms)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 3) {
		t.Fatalf("after restart: %+v", got)
	}
	if errsA.count()+errsB.count() != 0 {
		t.Fatal(errsA.all(), errsB.all())
	}
}

// First observation: the legacy empty cursor tails from_seq=0 — no full replay.
func TestTickTailFirstObservationFromZeroFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	if got := execRef(t, s, r.ID); got.Cursor != "" {
		t.Fatalf("fixture cursor: %q", got.Cursor)
	}
	l, errs := newTestLoop(s, noReplay(t), noDial(t))
	froms := []int64{}
	l.Tail = func(sid string, from int64) (TailResult, error) {
		froms = append(froms, from)
		return tailPage(from, []string{"subagent/spawn"}, false, "running"), nil
	}
	l.Tick()
	if !reflect.DeepEqual(froms, []int64{0}) || execRef(t, s, r.ID).Cursor != c19(t, 1) || errs.count() != 0 {
		t.Fatalf("first observation: %v %+v %v", froms, execRef(t, s, r.ID), errs.all())
	}
}

// --- (c) D22 gate ------------------------------------------------------------

// While an open approval request stays unsurfaced (socket query fails) the
// cursor is HELD; approval observations of other, already-gated keys in the
// same batch are still made durable.
func TestTickCursorHeldWhileRequestUnsurfacedFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	as := approval.Service{Store: s}
	k2 := gateKey("r2")
	if _, err := as.RecordInput(k2, approval.Allow, "", "resp", gateDigest, "test-operator", "", "", noAuthority()); err != nil {
		t.Fatal(err)
	}
	stream := requestLine(1, "r1") + requestLine(2, "r2") + policyLineFor(3, "r2") + replayLine(4, "other", `{}`)
	l, errs := newTestLoop(s, fixedReplay(stream), func() (net.Conn, error) { return nil, errors.New("socket down") })
	l.Tail = func(sid string, from int64) (TailResult, error) {
		return tailPage(from, []string{"subagent/approval_request"}, false, "running"), nil
	}
	l.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != "" || got.State != execution.Accepted {
		t.Fatalf("D22: cursor advanced past unsurfaced open request r1: %+v", got)
	}
	if len(observedEvents(s, r.ID)) != 0 {
		t.Fatal("execution.observed written while held")
	}
	if a := approvalRef(t, s, approval.IDFor(k2)); a.State != approval.Observed || a.ResponseSeq != 3 {
		t.Fatalf("approval observation not durable while held: %+v", a)
	}
	if n := errs.count(); n != 1 || !strings.HasPrefix(errs.all()[0], "approval|"+approval.IDFor(gateKey("r1"))) {
		t.Fatalf("errors: %v", errs.all())
	}
	// Second tick: still failing → still held, approval observation idempotent.
	before := len(s.All())
	l.Tick()
	if len(s.All()) != before || execRef(t, s, r.ID).Cursor != "" {
		t.Fatal("held tick wrote or advanced")
	}
}

// After the surfacing retry succeeds the cursor advances in the same tick.
func TestTickCursorAdvancesAfterSurfaceRetryFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	stream := requestLine(1, "r1") + replayLine(2, "other", `{}`)
	l, errs := newTestLoop(s, fixedReplay(stream), func() (net.Conn, error) { return nil, errors.New("socket down") })
	l.Tail = func(sid string, from int64) (TailResult, error) {
		return tailPage(from, []string{"subagent/approval_request", "other"}, false, "running"), nil
	}
	l.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != "" {
		t.Fatalf("held expected: %+v", got)
	}
	l.Client = Client{Dial: opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
	})}
	l.Tick()
	if w, d, g := surfaceRecords(s, id); w != 1 || d != 1 || g != 1 {
		t.Fatalf("not surfaced: %d %d %d", w, d, g)
	}
	// Surfacing records precede the cursor advance in the journal.
	all := s.All()
	surfaced, observed := -1, -1
	for i, e := range all {
		if e.AggregateType == "approvalrequest" && e.AggregateID == id && surfaced < 0 {
			surfaced = i
		}
		if e.AggregateType == "execution" && e.Type == "execution.observed" && observed < 0 {
			observed = i
		}
	}
	if surfaced < 0 || observed < 0 || surfaced > observed {
		t.Fatalf("order: approvalrequest@%d observed@%d", surfaced, observed)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 2) || got.State != execution.Observing {
		t.Fatalf("cursor after retry: %+v", got)
	}
	if errs.count() != 1 {
		t.Fatal(errs.all())
	}
}

// A request the log already resolved (policy/decision) or of an ended
// session is not open: it never holds the cursor even when its query fails.
func TestTickCursorNotHeldByResolvedOrExitedRequestFRRHZ119(t *testing.T) {
	for name, tc := range map[string]struct {
		stream string
		state  execution.State
		cursor int64
	}{
		"resolved": {stream: requestLine(1, "r1") + policyLineFor(2, "r1") + replayLine(3, "other", `{}`), state: execution.Observing, cursor: 3},
		"exited":   {stream: requestLine(1, "r1") + replayLine(2, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(3, "session/end", `{}`), state: execution.Succeeded, cursor: 3},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := loopStore(t)
			l, errs := newTestLoop(s, fixedReplay(tc.stream), func() (net.Conn, error) { return nil, errors.New("socket down") })
			l.Tail = func(sid string, from int64) (TailResult, error) { return TailResult{}, ErrUnavailable }
			l.Tick()
			if got := execRef(t, s, r.ID); got.Cursor != c19(t, tc.cursor) || got.State != tc.state {
				t.Fatalf("cursor held by a non-open request: %+v", got)
			}
			if w, d, g := surfaceRecords(s, approval.IDFor(gateKey("r1"))); w+d+g != 0 {
				t.Fatal("surfaced a non-open request")
			}
			if errs.count() != 0 {
				t.Fatalf("resolved/exited request was queried: %v", errs.all())
			}
		})
	}
}

// At-least-once across restart: the hold is purely durable, so a new Loop
// with a working socket surfaces the request and only then advances.
func TestTickSurfaceAtLeastOnceAcrossRestartFRRHZ119(t *testing.T) {
	s, r := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	stream := requestLine(1, "r1")
	a, _ := newTestLoop(s, fixedReplay(stream), func() (net.Conn, error) { return nil, errors.New("socket down") })
	a.Tail = func(sid string, from int64) (TailResult, error) { return TailResult{}, ErrUnavailable }
	a.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != "" {
		t.Fatalf("held expected: %+v", got)
	}
	b, errsB := newTestLoop(s, fixedReplay(stream), opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
	}))
	b.Tail = func(sid string, from int64) (TailResult, error) { return TailResult{}, ErrUnavailable }
	b.Tick()
	if w, d, g := surfaceRecords(s, id); w != 1 || d != 1 || g != 1 {
		t.Fatalf("restart did not surface: %d %d %d", w, d, g)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 1) || got.State != execution.Observing || errsB.count() != 0 {
		t.Fatalf("after restart: %+v %v", got, errsB.all())
	}
}

// --- (f) compatibility / determinism -------------------------------------------

// A loop without Tail (legacy wiring) observes exactly as before: replay
// only, same execution.observed payloads as a direct ParseReplay+ObserveBatch.
func TestLegacyJournalReplaysUnchangedFRRHZ119(t *testing.T) {
	stream := replayLine(1, "other", `{}`) + replayLine(2, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(3, "session/end", `{}`)
	s1, r1 := loopStore(t)
	l, errs := newTestLoop(s1, fixedReplay(stream), noDial(t))
	if l.Tail != nil {
		t.Fatal("test loop must not wire tail")
	}
	l.Tick()
	s2, r2 := loopStore(t)
	b, err := ParseReplay(strings.NewReader(stream), replayTrace, 0)
	if err != nil {
		t.Fatal(err)
	}
	b.SessionRef = replaySession
	if _, err = ObserveBatch(execution.Service{Store: s2}, approval.Service{Store: s2}, r2.ID, replaySession, b); err != nil {
		t.Fatal(err)
	}
	o1, o2 := observedEvents(s1, r1.ID), observedEvents(s2, r2.ID)
	if len(o1) != 1 || len(o2) != 1 || string(o1[0].Payload) != string(o2[0].Payload) || o1[0].Type != o2[0].Type {
		t.Fatalf("legacy journal differs:\n%+v\n%+v", o1, o2)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// Same fixtures twice → identical journals (types, aggregates, execution
// payloads); the hybrid introduces no process-dependent ordering.
func TestTickDeterministicOrderFRRHZ119(t *testing.T) {
	run := func() []string {
		s, r := loopStore(t)
		id := approval.IDFor(gateKey("r1"))
		tick := 0
		dial := opDial(t, map[string]func(map[string]any) map[string]any{
			"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
		})
		l, errs := newTestLoop(s, fixedReplay(replayLine(1, "other", `{}`)+replayLine(2, "other", `{}`)+requestLine(3, "r1")), dial)
		l.Tail = func(sid string, from int64) (TailResult, error) {
			if tick == 0 {
				return tailPage(from, []string{"other", "other"}, false, "running"), nil
			}
			return tailPage(from, []string{"subagent/approval_request"}, false, "running"), nil
		}
		l.Tick()
		tick++
		l.Tick()
		if w, d, g := surfaceRecords(s, id); w != 1 || d != 1 || g != 1 || execRef(t, s, r.ID).Cursor != c19(t, 3) || errs.count() != 0 {
			t.Fatalf("run: %d %d %d %+v %v", w, d, g, execRef(t, s, r.ID), errs.all())
		}
		out := []string{}
		for _, e := range s.All() {
			line := e.AggregateType + "/" + e.AggregateID + "/" + e.Type
			if e.AggregateType == "execution" {
				line += "/" + string(e.Payload)
			}
			out = append(out, line)
		}
		return out
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic journals:\n%s\n---\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if fmt.Sprint(a) == "" {
		t.Fatal("empty journal")
	}
}
