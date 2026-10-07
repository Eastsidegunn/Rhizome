package janusadapter

// RHZ-047 tests (FR-RHZ-078): approval-request collection, the surfacing
// pass, and the full human round trip — all against fake JANUS only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/gaterequest"
	"rhizome/internal/wake"
	"rhizome/internal/workspace"
)

const gateDigest = "hx-args-digest-v1:opaque-golden"
const argsSentinel = "SENTINEL-RAW-ARGS"

func requestLine(seq int, requestID string) string {
	return replayLine(seq, "subagent/approval_request", `{"args":{"cmd":"`+argsSentinel+`"},"call_id":"call","name":"tool","reason":"why","request_id":"`+requestID+`"}`)
}

func gateKey(requestID string) approval.RequestKey {
	return approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: requestID}
}

func pendingAnswer(expiresAt int64) map[string]any {
	return map[string]any{"status": "pending", "request_digest": gateDigest, "policy_hash": "ph", "display_summary": "do X", "expires_at": expiresAt}
}

// opDial routes each fake socket exchange by its op field.
func opDial(t *testing.T, handlers map[string]func(map[string]any) map[string]any) Dialer {
	return fake(func(q map[string]any) map[string]any {
		if h, ok := handlers[fmt.Sprint(q["op"])]; ok {
			return h(q)
		}
		t.Errorf("unexpected socket op %v", q["op"])
		return map[string]any{"status": "error", "reason": "UNEXPECTED"}
	})
}

func surfaceRecords(s events.Port, id string) (wakes, decisions, requests int) {
	return len(s.List("wake", "wake-"+id)), len(s.List("decision", "dec-"+id)), len(s.List("approvalrequest", id))
}

// Plan O1: observed requests are collected in order with key and display
// strings only.
func TestParseReplayCollectsApprovalRequestsFRRHZ078(t *testing.T) {
	stream := replayLine(1, "other", `{}`) + requestLine(2, "r1") + replayLine(3, "other", `{}`) + requestLine(4, "r2")
	b := parsed(t, stream, 0)
	want := []ApprovalRequest{
		{Seq: 2, Key: gateKey("r1"), Name: "tool", Reason: "why"},
		{Seq: 4, Key: gateKey("r2"), Name: "tool", Reason: "why"},
	}
	if !reflect.DeepEqual(b.ApprovalRequests, want) {
		t.Fatalf("requests: %+v", b.ApprovalRequests)
	}
	if len(b.Done) != 0 || b.SessionEnded {
		t.Fatalf("unrelated batch fields touched: %+v", b)
	}
}

// Plan O2: the raw args never cross into a Batch — machine-proven with a
// sentinel over the full serialization (비복제 헌장).
func TestParseReplayApprovalRequestArgsNeverCopiedFRRHZ078(t *testing.T) {
	b := parsed(t, requestLine(1, "r1"), 0)
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), argsSentinel) {
		t.Fatalf("raw args leaked into batch: %s", raw)
	}
	if len(b.ApprovalRequests) != 1 {
		t.Fatalf("request not collected: %+v", b)
	}
}

// Plan O3: type corruption and a missing or empty request_id
// are OBSERVATION_CORRUPT (JANUS schema: required, minLength 1); requests in
// the cursor prefix are still collected (재표면화 멱등의 기반).
func TestParseReplayApprovalRequestValidationAndPrefixFRRHZ078(t *testing.T) {
	corruptParse(t, mutateLine(t, requestLine(1, "r1"), func(m map[string]any) {
		m["payload"].(map[string]any)["call_id"] = 17
	}), 0, ErrObservationCorrupt)
	corruptParse(t, mutateLine(t, requestLine(1, "r1"), func(m map[string]any) {
		m["payload"].(map[string]any)["request_id"] = ""
	}), 0, ErrObservationCorrupt)
	corruptParse(t, mutateLine(t, requestLine(1, "r1"), func(m map[string]any) {
		delete(m["payload"].(map[string]any), "request_id")
	}), 0, ErrObservationCorrupt)
	b := parsed(t, requestLine(1, "r1")+replayLine(2, "other", `{}`), 2)
	if len(b.ApprovalRequests) != 1 || b.ApprovalRequests[0].Seq != 1 {
		t.Fatalf("prefix request not collected: %+v", b.ApprovalRequests)
	}
}

