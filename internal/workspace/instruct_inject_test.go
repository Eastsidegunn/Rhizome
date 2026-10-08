package workspace

// RHZ-093 relay tests (FR-RHZ-119): task.instruct records first,
// then injects into the mission's running session through the injected
// hook; rejections keep the record and carry the reason verbatim. The hook
// is a fake — no adapter, no socket.

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

const injectTrace = "0123456789abcdef0123456789abcdef"

// boundExecution creates a bound execution for mission m in the given state.
func boundExecution(t *testing.T, s *events.Store, key, trace string, st execution.State) execution.Ref {
	t.Helper()
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	r, err := es.IntentWithPolicy("m", key, p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Accept(r.ID, trace); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Bind(r.ID, execution.Binding{SessionDB: "/tmp/s.db", TraceID: trace, PolicyHash: "policy", RequestFingerprint: "request"}); err != nil {
		t.Fatal(err)
	}
	if st != execution.Accepted {
		if r, err = es.ObserveState(r.ID, fmt.Sprintf("%019d", 1), "JANUS tail: observing", "/tmp/s.db", st); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

type injectCall struct{ trace, text string }

// recordingInjector asserts the instruction is already durable at call time
// (record → inject order) and answers with the scripted result.
func recordingInjector(t *testing.T, s *events.Store, calls *[]injectCall, seq int64, reason string, err error) ExecInjector {
	return func(trace, text string) (int64, string, error) {
		t.Helper()
		if n := len(s.List("surface", "surface-m")); n != 1 {
			t.Errorf("inject before record: surface events %d", n)
		}
		*calls = append(*calls, injectCall{trace, text})
		return seq, reason, err
	}
}

func instruct(t *testing.T, s *events.Store, inject ExecInjector) RelayResult {
	t.Helper()
	r, e := RelayIntentWith(s, Intent{Kind: "task.instruct", TaskID: "m", Instruction: "do it"}, "op", noAuthority(), inject)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestRelayInstructNoExecutionUnchangedFRRHZ119(t *testing.T) {
	s := ws(t)
	calls := []injectCall{}
	before := len(s.All())
	r := instruct(t, s, recordingInjector(t, s, &calls, 1, "", nil))
	if !r.Accepted || r.Reason != "" || len(calls) != 0 {
		t.Fatalf("%+v calls=%v", r, calls)
	}
	if len(s.All()) != before+1 || len(s.List("surface", "surface-m")) != 1 {
		t.Fatal("instruction not recorded exactly once")
	}
}

func TestRelayInstructInjectsWhenRunningFRRHZ119(t *testing.T) {
	for _, st := range []execution.State{execution.Accepted, execution.Observing} {
		t.Run(string(st), func(t *testing.T) {
			s := ws(t)
			boundExecution(t, s, "k1", injectTrace, st)
			calls := []injectCall{}
			before := len(s.All())
			r := instruct(t, s, recordingInjector(t, s, &calls, 5, "", nil))
			if !r.Accepted || r.Reason != "" {
				t.Fatalf("%+v", r)
			}
			if !reflect.DeepEqual(calls, []injectCall{{injectTrace, "do it"}}) {
				t.Fatalf("calls: %v", calls)
			}
			if len(s.All()) != before+1 {
				t.Fatalf("journal grew by %d (want 1: the instruction only)", len(s.All())-before)
			}
		})
	}
}

func TestRelayInstructRejectionMapsReasonFRRHZ119(t *testing.T) {
	for _, tc := range []struct {
		reason string
		seq    int64
		err    error
		want   string
	}{
		{reason: "UNKNOWN_SESSION", want: "UNKNOWN_SESSION"},
		{reason: "NOT_MULTITURN", want: "NOT_MULTITURN"},
		{reason: "SESSION_TERMINAL", want: "SESSION_TERMINAL"},
		{reason: "SESSION_NOT_READY", want: "SESSION_NOT_READY"},
		{reason: "DELIVERY_FAILED", seq: 7, want: "DELIVERY_FAILED"},
		{reason: "REQUEST_MISMATCH", want: "REQUEST_MISMATCH"},
		{reason: "LOG_UNAVAILABLE", want: "LOG_UNAVAILABLE"},
		{err: errors.New("UNAVAILABLE"), want: "UNAVAILABLE"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			s := ws(t)
			boundExecution(t, s, "k1", injectTrace, execution.Observing)
			calls := []injectCall{}
			before := len(s.All())
			r := instruct(t, s, recordingInjector(t, s, &calls, tc.seq, tc.reason, tc.err))
			if r.Accepted || r.Reason != tc.want {
				t.Fatalf("%+v", r)
			}
			if len(calls) != 1 {
				t.Fatalf("calls: %v", calls)
			}
			all := s.All()
			if len(all) != before+1 || all[len(all)-1].AggregateType != "surface" || len(s.List("surface", "surface-m")) != 1 {
				t.Fatal("rejected instruction must stay recorded, and only it")
			}
		})
	}
}

func TestRelayInstructNilInjectUnchangedFRRHZ119(t *testing.T) {
	s := ws(t)
	boundExecution(t, s, "k1", injectTrace, execution.Observing)
	before := len(s.All())
	r, e := RelayIntent(s, Intent{Kind: "task.instruct", TaskID: "m", Instruction: "do it"}, "op", noAuthority())
	if e != nil || !r.Accepted || r.Reason != "" || len(s.All()) != before+1 {
		t.Fatalf("%+v %v", r, e)
	}
}

// Only a bound Accepted/Observing execution of THIS mission is a target:
// terminal, unbound and foreign-mission executions never trigger the hook.
func TestRelayInstructSkipsNonRunningExecutionFRRHZ119(t *testing.T) {
	s := ws(t)
	boundExecution(t, s, "k-done", injectTrace, execution.Succeeded)
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	// Accepted but UNBOUND (no Binding/trace yet): must be skipped, never
	// dereferenced (removing the Binding!=nil check would panic).
	if ru, err := es.IntentWithPolicy("m", "k-unbound", p, p); err != nil {
		t.Fatal(err)
	} else if ru, err = es.ClaimDispatch(ru.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	} else if _, err = es.Accept(ru.ID, "trace-unbound"); err != nil {
		t.Fatalf("accept unbound: %v", err)
	}
	if _, err := es.IntentWithPolicy("m", "k-intent", p, p); err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: s}).Create("m2", "g", "other", "ok"); err != nil {
		t.Fatal(err)
	}
	boundExecution2 := func() {
		r, err := es.IntentWithPolicy("m2", "k-other", p, p)
		if err != nil {
			t.Fatal(err)
		}
		if r, err = es.ClaimDispatch(r.ID, "janus", "corr"); err != nil {
			t.Fatal(err)
		}
		if r, err = es.Accept(r.ID, "abcdef0123456789abcdef0123456789"); err != nil {
			t.Fatal(err)
		}
		if _, err = es.Bind(r.ID, execution.Binding{SessionDB: "/tmp/o.db", TraceID: "abcdef0123456789abcdef0123456789", PolicyHash: "policy", RequestFingerprint: "request"}); err != nil {
			t.Fatal(err)
		}
	}
	boundExecution2() // Running, but for another mission.
	calls := []injectCall{}
	r := instruct(t, s, recordingInjector(t, s, &calls, 1, "", nil))
	if !r.Accepted || r.Reason != "" || len(calls) != 0 {
		t.Fatalf("%+v calls=%v", r, calls)
	}
}

// Two running sessions for one mission are ambiguous: recorded, not injected.
func TestRelayInstructAmbiguousSessionsNotInjectedFRRHZ119(t *testing.T) {
	s := ws(t)
	boundExecution(t, s, "k1", injectTrace, execution.Observing)
	boundExecution(t, s, "k2", "abcdef0123456789abcdef0123456789", execution.Observing)
	calls := []injectCall{}
	r := instruct(t, s, recordingInjector(t, s, &calls, 1, "", nil))
	if r.Accepted || r.Reason != "multiple running executions for mission" || len(calls) != 0 || len(s.List("surface", "surface-m")) != 1 {
		t.Fatalf("%+v calls=%v", r, calls)
	}
}

// No automatic retry: one relay = exactly one injection attempt, even when
// it fails (send_message is not idempotent by contract).
func TestRelayInstructNoAutoRetryFRRHZ119(t *testing.T) {
	s := ws(t)
	boundExecution(t, s, "k1", injectTrace, execution.Observing)
	calls := []injectCall{}
	instruct(t, s, recordingInjector(t, s, &calls, 0, "", errors.New("UNAVAILABLE")))
	if len(calls) != 1 {
		t.Fatalf("attempts: %d", len(calls))
	}
}

func TestIntentInstructInjectsViaHTTPFRRHZ119(t *testing.T) {
	s := ws(t)
	boundExecution(t, s, "k1", injectTrace, execution.Observing)
	h := NewHTTP(s)
	calls := []injectCall{}
	h.ExecInject = recordingInjector(t, s, &calls, 3, "", nil)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	out := postIntent(t, srv.URL, `{"kind":"task.instruct","taskId":"m","instruction":"do it","actor":"alice"}`)
	if out["Accepted"] != true || out["Reason"] != "" || len(calls) != 1 || calls[0].text != "do it" {
		t.Fatalf("%v calls=%v", out, calls)
	}
	h.ExecInject = func(trace, text string) (int64, string, error) { return 0, "SESSION_TERMINAL", nil }
	out = postIntent(t, srv.URL, `{"kind":"task.instruct","taskId":"m","instruction":"again","actor":"alice"}`)
	if out["Accepted"] != false || out["Reason"] != "SESSION_TERMINAL" || len(s.List("surface", "surface-m")) != 2 {
		t.Fatalf("%v", out)
	}
}

// Same inputs twice → identical journals; the injection leaves no record of
// its own.
func TestRelayInstructDeterministicFRRHZ119(t *testing.T) {
	run := func(reason string) []string {
		s := ws(t)
		boundExecution(t, s, "k1", injectTrace, execution.Observing)
		calls := []injectCall{}
		instruct(t, s, recordingInjector(t, s, &calls, 4, reason, nil))
		out := []string{}
		for _, e := range s.All() {
			out = append(out, e.AggregateType+"/"+e.AggregateID+"/"+e.Type+"/"+string(e.Payload))
		}
		return out
	}
	if a, b := run(""), run(""); !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic:\n%v\n%v", a, b)
	}
	if a, b := run(""), run("SESSION_TERMINAL"); !reflect.DeepEqual(a, b) {
		t.Fatalf("injection outcome leaked into the journal:\n%v\n%v", a, b)
	}
}
