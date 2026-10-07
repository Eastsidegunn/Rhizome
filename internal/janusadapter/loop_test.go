package janusadapter

// RHZ-046 part 1 loop tests. Every JANUS contact is a fake (net.Pipe sockets,
// io.Reader replay streams); no test touches a real hx binary or socket.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
	"rhizome/internal/question"
	"rhizome/internal/workspace"
)

type loopErrs struct {
	mu    sync.Mutex
	items []string
}

func (e *loopErrs) hook(scope, id string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.items = append(e.items, scope+"|"+id+"|"+err.Error())
}
func (e *loopErrs) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.items)
}
func (e *loopErrs) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.items...)
}

func newTestLoop(s events.Port, replay ReplaySource, dial Dialer) (*Loop, *loopErrs) {
	errs := &loopErrs{}
	return &Loop{
		ES:      execution.Service{Store: s},
		AS:      approval.Service{Store: s},
		Client:  Client{Dial: dial},
		Replay:  replay,
		Cfg:     runConfigFixture(),
		OnError: errs.hook,
	}, errs
}

func emptyReplay(cfg RunConfig, db, tr string) (io.Reader, error) { return strings.NewReader(""), nil }

func noDial(t *testing.T) Dialer {
	return func() (net.Conn, error) {
		t.Errorf("unexpected socket dial")
		return nil, errors.New("unexpected dial")
	}
}

// hookedFake runs pre at dial time (before any wire exchange) then behaves
// like the shared fake script dialer.
func hookedFake(pre func(), script func(map[string]any) map[string]any) Dialer {
	inner := fake(script)
	return func() (net.Conn, error) {
		if pre != nil {
			pre()
		}
		return inner()
	}
}

func assertWireKeys(t *testing.T, q map[string]any, want ...string) {
	t.Helper()
	m := map[string]bool{}
	for _, k := range want {
		m[k] = true
	}
	for k := range q {
		if !m[k] {
			t.Errorf("unexpected wire key %s", k)
		}
	}
	for k := range m {
		if _, ok := q[k]; !ok {
			t.Errorf("missing wire key %s", k)
		}
	}
}