// Plan P1: one tick surfaces a pending request as the durable chain
// Wake -> wait_human Decision -> approvalrequest, with digest and display
// material verbatim from the socket pending query — and no args anywhere.
func TestTickSurfacesPendingRequestFRRHZ078(t *testing.T) {
	s, r := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any {
			assertWireKeys(t, q, "op", "trace_id", "span_id", "request_id")
			if q["trace_id"] != replayTrace || q["span_id"] != replaySpan || q["request_id"] != "r1" {
				t.Errorf("query wire: %v", q)
			}
			return pendingAnswer(123)
		},
	})
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(requestLine(1, "r1")), nil
	}, dial)
	l.Tick()
	w, err := wake.Replay(s.List("wake", "wake-"+id))
	if err != nil || w.TargetType != "mission" || w.TargetID != "m" || w.CorrelationID != id || w.TickKey != r.ID+":"+replayTrace+":1" || w.Source != wake.Event {
		t.Fatalf("wake: %+v %v", w, err)
	}
	d, err := decision.Replay(s.List("decision", "dec-"+id))
	if err != nil || d.Kind != decision.WaitHuman || d.MissionID != "m" || d.TickKey != w.TickKey ||
		!reflect.DeepEqual(d.Evidence, []decision.Evidence{{SourceType: "execution", SourceID: r.ID}, {SourceType: "approvalrequest", SourceID: id}}) {
		t.Fatalf("decision: %+v %v", d, err)
	}
	gr, err := (gaterequest.Service{Store: s}).Get(id)
	if err != nil || gr.RequestDigest != gateDigest || gr.PolicyHash != "ph" || gr.DisplaySummary != "do X" || gr.ExpiresAt != 123 ||
		gr.MissionID != "m" || gr.ExecutionID != r.ID || gr.DecisionID != "dec-"+id || gr.Name != "tool" || gr.Reason != "why" {
		t.Fatalf("gaterequest: %+v %v", gr, err)
	}
	for _, e := range s.All() {
		if strings.Contains(string(e.Payload), argsSentinel) {
			t.Fatalf("raw args persisted in %s/%s", e.AggregateType, e.AggregateID)
		}
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// Plan P2: surfacing is idempotent — a second tick queries nothing and
// writes nothing.
func TestTickSurfaceIdempotentFRRHZ078(t *testing.T) {
	s, _ := loopStore(t)
	queries := 0
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { queries++; return pendingAnswer(123) },
	})
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(requestLine(1, "r1")), nil
	}, dial)
	l.Tick()
	if queries != 1 {
		t.Fatal("first tick queries:", queries)
	}
	before := len(s.All())
	l.Tick()
	if queries != 1 || len(s.All()) != before || errs.count() != 0 {
		t.Fatalf("second tick: queries=%d events %d->%d errs=%v", queries, before, len(s.All()), errs.all())
	}
}

// Plan P3 (D22, re-based by RHZ-093 gate B′ / FR-RHZ-119): a failed pending
// query surfaces nothing and HOLDS the observation cursor at the open request
// (it never advances past an unsurfaced one); the next tick re-reads the same
// range through replay, surfaces, and only then advances.
func TestTickSurfaceQueryFailureRetryFRRHZ078(t *testing.T) {
	s, r := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(requestLine(1, "r1")), nil
	}, func() (net.Conn, error) { return nil, errors.New("socket down") })
	l.Tick()
	if w, d, g := surfaceRecords(s, id); w+d+g != 0 {
		t.Fatalf("partial surface on failure: %d %d %d", w, d, g)
	}
	if got := execRef(t, s, r.ID); got.Cursor != "" || got.State != execution.Accepted {
		t.Fatalf("cursor advanced past an unsurfaced open request (D22 gate): %+v", got)
	}
	if errs.count() != 1 || !strings.HasPrefix(errs.all()[0], "approval|"+id) {
		t.Fatalf("failure not surfaced: %v", errs.all())
	}
	l.Client = Client{Dial: opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
	})}
	l.Tick()
	if w, d, g := surfaceRecords(s, id); w != 1 || d != 1 || g != 1 {
		t.Fatalf("retry did not surface: %d %d %d", w, d, g)
	}
	if got := execRef(t, s, r.ID); got.Cursor != c19(t, 1) || got.State != execution.Observing {
		t.Fatalf("cursor did not advance after surfacing: %+v", got)
	}
}

// Plan P4 (D23): requests with a recorded input are never queried; decided
// answers belong to replay observation and unknown retries silently.
func TestTickSurfaceSkipsResolvedRequestsFRRHZ078(t *testing.T) {
	for name, tc := range map[string]struct {
		preInput bool
		answer   map[string]any
	}{
		"input_recorded": {preInput: true},
		"decided":        {answer: map[string]any{"status": "decided", "decision": "allow"}},
		"unknown":        {answer: map[string]any{"status": "unknown"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := loopStore(t)
			id := approval.IDFor(gateKey("r1"))
			queries := 0
			if tc.preInput {
				if _, err := (approval.Service{Store: s}).RecordInput(gateKey("r1"), approval.Allow, "", "resp", gateDigest, "test-operator", "", "", true); err != nil {
					t.Fatal(err)
				}
			}
			dial := opDial(t, map[string]func(map[string]any) map[string]any{
				"query":  func(q map[string]any) map[string]any { queries++; return tc.answer },
				"submit": func(q map[string]any) map[string]any { return map[string]any{"status": "decided", "decision": "allow"} },
			})
			l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
				return strings.NewReader(requestLine(1, "r1")), nil
			}, dial)
			l.Tick()
			if w, d, g := surfaceRecords(s, id); w+d+g != 0 {
				t.Fatalf("surfaced a resolved request: %d %d %d", w, d, g)
			}
			if tc.preInput && queries != 0 {
				t.Fatal("queried despite recorded input:", queries)
			}
			if errs.count() != 0 {
				t.Fatal(errs.all())
			}
		})
	}
}

