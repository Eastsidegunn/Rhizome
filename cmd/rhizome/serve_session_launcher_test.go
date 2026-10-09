package main

// RHZ-124 S1 (FR-RHZ-TBD(124-S1)): -session-launcher-config flag (exclusive
// with -janus-exec-config, invalid ledger = exit 2) and the serve assembly:
// mission.start through POST /v1/intent spawns a fake claude, the mission
// reaches running only after accepted, the finished session lands exactly one
// usage note and the mission waits for a result. Fake binary only.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/journal"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
	"rhizome/internal/sessionlauncher"
	"rhizome/internal/trust"
)

type slFakeEnv struct{ dir, bin, cfg string }

func (f slFakeEnv) runs() int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "count"))
	return strings.Count(string(b), "run\n")
}

// slFake writes a fake claude (records a run, then runs body) and a ledger
// file pointing at it.
func slFake(t *testing.T, body string) slFakeEnv {
	t.Helper()
	root := t.TempDir()
	f := slFakeEnv{dir: filepath.Join(root, "rec"), bin: filepath.Join(root, "claude"), cfg: filepath.Join(root, "launcher.json")}
	work, logs := filepath.Join(root, "work"), filepath.Join(root, "logs")
	for _, d := range []string{f.dir, work, logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nD='" + f.dir + "'\necho run >> \"$D/count\"\n" + body + "\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, _ := json.Marshal(map[string]any{"claudePath": f.bin, "workdirs": map[string]any{"rhizome": work}, "defaultWorkdir": "rhizome", "model": "claude-opus-5-5",
		"permissionMode": "acceptEdits", "allowedTools": []any{"Read"}, "logDir": logs, "maxConcurrent": 2, "ceiling": map[string]any{"usd": 5, "timeMs": 600000}})
	if err := os.WriteFile(f.cfg, ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

const slResult = `{"type":"result","subtype":"success","is_error":false,"result":"finished","total_cost_usd":0.987654321098765432,"num_turns":4,"duration_ms":1200,"terminal_reason":"completed","permission_denials":[],"modelUsage":{"claude-opus-5-5":{"inputTokens":100,"outputTokens":200,"cacheReadInputTokens":300,"cacheCreationInputTokens":400,"costUSD":0.9000000000000000111},"claude-haiku-4-5":{"inputTokens":5,"outputTokens":6,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.087654321098765432}}}`

const slBlocking = `while [ ! -f "$D/release" ]; do sleep 0.02; done
cat <<'JSON'
` + slResult + `
JSON`

func slLauncher(t *testing.T, f slFakeEnv) *sessionlauncher.Launcher {
	t.Helper()
	c, err := sessionLauncherConfig(f.cfg, "")
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	return &sessionlauncher.Launcher{Cfg: c}
}

func usageNotes(t *testing.T, s events.Port, missionID string) []memory.Memory {
	t.Helper()
	all, err := (memory.Service{Store: s}).Search(memory.Observation, "usage", "")
	if err != nil {
		t.Fatal(err)
	}
	out := []memory.Memory{}
	for _, m := range all {
		if m.MissionID == missionID {
			out = append(out, m)
		}
	}
	return out
}

func missionState(t *testing.T, s events.Port, id string) string {
	t.Helper()
	m, err := projector.ReplayMission(s.List("mission", id))
	if err != nil {
		t.Fatal(err)
	}
	return string(m.State)
}

// L10: both backends → exit 2; an invalid ledger or a non-executable
// claudePath → exit 2; absent flag = backend off.
func TestServeSessionLauncherFlagsFRRHZ124S1(t *testing.T) {
	f := slFake(t, "")
	if c, err := sessionLauncherConfig("", ""); err != nil || c != nil {
		t.Fatal(c, err)
	}
	var eout bytes.Buffer
	if code := serve([]string{"-journal", filepath.Join(t.TempDir(), "j.ndjson"), "-session-launcher-config", f.cfg, "-janus-exec-config", execConfigFile(t)}, &bytes.Buffer{}, &eout); code != 2 || !strings.Contains(eout.String(), "pick one execution backend") {
		t.Fatalf("exit %d: %s", code, eout.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"claudePath":"`+f.bin+`","apiKey":"TEST-NON-CREDENTIAL"}`), 0o600)
	eout.Reset()
	if code := serve([]string{"-journal", filepath.Join(t.TempDir(), "j.ndjson"), "-session-launcher-config", bad}, &bytes.Buffer{}, &eout); code != 2 || !strings.Contains(eout.String(), "session launcher config") {
		t.Fatalf("exit %d: %s", code, eout.String())
	}
	if err := os.Chmod(f.bin, 0o600); err != nil {
		t.Fatal(err)
	}
	eout.Reset()
	if code := serve([]string{"-journal", filepath.Join(t.TempDir(), "j.ndjson"), "-session-launcher-config", f.cfg}, &bytes.Buffer{}, &eout); code != 2 || !strings.Contains(eout.String(), "claudePath must be an executable") {
		t.Fatalf("exit %d: %s", code, eout.String())
	}
}

// L1 + L4 + L12 through the relay: intent → claimed → accepted strictly
// before the mission's running transition; idempotent re-submission; one
// usage note with both models' numbers; mission ends waiting_for_result.
func TestServeSessionLauncherRelayFRRHZ124S1(t *testing.T) {
	s := rhz092Store(t)
	f := slFake(t, slBlocking)
	sl := slLauncher(t, f)
	handler, loop := assembleServeWith(s, nil, "", "", nil, sl, io.Discard, false)
	if loop != nil {
		t.Fatal("launcher must not start the JANUS loop")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	before := len(s.All())
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","budget":{"usd":2}}`)
	id, _ := out["executionId"].(string)
	if out["Accepted"] != true || !strings.HasPrefix(id, "exec-") {
		t.Fatalf("%v", out)
	}
	// Order: execution intent/claim/accepted, then mission ready/running.
	var order []string
	for _, e := range s.All()[before:] {
		order = append(order, e.AggregateType+":"+e.Type)
	}
	want := "execution:execution.intent,execution:execution.dispatch_claimed,execution:execution.accepted,mission:mission.transitioned,mission:mission.transitioned"
	if strings.Join(order, ",") != want {
		t.Fatalf("order %v", order)
	}
	if missionState(t, s, "mission-1") != "running" {
		t.Fatal("mission not running")
	}
	r, _ := execution.Replay(s.List("execution", id))
	if r.Provenance == nil || r.Provenance.Effective.Budget != 200 || r.Provenance.ProfileID != "claude-local" || !strings.HasPrefix(r.Provenance.Actor, "unverified-local-operator:") {
		t.Fatalf("%+v", r.Provenance)
	}
	// L12: same request → same execution, zero writes, no second spawn.
	before = len(s.All())
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","budget":{"usd":2}}`); out["Accepted"] != true || out["executionId"] != id || len(s.All()) != before {
		t.Fatalf("%v writes=%d", out, len(s.All())-before)
	}
	// L2: JANUS axes are refused by the launcher with zero writes.
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","instruction":"other","budget":{"tokens":10}}`); out["Accepted"] != false || out["Reason"] != "budget.tokens not supported by session launcher" || len(s.All()) != before {
		t.Fatalf("%v", out)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sl.Wait()
	if f.runs() != 1 {
		t.Fatalf("runs=%d", f.runs())
	}
	if r, _ := execution.Replay(s.List("execution", id)); r.State != execution.Succeeded || r.Summary != "finished" {
		t.Fatalf("%+v", r)
	}
	if st := missionState(t, s, "mission-1"); st != "waiting_for_result" {
		t.Fatalf("mission %s", st)
	}
	notes := usageNotes(t, s, "mission-1")
	if len(notes) != 1 {
		t.Fatalf("%d usage notes", len(notes))
	}
	c := notes[0].Content
	for _, want := range []string{`"executionId": "` + id + `"`, `"sessionId": "` + r.ExternalID + `"`, `"model": "claude-opus-5-5"`, `"claude-haiku-4-5"`, "0.9000000000000000111", "0.087654321098765432", `"totalCostUsd": 0.987654321098765432`, `"resumeOf": null`, `"logDigest": "sha256:`} {
		if !strings.Contains(c, want) {
			t.Fatalf("note missing %s:\n%s", want, c)
		}
	}
	if strings.Join(notes[0].Tags, ",") != "usage,session-launcher" {
		t.Fatal(notes[0].Tags)
	}
}

