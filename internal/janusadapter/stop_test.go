package janusadapter

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

// stopStore builds a bound, accepted execution on the replay trace so the
// stop path and the stage-B replay path share one session identity.
func stopStore(t *testing.T) (*events.Store, execution.Ref) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "work", "done"); err != nil {
		t.Fatal(err)
	}
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"fs:workspace"}, Budget: 5, Timeout: 100, MaxDepth: 2, Units: "tokens-ms-v1"}
	r, err := es.IntentWithPolicy("m", "stop-key", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr-1"); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Accept(r.ID, replayTrace); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Bind(r.ID, execution.Binding{SessionDB: replaySession, TraceID: replayTrace, PolicyHash: "policy", RequestFingerprint: "request"}); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func stopEcho(q map[string]any) map[string]any {
	return map[string]any{"status": "stop_accepted", "stop_id": q["stop_id"], "reason": q["reason"]}
}

// Plan C1: exact stop wire — no extra fields, optional fields only when set.
func TestStopWireExactFRRHZ076(t *testing.T) {
	cases := []struct {
		name, span, reason string
		seq                int64
		want               map[string]bool
	}{
		{"minimal", "", "user", 0, map[string]bool{"op": true, "trace_id": true, "stop_id": true, "reason": true}},
		{"target_span", "0123456789abcdef", "user", 0, map[string]bool{"op": true, "trace_id": true, "stop_id": true, "reason": true, "target_span_id": true}},
		{"policy_evidence", "", "policy", 4, map[string]bool{"op": true, "trace_id": true, "stop_id": true, "reason": true, "evidence_seq": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Client{Dial: fake(func(q map[string]any) map[string]any {
				for k := range tc.want {
					if _, ok := q[k]; !ok {
						t.Fatalf("missing %s", k)
					}
				}
				for k := range q {
					if !tc.want[k] {
						t.Fatalf("unexpected %s", k)
					}
				}
				if q["op"] != "stop" {
					t.Fatal(q["op"])
				}
				return stopEcho(q)
			})}
			r, err := c.Stop(replayTrace, "stop-1", tc.span, tc.reason, tc.seq)
			if err != nil || r.Status != "stop_accepted" || r.StopID != "stop-1" || r.Reason != tc.reason {
				t.Fatal(r, err)
			}
		})
	}
}

// Plan C2: submission requires the durable stop_requested event first, and
// the wire carries exactly the durable coordinates.
func TestStopRequiresDurableStopRequestedFRRHZ076(t *testing.T) {
	s, r := stopStore(t)
	called := 0
	c := Client{Dial: func() (net.Conn, error) { called++; return nil, errors.New("must not dial") }}
	if _, err := SubmitStop(execution.Service{Store: s}, c, r.ID); err == nil || called != 0 {
		t.Fatal("submitted without durable stop_requested:", called, err)
	}
	es := execution.Service{Store: s}
	r, err := es.RequestStop(r.ID, "policy", 5, "operator")
	if err != nil {
		t.Fatal(err)
	}
	c = Client{Dial: fake(func(q map[string]any) map[string]any {
		if q["stop_id"] != r.StopID || q["trace_id"] != replayTrace || q["reason"] != "policy" || q["evidence_seq"] != float64(5) {
			t.Fatalf("wire differs from durable intent: %v", q)
		}
		return stopEcho(q)
	})}
	got, err := SubmitStop(es, c, r.ID)
	if err != nil || got.Status != "stop_accepted" || got.StopID != r.StopID {
		t.Fatal(got, err)
	}
}

