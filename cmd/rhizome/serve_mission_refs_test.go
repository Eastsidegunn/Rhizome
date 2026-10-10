package main

// RHZ-107 G1 (FR-RHZ-140, SCP §5 / §8.1 ZV): mission.start refs through
// POST /v1/intent on the serve assembly with a file journal and a Fake
// Runner. Malformed refs carrying a crypto/rand sentinel are rejected with
// the fixed X7 reason; the sentinel never reaches the journal file, any
// HTTP response or the run request bytes. Valid refs land in the provenance
// approval record (not enforced) and leave the run request unchanged.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/execution"
	"rhizome/internal/janusadapter"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/trust"
)

func zvForms140(s string) []string {
	h := sha256.Sum256([]byte(s))
	return []string{s, hex.EncodeToString(h[:]), base64.StdEncoding.EncodeToString([]byte(s)), base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
}

func TestServeMissionStartRefsZVFRRHZ140(t *testing.T) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sn := "rhzsntl-" + hex.EncodeToString(b)
	jpath := filepath.Join(t.TempDir(), "journal.ndjson")
	j, err := journal.OpenGuarded(jpath, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-1", "g", "ship it", "done"); err != nil {
		t.Fatal(err)
	}
	refA, refB := "sref-"+strings.Repeat("a", 24), "sref-"+strings.Repeat("b", 24)
	ledger := filepath.Join(t.TempDir(), "exec.json")
	body := `{"adapterId":"claudecode","profileId":"manual","profileHash":"` + strings.Repeat("cd", 32) + `","workspaceRef":"/workspace","scope":"example/test","sessionMode":"multiturn","ceiling":{"tokens":200000,"timeMs":600000,"maxDepth":2},"refCeiling":["` + refA + `"]}`
	if err := os.WriteFile(ledger, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jc, err := janusServeFromFlags(fakeHX(t), filepath.Join(t.TempDir(), "absent.sock"), "/p.yaml", "/ar", "/w.json", "", defaultJanusEnvMode, false, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if jc.Exec, err = janusExecConfig(true, ledger); err != nil {
		t.Fatal(err)
	}
	var runReqs [][]byte
	jc.Runner = func(cfg janusadapter.RunConfig, req []byte) (io.Reader, error) {
		runReqs = append(runReqs, append([]byte(nil), req...))
		var seen map[string]any
		_ = json.Unmarshal(req, &seen)
		c := map[string]any{"version": 1, "status": "accepted", "session_ref": map[string]any{"session_db": "/tmp/s.db", "trace_id": rhz092Trace}, "idempotency_key": seen["idempotency_key"], "request_fingerprint": seen["request_fingerprint"], "policy_hash": "ph", "acceptance_seq": 1, "launch_claimed": true}
		out, _ := json.Marshal(c)
		return strings.NewReader(string(out) + "\n"), nil
	}
	handler, _ := assembleServe(j, nil, "", "", jc, io.Discard, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	// ZV ③: workspace and execution SSE are captured across the whole run.
	sse := func(path string) func(string) []byte {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatal(path, resp, err)
		}
		var mu sync.Mutex
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() {
			defer close(done)
			p := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(p)
				mu.Lock()
				buf.Write(p[:n])
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		// stop waits (bounded) until want has been pushed, then disconnects.
		return func(want string) []byte {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				mu.Lock()
				got := bytes.Contains(buf.Bytes(), []byte(want))
				mu.Unlock()
				if got {
					break
				}
			}
			cancel()
			<-done
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			return append([]byte(nil), buf.Bytes()...)
		}
	}
	stopWS := sse("/v1/workspace/stream")
	stopExec := sse("/v1/execution/mission-1/stream")
	var responses [][]byte
	post := func(body string) []byte {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		responses = append(responses, out)
		return out
	}
	before, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	for _, refs := range []string{
		`["` + sn + `"]`,
		`["` + refA + `","` + sn + `"]`,
		`["sref-` + sn[8:32] + `x"]`,
		`["` + refA + `","` + refA + `"]`,
	} {
		out := post(`{"kind":"mission.start","missionId":"mission-1","actor":"operator","refs":` + refs + `}`)
		if !bytes.Contains(out, []byte(`"Reason":"secret intent: invalid ref"`)) {
			t.Fatalf("%s", out)
		}
	}
	after, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(runReqs) != 0 {
		t.Fatal("X7 rejection wrote to the journal or ran")
	}
	// A valid start: refs recorded, run request bytes as a refs-free start.
	out := post(`{"kind":"mission.start","missionId":"mission-1","actor":"operator","refs":["` + refB + `","` + refA + `"]}`)
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	id, _ := res["executionId"].(string)
	if res["Accepted"] != true || id == "" {
		t.Fatalf("%s", out)
	}
	ref, err := execution.Replay(j.List("execution", id))
	if err != nil || ref.Provenance == nil {
		t.Fatalf("%+v %v", ref, err)
	}
	if p := ref.Provenance; len(p.RequestedRefs) != 2 || p.RequestedRefs[0] != refA || p.RequestedRefs[1] != refB || len(p.AuthorizedRefs) != 1 || p.AuthorizedRefs[0] != refA {
		t.Fatalf("%+v", p)
	}
	if len(runReqs) != 1 || bytes.Contains(runReqs[0], []byte("sref-")) {
		t.Fatalf("run requests %q", runReqs)
	}
	for _, path := range []string{"/v1/workspace", "/v1/context", "/v1/execution/mission-1"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		responses = append(responses, b)
	}
	wsSSE, execSSE := stopWS("event: projection"), stopExec(id)
	if !bytes.Contains(wsSSE, []byte("event: projection")) || !bytes.Contains(execSSE, []byte("data: ")) || !bytes.Contains(execSSE, []byte(id)) {
		t.Fatalf("SSE not captured:\n%s\n%s", wsSSE, execSSE)
	}
	responses = append(responses, wsSSE, execSSE)
	jbytes, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string][]byte{"journal": jbytes, "http": bytes.Join(responses, []byte{'\n'}), "run request": bytes.Join(runReqs, []byte{'\n'})}
	for name, data := range targets {
		for _, form := range zvForms140(sn) {
			if n := bytes.Count(data, []byte(form)); n != 0 {
				t.Errorf("ZV: %s carries a sentinel form %d times", name, n)
			}
		}
	}
}