func boundExec(t *testing.T, s *events.Store, key, trace, session string) execution.Ref {
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
	if r, err = es.Bind(r.ID, execution.Binding{SessionDB: session, TraceID: trace, PolicyHash: "policy", RequestFingerprint: "request"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func loopStore(t *testing.T) (*events.Store, execution.Ref) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "work", "done"); err != nil {
		t.Fatal(err)
	}
	return s, boundExec(t, s, "loop-key", replayTrace, replaySession)
}

func c19(t *testing.T, n int64) string {
	t.Helper()
	c, err := Cursor19(n)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func execRef(t *testing.T, s events.Port, id string) execution.Ref {
	t.Helper()
	r, err := execution.Replay(s.List("execution", id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func approvalRef(t *testing.T, s events.Port, id string) approval.Ref {
	t.Helper()
	a, err := approval.Replay(s.List("approval", id))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func countType(s events.Port, typ string) int {
	n := 0
	for _, e := range s.All() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func sseEvents(t *testing.T, url string) chan string {
	t.Helper()
	resp, err := http.Get(url + "/v1/workspace/stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	ch := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: ") {
				ch <- strings.TrimPrefix(sc.Text(), "event: ")
			}
		}
	}()
	return ch
}

func nextSSE(t *testing.T, ch chan string, within time.Duration) (string, bool) {
	t.Helper()
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(within):
		return "", false
	}
}

// TestTickQuestionCoexistsWithJANUSApprovalFRRHZ081 proves the loop's socket
// relay selection is aggregate based: an internal pending question is inert,
// while exactly one input_recorded JANUS approval is submitted.
func TestTickQuestionCoexistsWithJANUSApprovalFRRHZ081(t *testing.T) {
	s := &events.Store{}
	if _, err := (question.Service{Store: s}).Ask("human", "question", "recommend", "", "", "operator", ""); err != nil {
		t.Fatal(err)
	}
	k := approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "coexist"}
	if _, err := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "resp-coexist", "hx-args-digest-v1:opaque", "operator", "corr", "", false); err != nil {
		t.Fatal(err)
	}
	questionBefore := countType(s, "question.asked") + countType(s, "question.answered")
	dials := 0
	l, errs := newTestLoop(s, emptyReplay, hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
		if q["op"] != "submit" || q["response_id"] != "resp-coexist" {
			t.Errorf("unexpected wire request: %v", q)
		}
		return map[string]any{"status": "decided", "decision": "allow"}
	}))
	for i := 0; i < 3; i++ {
		l.Tick()
	}
	if dials != 3 {
		t.Fatalf("dials=%d want 3 JANUS submissions", dials)
	}
	if got := countType(s, "question.asked") + countType(s, "question.answered"); got != questionBefore {
		t.Fatalf("question aggregate changed: %d -> %d", questionBefore, got)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// C1: a bound, active execution's replay stream is consumed once per
// tick and lands as a durable observation with the 19-digit cursor.
func TestTickObservesBoundExecutionFRRHZ077(t *testing.T) {
	s, r := loopStore(t)
	var mu sync.Mutex
	calls := []string{}
	stream := replayLine(1, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(2, "session/end", `{}`)
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		mu.Lock()
		calls = append(calls, db+"|"+tr)
		mu.Unlock()
		return strings.NewReader(stream), nil
	}, noDial(t))
	l.Tick()
	if !reflect.DeepEqual(calls, []string{replaySession + "|" + replayTrace}) {
		t.Fatalf("replay calls: %v", calls)
	}
	got := execRef(t, s, r.ID)
	if got.State != execution.Succeeded || got.Cursor != c19(t, 2) || got.SourceRef != replaySession {
		t.Fatalf("observation: %+v", got)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// C2: only bound Accepted/Observing executions are observed — intent,
// dispatch_claimed, unbound-accepted, terminal and unknown are all skipped
// with no replay contact and no writes.
func TestTickSkipsUnboundAndTerminalFRRHZ077(t *testing.T) {
	s, _ := loopStore(t)
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	if _, err := es.IntentWithPolicy("m", "k-int", p, p); err != nil {
		t.Fatal(err)
	}
	r2, err := es.IntentWithPolicy("m", "k-claim", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = es.ClaimDispatch(r2.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	r3, err := es.IntentWithPolicy("m", "k-acc", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r3, err = es.ClaimDispatch(r3.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	if _, err = es.Accept(r3.ID, runTrace); err != nil {
		t.Fatal(err)
	}
	r4 := boundExec(t, s, "k-term", replayTrace, "/tmp/term.db")
	if _, err = es.ObserveState(r4.ID, c19(t, 1), "done", "/tmp/term.db", execution.Succeeded); err != nil {
		t.Fatal(err)
	}
	// The remaining bound execution goes unknown so no active binding is left.
	rMain := execRef(t, s, "exec-loop-key")
	if _, err = es.MarkUnknown(rMain.ID, execution.NeedsHuman, "", replaySession, "test"); err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	replayCalls := 0
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		replayCalls++
		return strings.NewReader(""), nil
	}, noDial(t))
	l.Tick()
	if replayCalls != 0 || len(s.All()) != before || errs.count() != 0 {
		t.Fatalf("calls=%d events %d->%d errs=%v", replayCalls, before, len(s.All()), errs.all())
	}
}

// C3: a failed tick leaves the cursor untouched and the next tick
// retries from the same cursor (§5.2 same-cursor safe retry); errors surface
// through the hook and never kill the loop.
func TestTickSameCursorSafeRetryFRRHZ077(t *testing.T) {
	s, r := loopStore(t)
	noop2 := replayLine(1, "other", `{}`) + replayLine(2, "other", `{}`)
	full4 := noop2 + replayLine(3, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(4, "session/end", `{}`)
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(noop2), nil
	}, noDial(t))
	l.Tick()
	if got := execRef(t, s, r.ID); got.State != execution.Observing || got.Cursor != c19(t, 2) {
		t.Fatalf("seed observation: %+v", got)
	}
	l.Replay = func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return nil, errors.New("transient replay failure")
	}
	l.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 2) || got.State != execution.Observing {
		t.Fatalf("cursor moved on failure: %+v", got)
	}
	if errs.count() != 1 || !strings.HasPrefix(errs.all()[0], "execution|"+r.ID) {
		t.Fatalf("failure not surfaced: %v", errs.all())
	}
	l.Replay = func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(full4), nil
	}
	l.Tick()
	if got := execRef(t, s, r.ID); got.State != execution.Succeeded || got.Cursor != c19(t, 4) {
		t.Fatalf("retry did not advance: %+v", got)
	}
}

// C4: the cursor advances only after durable observation, and a
// terminal execution makes further ticks no-ops without replay contact.
func TestTickCursorAdvanceAfterDurableAndIdempotentFRRHZ077(t *testing.T) {
	s, r := loopStore(t)
	noop2 := replayLine(1, "other", `{}`) + replayLine(2, "other", `{}`)
	full4 := noop2 + replayLine(3, "subagent/done", `{"result":"r","status":"ok"}`) + replayLine(4, "session/end", `{}`)
	stream := noop2
	calls := 0
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		calls++
		return strings.NewReader(stream), nil
	}, noDial(t))
	l.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 2) || got.State != execution.Observing {
		t.Fatalf("tick1: %+v", got)
	}
	stream = full4
	l.Tick()
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 4) || got.State != execution.Succeeded {
		t.Fatalf("tick2: %+v", got)
	}
	before, callsBefore := len(s.All()), calls
	l.Tick()
	if len(s.All()) != before || calls != callsBefore {
		t.Fatalf("terminal tick not a no-op: events %d->%d calls %d->%d", before, len(s.All()), callsBefore, calls)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// C5: one corrupt stream is isolated — reported, no writes for that
// execution — while the healthy execution is observed in the same tick.
func TestTickCorruptReplayIsolatedFRRHZ077(t *testing.T) {
	s, _ := loopStore(t)
	a := execRef(t, s, "exec-loop-key")
	b := boundExec(t, s, "loop-b", runTrace, "/tmp/b.db")
	corrupt := replayLine(1, "other", `{}`) + replayLine(3, "other", `{}`)
	okStream := strings.ReplaceAll(
		replayLine(1, "subagent/done", `{"result":"r","status":"ok"}`)+replayLine(2, "session/end", `{}`),
		replayTrace, runTrace)
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		if db == replaySession {
			return strings.NewReader(corrupt), nil
		}
		return strings.NewReader(okStream), nil
	}, noDial(t))
	l.Tick()
	if got := execRef(t, s, a.ID); got.State != execution.Accepted || got.Cursor != "" {
		t.Fatalf("corrupt execution written: %+v", got)
	}
	if got := execRef(t, s, b.ID); got.State != execution.Succeeded || got.Cursor != c19(t, 2) {
		t.Fatalf("healthy execution not observed: %+v", got)
	}
	if errs.count() != 1 || !strings.HasPrefix(errs.all()[0], "execution|"+a.ID) {
		t.Fatalf("isolation errors: %v", errs.all())
	}
	l.Tick() // The loop stays alive and keeps reporting the corrupt stream.
	if errs.count() != 2 {
		t.Fatalf("loop died after corrupt stream: %v", errs.all())
	}
}