// L1 (failure half) through the relay: a spawn failure is a refusal with the
// execution id; the mission state is untouched and no execution is accepted.
func TestServeSessionLauncherSpawnFailureFRRHZ124S1(t *testing.T) {
	s := rhz092Store(t)
	f := slFake(t, slBlocking)
	sl := slLauncher(t, f)
	handler, _ := assembleServeWith(s, nil, "", "", nil, sl, io.Discard, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	if err := os.Chmod(f.bin, 0o600); err != nil {
		t.Fatal(err)
	}
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1"}`)
	if out["Accepted"] != false || !strings.HasPrefix(out["Reason"].(string), "session spawn failed") || out["executionId"] == "" {
		t.Fatalf("%v", out)
	}
	if st := missionState(t, s, "mission-1"); st != "planned" {
		t.Fatalf("mission moved to %s", st)
	}
	if r, _ := execution.Replay(s.List("execution", out["executionId"].(string))); r.State != execution.DispatchClaimed {
		t.Fatalf("%+v", r)
	}
	// Unknown workdir: refused before any write (L3 via the wire).
	before := len(s.All())
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","workdir":"elsewhere"}`); out["Accepted"] != false || !strings.HasPrefix(out["Reason"].(string), "POLICY_DENIED: workdir") || len(s.All()) != before {
		t.Fatalf("%v", out)
	}
}

func getWorkspace(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url + "/v1/workspace")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

