package workspace

// RHZ-050 supplemental self-report coverage (FR-RHZ-081). These tests stay
// in-process so the HTTP bind restriction does not hide aggregate invariants.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"rhizome/internal/domain"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/question"
)

// TestRHZ050QuestionHTTPRoundTripSSEAndDecisionFRRHZ081 covers the complete
// internal gate path through the real HTTP handler, including both pushes.
func TestRHZ050QuestionHTTPRoundTripSSEAndDecisionFRRHZ081(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-z1")
	srv := httptest.NewServer(NewHTTP(s).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v1/workspace/stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: ") {
				lines <- strings.TrimPrefix(sc.Text(), "event: ")
			}
		}
	}()
	select {
	case got := <-lines:
		if got != "snapshot" {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot timeout")
	}
	post := func(body string) map[string]any {
		r, e := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("status %d: %s", r.StatusCode, b)
		}
		var v map[string]any
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	// RHZ-071 (FR-RHZ-100): question.ask now requires an existing missionId;
	// the mission is seeded before the SSE subscription so the first push is
	// still the snapshot.
	if v := post(`{"kind":"question.ask","name":"Z1 title","body":"Z1 body","recommendation":"use it","missionId":"mission-z1","actor":"operator"}`); v["accepted"] != true && v["Accepted"] != true {
		t.Fatalf("ask rejected: %v", v)
	}
	select {
	case got := <-lines:
		if got != "projection" {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask projection timeout")
	}
	get := func() map[string]json.RawMessage {
		r, e := http.Get(srv.URL + "/v1/workspace")
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		var v map[string]json.RawMessage
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	env := get()
	var body struct {
		Gates []Gate `json:"gates"`
	}
	if e := json.Unmarshal(env["body"], &body); e != nil || len(body.Gates) != 1 {
		t.Fatalf("pending: %+v %v", body, e)
	}
	g := body.Gates[0]
	if g.State != "pending" || g.Source != "internal" || g.RequestDigest == "" {
		t.Fatalf("pending gate: %+v", g)
	}
	if v := post(`{"kind":"gate.approve","gateId":"` + g.ID + `","digest":"` + g.RequestDigest + `","actor":"test-operator"}`); v["accepted"] != true && v["Accepted"] != true {
		t.Fatalf("approve rejected: %v", v)
	}
	select {
	case got := <-lines:
		if got != "projection" {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approve projection timeout")
	}
	env = get()
	if e := json.Unmarshal(env["body"], &body); e != nil || len(body.Gates) != 1 {
		t.Fatal(e)
	}
	g = body.Gates[0]
	if g.State != "approved" || g.DecidedBy != "unverified-local-operator:test-operator" || !strings.HasPrefix(g.DecidedBy, "unverified-local-operator:") {
		t.Fatalf("approved gate: %+v", g)
	}
}

// TestRHZ050QuestionJournalRestartFRRHZ081 verifies durable question replay.
func TestRHZ050QuestionJournalRestartFRRHZ081(t *testing.T) {
	path := t.TempDir() + "/events.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	q, err := (question.Service{Store: j}).Ask("title", "body", "recommend", "", "", "actor", "corr")
	if err != nil {
		t.Fatal(err)
	}
	q, err = (question.Service{Store: j}).Answer(q.ID, question.Approve, "", "test-operator", q.Digest)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := j.All()
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	got, err := (question.Service{Store: j}).Get(q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, q) {
		t.Fatalf("question mismatch: got %+v want %+v", got, q)
	}
	gotEvents := j.List("question", q.ID)
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("journal events mismatch: got %#v want %#v", gotEvents, wantEvents)
	}
	p, err := Snapshot(j)
	if err != nil || len(p.Gates) != 1 {
		t.Fatalf("snapshot: %+v %v", p, err)
	}
	if p.Gates[0].Body != "body" || p.Gates[0].RequestDigest != q.Digest || p.Gates[0].DecidedBy != "unverified-local-operator:test-operator" {
		t.Fatalf("restored gate: %+v", p.Gates[0])
	}
}

// TestRHZ050SpecialCharacterJSONRoundTripFRRHZ081 guards raw JSON validity and bytes.
func TestRHZ050SpecialCharacterJSONRoundTripFRRHZ081(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-special") // RHZ-071 (FR-RHZ-100): missionId required
	srv := httptest.NewServer(NewHTTP(s).Handler())
	t.Cleanup(srv.Close)
	bodyText := "<tag> \"quote\"\n개행 한글 😀"
	res, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(`{"kind":"question.ask","name":"special","body":`+mustJSONString(bodyText)+`,"missionId":"mission-special","actor":"operator"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	r, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err = json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var b struct {
		Gates []Gate `json:"gates"`
	}
	if err = json.Unmarshal(env["body"], &b); err != nil || len(b.Gates) != 1 {
		t.Fatalf("json: %v %+v", err, b)
	}
	if b.Gates[0].Body != bodyText {
		t.Fatalf("body bytes changed: %q", b.Gates[0].Body)
	}
}

func mustJSONString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRHZ050QuestionPendingAndAnsweredProjectionFRRHZ081(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("특수 <title>", "body \"quoted\"\n한글", "recommend", "", "", "operator", "corr")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Snapshot(s)
	if err != nil || len(p.Gates) != 1 {
		t.Fatalf("pending snapshot: %+v %v", p, err)
	}
	g := p.Gates[0]
	if g.Source != "internal" || g.State != "pending" || g.Body != "body \"quoted\"\n한글" || g.Recommendation != "recommend" {
		t.Fatalf("pending gate: %+v", g)
	}
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: q.ID, Digest: q.Digest}, "operator", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("approve: %+v %v", res, err)
	}
	for _, e := range s.All()[before:] {
		if e.AggregateType == "approval" {
			t.Fatal("internal gate created approval aggregate")
		}
	}
	p, err = Snapshot(s)
	if err != nil || len(p.Gates) != 1 || p.Gates[0].State != "approved" || p.Gates[0].DecisionReason != "" || p.Gates[0].DecidedBy != "unverified-local-operator:operator" {
		t.Fatalf("answered gate: %+v %v", p, err)
	}
}

func TestRHZ050QuestionRelayValidationFRRHZ081(t *testing.T) {
	s := &events.Store{}
	// RHZ-071 (FR-RHZ-100): missionId is now checked first, so the title/body
	// validation is only reached with a real mission — pin the Reason so this
	// test keeps covering the kernel's title/body rule, not the missionId gate.
	missionIn062(t, s, "mission-v", domain.MissionReady, domain.MissionRunning)
	if r, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "", Body: "body", MissionID: "mission-v"}, "operator", noAuthority()); err != nil || r.Accepted || r.Reason != "title and body required" {
		t.Fatalf("missing title: %+v %v", r, err)
	}
	if r, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "title", Body: "", MissionID: "mission-v"}, "operator", noAuthority()); err != nil || r.Accepted || r.Reason != "title and body required" {
		t.Fatalf("missing body: %+v %v", r, err)
	}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	if r, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: q.ID, Digest: ""}, "operator", noAuthority()); err != nil || r.Accepted || len(s.All()) != before {
		t.Fatalf("missing digest accepted: %+v %v", r, err)
	}
	if r, err := RelayIntent(s, Intent{Kind: "gate.reject", GateID: q.ID, Digest: q.Digest}, "operator", noAuthority()); err != nil || r.Accepted || r.Reason != "reason required" {
		t.Fatalf("missing reject reason: %+v %v", r, err)
	}
}

func TestRHZ050QuestionDTOGoldenAndDeterminismFRRHZ081(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(toDTO(p))
	if err != nil {
		t.Fatal(err)
	}
	var gates []map[string]json.RawMessage
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env["gates"], &gates); err != nil || len(gates) != 1 {
		t.Fatal("gate dto")
	}
	for _, key := range []string{"source", "body", "recommendation"} {
		if _, ok := gates[0][key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	for _, key := range []string{"decisionReason", "decidedBy"} {
		if _, ok := gates[0][key]; ok {
			t.Fatalf("pre-decision field %s present", key)
		}
	}
	if q.ID != p.Gates[0].ID {
		t.Fatal("id mismatch")
	}
}

type corruptQuestionPort struct{ events.Port }

func (p corruptQuestionPort) List(kind, id string) []events.Event {
	log := p.Port.List(kind, id)
	if kind == "question" && len(log) > 0 {
		log[0].Payload = []byte(`{"title":"","body":"body","requestedBy":"unverified-local-operator:operator","digest":"` + question.Digest("title", "body", "recommend") + `"}`)
	}
	return log
}

func TestRHZ050QuestionCorruptStreamSurfacesErrorFRRHZ081(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = q
	if _, err := Snapshot(corruptQuestionPort{s}); err == nil {
		t.Fatal("corrupt question stream hidden")
	}
}