// C6: session/end without done goes unknown(needs_human), surfaces in
// the workspace attention list, and pushes the existing projection over SSE.
func TestTickUnknownSurfacesAttentionFRRHZ077(t *testing.T) {
	s, r := loopStore(t)
	h := workspace.NewHTTP(s)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close) // LIFO: the SSE body closes first, then the server
	ch := sseEvents(t, srv.URL)
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "snapshot" {
		t.Fatal(ev, ok)
	}
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(replayLine(1, "session/end", `{}`)), nil
	}, noDial(t))
	l.Broadcast = func() {
		if p, e := workspace.Snapshot(s); e == nil {
			h.Broadcast(p)
		}
	}
	l.Tick()
	got := execRef(t, s, r.ID)
	if got.State != execution.Unknown || got.UnknownClass != execution.NeedsHuman {
		t.Fatalf("unknown classification: %+v", got)
	}
	p, err := workspace.Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range p.Attention {
		if a.Kind == "execution_unknown" && a.RefID == r.ID && a.Cause == string(execution.NeedsHuman) {
			found = true
		}
	}
	if !found {
		t.Fatalf("attention missing execution_unknown: %+v", p.Attention)
	}
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "projection" {
		t.Fatalf("no projection push: %q %v", ev, ok)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// D1: the durable dispatch record precedes socket contact, the submit
// wire is exact, and the socket's decided reply never becomes a durable
// observation.
func TestTickApprovalDispatchThenSubmitWireExactFRRHZ077(t *testing.T) {
	s, _, a := observationStore(t)
	dispatchedAtDial := false
	dial := hookedFake(func() {
		log := s.List("approval", a.ID)
		dispatchedAtDial = len(log) == 2 && log[1].Type == "approval.response_dispatched"
	}, func(q map[string]any) map[string]any {
		assertWireKeys(t, q, "op", "trace_id", "span_id", "request_id", "response_id", "decision", "reason")
		if q["op"] != "submit" || q["trace_id"] != replayTrace || q["span_id"] != replaySpan || q["request_id"] != "r1" || q["response_id"] != "resp" || q["decision"] != "allow" || q["reason"] != "" {
			t.Errorf("submit wire: %v", q)
		}
		return map[string]any{"status": "decided", "decision": "allow"}
	})
	l, errs := newTestLoop(s, emptyReplay, dial)
	l.Tick()
	if !dispatchedAtDial {
		t.Fatal("socket contact before durable dispatch record")
	}
	got := approvalRef(t, s, a.ID)
	if got.State != approval.Dispatched {
		t.Fatalf("state: %+v", got)
	}
	if n := countType(s, "approval.response_observed"); n != 0 {
		t.Fatalf("socket reply became durable observation: %d", n)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// D2: a failed submit keeps the approval dispatched, surfaces the error,
// and the next tick resubmits without a second dispatch record (D5 guard —
// contract §4 resubmission is a lookup).
func TestTickApprovalSubmitFailureRetryNoDuplicateDispatchFRRHZ077(t *testing.T) {
	s, _, a := observationStore(t)
	l, errs := newTestLoop(s, emptyReplay, func() (net.Conn, error) { return nil, errors.New("socket down") })
	l.Tick()
	if got := approvalRef(t, s, a.ID); got.State != approval.Dispatched {
		t.Fatalf("state after failed submit: %+v", got)
	}
	if errs.count() != 1 || !strings.HasPrefix(errs.all()[0], "approval|"+a.ID) {
		t.Fatalf("submit failure not surfaced: %v", errs.all())
	}
	dials := 0
	l.Client = Client{Dial: hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
		if q["response_id"] != "resp" {
			t.Errorf("retry wire: %v", q)
		}
		return map[string]any{"status": "decided", "decision": "allow"}
	})}
	l.Tick()
	if dials != 1 {
		t.Fatalf("dials: %d", dials)
	}
	if n := countType(s, "approval.response_dispatched"); n != 1 {
		t.Fatalf("duplicate dispatch records: %d", n)
	}
	if n := countType(s, "approval.response_observed"); n != 0 {
		t.Fatalf("submit reply written: %d", n)
	}
	if errs.count() != 1 {
		t.Fatal(errs.all())
	}
}

// D3: durable approval observation comes only from replay — the
// observed approval needs no further socket contact in the same tick.
func TestTickApprovalObservedViaReplayOnlyFRRHZ077(t *testing.T) {
	s, r, a := observationStore(t)
	if _, err := (approval.Service{Store: s}).Dispatch(a.ID, "resp"); err != nil {
		t.Fatal(err)
	}
	dials := 0
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(policyLine(1)), nil
	}, func() (net.Conn, error) { dials++; return nil, errors.New("must not dial") })
	l.Tick()
	got := approvalRef(t, s, a.ID)
	if got.State != approval.Observed || got.JanusDecision != approval.Deny || got.JanusReason != "EXPIRED" || got.ResponseSeq != 1 {
		t.Fatalf("observation: %+v", got)
	}
	if dials != 0 {
		t.Fatalf("observed approval still dialed: %d", dials)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 1) || got.State != execution.Observing {
		t.Fatalf("cursor after approval observation: %+v", got)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// D4: the human deny travels verbatim; the loop fabricates no approval
// inputs and touches no socket when there is nothing to relay.
func TestTickApprovalDenyVerbatimNoSynthesisFRRHZ077(t *testing.T) {
	s := &events.Store{}
	k := approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "r2"}
	a, err := (approval.Service{Store: s}).RecordInput(k, approval.Deny, "policy says no", "resp2", "hx-args-digest-v1:opaque", "operator", "corr", "", false)
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	l, errs := newTestLoop(s, emptyReplay, hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
		if q["decision"] != "deny" || q["reason"] != "policy says no" {
			t.Errorf("deny not verbatim: %v", q)
		}
		return map[string]any{"status": "decided", "decision": "deny", "reason": "policy says no"}
	}))
	before := countType(s, "approval.input_recorded")
	l.Tick()
	if dials != 1 || countType(s, "approval.input_recorded") != before {
		t.Fatalf("dials=%d inputs %d->%d", dials, before, countType(s, "approval.input_recorded"))
	}
	if got := approvalRef(t, s, a.ID); got.HumanDecision != approval.Deny || got.Reason != "policy says no" {
		t.Fatalf("input mutated: %+v", got)
	}
	// Nothing to relay: an empty store never contacts the socket.
	s2 := &events.Store{}
	l2, errs2 := newTestLoop(s2, emptyReplay, noDial(t))
	l2.Tick()
	if errs.count() != 0 || errs2.count() != 0 {
		t.Fatal(errs.all(), errs2.all())
	}
}

