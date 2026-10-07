package workspace

// RHZ-093 (FR-RHZ-119): additive /v1/execution session status —
// derived only from the projected JANUS session log, absent (byte-identical
// v1 shape) without a source.

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/execution"
)

func bindExec(t *testing.T, s *events.Store, id string) {
	t.Helper()
	if _, err := (execution.Service{Store: s}).Bind(id, execution.Binding{SessionDB: "/tmp/s.db", TraceID: execTrace, PolicyHash: "policy", RequestFingerprint: "request"}); err != nil {
		t.Fatal(err)
	}
}

func fixedEvents(evs []SessionEvent) SessionEventSource {
	return func(sessionDB, traceID string) ([]SessionEvent, error) { return evs, nil }
}

func sessionKeys(t *testing.T, raw []byte) (map[string]json.RawMessage, []string) {
	t.Helper()
	var env struct {
		Body struct {
			Sessions []map[string]json.RawMessage `json:"sessions"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Body.Sessions) != 1 {
		t.Fatal(err, string(raw))
	}
	keys := []string{}
	for k := range env.Body.Sessions[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return env.Body.Sessions[0], keys
}

func TestExecutionSnapshotSessionStatusAndUsageFRRHZ119(t *testing.T) {
	for name, tc := range map[string]struct {
		evs  []SessionEvent
		want SessionStatus
	}{
		"pending_empty_log": {evs: []SessionEvent{}, want: SessionStatus{JanusState: "pending"}},
		"pending_before_spawn": {evs: []SessionEvent{{Seq: 1, Kind: "session/start", TS: 10, UsageIn: 1}},
			want: SessionStatus{JanusState: "pending", UsageInTotal: 1, LastActivityTS: 10}},
		"running_sums_usage": {evs: []SessionEvent{{Seq: 1, Kind: "subagent/spawn", TS: 10}, {Seq: 2, Kind: "usage", TS: 20, UsageIn: 3, UsageOut: 4}, {Seq: 3, Kind: "tool_call", TS: 30, UsageIn: 5, UsageOut: 6}},
			want: SessionStatus{JanusState: "running", UsageInTotal: 8, UsageOutTotal: 10, LastActivityTS: 30}},
		"exited_on_done": {evs: []SessionEvent{{Seq: 1, Kind: "subagent/spawn", TS: 10}, {Seq: 2, Kind: "subagent/done", TS: 40}},
			want: SessionStatus{JanusState: "exited", LastActivityTS: 40}},
		"exited_on_session_end": {evs: []SessionEvent{{Seq: 1, Kind: "subagent/spawn", TS: 10}, {Seq: 2, Kind: "session/end", TS: 50}},
			want: SessionStatus{JanusState: "exited", LastActivityTS: 50}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := execHTTPFixture(t)
			r := acceptedExec(t, s, "m", "k")
			bindExec(t, s, r.ID)
			p, err := ExecutionSnapshot(s, "m", fixedEvents(tc.evs))
			if err != nil || len(p.Sessions) != 1 || p.Sessions[0].Status == nil {
				t.Fatalf("%+v %v", p, err)
			}
			if got := *p.Sessions[0].Status; got != tc.want {
				t.Fatalf("status %+v want %+v", got, tc.want)
			}
			// The v1 state mapping is untouched by the JANUS state.
			if p.Sessions[0].State != "running" {
				t.Fatalf("v1 state changed: %+v", p.Sessions[0])
			}
		})
	}
	// Overflow is an error, never a wrapped total.
	s, _, _ := execHTTPFixture(t)
	r := acceptedExec(t, s, "m", "k")
	bindExec(t, s, r.ID)
	_, err := ExecutionSnapshot(s, "m", fixedEvents([]SessionEvent{{Seq: 1, Kind: "x", TS: 1, UsageIn: math.MaxInt64}, {Seq: 2, Kind: "x", TS: 2, UsageIn: 1}}))
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow not rejected: %v", err)
	}
	// Without a binding there is no log to project: no status.
	s, _, _ = execHTTPFixture(t)
	acceptedExec(t, s, "m", "k")
	p, err := ExecutionSnapshot(s, "m", fixedEvents([]SessionEvent{{Seq: 1, Kind: "subagent/spawn", TS: 1}}))
	if err != nil || len(p.Sessions) != 1 || p.Sessions[0].Status != nil {
		t.Fatalf("status without binding: %+v %v", p, err)
	}
}

// Without a source the session object carries exactly the v1 keys (RHZ-046
// golden); with one, the additive keys appear and are omitempty-shaped.
func TestExecutionDTOBackwardCompatibleFRRHZ119(t *testing.T) {
	s, h, srv := execHTTPFixture(t)
	r := acceptedExec(t, s, "m", "k")
	bindExec(t, s, r.ID)
	observeExec(t, s, r.ID, pad19(1), "요약", execution.Observing)
	_, raw := getExecution(t, srv.URL, "m")
	_, keys := sessionKeys(t, raw)
	if !reflect.DeepEqual(keys, []string{"id", "label", "state", "taskId"}) {
		t.Fatalf("v1 keys changed without a source: %v", keys)
	}
	h.ExecEvents = fixedEvents([]SessionEvent{{Seq: 1, Kind: "subagent/spawn", Actor: "parent", TS: 1700000000000, UsageIn: 2, UsageOut: 3}})
	_, raw = getExecution(t, srv.URL, "m")
	sess, keys := sessionKeys(t, raw)
	if !reflect.DeepEqual(keys, []string{"id", "janusState", "label", "lastActivityTs", "state", "taskId", "usageInTotal", "usageOutTotal"}) {
		t.Fatalf("keys with source: %v", keys)
	}
	if string(sess["janusState"]) != `"running"` || string(sess["usageInTotal"]) != "2" || string(sess["usageOutTotal"]) != "3" || string(sess["lastActivityTs"]) != "1700000000000" {
		t.Fatalf("values: %s", raw)
	}
	if _, ok := sess["doneStatus"]; ok {
		t.Fatal("doneStatus is not derivable and must not be emitted")
	}
	// Zero totals are still present (presence = source wired), not dropped.
	h.ExecEvents = fixedEvents([]SessionEvent{})
	_, raw = getExecution(t, srv.URL, "m")
	sess, _ = sessionKeys(t, raw)
	if string(sess["janusState"]) != `"pending"` || string(sess["usageInTotal"]) != "0" || string(sess["lastActivityTs"]) != "0" {
		t.Fatalf("zero values dropped: %s", raw)
	}
}

// Same journal + same projection → identical bytes; the status is a pure
// function of its inputs (no clock, no process state).
func TestExecutionStatusDeterministicFRRHZ119(t *testing.T) {
	evs := []SessionEvent{{Seq: 1, Kind: "subagent/spawn", Actor: "parent", TS: 10, UsageIn: 1}, {Seq: 2, Kind: "usage", Actor: "parent", TS: 20, UsageOut: 2}}
	render := func() []byte {
		s, h, srv := execHTTPFixture(t)
		r := acceptedExec(t, s, "m", "k")
		bindExec(t, s, r.ID)
		h.ExecEvents = fixedEvents(evs)
		_, raw := getExecution(t, srv.URL, "m")
		return raw
	}
	a, b := render(), render()
	if string(a) != string(b) {
		t.Fatalf("non-deterministic:\n%s\n%s", a, b)
	}
	// Repeated projection of the same snapshot is stable too.
	s, h, srv := execHTTPFixture(t)
	r := acceptedExec(t, s, "m", "k")
	bindExec(t, s, r.ID)
	h.ExecEvents = fixedEvents(evs)
	_, x := getExecution(t, srv.URL, "m")
	_, y := getExecution(t, srv.URL, "m")
	if string(x) != string(y) {
		t.Fatal("unstable across requests")
	}
}