// Plan P5: surfacing never fabricates a human decision (수동 고정).
func TestTickSurfaceNoAutoDecisionFRRHZ078(t *testing.T) {
	s, _ := loopStore(t)
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
	})
	l, _ := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(requestLine(1, "r1")), nil
	}, dial)
	for i := 0; i < 3; i++ {
		l.Tick()
	}
	if n := countType(s, "approval.input_recorded"); n != 0 {
		t.Fatalf("loop fabricated inputs: %d", n)
	}
	g, err := (gaterequest.Service{Store: s}).Get(approval.IDFor(gateKey("r1")))
	if err != nil || g.RequestDigest != gateDigest {
		t.Fatal(g, err)
	}
}

// Plan P6 (D21): a partial crash leaving only the wake converges on the next
// tick — the remaining records are written and nothing dies on duplicates.
func TestTickSurfacePartialCrashConvergesFRRHZ078(t *testing.T) {
	s, r := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	if _, err := (wake.Service{Store: s}).Create(wake.Wake{ID: "wake-" + id, Source: wake.Event, RequestedAt: time.Now().UTC(), TargetType: "mission", TargetID: "m", CorrelationID: id, TickKey: r.ID + ":" + replayTrace + ":1"}); err != nil {
		t.Fatal(err)
	}
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { return pendingAnswer(123) },
	})
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(requestLine(1, "r1")), nil
	}, dial)
	l.Tick()
	if w, d, g := surfaceRecords(s, id); w != 1 || d != 1 || g != 1 {
		t.Fatalf("no convergence: %d %d %d", w, d, g)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// Plan R6 (D25): Rhizome never pre-judges expiry. A late
// approve is recorded and submitted; the expired{deny} finality arrives only
// through durable replay observation.
func TestGateApproveExpiredFinalityViaObservationFRRHZ078(t *testing.T) {
	s, _ := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	stream := requestLine(1, "r1")
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"query": func(q map[string]any) map[string]any { return pendingAnswer(1) }, // long past — carried verbatim
		"submit": func(q map[string]any) map[string]any {
			return map[string]any{"status": "expired", "decision": "deny", "reason": "EXPIRED"}
		},
	})
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(stream), nil
	}, dial)
	l.Tick() // surface with expires_at already in the past
	res, err := workspace.RelayIntent(s, workspace.Intent{Kind: "gate.approve", GateID: id, Digest: gateDigest}, "alice", false)
	if err != nil || !res.Accepted {
		t.Fatalf("late approve refused — expiry was pre-judged: %+v %v", res, err)
	}
	l.Tick() // dispatch + submit; the expired receipt is never written
	a := approvalRef(t, s, id)
	if a.State != approval.Dispatched || a.JanusDecision != "" {
		t.Fatalf("receipt wrote state: %+v", a)
	}
	stream = requestLine(1, "r1") + policyLine(2)
	l.Tick() // durable deny observed from replay
	a = approvalRef(t, s, id)
	if a.State != approval.Observed || a.JanusDecision != approval.Deny || a.JanusReason != "EXPIRED" || a.ResponseSeq != 2 {
		t.Fatalf("finality: %+v", a)
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

// Plan S1: request observed -> surfaced pending gate ->
// human approve over HTTP -> dispatch+submit -> durable observation ->
// approved gate, with a projection push at every accepted stage. Fake JANUS
// only.
func TestApprovalRoundTripEndToEndFRRHZ078(t *testing.T) {
	s, _ := loopStore(t)
	id := approval.IDFor(gateKey("r1"))
	h := workspace.NewHTTP(s)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	ch := sseEvents(t, srv.URL)
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "snapshot" {
		t.Fatal(ev, ok)
	}
	stream := requestLine(1, "r1")
	dial := opDial(t, map[string]func(map[string]any) map[string]any{
		"query":  func(q map[string]any) map[string]any { return pendingAnswer(123) },
		"submit": func(q map[string]any) map[string]any { return map[string]any{"status": "decided", "decision": "allow"} },
	})
	l, errs := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
		return strings.NewReader(stream), nil
	}, dial)
	l.Broadcast = func() {
		if p, e := workspace.Snapshot(s); e == nil {
			h.Broadcast(p)
		}
	}
	l.Tick() // ① surface
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "projection" {
		t.Fatal("no push after surfacing", ev, ok)
	}
	p, err := workspace.Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range p.Gates {
		if g.ID == id && g.State == "pending" && g.RequestDigest == gateDigest && g.DisplaySummary == "do X" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending gate missing: %+v", p.Gates)
	}
	res := postIntentJSON(t, srv.URL, `{"kind":"gate.approve","gateId":"`+id+`","digest":"`+gateDigest+`","actor":"op"}`)
	if res["Accepted"] != true && res["accepted"] != true {
		t.Fatalf("approve rejected: %v", res)
	}
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "projection" {
		t.Fatal("no push after intent", ev, ok)
	}
	l.Tick() // ② dispatch durable + submit
	if a := approvalRef(t, s, id); a.State != approval.Dispatched {
		t.Fatalf("dispatch: %+v", a)
	}
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "projection" {
		t.Fatal("no push after dispatch", ev, ok)
	}
	stream = requestLine(1, "r1") + policyLine2Allow()
	l.Tick() // ③ durable observation
	if ev, ok := nextSSE(t, ch, 2*time.Second); !ok || ev != "projection" {
		t.Fatal("no push after observation", ev, ok)
	}
	a := approvalRef(t, s, id)
	if a.State != approval.Observed || a.HumanDecision != approval.Allow || a.JanusDecision != approval.Allow || a.ResponseSeq != 2 {
		t.Fatalf("final approval: %+v", a)
	}
	p, err = workspace.Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range p.Gates {
		if g.ID == id && g.State != "approved" {
			t.Fatalf("gate not approved: %+v", g)
		}
	}
	if errs.count() != 0 {
		t.Fatal(errs.all())
	}
}