// E1: only the durable stop_requested intent is submitted, with the
// exact minimal stop wire; executions without the intent get no contact.
func TestTickStopSubmitsDurableOnlyWireExactFRRHZ077(t *testing.T) {
	s, r := stopStore(t)
	boundExec(t, s, "no-stop", runTrace, "/tmp/b.db")
	es := execution.Service{Store: s}
	r, err := es.RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	l, errs := newTestLoop(s, emptyReplay, hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
		assertWireKeys(t, q, "op", "trace_id", "stop_id", "reason")
		if q["op"] != "stop" || q["trace_id"] != replayTrace || q["stop_id"] != r.StopID || q["reason"] != "user" {
			t.Errorf("stop wire: %v", q)
		}
		return stopEcho(q)
	}))
	before := len(s.All())
	l.Tick()
	if dials != 1 {
		t.Fatalf("dials: %d", dials)
	}
	if len(s.All()) != before {
		t.Fatalf("stop path wrote events: %d->%d", before, len(s.All()))
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// E2: neither stop receipt (stop_accepted, already_terminal) writes
// anything; cancelled is concluded only from replay observation.
func TestTickStopReceiptNeverWritesFRRHZ077(t *testing.T) {
	s, r := stopStore(t)
	es := execution.Service{Store: s}
	r, err := es.RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	l, errs := newTestLoop(s, emptyReplay, fake(stopEcho))
	before := len(s.All())
	l.Tick()
	if len(s.All()) != before {
		t.Fatalf("stop_accepted wrote: %d->%d", before, len(s.All()))
	}
	if got := execRef(t, s, r.ID); got.State != execution.Accepted || !got.StopRequested {
		t.Fatalf("receipt changed state: %+v", got)
	}
	// Cancellation arrives only through the durable session log.
	l.Replay = func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(replayLine(1, "subagent/done", `{"result":"r","status":"stopped"}`) + replayLine(2, "session/end", `{}`)), nil
	}
	l.Tick()
	if got := execRef(t, s, r.ID); got.State != execution.Cancelled || got.Cursor != c19(t, 2) {
		t.Fatalf("replay cancellation: %+v", got)
	}
	s2, r2 := stopStore(t)
	if _, err = (execution.Service{Store: s2}).RequestStop(r2.ID, "user", 0, "operator"); err != nil {
		t.Fatal(err)
	}
	l2, errs2 := newTestLoop(s2, emptyReplay, fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "already_terminal", "stop_id": q["stop_id"], "terminal_ref": 7}
	}))
	before2 := len(s2.All())
	l2.Tick()
	if len(s2.All()) != before2 {
		t.Fatalf("already_terminal wrote: %d->%d", before2, len(s2.All()))
	}
	if errs.count() != 0 || errs2.count() != 0 {
		t.Fatal(errs.all(), errs2.all())
	}
}

