package workspace

// RHZ-046 part 2 tests: /v1/execution/{taskId} surface v1 (FR-RHZ-077).
// Fixtures are built through the real domain services; the surface is
// asserted at the wire (raw JSON) level where the contract shape matters.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

const execTrace = "0123456789abcdef0123456789abcdef"

func pad19(n int) string { return fmt.Sprintf("%019d", n) }

func execHTTPFixture(t *testing.T) (*events.Store, *HTTPServer, *httptest.Server) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, e := ms.CreateGoal("g", "목표", "done", "p"); e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Create("m", "g", "미션", "done"); e != nil {
		t.Fatal(e)
	}
	h := NewHTTP(s)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return s, h, srv
}

func acceptedExec(t *testing.T, s *events.Store, missionID, key string) execution.Ref {
	t.Helper()
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	r, err := es.IntentWithPolicy(missionID, key, p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Accept(r.ID, execTrace); err != nil {
		t.Fatal(err)
	}
	return r
}

func observeExec(t *testing.T, s *events.Store, id, cursor, summary string, st execution.State) {
	t.Helper()
	if _, err := (execution.Service{Store: s}).ObserveState(id, cursor, summary, "src", st); err != nil {
		t.Fatal(err)
	}
}

func maxSequence(s events.Port) uint64 {
	var n uint64
	for _, e := range s.All() {
		if e.Sequence > n {
			n = e.Sequence
		}
	}
	return n
}

func getExecution(t *testing.T, url, task string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url + "/v1/execution/" + task)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

type execEnvelope struct {
	Revision uint64 `json:"revision"`
	Body     struct {
		TaskID   string           `json:"taskId"`
		Sessions []map[string]any `json:"sessions"`
		Events   []any            `json:"events"`
	} `json:"body"`
}

func decodeExecution(t *testing.T, raw []byte) execEnvelope {
	t.Helper()
	var env execEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err, string(raw))
	}
	return env
}

type execSSEReader struct{ events chan [2]string }

func openExecutionSSE(t *testing.T, url, task string) *execSSEReader {
	t.Helper()
	resp, err := http.Get(url + "/v1/execution/" + task + "/stream")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	r := &execSSEReader{events: make(chan [2]string, 8)}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		kind := ""
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				kind = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") && kind != "" {
				r.events <- [2]string{kind, strings.TrimPrefix(line, "data: ")}
				kind = ""
			}
		}
	}()
	return r
}

func (r *execSSEReader) next(t *testing.T, within time.Duration) (string, string, bool) {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev[0], ev[1], true
	case <-time.After(within):
		return "", "", false
	}
}

// K1: an existing mission with no executions serializes literal empty
// arrays — never null — inside the {revision, body} envelope.
func TestExecutionSnapshotEmptyMissionLiteralArraysFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	code, raw := getExecution(t, srv.URL, "m")
	if code != 200 {
		t.Fatal(code, string(raw))
	}
	if !strings.Contains(string(raw), `"sessions":[]`) || !strings.Contains(string(raw), `"events":[]`) {
		t.Fatalf("null or missing arrays: %s", raw)
	}
	env := decodeExecution(t, raw)
	if env.Body.TaskID != "m" || env.Revision != maxSequence(s) || env.Revision == 0 {
		t.Fatalf("envelope: %+v (want revision %d)", env, maxSequence(s))
	}
}

// K2: the ratified state mapping, every case, sorted by session id.
func TestExecutionSnapshotStateMappingAllCasesFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	acceptedExec(t, s, "m", "a-acc")
	rObs := acceptedExec(t, s, "m", "b-obs")
	observeExec(t, s, rObs.ID, pad19(1), "", execution.Observing)
	rCan := acceptedExec(t, s, "m", "c-can")
	observeExec(t, s, rCan.ID, pad19(1), "stopped", execution.Cancelled)
	rSuc := acceptedExec(t, s, "m", "d-suc")
	observeExec(t, s, rSuc.ID, pad19(1), "done", execution.Succeeded)
	rFail := acceptedExec(t, s, "m", "e-fail")
	observeExec(t, s, rFail.ID, pad19(1), "boom", execution.Failed)
	code, raw := getExecution(t, srv.URL, "m")
	if code != 200 {
		t.Fatal(code)
	}
	env := decodeExecution(t, raw)
	want := [][2]string{
		{"exec-a-acc", "running"}, {"exec-b-obs", "running"}, {"exec-c-can", "killed"},
		{"exec-d-suc", "ended"}, {"exec-e-fail", "ended"},
	}
	if len(env.Body.Sessions) != len(want) {
		t.Fatalf("sessions: %v", env.Body.Sessions)
	}
	for i, w := range want {
		got := env.Body.Sessions[i]
		if got["id"] != w[0] || got["state"] != w[1] || got["taskId"] != "m" {
			t.Fatalf("session %d: %v want %v", i, got, w)
		}
	}
}