// R1: a full launch on an NDJSON journal survives Close → Open with the same
// event count and byte-identical /v1/workspace.
func TestServeSessionLauncherJournalRoundTripFRRHZ124S1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.ndjson")
	j, err := journal.OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-1", "g", "ship it", "done"); err != nil {
		t.Fatal(err)
	}
	f := slFake(t, "cat <<'JSON'\n"+slResult+"\nJSON")
	sl := slLauncher(t, f)
	handler, _ := assembleServeWith(j, nil, "", "", nil, sl, io.Discard, false)
	srv := httptest.NewServer(handler)
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1"}`); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	sl.Wait()
	if st := missionState(t, j, "mission-1"); st != "waiting_for_result" || len(usageNotes(t, j, "mission-1")) != 1 {
		t.Fatalf("mission %s", st)
	}
	n := len(j.All())
	first := getWorkspace(t, srv.URL)
	srv.Close()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := journal.OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	handler2, _ := assembleServeWith(j2, nil, "", "", nil, slLauncher(t, f), io.Discard, false)
	srv2 := httptest.NewServer(handler2)
	defer srv2.Close()
	if len(j2.All()) != n || !bytes.Equal(getWorkspace(t, srv2.URL), first) {
		t.Fatalf("round trip: events %d→%d", n, len(j2.All()))
	}
}

// MUST-2 via the relay: 500 permission denials still land exactly one note
// (≤16KiB) with modelUsage intact and the full count.
func TestServeSessionLauncherManyDenialsFRRHZ124S1(t *testing.T) {
	ds := make([]string, 500)
	for i := range ds {
		ds[i] = fmt.Sprintf(`{"tool_name":"Bash","tool_use_id":"toolu_%04d","tool_input":{"command":"echo"}}`, i)
	}
	result := strings.Replace(slResult, `"permission_denials":[]`, `"permission_denials":[`+strings.Join(ds, ",")+`]`, 1)
	s := rhz092Store(t)
	f := slFake(t, "cat <<'JSON'\n"+result+"\nJSON")
	sl := slLauncher(t, f)
	var errs bytes.Buffer
	handler, _ := assembleServeWith(s, nil, "", "", nil, sl, &errs, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1"}`); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	sl.Wait()
	notes := usageNotes(t, s, "mission-1")
	if len(notes) != 1 || errs.Len() != 0 {
		t.Fatalf("%d notes, errors: %s", len(notes), errs.String())
	}
	c := notes[0].Content
	if len(c) > 16*1024 || !strings.Contains(c, `"permissionDenialsCount": 500`) || !strings.Contains(c, "0.9000000000000000111") || !strings.Contains(c, "0.087654321098765432") {
		t.Fatalf("%d bytes", len(c))
	}
}

// MUST-1: serve shutdown stops the live session and drains its completion
// before the journal closes — child group gone, stop request, Cancelled
// outcome, mission waiting_for_result and the usage note are all durable.
// The preamble names serve's own -addr.
func TestServeShutdownStopsSessionsFRRHZ124S1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.ndjson")
	j, err := journal.OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-1", "g", "ship it", "done"); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	f := slFake(t, `for a in "$@"; do printf '%s\0' "$a"; done > "$D/args"
trap 'cat "$D/result.json"; exit 0' TERM
sleep 1000 &
echo $! > "$D/gc.tmp"
mv "$D/gc.tmp" "$D/gc"
wait`)
	if err := os.WriteFile(filepath.Join(f.dir, "result.json"), []byte(slResult+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() {
		code <- serveCtx(ctx, []string{"-journal", path, "-addr", addr, "-session-launcher-config", f.cfg}, io.Discard, io.Discard)
	}()
	url := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(url + "/v1/workspace"); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never listened")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := postIntent(t, url, `{"kind":"mission.start","missionId":"mission-1"}`)
	id, _ := out["executionId"].(string)
	if out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	var gc []byte
	for deadline = time.Now().Add(10 * time.Second); ; {
		if gc, err = os.ReadFile(filepath.Join(f.dir, "gc")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	args, _ := os.ReadFile(filepath.Join(f.dir, "args"))
	if !strings.Contains(string(args), "curl -s '"+url+"/v1/context?task=mission-1'") {
		t.Fatalf("preamble does not name serve's address: %q", args)
	}
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("serve exit %d", c)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(gc)))
	for deadline = time.Now().Add(10 * time.Second); syscall.Kill(pid, 0) == nil; {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("session process %d outlived serve", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	j2, err := journal.OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	r, err := execution.Replay(j2.List("execution", id))
	if err != nil || !r.StopRequested || r.StopReason != "user" || r.State != execution.Cancelled || !strings.HasPrefix(r.Summary, "stopped: serve shutdown") {
		t.Fatalf("%+v %v", r, err)
	}
	if st := missionState(t, j2, "mission-1"); st != "waiting_for_result" {
		t.Fatalf("mission %s", st)
	}
	if n := usageNotes(t, j2, "mission-1"); len(n) != 1 || !strings.Contains(n[0].Content, "Stopped on serve shutdown.") {
		t.Fatalf("%d notes", len(n))
	}
}