// E3: an acknowledged receipt is remembered per process (no repeat
// dial); a fresh loop instance — restart shape — resubmits the same durable
// stop_id safely and still writes nothing.
func TestTickStopDedupeAndRestartResubmitFRRHZ077(t *testing.T) {
	s, r := stopStore(t)
	r, err := (execution.Service{Store: s}).RequestStop(r.ID, "user", 0, "operator")
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	dial := hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
		if q["stop_id"] != r.StopID {
			t.Errorf("stop identity drifted: %v", q)
		}
		return stopEcho(q)
	})
	l, _ := newTestLoop(s, emptyReplay, dial)
	l.Tick()
	l.Tick()
	if dials != 1 {
		t.Fatalf("same-process resubmission: %d dials", dials)
	}
	before := len(s.All())
	l2, errs2 := newTestLoop(s, emptyReplay, dial) // restart: receipts are not durable
	l2.Tick()
	if dials != 2 || len(s.All()) != before || errs2.count() != 0 {
		t.Fatalf("restart resubmit: dials=%d events %d->%d errs=%v", dials, before, len(s.All()), errs2.all())
	}
}

// F1: the loop never starts executions. It has no Runner at all, so a
// dispatch_claimed execution stays untouched across ticks — no acceptance,
// no unknown, no socket or replay contact (D7).
func TestLoopNeverInvokesRunnerFRRHZ077(t *testing.T) {
	s, r := runStore(t) // dispatch_claimed execution
	replayCalls := 0
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		replayCalls++
		return strings.NewReader(""), nil
	}, noDial(t))
	before := len(s.All())
	for i := 0; i < 3; i++ {
		l.Tick()
	}
	if replayCalls != 0 || len(s.All()) != before {
		t.Fatalf("loop acted on dispatch_claimed: calls=%d events %d->%d", replayCalls, before, len(s.All()))
	}
	got := execRef(t, s, r.ID)
	if got.State != execution.DispatchClaimed {
		t.Fatalf("state drifted: %+v", got)
	}
	if countType(s, "execution.accepted") != 0 || countType(s, "execution.unknown") != 0 {
		t.Fatal("start-shaped events appeared")
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

type conflictOnce struct {
	events.Port
	mu      sync.Mutex
	tripped bool
}

func (c *conflictOnce) Append(expected uint64, e events.Event) error {
	c.mu.Lock()
	trip := !c.tripped
	c.tripped = true
	c.mu.Unlock()
	if trip {
		return events.ErrRevisionConflict
	}
	return c.Port.Append(expected, e)
}

// G1: a concurrent-write revision conflict surfaces through the hook,
// corrupts nothing, and the next tick converges.
func TestTickRevisionConflictSurfacesAndConvergesFRRHZ077(t *testing.T) {
	underlying, _, a := observationStore(t)
	s := &conflictOnce{Port: underlying}
	dials := 0
	l, errs := newTestLoop(s, emptyReplay, hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
		return map[string]any{"status": "decided", "decision": "allow"}
	}))
	l.Tick()
	if errs.count() != 1 || !strings.Contains(errs.all()[0], events.ErrRevisionConflict.Error()) {
		t.Fatalf("conflict not surfaced: %v", errs.all())
	}
	if dials != 0 {
		t.Fatal("dialed despite failed dispatch record")
	}
	if got := approvalRef(t, s, a.ID); got.State != approval.InputRecorded {
		t.Fatalf("partial write: %+v", got)
	}
	l.Tick()
	if got := approvalRef(t, s, a.ID); got.State != approval.Dispatched || dials != 1 {
		t.Fatalf("no convergence: %+v dials=%d", got, dials)
	}
}

