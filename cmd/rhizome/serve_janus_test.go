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
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/janusadapter"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
	"rhizome/internal/workspace"
)

// Plan A1: with no JANUS flags the adapter is fully disabled — nil config,
// no loop, and the HTTP surface behaves exactly as before.
func TestServeNoJanusFlagsUnchangedFRRHZ077(t *testing.T) {
	jc, err := janusServeFromFlags("", "", "", "", "", "", defaultJanusEnvMode, false, "", 5*time.Second)
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
	jc, err := janusServeFromFlags("hx", "/tmp/approval.sock", "/tmp/profile.yaml", "/tmp/accept", "/tmp/world.json", "/tmp/session.db", defaultJanusEnvMode, false, "", 5*time.Second)
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
	jc, err := janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "/s.db", defaultJanusEnvMode, false, "", 7*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	if jc.HX != "hx" || jc.Interval != 7*time.Second || jc.Cfg.ApprovalEndpoint != "/tmp/a.sock" || jc.Cfg.ProfilePath != "/p.yaml" || jc.Cfg.AcceptRoot != "/ar" || jc.Cfg.WorldConfigPath != "/w.json" || jc.Cfg.SessionDB != "/s.db" {
		t.Fatalf("config mapping: %+v", jc)
	}
	// Session is operator-optional; the other five are all-or-nothing.
	if jc, err = janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second); err != nil || jc == nil || jc.Cfg.SessionDB != "" {
		t.Fatal(jc, err)
	}
	for _, c := range [][6]string{
		{"hx", "", "", "", "", ""},
		{"", "/tmp/a.sock", "", "", "", ""},
		{"", "", "", "", "", "/s.db"},
		{"hx", "/tmp/a.sock", "/p.yaml", "/ar", "", ""},
	} {
		if jc, err = janusServeFromFlags(c[0], c[1], c[2], c[3], c[4], c[5], defaultJanusEnvMode, false, "", 5*time.Second); err == nil || jc != nil {
			t.Fatalf("partial config accepted: %v -> %+v %v", c, jc, err)
		}
	}
	if jc, err = janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 500*time.Millisecond); err == nil || jc != nil {
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

func TestServeJanusEnvModeDefaultsToInheritFRRHZ126(t *testing.T) {
	if defaultJanusEnvMode != janusadapter.EnvModeInherit {
		t.Fatalf("default janus env mode = %q, want %q", defaultJanusEnvMode, janusadapter.EnvModeInherit)
	}
	jc, err := janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	if jc.Cfg.EnvMode != janusadapter.EnvModeInherit {
		t.Fatalf("RunConfig env mode = %q, want inherit", jc.Cfg.EnvMode)
	}
}

func TestServeJanusEnvModeValidationFRRHZ126(t *testing.T) {
	want := "janus-env-mode/janus-env-passthrough requires the janus flag set (janus-hx, janus-approval-endpoint, janus-profile, janus-accept-root, janus-world-config)"
	for name, args := range map[string][]string{
		"invalid mode without janus": {"-janus-env-mode=everything"},
		"inherit without janus":      {"-janus-env-mode=inherit"},
		"passthrough without janus":  {"-janus-env-passthrough=A"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			args = append([]string{"serve", "-journal", dir + "/j.log"}, args...)
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 2 {
				t.Fatalf("exit=%d stderr=%s", code, errOut.String())
			}
			if got := strings.TrimSpace(errOut.String()); got != want {
				t.Fatalf("error = %q, want %q", got, want)
			}
			if _, err := os.Stat(dir + "/j.log"); !os.IsNotExist(err) {
				t.Fatal("journal touched despite env-mode configuration error")
			}
		})
	}
}