func policyLine2Allow() string {
	return replayLine(2, "policy/decision", `{"decision":"allow","profile_id":"manual","request_id":"r1","response_id":"resp"}`)
}

func postIntentJSON(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, e := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
	if e != nil || resp.StatusCode != 200 {
		t.Fatal(resp, e)
	}
	defer resp.Body.Close()
	var out map[string]any
	if e := json.NewDecoder(resp.Body).Decode(&out); e != nil {
		t.Fatal(e)
	}
	return out
}

// Plan S2: the CLI (smoke) path and the UI path decide over the same
// aggregate — whichever human input lands first is the only one, in both
// orders.
func TestSmokeCoexistenceFirstDurableWinsFRRHZ078(t *testing.T) {
	surface := func(t *testing.T) (*events.Store, string) {
		s, _ := loopStore(t)
		id := approval.IDFor(gateKey("r1"))
		dial := opDial(t, map[string]func(map[string]any) map[string]any{
			"query":  func(q map[string]any) map[string]any { return pendingAnswer(123) },
			"submit": func(q map[string]any) map[string]any { return map[string]any{"status": "decided", "decision": "allow"} },
		})
		l, _ := newTestLoop(s, func(cfg RunConfig, db, tr string) (io.Reader, error) {
			return strings.NewReader(requestLine(1, "r1")), nil
		}, dial)
		l.Tick()
		return s, id
	}
	t.Run("smoke_first", func(t *testing.T) {
		s, id := surface(t)
		if _, err := (approval.Service{Store: s}).RecordInputWithGate(gateKey("r1"), approval.Allow, "", "resp-manual", gateDigest, "test-operator", "smoke", "", true, approval.GateFields{}); err != nil {
			t.Fatal(err)
		}
		res, err := workspace.RelayIntent(s, workspace.Intent{Kind: "gate.approve", GateID: id, Digest: gateDigest}, "alice", false)
		if err != nil || res.Accepted || res.Reason != "gate already has input" {
			t.Fatal(res, err)
		}
		if n := countType(s, "approval.input_recorded"); n != 1 {
			t.Fatalf("inputs: %d", n)
		}
	})
	t.Run("ui_first", func(t *testing.T) {
		s, id := surface(t)
		res, err := workspace.RelayIntent(s, workspace.Intent{Kind: "gate.approve", GateID: id, Digest: gateDigest}, "alice", false)
		if err != nil || !res.Accepted {
			t.Fatal(res, err)
		}
		if _, err = (approval.Service{Store: s}).RecordInputWithGate(gateKey("r1"), approval.Allow, "", "resp-manual", gateDigest, "test-operator", "smoke", "", true, approval.GateFields{}); err == nil {
			t.Fatal("second durable input accepted")
		}
		if n := countType(s, "approval.input_recorded"); n != 1 {
			t.Fatalf("inputs: %d", n)
		}
	})
}
