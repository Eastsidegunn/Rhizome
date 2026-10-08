package main

// RHZ-090 (FR-RHZ-118): serve JANUS wiring lit. T1 — the composition-root
// ExecEvents branch (RealReplay over the configured hx → ProjectSessionEvents)
// projects a bound session's log into /v1/execution/<mission> events (≠ []),
// driven by a fake hx that prints a replay fixture. T2 — the flag values used
// by the live serve assemble a non-nil loop and ExecEvents, and a serve whose
// journal holds no execution ticks quietly with an absent socket: no
// "janus loop" output, exit 0 — the evidence that no -janus-dry-run flag is
// needed (Tick over 0 executions / 0 approvals is a no-op and never dials).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

const rhz090Trace = "0123456789abcdef0123456789abcdef"

// fakeHX writes an executable that answers `hx replay --session <db>` with a
// two-row session log (contract envelope: seq contiguous from 1, same trace).
// Any other subcommand exits 3 so an unexpected spawn is loud.
func fakeHX(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	hx := filepath.Join(dir, "hx")
	script := `#!/bin/sh
[ "$1" = "replay" ] || exit 3
printf '%s\n' '{"actor":"parent","kind":"session/start","payload":{},"seq":1,"span_id":"0123456789abcdef","trace_id":"` + rhz090Trace + `","ts":10}'
printf '%s\n' '{"actor":"child","kind":"subagent/usage","payload":{},"seq":2,"span_id":"0123456789abcdef","trace_id":"` + rhz090Trace + `","ts":20,"usage_in":120,"usage_out":45}'
`
	if err := os.WriteFile(hx, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return hx
}

// boundMissionExec creates goal g / mission m and an accepted execution bound
// to a session log path, as the loop would leave it after hx run accepted.
func boundMissionExec(t *testing.T, s *events.Store) execution.Ref {
	t.Helper()
	ms := mission.Service{Store: s}
	if _, e := ms.CreateGoal("g", "goal", "done", ""); e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Create("m", "g", "work", "done"); e != nil {
		t.Fatal(e)
	}
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	r, err := es.IntentWithPolicy("m", "rhz090", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Accept(r.ID, rhz090Trace); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Bind(r.ID, execution.Binding{SessionDB: "/tmp/example-session.db", TraceID: rhz090Trace, PolicyHash: "ph", RequestFingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// T1: with the adapter configured, GET /v1/execution/<mission> runs the
// configured hx replay for the bound session and projects its rows — events
// ≠ [] through the real composition-root branch (no handler-level injection).
func TestServeExecEventsProjectsReplayFixtureFRRHZ118(t *testing.T) {
	s := &events.Store{}
	r := boundMissionExec(t, s)
	jc, err := janusServeFromFlags(fakeHX(t), filepath.Join(t.TempDir(), "absent.sock"), "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	var eout bytes.Buffer
	handler, loop := assembleServe(s, nil, "", "", jc, &eout, false)
	if loop == nil {
		t.Fatal("loop missing despite full configuration")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/execution/m")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	defer resp.Body.Close()
	var env struct {
		Body struct {
			Sessions []map[string]any `json:"sessions"`
			Events   []map[string]any `json:"events"`
		} `json:"body"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if len(env.Body.Sessions) != 1 || env.Body.Sessions[0]["id"] != r.ID || env.Body.Sessions[0]["state"] != "running" {
		t.Fatalf("sessions: %+v", env.Body)
	}
	if len(env.Body.Events) != 2 {
		t.Fatalf("events not projected from hx replay: %+v", env.Body)
	}
	e0, e1 := env.Body.Events[0], env.Body.Events[1]
	if e0["sessionId"] != r.ID || e0["kind"] != "session/start" || e0["seq"].(float64) != 1 {
		t.Fatalf("event0: %v", e0)
	}
	if e1["kind"] != "subagent/usage" || e1["actor"] != "child" || e1["usageIn"].(float64) != 120 || e1["usageOut"].(float64) != 45 {
		t.Fatalf("event1: %v", e1)
	}
	if eout.Len() != 0 {
		t.Fatalf("unexpected loop output on a read: %q", eout.String())
	}
}

// T2a: the live flag set assembles a loop with its seams filled (replay source,
// socket dialer, config) and one Tick over an empty journal with an absent
// socket reports nothing — the adapter idles without JANUS present.
func TestServeJanusLoopIdlesWithoutExecutionsFRRHZ118(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")
	jc, err := janusServeFromFlags(fakeHX(t), sock, "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", time.Second)
	if err != nil || jc == nil {
		t.Fatal(jc, err)
	}
	var eout bytes.Buffer
	s := &events.Store{}
	_, loop := assembleServe(s, nil, "", "", jc, &eout, false)
	if loop == nil || loop.Replay == nil || loop.Client.Dial == nil || loop.ES.Store == nil || loop.AS.Store == nil {
		t.Fatalf("loop seams not wired: %+v", loop)
	}
	if !reflect.DeepEqual(loop.Cfg, jc.Cfg) || loop.Cfg.ApprovalEndpoint != sock {
		t.Fatalf("config not carried into loop: %+v", loop.Cfg)
	}
	if _, err = loop.Client.Dial(); err == nil {
		t.Fatal("absent socket dialed successfully")
	}
	loop.Tick()
	loop.Tick()
	if eout.Len() != 0 {
		t.Fatalf("idle tick produced output: %q", eout.String())
	}
	if len(s.All()) != 0 {
		t.Fatal("idle tick wrote to the journal")
	}
}

// T2b: serve started with the full -janus-* set on a journal without
// executions runs ≥2 ticks against an absent socket and exits 0 on cancel with
// no "janus loop" line — the dry-run question answered by behaviour, not a flag.
func TestServeWithJanusFlagsRunsQuietlyFRRHZ118(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var eout bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- serveCtx(ctx, []string{"-journal", jp, "-addr", "127.0.0.1:0",
			"-janus-hx", fakeHX(t), "-janus-approval-endpoint", filepath.Join(d, "absent.sock"),
			"-janus-profile", filepath.Join(d, "profile.yaml"), "-janus-accept-root", filepath.Join(d, "accept"),
			"-janus-world-config", filepath.Join(d, "world.json"), "-janus-observe-interval", "1s"}, io.Discard, &eout)
	}()
	waitLock(t, jp+".lock", true)
	time.Sleep(2500 * time.Millisecond) // ≥2 ticks at 1s
	cancel()
	waitDone(t, done, &eout, 10*time.Second)
	if strings.Contains(eout.String(), "janus loop") || strings.Contains(eout.String(), "panic") {
		t.Fatalf("loop noise without executions: %q", eout.String())
	}
	waitLock(t, jp+".lock", false)
}