// K3: intent, dispatch_claimed and unknown executions are absent — none
// of running|killed|ended can be claimed honestly for them.
func TestExecutionSnapshotExcludesUnderivableStatesFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	if _, err := es.IntentWithPolicy("m", "i1", p, p); err != nil {
		t.Fatal(err)
	}
	r2, err := es.IntentWithPolicy("m", "i2", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = es.ClaimDispatch(r2.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	r3 := acceptedExec(t, s, "m", "i3")
	if _, err = es.MarkUnknown(r3.ID, execution.NeedsHuman, "", "src", "hazy"); err != nil {
		t.Fatal(err)
	}
	r4 := acceptedExec(t, s, "m", "i4")
	observeExec(t, s, r4.ID, pad19(1), "", execution.Observing)
	code, raw := getExecution(t, srv.URL, "m")
	if code != 200 {
		t.Fatal(code)
	}
	env := decodeExecution(t, raw)
	if len(env.Body.Sessions) != 1 || env.Body.Sessions[0]["id"] != "exec-i4" {
		t.Fatalf("sessions: %v", env.Body.Sessions)
	}
	for _, absent := range []string{"exec-i1", "exec-i2", "exec-i3"} {
		if strings.Contains(string(raw), absent) {
			t.Fatalf("underivable execution %s leaked: %s", absent, raw)
		}
	}
}

// K4: label carries the observation summary when present and the key is
// omitted entirely when the summary is empty.
func TestExecutionSnapshotLabelOmitFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	rA := acceptedExec(t, s, "m", "with-label")
	observeExec(t, s, rA.ID, pad19(1), "JANUS replay: observing", execution.Observing)
	acceptedExec(t, s, "m", "zz-no-label")
	_, raw := getExecution(t, srv.URL, "m")
	var env struct {
		Body struct {
			Sessions []map[string]json.RawMessage `json:"sessions"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Body.Sessions) != 2 {
		t.Fatal(err, string(raw))
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, sess := range env.Body.Sessions {
		var id string
		if json.Unmarshal(sess["id"], &id) != nil {
			t.Fatal("session without id")
		}
		byID[id] = sess
	}
	var label string
	if v, ok := byID["exec-with-label"]["label"]; !ok || json.Unmarshal(v, &label) != nil || label != "JANUS replay: observing" {
		t.Fatalf("label missing or mutated: %v", byID["exec-with-label"])
	}
	if _, ok := byID["exec-zz-no-label"]["label"]; ok {
		t.Fatalf("empty label not omitted: %v", byID["exec-zz-no-label"])
	}
}

// K5: golden key sets at every level — camelCase, no inferred fields
// from the wider Gunnflow session vocabulary, no state outside the mapping.
func TestExecutionSnapshotSchemaGoldenFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	r := acceptedExec(t, s, "m", "golden")
	observeExec(t, s, r.ID, pad19(1), "요약", execution.Observing)
	_, raw := getExecution(t, srv.URL, "m")
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	for k := range env {
		if k != "revision" && k != "body" {
			t.Fatalf("envelope key %q", k)
		}
	}
	for _, k := range []string{"revision", "body"} {
		if _, ok := env[k]; !ok {
			t.Fatalf("envelope missing %q", k)
		}
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(env["body"], &body); err != nil {
		t.Fatal(err)
	}
	for k := range body {
		if k != "taskId" && k != "sessions" && k != "events" {
			t.Fatalf("body key %q", k)
		}
	}
	var sessions []map[string]json.RawMessage
	if err := json.Unmarshal(body["sessions"], &sessions); err != nil || len(sessions) != 1 {
		t.Fatal(err, string(raw))
	}
	for k := range sessions[0] {
		if k != "id" && k != "taskId" && k != "state" && k != "label" {
			t.Fatalf("session key %q outside contract v1", k)
		}
	}
	for _, k := range []string{"id", "taskId", "state", "label"} {
		if _, ok := sessions[0][k]; !ok {
			t.Fatalf("session missing %q", k)
		}
	}
	// The wider Gunnflow vocabulary must be absent (추론 금지), and no state
	// outside the ratified mapping may appear.
	for _, forbidden := range []string{"agentLabel", "startedAt", "model", "parentSessionId", "policy", "runtime", "capabilities", "paused"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("forbidden vocabulary %q emitted: %s", forbidden, raw)
		}
	}
}

// K6: sessions are isolated per mission and echo their own taskId.
func TestExecutionSnapshotMissionIsolationFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	if _, e := (mission.Service{Store: s}).Create("m2", "g", "둘째", "done"); e != nil {
		t.Fatal(e)
	}
	acceptedExec(t, s, "m", "in-m")
	acceptedExec(t, s, "m2", "in-m2")
	_, raw1 := getExecution(t, srv.URL, "m")
	env1 := decodeExecution(t, raw1)
	if len(env1.Body.Sessions) != 1 || env1.Body.Sessions[0]["id"] != "exec-in-m" || env1.Body.Sessions[0]["taskId"] != "m" || strings.Contains(string(raw1), "exec-in-m2") {
		t.Fatalf("m: %s", raw1)
	}
	_, raw2 := getExecution(t, srv.URL, "m2")
	env2 := decodeExecution(t, raw2)
	if len(env2.Body.Sessions) != 1 || env2.Body.Sessions[0]["id"] != "exec-in-m2" || env2.Body.Sessions[0]["taskId"] != "m2" {
		t.Fatalf("m2: %s", raw2)
	}
}

// K7 (D10): an unknown taskId is 404 on both routes — distinct from an
// existing mission with no sessions (200 + []).
func TestExecutionSnapshotUnknownTaskFRRHZ077(t *testing.T) {
	_, _, srv := execHTTPFixture(t)
	code, _ := getExecution(t, srv.URL, "ghost")
	if code != 404 {
		t.Fatal(code)
	}
	resp, err := http.Get(srv.URL + "/v1/execution/ghost/stream")
	if err != nil || resp.StatusCode != 404 {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
}

// K8 (L-c): revision is the global journal sequence — an unrelated
// append raises it while sessions stay identical.
func TestExecutionSnapshotRevisionIsGlobalSequenceFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	acceptedExec(t, s, "m", "rev")
	_, raw1 := getExecution(t, srv.URL, "m")
	env1 := decodeExecution(t, raw1)
	if env1.Revision != maxSequence(s) {
		t.Fatalf("revision %d want %d", env1.Revision, maxSequence(s))
	}
	if _, e := (mission.Service{Store: s}).Create("m2", "g", "무관", "done"); e != nil {
		t.Fatal(e)
	}
	_, raw2 := getExecution(t, srv.URL, "m")
	env2 := decodeExecution(t, raw2)
	if env2.Revision <= env1.Revision || env2.Revision != maxSequence(s) {
		t.Fatalf("revision not global: %d -> %d", env1.Revision, env2.Revision)
	}
	if fmt.Sprint(env1.Body.Sessions) != fmt.Sprint(env2.Body.Sessions) {
		t.Fatalf("sessions drifted: %v -> %v", env1.Body.Sessions, env2.Body.Sessions)
	}
}

// L1: connect → immediate snapshot; a durable change followed by
// Broadcast pushes the updated projection.
func TestExecutionSSESnapshotThenPushFRRHZ077(t *testing.T) {
	s, h, srv := execHTTPFixture(t)
	r := acceptedExec(t, s, "m", "sse")
	observeExec(t, s, r.ID, pad19(1), "", execution.Observing)
	sse := openExecutionSSE(t, srv.URL, "m")
	kind, data, ok := sse.next(t, 2*time.Second)
	if !ok || kind != "snapshot" || !strings.Contains(data, `"exec-sse"`) || !strings.Contains(data, `"running"`) {
		t.Fatal(kind, data, ok)
	}
	observeExec(t, s, r.ID, pad19(2), "done", execution.Succeeded)
	if p, e := Snapshot(s); e == nil {
		h.Broadcast(p)
	} else {
		t.Fatal(e)
	}
	kind, data, ok = sse.next(t, 2*time.Second)
	if !ok || kind != "projection" || !strings.Contains(data, `"ended"`) {
		t.Fatalf("push: %q %q %v", kind, data, ok)
	}
}

// L2: a rejected intent pushes nothing to the execution stream.
func TestExecutionSSERejectedNoPushFRRHZ077(t *testing.T) {
	_, _, srv := execHTTPFixture(t)
	sse := openExecutionSSE(t, srv.URL, "m")
	if kind, _, ok := sse.next(t, 2*time.Second); !ok || kind != "snapshot" {
		t.Fatal(kind, ok)
	}
	res := postIntent(t, srv.URL, `{"kind":"mission.create","name":"","actor":"op"}`)
	if res["Accepted"] == true || res["accepted"] == true {
		t.Fatal("empty name accepted")
	}
	if kind, data, ok := sse.next(t, 700*time.Millisecond); ok {
		t.Fatalf("rejected intent pushed %q %q", kind, data)
	}
}

// L3 (D12): every accepted change pushes to every subscriber — an
// unrelated mission's change arrives with a higher revision and identical
// sessions; de-duplication is the consumer's revision key job (§2).
func TestExecutionSSEPushOnAnyAcceptedChangeFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	acceptedExec(t, s, "m", "steady")
	sse := openExecutionSSE(t, srv.URL, "m")
	kind, snapData, ok := sse.next(t, 2*time.Second)
	if !ok || kind != "snapshot" {
		t.Fatal(kind, ok)
	}
	res := postIntent(t, srv.URL, `{"kind":"mission.create","name":"other","prompt":"p","actor":"op"}`)
	if res["Accepted"] != true && res["accepted"] != true {
		t.Fatalf("intent rejected: %v", res)
	}
	kind, pushData, ok := sse.next(t, 2*time.Second)
	if !ok || kind != "projection" {
		t.Fatalf("no push on unrelated accepted change: %q %v", kind, ok)
	}
	var snap, push execEnvelope
	if json.Unmarshal([]byte(snapData), &snap) != nil || json.Unmarshal([]byte(pushData), &push) != nil {
		t.Fatal(snapData, pushData)
	}
	if push.Revision <= snap.Revision || fmt.Sprint(push.Body.Sessions) != fmt.Sprint(snap.Body.Sessions) {
		t.Fatalf("semantics: rev %d->%d sessions %v -> %v", snap.Revision, push.Revision, snap.Body.Sessions, push.Body.Sessions)
	}
}

// M1: the terminal surface keeps its 501 (contract unresolved) while
// execution has been separated out of that gate.
func TestTerminal501PreservedExecutionSeparatedFRRHZ077(t *testing.T) {
	_, _, srv := execHTTPFixture(t)
	for _, p := range []string{"/v1/terminal/y", "/v1/terminal/y/stream"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil || resp.StatusCode != http.StatusNotImplemented {
			t.Fatal(p, resp, err)
		}
		var body map[string]string
		if json.NewDecoder(resp.Body).Decode(&body) != nil || !strings.Contains(body["reason"], "JANUS") {
			t.Fatal(body)
		}
		resp.Body.Close()
	}
	if code, _ := getExecution(t, srv.URL, "m"); code != 200 {
		t.Fatal("execution still gated:", code)
	}
}

// M2: session.* intents stay rejected with the upstream reason verbatim
// and append nothing.
func TestSessionIntentStillRejectedFRRHZ077(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	before := len(s.All())
	for _, kind := range []string{"session.kill", "session.pause"} {
		res := postIntent(t, srv.URL, `{"kind":"`+kind+`","taskId":"m","actor":"op"}`)
		reason, _ := res["Reason"].(string)
		if res["Accepted"] == true || reason != "JANUS T17-19 표면 의존" {
			t.Fatalf("%s: %v", kind, res)
		}
	}
	if len(s.All()) != before {
		t.Fatalf("session intent appended: %d->%d", before, len(s.All()))
	}
}

// M4 (D14): only the two contract GET routes exist; every other shape
// under /v1/execution/ falls through to 404.
func TestExecutionRouteShapeFRRHZ077(t *testing.T) {
	_, _, srv := execHTTPFixture(t)
	get := func(p string) int {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	post := func(p string) int {
		resp, err := http.Post(srv.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for p, code := range map[string]int{
		"/v1/execution/":          get("/v1/execution/"),
		"/v1/execution/m/unknown": get("/v1/execution/m/unknown"),
	} {
		if code != 404 {
			t.Fatal(p, code)
		}
	}
	for _, p := range []string{"/v1/execution/m", "/v1/execution/m/stream"} {
		if code := post(p); code != 404 {
			t.Fatal("POST", p, code)
		}
	}
}