func TestServeJanusEnvPassthroughFlagFRRHZ126(t *testing.T) {
	jc, err := janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", janusadapter.EnvModeAllowlist, true, " CUSTOM_TOKEN,API_KEY ", 5*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	want := []string{"CUSTOM_TOKEN", "API_KEY"}
	if len(jc.Cfg.Passthrough) != len(want) || jc.Cfg.Passthrough[0] != want[0] || jc.Cfg.Passthrough[1] != want[1] {
		t.Fatalf("passthrough = %#v, want %#v", jc.Cfg.Passthrough, want)
	}

	dir := t.TempDir()
	var out, errOut bytes.Buffer
	args := []string{"serve", "-journal", dir + "/j.log",
		"-janus-hx", "hx", "-janus-approval-endpoint", "/tmp/a.sock",
		"-janus-profile", "/p.yaml", "-janus-accept-root", "/ar",
		"-janus-world-config", "/w.json", "-janus-env-passthrough", "CUSTOM_TOKEN"}
	if code := run(args, &out, &errOut); code != 2 {
		t.Fatalf("passthrough without allowlist mode: exit=%d stderr=%s", code, errOut.String())
	}
	wantErr := "janus-env-passthrough requires janus-env-mode=allowlist"
	if got := strings.TrimSpace(errOut.String()); got != wantErr {
		t.Fatalf("error = %q, want %q", got, wantErr)
	}
	if _, err := os.Stat(dir + "/j.log"); !os.IsNotExist(err) {
		t.Fatal("journal touched despite passthrough configuration error")
	}
}

func TestServeJanusEnvPassthroughNamesOnlyFRRHZ126(t *testing.T) {
	if jc, err := janusServeFromFlags("hx", "/tmp/a.sock", "/p.yaml", "/ar", "/w.json", "", janusadapter.EnvModeAllowlist, true, "API_KEY=value", 5*time.Second); err == nil || jc != nil {
		t.Fatalf("name/value passthrough accepted: config=%+v err=%v", jc, err)
	}
}

func TestAssembleServeJanusEnvWiringFRRHZ126(t *testing.T) {
	want := janusadapter.RunConfig{
		ProfilePath:      "/p.yaml",
		AcceptRoot:       "/ar",
		WorldConfigPath:  "/w.json",
		ApprovalEndpoint: "/tmp/a.sock",
		EnvMode:          janusadapter.EnvModeAllowlist,
		Passthrough:      []string{"A", "B"},
	}
	jc, err := janusServeFromFlags("hx", want.ApprovalEndpoint, want.ProfilePath, want.AcceptRoot, want.WorldConfigPath, "", want.EnvMode, true, "A,B", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var starterCfg janusadapter.RunConfig
	oldInspect := assembleServeInspect
	assembleServeInspect = func(h *workspace.HTTPServer) {
		starter, ok := h.ExecStart.(execStarter)
		if !ok {
			t.Fatalf("ExecStart = %T, want execStarter", h.ExecStart)
		}
		starterCfg = starter.st.Cfg
	}
	t.Cleanup(func() { assembleServeInspect = oldInspect })
	_, loop := assembleServe(&events.Store{}, nil, "", "", jc, io.Discard)
	if loop == nil {
		t.Fatal("loop missing")
	}
	if !reflect.DeepEqual(loop.Cfg, want) {
		t.Fatalf("Loop.Cfg = %#v, want %#v", loop.Cfg, want)
	}
	if !reflect.DeepEqual(starterCfg, want) {
		t.Fatalf("Starter.Cfg = %#v, want %#v", starterCfg, want)
	}
}

func TestRunServeJanusEnvFlagsFRRHZ126(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"serve", "-journal", jp, "-addr", "127.0.0.1:0",
			"-janus-hx", "hx", "-janus-approval-endpoint", filepath.Join(d, "approval.sock"),
			"-janus-profile", "/p.yaml", "-janus-accept-root", "/ar", "-janus-world-config", "/w.json",
			"-janus-env-mode", "allowlist", "-janus-env-passthrough", "A,B"}, &out, &errOut)
	}()
	waitLock(t, jp+".lock", true)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done, &errOut, 10*time.Second)
	waitLock(t, jp+".lock", false)
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