// G2: loop ticks and HTTP intents append concurrently to one real
// journal without corruption (-race is part of make ci).
func TestConcurrentLoopAndIntentRaceFRRHZ077(t *testing.T) {
	path := t.TempDir() + "/journal.log"
	j, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	k := approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "rc"}
	if _, err = (approval.Service{Store: j}).RecordInput(k, approval.Allow, "", "resp", "hx-args-digest-v1:opaque", "operator", "", "", false); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(workspace.NewHTTP(j).Handler())
	defer srv.Close()
	l, _ := newTestLoop(j, emptyReplay, fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "decided", "decision": "allow"}
	}))
	const n = 20
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			l.Tick()
		}
	}()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`{"kind":"mission.create","name":"n%02d","prompt":"p","actor":"op"}`, i)
		resp, e := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(body))
		if e != nil || resp.StatusCode != 200 {
			t.Fatal(resp, e)
		}
		resp.Body.Close()
	}
	wg.Wait()
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := journal.Open(path)
	if err != nil {
		t.Fatalf("journal corrupted: %v", err)
	}
	defer j2.Close()
	missions := map[string]bool{}
	for _, e := range j2.All() {
		if e.AggregateType == "mission" {
			missions[e.AggregateID] = true
		}
	}
	if len(missions) != n {
		t.Fatalf("missions: %d", len(missions))
	}
}

