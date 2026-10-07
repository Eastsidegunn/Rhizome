package main

// RHZ-092 (FR-RHZ-123): -janus-exec-config flag (D4 coupling, invalid file
// = exit 2) and the serve assembly: mission.start through POST /v1/intent
// with a Fake Runner lands intent/claim/accepted and shows as a running
// session on /v1/execution; without a ledger it is rejected with zero
// writes. No hx process is ever started.

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
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/janusadapter"
	"rhizome/internal/mission"
)

const rhz092Trace = "0123456789abcdef0123456789abcdef"

func execConfigFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "exec.json")
	body := `{"adapterId":"claudecode","profileId":"manual","profileHash":"` + strings.Repeat("cd", 32) + `","workspaceRef":"/workspace","scope":"example/test","sessionMode":"multiturn","ceiling":{"tokens":200000,"timeMs":600000,"maxDepth":2}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServeExecConfigFlagFRRHZ123(t *testing.T) {
	cfg := execConfigFile(t)
	if c, err := janusExecConfig(true, ""); err != nil || c != nil {
		t.Fatal(c, err)
	}
	if _, err := janusExecConfig(false, cfg); err == nil || !strings.Contains(err.Error(), "janus-exec-config requires") {
		t.Fatal(err)
	}
	c, err := janusExecConfig(true, cfg)
	if err != nil || c == nil || c.Ceiling.Tokens != 200000 || c.SessionMode != "multiturn" {
		t.Fatalf("%+v %v", c, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"adapterId":"claudecode","apiKey":"TEST-NON-CREDENTIAL"}`), 0o600)
	if _, err := janusExecConfig(true, bad); err == nil {
		t.Fatal("invalid ledger accepted")
	}
	// serve: ledger without the JANUS flag set is a configuration error (exit 2); an invalid ledger too.
	var eout bytes.Buffer
	if code := serve([]string{"-journal", filepath.Join(t.TempDir(), "j.ndjson"), "-janus-exec-config", cfg}, &bytes.Buffer{}, &eout); code != 2 || !strings.Contains(eout.String(), "janus-exec-config") {
		t.Fatalf("exit %d: %s", code, eout.String())
	}
	eout.Reset()
	args := []string{"-journal", filepath.Join(t.TempDir(), "j.ndjson"), "-janus-hx", fakeHX(t), "-janus-approval-endpoint", "/tmp/absent.sock", "-janus-profile", "/p.yaml", "-janus-accept-root", "/ar", "-janus-world-config", "/w.json", "-janus-exec-config", bad}
	if code := serve(args, &bytes.Buffer{}, &eout); code != 2 || !strings.Contains(eout.String(), "exec config") {
		t.Fatalf("exit %d: %s", code, eout.String())
	}
}

func rhz092Store(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-1", "g", "ship it", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func postIntent(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return out
}

func TestServeMissionStartWiredFRRHZ123(t *testing.T) {
	s := rhz092Store(t)
	jc, err := janusServeFromFlags(fakeHX(t), filepath.Join(t.TempDir(), "absent.sock"), "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	exec, err := janusExecConfig(true, execConfigFile(t))
	if err != nil {
		t.Fatal(err)
	}
	jc.Exec = exec
	calls := 0
	var seen map[string]any
	jc.Runner = func(cfg janusadapter.RunConfig, req []byte) (io.Reader, error) {
		calls++
		if !reflect.DeepEqual(cfg, jc.Cfg) {
			t.Errorf("runner config %+v", cfg)
		}
		_ = json.Unmarshal(req, &seen)
		c := map[string]any{"version": 1, "status": "accepted", "session_ref": map[string]any{"session_db": "/tmp/s.db", "trace_id": rhz092Trace}, "idempotency_key": seen["idempotency_key"], "request_fingerprint": seen["request_fingerprint"], "policy_hash": "ph", "acceptance_seq": 1, "launch_claimed": true}
		b, _ := json.Marshal(c)
		return strings.NewReader(string(b) + "\n"), nil
	}
	handler, loop := assembleServe(s, nil, "", "", jc, io.Discard)
	if loop == nil {
		t.Fatal("loop")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	before := len(s.All())
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","budget":{"tokens":1000}}`)
	id, _ := out["executionId"].(string)
	if out["Accepted"] != true || !strings.HasPrefix(id, "exec-") {
		t.Fatalf("%v", out)
	}
	if calls != 1 || seen["task_ref"].(map[string]any)["instruction"] != "ship it" || seen["budget"].(map[string]any)["tokens"].(float64) != 1000 || seen["budget"].(map[string]any)["time_ms"].(float64) != 600000 || seen["session_mode"] != "multiturn" {
		t.Fatalf("calls=%d request=%v", calls, seen)
	}
	// planned → ready → running + intent/claim/accepted = 5 events.
	if len(s.All()) != before+5 {
		t.Fatalf("journal grew by %d", len(s.All())-before)
	}
	resp, err := http.Get(srv.URL + "/v1/execution/mission-1")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Body struct {
			Sessions []struct{ ID, State string }
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if len(env.Body.Sessions) != 1 || env.Body.Sessions[0].ID != id || env.Body.Sessions[0].State != "running" {
		t.Fatalf("%+v", env)
	}
	// Idempotent re-submission: same id, no new journal event, Runner lookup.
	before = len(s.All())
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","budget":{"tokens":1000}}`); out["Accepted"] != true || out["executionId"] != id || len(s.All()) != before {
		t.Fatalf("%v writes=%d", out, len(s.All())-before)
	}
	// The loop ticks do not start anything (D7).
	loop.Tick()
	if calls != 1 || len(s.All()) != before {
		t.Fatalf("tick: calls=%d writes=%d", calls, len(s.All())-before)
	}
	// Over the ceiling: rejected, zero writes, no Runner contact.
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","instruction":"more","budget":{"maxDepth":9}}`); out["Accepted"] != false || !strings.HasPrefix(out["Reason"].(string), "POLICY_DENIED: budget.maxDepth") || len(s.All()) != before || calls != 1 {
		t.Fatalf("%v", out)
	}
}

func TestServeMissionStartWithoutLedgerFRRHZ123(t *testing.T) {
	s := rhz092Store(t)
	jc, err := janusServeFromFlags(fakeHX(t), filepath.Join(t.TempDir(), "absent.sock"), "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jc.Runner = func(janusadapter.RunConfig, []byte) (io.Reader, error) {
		t.Fatal("runner without ledger")
		return nil, nil
	}
	handler, _ := assembleServe(s, nil, "", "", jc, io.Discard)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	before := len(s.All())
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1"}`); out["Accepted"] != false || out["Reason"] != "execution config not loaded" || len(s.All()) != before {
		t.Fatalf("%v", out)
	}
	// Adapter fully off: the hook is nil.
	handler, _ = assembleServe(s, nil, "", "", nil, io.Discard)
	srv2 := httptest.NewServer(handler)
	defer srv2.Close()
	if out := postIntent(t, srv2.URL, `{"kind":"mission.start","missionId":"mission-1"}`); out["Accepted"] != false || out["Reason"] != "execution start unavailable" || len(s.All()) != before {
		t.Fatalf("%v", out)
	}
}