// Plan C3: client-side validation refuses before any socket contact.
func TestStopClientInputValidationFRRHZ076(t *testing.T) {
	called := false
	c := Client{Dial: func() (net.Conn, error) { called = true; return nil, nil }}
	cases := []struct {
		trace, stop, span, reason string
		seq                       int64
	}{
		{"bad", "stop-1", "", "user", 0},
		{"", "stop-1", "", "user", 0},
		{strings.Repeat("0", 32), "stop-1", "", "user", 0},
		{strings.ToUpper(replayTrace), "stop-1", "", "user", 0},
		{replayTrace, "", "", "user", 0},
		{replayTrace, "stop-1", "", "", 0},
		{replayTrace, "stop-1", "", "timeout", 0},
		{replayTrace, "stop-1", "", "policy", 0},
		{replayTrace, "stop-1", "", "policy", -1},
		{replayTrace, "stop-1", "", "user", 5},
		{replayTrace, "stop-1", "", "user", -1},
		{replayTrace, "stop-1", "shortspan", "user", 0},
	}
	for _, tc := range cases {
		if _, err := c.Stop(tc.trace, tc.stop, tc.span, tc.reason, tc.seq); err == nil || called {
			t.Fatalf("invalid stop dialed: %+v", tc)
		}
	}
	if _, err := c.StopQuery("bad", "stop-1"); err == nil || called {
		t.Fatal("invalid stop_query dialed")
	}
	if _, err := c.StopQuery(replayTrace, ""); err == nil || called {
		t.Fatal("empty stop_id dialed")
	}
}

// Plan C4: retry keeps the same stop_id and identical bytes; a conflict is
// surfaced verbatim; no execution writes either way.
func TestStopIdempotentAndConflictFRRHZ076(t *testing.T) {
	s, r := stopStore(t)
	es := execution.Service{Store: s}
	r, err := es.RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	var wires []string
	calls := 0
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		calls++
		raw, e := json.Marshal(q)
		if e != nil {
			t.Fatal(e)
		}
		wires = append(wires, string(raw))
		if calls == 2 {
			return map[string]any{"status": "error", "reason": "STOP_CONFLICT"}
		}
		return stopEcho(q)
	})}
	before := s.All()
	if _, err := SubmitStop(es, c, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := SubmitStop(es, c, r.ID); err == nil || err.Error() != "STOP_CONFLICT" {
		t.Fatal("conflict not surfaced:", err)
	}
	if len(wires) != 2 || wires[0] != wires[1] {
		t.Fatal("retry changed the wire content")
	}
	unchanged(t, before, s)
}

// Plan C5: no stop response is ever a cancellation; a dial failure is not a
// terminal fact and retry keeps the durable stop_id.
func TestStopResponseNeverCancelsFRRHZ076(t *testing.T) {
	build := func(t *testing.T) (*events.Store, execution.Service, execution.Ref) {
		s, r := stopStore(t)
		es := execution.Service{Store: s}
		r, err := es.RequestStop(r.ID, "user", 0, "operator")
		if err != nil {
			t.Fatal(err)
		}
		return s, es, r
	}
	t.Run("stop_accepted", func(t *testing.T) {
		s, es, r := build(t)
		before := s.All()
		got, err := SubmitStop(es, Client{Dial: fake(stopEcho)}, r.ID)
		if err != nil || got.Status != "stop_accepted" {
			t.Fatal(got, err)
		}
		unchanged(t, before, s)
		ref, err := execution.Replay(s.List("execution", r.ID))
		if err != nil || ref.State != execution.Accepted {
			t.Fatal("stop receipt changed execution state:", ref.State, err)
		}
	})
	t.Run("already_terminal", func(t *testing.T) {
		s, es, r := build(t)
		before := s.All()
		got, err := SubmitStop(es, Client{Dial: fake(func(q map[string]any) map[string]any {
			return map[string]any{"status": "already_terminal", "stop_id": q["stop_id"], "terminal_ref": 9}
		})}, r.ID)
		if err != nil || got.Status != "already_terminal" || got.TerminalRef != 9 {
			t.Fatal(got, err)
		}
		unchanged(t, before, s)
		ref, err := execution.Replay(s.List("execution", r.ID))
		if err != nil || ref.State == execution.Cancelled {
			t.Fatal("already_terminal treated as cancelled")
		}
	})
	t.Run("unavailable_then_retry", func(t *testing.T) {
		s, es, r := build(t)
		before := s.All()
		if _, err := SubmitStop(es, Client{Dial: func() (net.Conn, error) { return nil, errors.New("down") }}, r.ID); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
		unchanged(t, before, s)
		got, err := SubmitStop(es, Client{Dial: fake(func(q map[string]any) map[string]any {
			if q["stop_id"] != r.StopID {
				t.Fatal("retry minted a new stop_id")
			}
			return stopEcho(q)
		})}, r.ID)
		if err != nil || got.StopID != r.StopID {
			t.Fatal(got, err)
		}
	})
}