// G3: ticks never overlap — the loop is a single logical writer even
// when Tick is invoked concurrently.
func TestNoOverlappingTicksFRRHZ077(t *testing.T) {
	s, _ := loopStore(t)
	var mu sync.Mutex
	active, max := 0, 0
	l, _ := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		mu.Lock()
		active++
		if active > max {
			max = active
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return strings.NewReader(""), nil
	}, noDial(t))
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				l.Tick()
			}
		}()
	}
	wg.Wait()
	if max != 1 {
		t.Fatalf("overlapping ticks: max concurrency %d", max)
	}
}

// H1: every induced failure reaches the hook (nothing swallowed), the
// healthy item keeps being processed, and a nil hook does not panic.
func TestLoopErrorHookNotFatalNotSilentFRRHZ077(t *testing.T) {
	s, _ := loopStore(t) // exec A: /tmp/rhz-043-test-session.db (corrupt stream)
	b := boundExec(t, s, "loop-b", runTrace, "/tmp/b.db")
	k := approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "rh"}
	if _, err := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "resp", "hx-args-digest-v1:opaque", "operator", "", "", false); err != nil {
		t.Fatal(err)
	}
	corrupt := replayLine(1, "other", `{}`) + replayLine(3, "other", `{}`)
	okStream := strings.ReplaceAll(
		replayLine(1, "other", `{}`)+replayLine(2, "other", `{}`),
		replayTrace, runTrace)
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		if db == "/tmp/b.db" {
			return strings.NewReader(okStream), nil
		}
		return strings.NewReader(corrupt), nil
	}, func() (net.Conn, error) { return nil, errors.New("socket down") })
	l.Tick()
	l.Tick()
	// Per tick: corrupt replay (1) + failed submit (1); the healthy execution
	// and the dispatch write go through on tick 1.
	if errs.count() != 4 {
		t.Fatalf("errors: %v", errs.all())
	}
	if got := execRef(t, s, b.ID); got.Cursor != c19(t, 2) || got.State != execution.Observing {
		t.Fatalf("healthy item starved: %+v", got)
	}
	// A nil hook must not panic (default stderr path).
	s2, _ := loopStore(t)
	l2 := &Loop{ES: execution.Service{Store: s2}, AS: approval.Service{Store: s2}, Client: Client{}, Cfg: runConfigFixture(),
		Replay: func(cfg RunConfig, db, tr string) (io.Reader, error) { return nil, errors.New("down") }}
	l2.Tick()
}

