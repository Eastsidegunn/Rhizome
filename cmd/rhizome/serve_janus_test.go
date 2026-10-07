package main

// RHZ-046 serve assembly tests: adapter absent = full preservation, adapter
// present adds only the internal loop (part 1), the /v1/execution surface is
// part 2's contract routes, and flag assembly is loud about partial
// configuration.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

// Plan A1: with no JANUS flags the adapter is fully disabled — nil config,
// no loop, and the HTTP surface behaves exactly as before.
func TestServeNoJanusFlagsUnchangedFRRHZ077(t *testing.T) {
	jc, err := janusServeFromFlags("", "", "", "", "", "", 5*time.Second)
	if err != nil || jc != nil {
		t.Fatal(jc, err)
	}
	handler, loop := assembleServe(&events.Store{}, nil, "", "", jc, io.Discard)
	if loop != nil {
		t.Fatal("loop constructed without configuration")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	// Part 2: /v1/execution/{taskId} is a contract route — an unknown task
	// is 404 (D10); the workspace surface is untouched.
	resp, err := http.Get(srv.URL + "/v1/execution/x")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/v1/terminal/y")
	if err != nil || resp.StatusCode != http.StatusNotImplemented {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
}

// B1: adapter presence changes no HTTP
// routes — /v1/execution follows the contract surface (unknown task = 404,
// D10) and /v1/terminal keeps its 501, with or without the loop.
func TestExecutionRoutesIdenticalWithAdapterFRRHZ077(t *testing.T) {
	jc, err := janusServeFromFlags("hx", "/tmp/approval.sock", "/tmp/profile.yaml", "/tmp/accept", "/tmp/world.json", "/tmp/session.db", 5*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	handler, loop := assembleServe(&events.Store{}, nil, "", "", jc, io.Discard)
	if loop == nil {
		t.Fatal("loop missing despite full configuration")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	for _, p := range []string{"/v1/execution/x", "/v1/execution/x/stream"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil || resp.StatusCode != http.StatusNotFound {
			t.Fatal(p, resp, err)
		}
		resp.Body.Close()
	}
	resp, err := http.Get(srv.URL + "/v1/terminal/y")
	if err != nil || resp.StatusCode != http.StatusNotImplemented {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
}

// Plan J4: full flags map verbatim into the operator config; a partial set
// or a sub-second interval is a configuration error (exit 2 through serve),
// never a silent disable (D4, D9).
func TestServeJanusFlagParseFRRHZ077(t *testing.T) {
	jc, err := janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "/s.db", 7*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	if jc.HX != "hx" || jc.Interval != 7*time.Second || jc.Cfg.ApprovalEndpoint != "/tmp/a.sock" || jc.Cfg.ProfilePath != "/p.yaml" || jc.Cfg.AcceptRoot != "/ar" || jc.Cfg.WorldConfigPath != "/w.json" || jc.Cfg.SessionDB != "/s.db" {
		t.Fatalf("config mapping: %+v", jc)
	}
	// Session is operator-optional; the other five are all-or-nothing.
	if jc, err = janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", 5*time.Second); err != nil || jc == nil || jc.Cfg.SessionDB != "" {
		t.Fatal(jc, err)
	}
	for _, c := range [][6]string{
		{"hx", "", "", "", "", ""},
		{"", "/tmp/a.sock", "", "", "", ""},
		{"", "", "", "", "", "/s.db"},
		{"hx", "/tmp/a.sock", "/p.yaml", "/ar", "", ""},
	} {
		if jc, err = janusServeFromFlags(c[0], c[1], c[2], c[3], c[4], c[5], 5*time.Second); err == nil || jc != nil {
			t.Fatalf("partial config accepted: %v -> %+v %v", c, jc, err)
		}
	}
	if jc, err = janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", 500*time.Millisecond); err == nil || jc != nil {
		t.Fatal("sub-second interval accepted", jc, err)
	}
	// serve refuses loudly before touching the journal.
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if code := run([]string{"serve", "-journal", dir + "/j.log", "-janus-hx", "hx"}, &out, &errOut); code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "incomplete janus configuration") {
		t.Fatalf("silent failure: %q", errOut.String())
	}
	if _, err := os.Stat(dir + "/j.log"); !os.IsNotExist(err) {
		t.Fatal("journal touched despite configuration error")
	}
	if _, err := os.Stat(dir + "/j.log.lock"); !os.IsNotExist(err) {
		t.Fatal("lock created despite configuration error")
	}
}

// Plan M3 (RHZ-046 part 2): the execution surface is a pure function of the
// journal projection — it serves observed data even when the JANUS adapter
// is fully disabled and no loop exists.
func TestExecutionSurfaceWorksWithoutAdapterFRRHZ077(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, e := ms.CreateGoal("g", "goal", "done", ""); e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Create("m", "g", "work", "done"); e != nil {
		t.Fatal(e)
	}
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	r, err := es.IntentWithPolicy("m", "no-adapter", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Accept(r.ID, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err = es.ObserveState(r.ID, "0000000000000000001", "", "src", execution.Observing); err != nil {
		t.Fatal(err)
	}
	handler, loop := assembleServe(s, nil, "", "", nil, io.Discard)
	if loop != nil {
		t.Fatal("loop constructed without configuration")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/execution/m")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	defer resp.Body.Close()
	var env struct {
		Revision uint64 `json:"revision"`
		Body     struct {
			TaskID   string           `json:"taskId"`
			Sessions []map[string]any `json:"sessions"`
		} `json:"body"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Body.TaskID != "m" || len(env.Body.Sessions) != 1 || env.Body.Sessions[0]["id"] != r.ID || env.Body.Sessions[0]["state"] != "running" {
		t.Fatalf("surface without adapter: %+v", env)
	}
}
