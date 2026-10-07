package main

// RHZ-096 (FR-RHZ-124): serve assembly — mission.start through POST
// /v1/intent journals the provenance with the relay actor (threaded through
// toStartRequest) and /v1/execution projects the run's limits. Fake Runner
// only.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/execution"
	"rhizome/internal/janusadapter"
)

func TestServeMissionStartProvenanceFRRHZ124(t *testing.T) {
	s := rhz092Store(t)
	jc, err := janusServeFromFlags(fakeHX(t), filepath.Join(t.TempDir(), "absent.sock"), "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if jc.Exec, err = janusExecConfig(true, execConfigFile(t)); err != nil {
		t.Fatal(err)
	}
	jc.Runner = func(cfg janusadapter.RunConfig, req []byte) (io.Reader, error) {
		var seen map[string]any
		_ = json.Unmarshal(req, &seen)
		c := map[string]any{"version": 1, "status": "accepted", "session_ref": map[string]any{"session_db": "/tmp/s.db", "trace_id": rhz092Trace}, "idempotency_key": seen["idempotency_key"], "request_fingerprint": seen["request_fingerprint"], "policy_hash": "ph", "acceptance_seq": 1, "launch_claimed": true}
		b, _ := json.Marshal(c)
		return strings.NewReader(string(b) + "\n"), nil
	}
	handler, _ := assembleServe(s, nil, "", "", jc, io.Discard)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","actor":"operator","budget":{"tokens":1000}}`)
	id, _ := out["executionId"].(string)
	if out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	ref, err := execution.Replay(s.List("execution", id))
	if err != nil || ref.Provenance == nil {
		t.Fatalf("%+v %v", ref, err)
	}
	p := ref.Provenance
	if p.Actor != "unverified-local-operator:operator" || p.Effective.Budget != 1000 || p.Ceiling.Budget != jc.Exec.Ceiling.Tokens || p.ExecConfigDigest != jc.Exec.Digest() || p.ProfileID != jc.Exec.ProfileID {
		t.Fatalf("%+v", p)
	}
	resp, err := http.Get(srv.URL + "/v1/execution/mission-1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"limits":{"ceiling":{"tokens":`) || !strings.Contains(string(b), `"effective":{"tokens":1000,`) {
		t.Fatalf("%s", b)
	}
}
