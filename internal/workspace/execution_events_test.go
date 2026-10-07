package workspace

// FR-RHZ-083 (T25 multi-turn): the /v1/execution outbound emit projects each
// bound session's JANUS log into the events array. Uses helpers from
// executionhttp_test.go (execHTTPFixture, acceptedExec, getExecution,
// decodeExecution, execTrace, pad19).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/execution"
)

// boundExec accepts and binds an execution so it carries a session-log path.
func boundExec(t *testing.T, s *events.Store, missionID, key, sessionDB string) execution.Ref {
	t.Helper()
	r := acceptedExec(t, s, missionID, key)
	r, err := (execution.Service{Store: s}).Bind(r.ID, execution.Binding{
		SessionDB: sessionDB, TraceID: execTrace, PolicyHash: "ph", RequestFingerprint: "fp",
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// FR-RHZ-083: an injected source projects a bound session's events (with usage)
// into the flat events array, tagged with the owning session id.
func TestExecutionEmitProjectsSessionEventsFRRHZ083(t *testing.T) {
	s, h, srv := execHTTPFixture(t)
	boundExec(t, s, "m", "sess", "/tmp/example-session.db")
	var gotDB, gotTrace string
	h.ExecEvents = func(sessionDB, traceID string) ([]SessionEvent, error) {
		gotDB, gotTrace = sessionDB, traceID
		return []SessionEvent{
			{Seq: 1, Kind: "session/start", Actor: "parent", TS: 10},
			{Seq: 2, Kind: "usage", Actor: "child", TS: 20, UsageIn: 120, UsageOut: 45},
		}, nil
	}
	code, raw := getExecution(t, srv.URL, "m")
	if code != 200 {
		t.Fatal(code, string(raw))
	}
	if gotDB != "/tmp/example-session.db" || gotTrace != execTrace {
		t.Fatalf("source not called with binding: %q %q", gotDB, gotTrace)
	}
	var env struct {
		Body struct {
			Events []map[string]any `json:"events"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Body.Events) != 2 {
		t.Fatalf("events: %s", raw)
	}
	e0, e1 := env.Body.Events[0], env.Body.Events[1]
	if e0["sessionId"] != "exec-sess" || e0["kind"] != "session/start" || fmt.Sprint(e0["seq"]) != "1" {
		t.Fatalf("event0: %v", e0)
	}
	if fmt.Sprint(e1["usageIn"]) != "120" || fmt.Sprint(e1["usageOut"]) != "45" || e1["actor"] != "child" {
		t.Fatalf("event1 usage: %v", e1)
	}
	// camelCase keys only, and the raw payload/args never leak.
	for k := range e0 {
		switch k {
		case "sessionId", "seq", "kind", "actor", "ts", "usageIn", "usageOut":
		default:
			t.Fatalf("unexpected event key %q", k)
		}
	}
}

// FR-RHZ-083: with no source injected the events array stays empty (backward
// compatible) — no synthesis from Rhizome's own journal.
func TestExecutionEmitNoSourceEmptyEventsFRRHZ083(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	boundExec(t, s, "m", "sess", "/tmp/example-session.db")
	_, raw := getExecution(t, srv.URL, "m")
	if !strings.Contains(string(raw), `"events":[]`) {
		t.Fatalf("events not empty without source: %s", raw)
	}
}

// FR-RHZ-083: a session without a binding is never queried and contributes no
// events, even when a source is present.
func TestExecutionEmitUnboundSessionNoQueryFRRHZ083(t *testing.T) {
	s, h, srv := execHTTPFixture(t)
	acceptedExec(t, s, "m", "unbound") // accepted, no Bind
	called := false
	h.ExecEvents = func(sessionDB, traceID string) ([]SessionEvent, error) {
		called = true
		return nil, nil
	}
	_, raw := getExecution(t, srv.URL, "m")
	if called {
		t.Fatal("unbound session queried")
	}
	if !strings.Contains(string(raw), `"events":[]`) {
		t.Fatalf("events not empty: %s", raw)
	}
}

// FR-RHZ-083: a projection fault surfaces as an error (HTTP 500) — the emit
// never fabricates or gap-fills events it cannot derive.
func TestExecutionEmitProjectionErrorSurfacesFRRHZ083(t *testing.T) {
	s, h, srv := execHTTPFixture(t)
	boundExec(t, s, "m", "sess", "/tmp/example-session.db")
	h.ExecEvents = func(sessionDB, traceID string) ([]SessionEvent, error) {
		return nil, fmt.Errorf("OBSERVATION_CORRUPT: sequence")
	}
	code, _ := getExecution(t, srv.URL, "m")
	if code != 500 {
		t.Fatalf("projection error not surfaced: %d", code)
	}
}
