package janusadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

const replayTrace = "0123456789abcdef0123456789abcdef"
const replaySpan = "0123456789abcdef"
const replaySession = "/tmp/rhz-043-test-session.db"

func replayLine(seq int, kind, payload string) string {
	return `{"actor":"parent","kind":"` + kind + `","payload":` + payload + `,"seq":` + strconv.Itoa(seq) + `,"span_id":"` + replaySpan + `","trace_id":"` + replayTrace + `","ts":1}` + "\n"
}
func policyLine(seq int) string {
	return replayLine(seq, "policy/decision", `{"decision":"deny","profile_id":"manual","request_id":"r1","response_id":"resp","reason":"EXPIRED"}`)
}
func parsed(t *testing.T, s string, from int64) Batch {
	t.Helper()
	b, err := ParseReplay(strings.NewReader(s), replayTrace, from)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func corruptParse(t *testing.T, s string, from int64, target error) {
	t.Helper()
	b, err := ParseReplay(strings.NewReader(s), replayTrace, from)
	if !errors.Is(err, target) || !reflect.DeepEqual(b, Batch{}) {
		t.Fatalf("error=%v batch=%+v", err, b)
	}
}
func mutateLine(t *testing.T, line string, change func(map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

// Real domain services, not hand-written valid aggregate payloads.
func observationStore(t *testing.T) (*events.Store, execution.Ref, approval.Ref) {
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
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	r, err := es.IntentWithPolicy("m", "observation", p, p)
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.ClaimDispatch(r.ID, "janus", "corr")
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.Accept(r.ID, replayTrace)
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.Bind(r.ID, execution.Binding{SessionDB: replaySession, TraceID: replayTrace, PolicyHash: "policy", RequestFingerprint: "request"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := (approval.Service{Store: s}).RecordInput(approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "r1"}, approval.Allow, "", "resp", "hx-args-digest-v1:opaque", "operator", "corr", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return s, r, a
}
func observe(t *testing.T, s events.Port, id string, b Batch) execution.Ref {
	t.Helper()
	r, err := ObserveBatch(execution.Service{Store: s}, approval.Service{Store: s}, id, replaySession, b)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := execution.Replay(s.List("execution", id))
	if err != nil || !reflect.DeepEqual(r, replayed) {
		t.Fatalf("returned/replayed ref mismatch: %v", err)
	}
	return r
}
func unchanged(t *testing.T, before []events.Event, s events.Port) {
	t.Helper()
	after := s.All()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("log changed")
	}
	for i := range before {
		if !bytes.Equal(before[i].Payload, after[i].Payload) {
			t.Fatal("payload changed")
		}
	}
}
func clonedStore(t *testing.T, log []events.Event) *events.Store {
	t.Helper()
	s := &events.Store{}
	for _, ev := range log {
		if err := s.Append(ev.Revision-1, ev); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// Plan 1: full summary, durable response coordinates, no raw or args retained.
func TestParseReplaySummaryFRRHZ076(t *testing.T) {
	first := mutateLine(t, replayLine(1, "session/start", "{}"), func(m map[string]any) { m["raw"] = "RAW_SECRET" })
	s := first + replayLine(2, "subagent/approval_request", `{"args":{"token":"ARGS_SECRET"},"call_id":"call","name":"tool","reason":"why","request_id":"r1"}`) + policyLine(3) + replayLine(4, "subagent/done", `{"status":"ok","result":"r"}`) + replayLine(5, "session/end", "{}")
	b := parsed(t, s, 0)
	if b.FromSeq != 0 || b.ThroughSeq != 5 || !b.SessionEnded || b.TraceID != replayTrace || b.SessionRef != "" || !reflect.DeepEqual(b.Done, []SubagentDone{{Result: "r", Status: "ok"}}) {
		t.Fatalf("%+v", b)
	}
	want := []ApprovalResponse{{Seq: 3, Key: approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "r1"}, Decision: approval.Deny, Reason: "EXPIRED", ResponseID: "resp"}}
	if !reflect.DeepEqual(b.ApprovalResponses, want) {
		t.Fatalf("%+v", b.ApprovalResponses)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("RAW_SECRET")) || bytes.Contains(raw, []byte("ARGS_SECRET")) {
		t.Fatal("raw content retained")
	}
}

// Plan 2, stage A carry-over: genuinely broken JSON after a valid prefix.
func TestParseReplayMalformedJSONFRRHZ076(t *testing.T) {
	corruptParse(t, policyLine(1)+"{invalid\n"+replayLine(3, "session/end", "{}"), 0, ErrObservationCorrupt)
	for _, bad := range []string{"null\n", "\n", "[]\n"} {
		corruptParse(t, bad, 0, ErrObservationCorrupt)
	}
}

// Plan 3: also check trace identity in the cursor prefix.
func TestParseReplayTraceReplacementFRRHZ076(t *testing.T) {
	other := strings.Repeat("a", 32)
	wrong := strings.ReplaceAll(replayLine(1, "session/start", "{}"), replayTrace, other)
	corruptParse(t, wrong, 0, ErrSessionReplaced)
	corruptParse(t, replayLine(1, "session/start", "{}")+strings.ReplaceAll(replayLine(2, "session/end", "{}"), replayTrace, other), 2, ErrSessionReplaced)
}

// Plan 4: zero, negative, duplicate, reversal, gaps (including first seq).
func TestParseReplaySequenceIntegrityFRRHZ076(t *testing.T) {
	for _, seqs := range [][]int{{0}, {-1}, {2}, {1, 1}, {1, 2, 1}, {1, 3}} {
		t.Run(fmt.Sprint(seqs), func(t *testing.T) {
			var s strings.Builder
			for _, n := range seqs {
				s.WriteString(replayLine(n, "other", "{}"))
			}
			corruptParse(t, s.String(), 0, ErrObservationCorrupt)
		})
	}
}

// Plan 5, revised: prefix responses/done are collected; store dedupes writes.
func TestParseReplayPrefixValidationFRRHZ076(t *testing.T) {
	s, r, a := observationStore(t)
	prefix := policyLine(1) + replayLine(2, "subagent/done", `{"status":"ok"}`)
	first := observe(t, s, r.ID, parsed(t, prefix, 0))
	if first.State != execution.Observing {
		t.Fatal(first.State)
	}
	approvals := s.List("approval", a.ID)
	b := parsed(t, prefix+replayLine(3, "session/end", "{}"), 2)
	if len(b.ApprovalResponses) != 1 || len(b.Done) != 1 || b.FromSeq != 2 || b.ThroughSeq != 3 {
		t.Fatalf("%+v", b)
	}
	next := observe(t, s, r.ID, b)
	if next.State != execution.Succeeded || next.Cursor != "0000000000000000003" {
		t.Fatalf("%+v", next)
	}
	if !reflect.DeepEqual(approvals, s.List("approval", a.ID)) {
		t.Fatal("prefix approval duplicated")
	}
	before := s.All()
	observe(t, s, r.ID, b)
	unchanged(t, before, s)
	corruptParse(t, replayLine(1, "other", "{}")+replayLine(1, "other", "{}"), 2, ErrObservationCorrupt)
	corruptParse(t, replayLine(1, "other", "{}")+replayLine(3, "other", "{}"), 3, ErrObservationCorrupt)
}

// Plan 6: empty is also shorter than a positive cursor; equal length is valid.
func TestParseReplayTruncatedHistoryFRRHZ076(t *testing.T) {
	corruptParse(t, "", 5, ErrObservationCorrupt)
	var s string
	for n := 1; n <= 4; n++ {
		s += replayLine(n, "other", "{}")
	}
	corruptParse(t, s, 5, ErrObservationCorrupt)
	b := parsed(t, s+replayLine(5, "other", "{}"), 5)
	if b.ThroughSeq != 5 || len(b.ApprovalResponses) != 0 {
		t.Fatal(b)
	}
}

// Plan 7: empty input is valid only at zero; caller trace must be valid hex.
func TestParseReplayEmptyAndInvalidArgumentsFRRHZ076(t *testing.T) {
	b := parsed(t, "", 0)
	if b.ThroughSeq != 0 || b.SessionEnded || len(b.Done) != 0 {
		t.Fatal(b)
	}
	for _, tc := range []struct {
		trace string
		from  int64
	}{{replayTrace, -1}, {"", 0}, {"bad", 0}, {strings.Repeat("A", 32), 0}} {
		b, err := ParseReplay(strings.NewReader(""), tc.trace, tc.from)
		if err == nil || !reflect.DeepEqual(b, Batch{}) {
			t.Fatal(b, err)
		}
	}
	if _, err := ParseReplay(nil, replayTrace, 0); err == nil {
		t.Fatal("nil reader")
	}
}

type fakeErrorReader struct{ err error }

func (f fakeErrorReader) Read([]byte) (int, error) { return 0, f.err }

// Plan 8: discard completed prefix on a subsequent I/O error.
func TestParseReplayReaderFailureFRRHZ076(t *testing.T) {
	injected := errors.New("injected read failure")
	b, err := ParseReplay(io.MultiReader(strings.NewReader(policyLine(1)), fakeErrorReader{injected}), replayTrace, 0)
	if !errors.Is(err, ErrObservationCorrupt) || !errors.Is(err, injected) || !reflect.DeepEqual(b, Batch{}) {
		t.Fatal(b, err)
	}
}

// Plan 9: additions to kind/payload vocabulary are intentionally accepted.
func TestParseReplayForwardCompatibilityFRRHZ076(t *testing.T) {
	b := parsed(t, replayLine(1, "future/kind", `{"decision":9,"anything":[1,2]}`)+replayLine(2, "subagent/done", `{"status":"ok","future":{"x":1}}`), 0)
	if b.ThroughSeq != 2 || len(b.Done) != 1 || b.SessionEnded || len(b.ApprovalResponses) != 0 {
		t.Fatal(b)
	}
}

// Plan 10: all known fields are checked, including optional fields and null.
func TestParseReplayKnownFieldTypesFRRHZ076(t *testing.T) {
	for _, k := range []string{"actor", "kind", "parent_span_id", "raw", "span_id", "trace_id", "seq", "ts", "usage_in", "usage_out", "payload"} {
		for _, bad := range []any{nil, []any{1}} {
			t.Run(k+fmt.Sprint(bad), func(t *testing.T) {
				line := mutateLine(t, replayLine(1, "other", "{}"), func(m map[string]any) { m[k] = bad })
				corruptParse(t, line, 0, ErrObservationCorrupt)
			})
		}
	}
	for _, k := range []string{"actor", "kind", "seq", "span_id", "trace_id", "ts"} {
		t.Run("missing_"+k, func(t *testing.T) {
			line := mutateLine(t, replayLine(1, "other", "{}"), func(m map[string]any) { delete(m, k) })
			corruptParse(t, line, 0, ErrObservationCorrupt)
		})
	}
	cases := map[string][]string{
		"subagent/done":             {"result", "status"},
		"subagent/approval_request": {"call_id", "name", "reason", "request_id"},
		"policy/decision":           {"decision", "profile_id", "request_id", "response_id", "reason", "decision_source", "actor_ref", "operation_id", "correlation_id", "human_intent_id"},
	}
	for kind, fields := range cases {
		for _, k := range fields {
			for _, bad := range []any{17, nil} {
				t.Run(kind+"/"+k+fmt.Sprint(bad), func(t *testing.T) {
					line := mutateLine(t, replayLine(1, kind, `{"decision":"allow","profile_id":"p","status":"ok"}`), func(m map[string]any) { m["payload"].(map[string]any)[k] = bad })
					corruptParse(t, line, 1, ErrObservationCorrupt)
				})
			}
		}
	}
}

// Plan 11: request_id is optional; reason for deny is never synthesized.
func TestParseReplayPolicyDecisionValidationFRRHZ076(t *testing.T) {
	for _, p := range []string{`{"decision":"allow","profile_id":"p"}`, `{"decision":"deny","profile_id":"p","reason":"policy"}`, `{"decision":"allow","profile_id":"p","request_id":"r1"}`} {
		parsed(t, replayLine(1, "policy/decision", p), 0)
	}
	for _, p := range []string{`{"decision":"deny","profile_id":"p","decision_source":"forced"}`, `{"decision":"deny","profile_id":"p","reason":" "}`, `{"decision":"automatic","profile_id":"p"}`, `{"decision":"allow"}`, `{"profile_id":"p"}`} {
		corruptParse(t, replayLine(1, "policy/decision", p), 0, ErrObservationCorrupt)
	}
}

// Plan 12: a subagent finishing never implies session completion by itself.
func TestExecutionRemainsObservingWithoutSessionEndFRRHZ076(t *testing.T) {
	for _, status := range []string{"ok", "stopped", "error"} {
		t.Run(status, func(t *testing.T) {
			s, r, _ := observationStore(t)
			got := observe(t, s, r.ID, parsed(t, replayLine(1, "subagent/done", `{"status":"`+status+`"}`), 0))
			if got.State != execution.Observing || got.Cursor != "0000000000000000001" {
				t.Fatal(got)
			}
		})
	}
}

// Plan 13: the five approved rows; error takes precedence over stopped.
func TestSessionEndClassificationFRRHZ076(t *testing.T) {
	cases := []struct {
		name     string
		end      bool
		statuses []string
		want     execution.State
	}{
		{"no_end", false, []string{"ok"}, execution.Observing},
		{"all_ok", true, []string{"ok", "ok"}, execution.Succeeded},
		{"error_precedes_stopped", true, []string{"stopped", "error"}, execution.Failed},
		{"stopped_without_error", true, []string{"ok", "stopped"}, execution.Cancelled},
		{"no_done", true, nil, execution.Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, _ := observationStore(t)
			var input string
			n := 1
			for _, status := range tc.statuses {
				input += replayLine(n, "subagent/done", `{"status":"`+status+`"}`)
				n++
			}
			if tc.end {
				input += mutateLine(t, replayLine(n, "session/end", "{}"), func(m map[string]any) { delete(m, "payload") })
			}
			b := parsed(t, input, 0)
			got := observe(t, s, r.ID, b)
			if got.State != tc.want {
				t.Fatalf("state=%s", got.State)
			}
			if tc.want == execution.Unknown {
				if got.UnknownClass != execution.NeedsHuman || got.Cursor != r.Cursor {
					t.Fatal(got)
				}
			} else {
				c, err := Cursor19(b.ThroughSeq)
				if err != nil || got.Cursor != c {
					t.Fatal(got, err)
				}
			}
			before := s.All()
			observe(t, s, r.ID, b)
			unchanged(t, before, s)
		})
	}
}

// Plan 14: durable coordinates and human/JANUS facts; unmatched keys pass intentionally.
func TestApprovalObservationTupleMatchFRRHZ076(t *testing.T) {
	s, r, a := observationStore(t)
	observe(t, s, r.ID, parsed(t, policyLine(1), 0))
	got, err := (approval.Service{Store: s}).Get(a.ID)
	if err != nil || got.ResponseSeq != 1 || got.JanusDecision != approval.Deny || got.HumanDecision != approval.Allow || got.JanusReason != "EXPIRED" {
		t.Fatal(got, err)
	}
	for _, field := range []string{"trace_id", "span_id", "request_id", "local_without_request"} {
		t.Run(field, func(t *testing.T) {
			s, r, a := observationStore(t)
			line := policyLine(1)
			if field == "trace_id" {
				other := approval.RequestKey{TraceID: strings.Repeat("a", 32), SpanID: replaySpan, RequestID: "r1"}
				var err error
				a, err = (approval.Service{Store: s}).RecordInput(other, approval.Allow, "", "resp", "digest", "op", "", "", false)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				line = mutateLine(t, line, func(m map[string]any) {
					switch field {
					case "span_id":
						m[field] = "aaaaaaaaaaaaaaaa"
					case "request_id":
						m["payload"].(map[string]any)[field] = "other"
					default:
						delete(m["payload"].(map[string]any), "request_id")
					}
				})
			}
			before := s.List("approval", a.ID)
			out := observe(t, s, r.ID, parsed(t, line, 0))
			if !reflect.DeepEqual(before, s.List("approval", a.ID)) || out.State != execution.Observing || out.Cursor != "0000000000000000001" {
				t.Fatal("unmatched/local decision must pass without changing target gate")
			}
		})
	}
}

// Plan 15: deadline deny can be observed without inventing a dispatch attempt.
func TestApprovalObservationBeforeDispatchFRRHZ076(t *testing.T) {
	s, r, a := observationStore(t)
	if a.State != approval.InputRecorded {
		t.Fatal(a.State)
	}
	observe(t, s, r.ID, parsed(t, policyLine(1), 0))
	log := s.List("approval", a.ID)
	if len(log) != 2 || log[1].Type != "approval.response_observed" {
		t.Fatal(log)
	}
}

// Plan 16: exact 19-digit range and direct 9 -> 10 numerical advancement.
func TestCursorNumericSerializationFRRHZ076(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{{0, "0000000000000000000"}, {9, "0000000000000000009"}, {10, "0000000000000000010"}, {math.MaxInt64, "9223372036854775807"}} {
		s, e := Cursor19(tc.n)
		if e != nil || s != tc.want || len(s) != 19 {
			t.Fatal(s, e)
		}
		n, e := cursorNumber(s)
		if e != nil || n != tc.n {
			t.Fatal(n, e)
		}
	}
	if _, err := Cursor19(-1); !errors.Is(err, ErrCursorMigration) {
		t.Fatal(err)
	}
	for _, bad := range []string{"9", "10", "000000000000000000x", "-000000000000000001", "9223372036854775808"} {
		if _, err := cursorNumber(bad); !errors.Is(err, ErrCursorMigration) {
			t.Fatal(bad, err)
		}
	}
	s, r, _ := observationStore(t)
	var input string
	for n := 1; n <= 9; n++ {
		input += replayLine(n, "other", "{}")
	}
	one := observe(t, s, r.ID, parsed(t, input, 0))
	two := observe(t, s, r.ID, parsed(t, input+replayLine(10, "other", "{}"), 9))
	if one.Cursor != "0000000000000000009" || two.Cursor != "0000000000000000010" {
		t.Fatal(one, two)
	}
	for _, bad := range []string{"9", "broken", "9223372036854775808"} {
		t.Run("stored_"+bad, func(t *testing.T) {
			log := s.All()
			for i := range log {
				if log[i].Type == "execution.observed" {
					var p map[string]any
					if err := json.Unmarshal(log[i].Payload, &p); err != nil {
						t.Fatal(err)
					}
					p["Cursor"] = bad
					raw, err := json.Marshal(p)
					if err != nil {
						t.Fatal(err)
					}
					log[i].Payload = raw
					break
				}
			}
			corrupt := clonedStore(t, log)
			before := corrupt.All()
			_, err := ObserveBatch(execution.Service{Store: corrupt}, approval.Service{Store: corrupt}, r.ID, replaySession, parsed(t, input+replayLine(10, "other", "{}"), 0))
			if !errors.Is(err, ErrCursorMigration) {
				t.Fatal(err)
			}
			unchanged(t, before, corrupt)
		})
	}
}

var errFakeAppend = errors.New("injected append failure")

type fakeFailPort struct {
	events.Port
	failType      string
	nth, attempts int
}

func (f *fakeFailPort) Append(rev uint64, ev events.Event) error {
	if ev.Type == f.failType {
		f.attempts++
		if f.attempts == f.nth {
			return errFakeAppend
		}
	}
	return f.Port.Append(rev, ev)
}

// Plan 17: failed approval append leaves the execution cursor untouched.
func TestApprovalFailurePreservesExecutionCursorFRRHZ076(t *testing.T) {
	s, r, _ := observationStore(t)
	f := &fakeFailPort{Port: s, failType: "approval.response_observed", nth: 1}
	before := f.All()
	_, err := ObserveBatch(execution.Service{Store: f}, approval.Service{Store: f}, r.ID, replaySession, parsed(t, policyLine(1), 0))
	if !errors.Is(err, errFakeAppend) {
		t.Fatal(err)
	}
	unchanged(t, before, f)
	got, err := execution.Replay(f.List("execution", r.ID))
	if err != nil || got.Cursor != r.Cursor {
		t.Fatal(got, err)
	}
}

// Plan 18: committed approvals survive a later failure; retry cannot duplicate them.
func TestExecutionAppendFailureAndRetryFRRHZ076(t *testing.T) {
	s, r, a := observationStore(t)
	f := &fakeFailPort{Port: s, failType: "execution.observed", nth: 1}
	b := parsed(t, policyLine(1), 0)
	_, err := ObserveBatch(execution.Service{Store: f}, approval.Service{Store: f}, r.ID, replaySession, b)
	if !errors.Is(err, errFakeAppend) {
		t.Fatal(err)
	}
	got, err := execution.Replay(f.List("execution", r.ID))
	if err != nil || got.Cursor != r.Cursor || got.Revision != r.Revision {
		t.Fatal(got, err)
	}
	alog := f.List("approval", a.ID)
	ar, err := approval.Replay(alog)
	if err != nil || ar.State != approval.Observed {
		t.Fatal(ar, err)
	}
	out := observe(t, f, r.ID, b)
	if out.Cursor != "0000000000000000001" || !reflect.DeepEqual(alog, f.List("approval", a.ID)) {
		t.Fatal("retry duplicated or lost observation")
	}
	// Also fail the second approval append after the first has committed.
	s, r, a = observationStore(t)
	a2, err := (approval.Service{Store: s}).RecordInput(approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "r2"}, approval.Allow, "", "resp2", "digest", "op", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	f = &fakeFailPort{Port: s, failType: "approval.response_observed", nth: 2}
	b = parsed(t, policyLine(1)+strings.ReplaceAll(policyLine(2), `"r1"`, `"r2"`), 0)
	_, err = ObserveBatch(execution.Service{Store: f}, approval.Service{Store: f}, r.ID, replaySession, b)
	if !errors.Is(err, errFakeAppend) {
		t.Fatal(err)
	}
	got, err = execution.Replay(f.List("execution", r.ID))
	if err != nil || got.Cursor != r.Cursor {
		t.Fatal(got, err)
	}
	first := f.List("approval", a.ID)
	if len(first) != 2 || len(f.List("approval", a2.ID)) != 1 {
		t.Fatal("partial write expectation")
	}
	observe(t, f, r.ID, b)
	if !reflect.DeepEqual(first, f.List("approval", a.ID)) || len(f.List("approval", a2.ID)) != 2 {
		t.Fatal("partial retry")
	}
}

// Plan 19: all three durable coordinates must match for an idempotent skip.
func TestObservedApprovalConflictFRRHZ076(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seq    int64
		d      approval.Decision
		reason string
	}{{"seq", 2, approval.Deny, "EXPIRED"}, {"decision", 1, approval.Allow, "EXPIRED"}, {"reason", 1, approval.Deny, "other"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, a := observationStore(t)
			if _, err := (approval.Service{Store: s}).Observe(a.ID, tc.seq, tc.d, tc.reason); err != nil {
				t.Fatal(err)
			}
			before := s.All()
			_, err := ObserveBatch(execution.Service{Store: s}, approval.Service{Store: s}, r.ID, replaySession, parsed(t, policyLine(1), 0))
			if err == nil || !strings.Contains(err.Error(), "approval observation conflict") {
				t.Fatal(err)
			}
			unchanged(t, before, s)
		})
	}
}

// Plan 20: no cursor growth means no duplicate execution event.
func TestNoNewEventsNoWritesFRRHZ076(t *testing.T) {
	s, r, _ := observationStore(t)
	input := policyLine(1)
	observe(t, s, r.ID, parsed(t, input, 0))
	before := s.All()
	observe(t, s, r.ID, parsed(t, input, 1))
	unchanged(t, before, s)
	s, r, _ = observationStore(t)
	before = s.All()
	observe(t, s, r.ID, parsed(t, "", 0))
	unchanged(t, before, s)
}

// Plan 21: parsing and observation never manufacture requests or dispatches.
func TestObservationHasNoDispatchSideEffectsFRRHZ076(t *testing.T) {
	s, r, _ := observationStore(t)
	before := s.All()
	b := parsed(t, policyLine(1), 0)
	unchanged(t, before, s)
	observe(t, s, r.ID, b)
	added := s.All()[len(before):]
	if len(added) != 2 || added[0].Type != "approval.response_observed" || added[1].Type != "execution.observed" {
		t.Fatal(added)
	}
	after := s.All()
	observe(t, s, r.ID, b)
	unchanged(t, after, s)
}

// Plan 22: nil, absent/corrupt references, state guards and binding identity.
func TestObservationValidationAndStoreErrorsFRRHZ076(t *testing.T) {
	b := parsed(t, policyLine(1), 0)
	for _, which := range []string{"both", "execution", "approval"} {
		s, r, _ := observationStore(t)
		es, as := execution.Service{Store: s}, approval.Service{Store: s}
		if which != "approval" {
			es.Store = nil
		}
		if which != "execution" {
			as.Store = nil
		}
		before := s.All()
		if _, err := ObserveBatch(es, as, r.ID, replaySession, b); err == nil {
			t.Fatal("nil store")
		}
		unchanged(t, before, s)
	}
	s, r, _ := observationStore(t)
	before := s.All()
	if _, err := ObserveBatch(execution.Service{Store: s}, approval.Service{Store: s}, "missing", replaySession, b); err == nil {
		t.Fatal("missing execution")
	}
	unchanged(t, before, s)
	for _, typ := range []string{"execution", "approval"} {
		t.Run("corrupt_"+typ, func(t *testing.T) {
			s, r, _ := observationStore(t)
			log := s.All()
			for i := range log {
				if log[i].AggregateType == typ {
					log[i].Payload = []byte("{invalid")
					break
				}
			}
			bad := clonedStore(t, log)
			before := bad.All()
			if _, err := ObserveBatch(execution.Service{Store: bad}, approval.Service{Store: bad}, r.ID, replaySession, b); err == nil {
				t.Fatal("corruption hidden")
			}
			unchanged(t, before, bad)
		})
	}
	for _, state := range []execution.State{execution.Succeeded, execution.Failed, execution.Cancelled, execution.Unknown} {
		t.Run(string(state), func(t *testing.T) {
			s, r, a := observationStore(t)
			es := execution.Service{Store: s}
			var err error
			if state == execution.Unknown {
				r, err = es.MarkUnknown(r.ID, execution.NeedsHuman, "", replaySession, "uncertain")
			} else {
				r, err = es.ObserveState(r.ID, "0000000000000000001", "done", replaySession, state)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := s.All()
			got := observe(t, s, r.ID, b)
			if !reflect.DeepEqual(got, r) || len(s.List("approval", a.ID)) != 1 {
				t.Fatal("inactive execution or approval changed")
			}
			unchanged(t, before, s)
		})
	}
	for _, keep := range []int{1, 2} {
		t.Run(fmt.Sprint("preaccept_", keep), func(t *testing.T) {
			s, r, _ := observationStore(t)
			log := s.All()
			filtered := []events.Event{}
			n := 0
			for _, ev := range log {
				if ev.AggregateType == "execution" {
					n++
					if n > keep {
						continue
					}
				}
				filtered = append(filtered, ev)
			}
			pre := clonedStore(t, filtered)
			before := pre.All()
			if _, err := ObserveBatch(execution.Service{Store: pre}, approval.Service{Store: pre}, r.ID, replaySession, b); err == nil {
				t.Fatal("preaccept")
			}
			unchanged(t, before, pre)
		})
	}
	for _, source := range []string{"", "/other.db"} {
		before := s.All()
		if _, err := ObserveBatch(execution.Service{Store: s}, approval.Service{Store: s}, r.ID, source, b); err == nil {
			t.Fatal("source mismatch")
		}
		unchanged(t, before, s)
	}
	wrong := b
	wrong.TraceID = strings.Repeat("a", 32)
	wrong.ApprovalResponses = nil
	before = s.All()
	if _, err := ObserveBatch(execution.Service{Store: s}, approval.Service{Store: s}, r.ID, replaySession, wrong); !errors.Is(err, ErrSessionReplaced) {
		t.Fatal(err)
	}
	unchanged(t, before, s)
}

// Plan 23: repeat parsing and equivalent stores produce identical observations.
func TestObservationDeterminismFRRHZ076(t *testing.T) {
	input := policyLine(1) + replayLine(2, "subagent/done", `{"status":"ok"}`) + replayLine(3, "session/end", "{}")
	b1, b2 := parsed(t, input, 0), parsed(t, input, 0)
	if !reflect.DeepEqual(b1, b2) {
		t.Fatal("nondeterministic parser")
	}
	s, r, _ := observationStore(t)
	clone := clonedStore(t, s.All())
	one := observe(t, s, r.ID, b1)
	two := observe(t, clone, r.ID, b2)
	if !reflect.DeepEqual(one, two) {
		t.Fatal("projection differs")
	}
	left, right := s.All(), clone.All()
	for i := range left {
		left[i].CreatedAt = time.Time{}
	}
	for i := range right {
		right[i].CreatedAt = time.Time{}
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatal("observation payload/order differs")
	}
}