// Plan C6 (+L1): strict stop response — pollution refused, the JANUS error
// vocabulary surfaced verbatim, unknown only valid for stop_query.
func TestStopResponseStrictFRRHZ076(t *testing.T) {
	stop := func(resp map[string]any) error {
		c := Client{Dial: fake(func(q map[string]any) map[string]any { return resp })}
		_, err := c.Stop(replayTrace, "stop-1", "", "user", 0)
		return err
	}
	invalid := map[string]map[string]any{
		"invented_status":      {"status": "stopping"},
		"unknown_field":        {"status": "stop_accepted", "stop_id": "stop-1", "reason": "user", "weird": 1},
		"terminal_ref_string":  {"status": "already_terminal", "stop_id": "stop-1", "terminal_ref": "x"},
		"terminal_ref_missing": {"status": "already_terminal", "stop_id": "stop-1"},
		"stop_id_number":       {"status": "stop_accepted", "stop_id": 3, "reason": "user"},
		"stop_id_mismatch":     {"status": "stop_accepted", "stop_id": "other", "reason": "user"},
		"stop_id_missing":      {"status": "stop_accepted", "reason": "user"},
		"reason_mismatch":      {"status": "stop_accepted", "stop_id": "stop-1", "reason": "policy"},
		"error_without_reason": {"status": "error"},
		"null_status":          {"status": nil},
		"unknown_for_stop":     {"status": "unknown", "stop_id": "stop-1"},
	}
	for name, resp := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := stop(resp); err == nil {
				t.Fatal("polluted stop response accepted")
			}
		})
	}
	// L1: the relay error vocabulary is surfaced verbatim, never reclassified.
	for _, reason := range []string{"REQUEST_MISMATCH", "STOP_CONFLICT", "UNAUTHORIZED", "UNAUTHENTICATED", "UNAVAILABLE"} {
		t.Run(reason, func(t *testing.T) {
			if err := stop(map[string]any{"status": "error", "reason": reason}); err == nil || err.Error() != reason {
				t.Fatal(reason, err)
			}
		})
	}
	// Pinned from JANUS handleStop: an unknown key answers unknown+stop_id,
	// and that classification is only legal on the stop_query op.
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		if q["op"] != "stop_query" {
			t.Fatal(q["op"])
		}
		return map[string]any{"status": "unknown", "stop_id": q["stop_id"]}
	})}
	r, err := c.StopQuery(replayTrace, "stop-1")
	if err != nil || r.Status != "unknown" || r.StopID != "stop-1" {
		t.Fatal(r, err)
	}
}

// Plan C7: cancelled is concluded exclusively by the stage-B replay path
// after the stop receipt.
func TestStoppedViaReplayOnlyFRRHZ076(t *testing.T) {
	s, r := stopStore(t)
	es := execution.Service{Store: s}
	r, err := es.RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	got, err := SubmitStop(es, Client{Dial: fake(stopEcho)}, r.ID)
	if err != nil || got.Status != "stop_accepted" {
		t.Fatal(got, err)
	}
	for _, ev := range s.All() {
		if ev.Type == "execution.observed" {
			t.Fatal("stop client wrote an observation")
		}
	}
	b := parsed(t, replayLine(1, "subagent/done", `{"status":"stopped"}`)+replayLine(2, "session/end", "{}"), 0)
	ref, err := ObserveBatch(es, approval.Service{Store: s}, r.ID, replaySession, b)
	if err != nil || ref.State != execution.Cancelled {
		t.Fatal(ref.State, err)
	}
	if !ref.StopRequested || ref.StopID != r.StopID {
		t.Fatal("stop intent lost across replay")
	}
}