// I1: a tick that made durable writes pushes the existing projection to
// SSE subscribers; a failing tick pushes nothing.
func TestTickBroadcastAfterDurableWriteFRRHZ077(t *testing.T) {
	s, _, a := observationStore(t)
	if _, err := (approval.Service{Store: s}).Dispatch(a.ID, "resp"); err != nil {
		t.Fatal(err)
	}
	h := workspace.NewHTTP(s)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close) // LIFO: the SSE body closes first, then the server
	ch := sseEvents(t, srv.URL)
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "snapshot" {
		t.Fatal(ev, ok)
	}
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(policyLine(1)), nil
	}, noDial(t))
	l.Broadcast = func() {
		if p, e := workspace.Snapshot(s); e == nil {
			h.Broadcast(p)
		}
	}
	l.Tick()
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "projection" {
		t.Fatalf("no push after durable write: %q %v", ev, ok)
	}
	p, err := workspace.Snapshot(s)
	if err != nil || len(p.Gates) != 1 || p.Gates[0].State != "rejected" {
		t.Fatalf("gate not updated: %+v %v", p.Gates, err)
	}
	l.Replay = func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return nil, errors.New("replay down")
	}
	l.Tick()
	if ev, ok := nextSSE(t, ch, 500*time.Millisecond); ok {
		t.Fatalf("failing tick pushed %q", ev)
	}
	if errs.count() != 1 {
		t.Fatal(errs.all())
	}
}

// J1: hx run argv assembly is pure and exact; --session only when set.
func TestRunArgvAssemblyFRRHZ077(t *testing.T) {
	cfg := runConfigFixture()
	want := []string{"run", "--request", "/tmp/req.json", "--profile", cfg.ProfilePath, "--accept-root", cfg.AcceptRoot, "--world-config", cfg.WorldConfigPath, "--approval-endpoint", cfg.ApprovalEndpoint}
	if got := RunArgv(cfg, "/tmp/req.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv: %v", got)
	}
	cfg.SessionDB = "/tmp/session.db"
	if got := RunArgv(cfg, "/tmp/req.json"); !reflect.DeepEqual(got, append(want, "--session", "/tmp/session.db")) {
		t.Fatalf("argv with session: %v", got)
	}
}

// J2: hx replay argv assembly — full log, no --to bound in v1.
func TestReplayArgvAssemblyFRRHZ077(t *testing.T) {
	if got := ReplayArgv("/tmp/session.db"); !reflect.DeepEqual(got, []string{"replay", "--session", "/tmp/session.db"}) {
		t.Fatalf("argv: %v", got)
	}
}

// J3: an absent unix socket surfaces as UNAVAILABLE through the client;
// no live JANUS is ever contacted by tests.
func TestRealDialerAbsentSocketUnavailableFRRHZ077(t *testing.T) {
	c := Client{Dial: RealDialer(t.TempDir() + "/absent.sock")}
	if _, err := c.Query(key()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
