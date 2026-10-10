package main

// RHZ-124 S2 (FR-RHZ-124-S2) through serve: a denied session raises one
// internal gate via the relay (question.ask, correlation launcher:<execId>),
// the mission waits for the human; gate.approve over POST /v1/intent resumes
// the same session (--resume, widened tools) and the mission runs again; the
// resumed session's usage note carries resumeOf. gate.reject "STOP …" leaves
// a mission note and no execution. /v1/execution names the budget unit.
// Fake claude only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/question"
	"rhizome/internal/sessionlauncher"
	"rhizome/internal/trust"
	"rhizome/internal/workspace"
)

const slDenied = `{"type":"result","subtype":"success","is_error":false,"result":"blocked\n\nneed to push","total_cost_usd":0.2,"num_turns":2,"duration_ms":50,"terminal_reason":"completed","permission_denials":[{"tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"git push SECRET-INPUT-MARKER"}}],"modelUsage":{"claude-opus-5-5":{"inputTokens":3,"costUSD":0.2}}}`

// slGateBody: first run denies, a --resume run blocks on release2 and then
// succeeds; every run keeps its argv as args.<n> and prints its cwd.
const slGateBody = `n=$(wc -l < "$D/count" | tr -d ' ')
for a in "$@"; do printf '%s\0' "$a"; done > "$D/args.$n.tmp"
mv "$D/args.$n.tmp" "$D/args.$n"
printf '{"type":"system","subtype":"init","cwd":"%s"}\n' "$(pwd -P)"
case " $* " in
*" --resume "*)
  while [ ! -f "$D/release2" ]; do sleep 0.02; done
  cat <<'JSON'
` + slResult + `
JSON
  ;;
*)
  cat <<'JSON'
` + slDenied + `
JSON
  ;;
esac`

func slArgs(t *testing.T, f slFakeEnv, n int) []string {
	t.Helper()
	p := filepath.Join(f.dir, fmt.Sprintf("args.%d", n))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 && f.runs() >= n {
			return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d never started", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func launcherGates(t *testing.T, s events.Port) []question.Ref {
	t.Helper()
	out := []question.Ref{}
	seen := map[string]bool{}
	for _, e := range s.All() {
		if e.AggregateType != "question" || seen[e.AggregateID] {
			continue
		}
		seen[e.AggregateID] = true
		q, err := (question.Service{Store: s}).Get(e.AggregateID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(q.CorrelationID, "launcher:") {
			out = append(out, q)
		}
	}
	return out
}

func slDeniedServe(t *testing.T) (events.Port, slFakeEnv, *sessionlauncher.Launcher, *httptest.Server, string) {
	t.Helper()
	s := rhz092Store(t)
	f := slFake(t, slGateBody)
	sl := slLauncher(t, f)
	handler, _ := assembleServeWith(s, nil, "", "", nil, sl, io.Discard, false)
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { f.reap(); sl.Shutdown(); sl.Wait(); srv.Close() })
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1","budget":{"usd":1.5}}`)
	id, _ := out["executionId"].(string)
	if out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	sl.Wait()
	return s, f, sl, srv, id
}

func TestServeLauncherDenialGateApproveFRRHZ124S2(t *testing.T) {
	s, f, sl, srv, orig := slDeniedServe(t)
	gates := launcherGates(t, s)
	if len(gates) != 1 || gates[0].CorrelationID != "launcher:"+orig || gates[0].MissionID != "mission-1" || gates[0].Title != "RHZ 런처 승인 요청: Bash" ||
		!strings.Contains(gates[0].Body, "SECRET-INPUT-MARKER") || gates[0].RequestedBy != "unverified-local-operator:"+sessionlauncher.Actor {
		t.Fatalf("%+v", gates)
	}
	if st := missionState(t, s, "mission-1"); st != "waiting_for_human" {
		t.Fatalf("mission %s", st)
	}
	notes := usageNotes(t, s, "mission-1")
	if len(notes) != 1 || strings.Contains(notes[0].Content, "SECRET-INPUT-MARKER") || !strings.Contains(notes[0].Content, gates[0].ID) {
		t.Fatalf("%+v", notes)
	}
	origRef, _ := execution.Replay(s.List("execution", orig))
	// 3a: the launcher's budget axis is labelled as USD cents.
	resp, err := http.Get(srv.URL + "/v1/execution/mission-1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"effective":{"tokens":150,"timeMs":600000,"maxDepth":1},"budgetUnits":"usd_cents"`) {
		t.Fatalf("%s", b)
	}
	// Approve over the wire → resume of the same session.
	body, _ := json.Marshal(map[string]string{"kind": "gate.approve", "gateId": gates[0].ID, "digest": gates[0].Digest, "reason": "push is fine", "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(body)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	args := slArgs(t, f, 2)
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "\x00--resume\x00"+origRef.ExternalID+"\x00") || !strings.Contains(joined, "\x00--allowedTools\x00Read\x00Bash\x00--max-budget-usd\x001.50") || !strings.Contains(args[1], "승인됨: push is fine") {
		t.Fatalf("%q", args)
	}
	if st := missionState(t, s, "mission-1"); st != "running" {
		t.Fatalf("mission %s", st)
	}
	// X10: the hook path records exactly one unverified prefix (the rescan
	// path must record the same; see ...RetriedAtStart).
	if r, _ := execution.Replay(s.List("execution", sessionlauncher.ResumeExecutionID(gates[0].ID, "approve"))); r.Provenance == nil || r.Provenance.Actor != resumeActor {
		t.Fatalf("%+v", r.Provenance)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sl.Wait()
	if st := missionState(t, s, "mission-1"); st != "waiting_for_result" {
		t.Fatalf("mission %s", st)
	}
	notes = usageNotes(t, s, "mission-1")
	if len(notes) != 2 {
		t.Fatalf("%d usage notes", len(notes))
	}
	var resumed memory.Memory
	for _, n := range notes {
		if strings.Contains(n.Content, `"resumeOf": "`+orig+`"`) {
			resumed = n
		}
	}
	if resumed.ID == "" || strings.Contains(resumed.Content, "SECRET-INPUT-MARKER") {
		t.Fatalf("%+v", notes)
	}
	if f.runs() != 2 {
		t.Fatalf("runs=%d", f.runs())
	}
}

func TestServeLauncherDenialGateStopFRRHZ124S2(t *testing.T) {
	s, f, _, srv, orig := slDeniedServe(t)
	gates := launcherGates(t, s)
	if len(gates) != 1 {
		t.Fatalf("%+v", gates)
	}
	// requestChanges keeps the gate open and resumes nothing.
	rc, _ := json.Marshal(map[string]string{"kind": "gate.requestChanges", "gateId": gates[0].ID, "digest": gates[0].Digest, "reason": "explain first", "actor": "eastside"})
	before := len(s.All())
	if out := postIntent(t, srv.URL, string(rc)); out["Accepted"] != true || len(s.All()) != before+1 || f.runs() != 1 {
		t.Fatalf("%v", out)
	}
	body, _ := json.Marshal(map[string]string{"kind": "gate.reject", "gateId": gates[0].ID, "digest": gates[0].Digest, "reason": "STOP not today", "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(body)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	if f.runs() != 1 {
		t.Fatalf("runs=%d", f.runs())
	}
	for _, e := range s.All() {
		if e.AggregateType == "execution" && e.AggregateID != orig {
			t.Fatalf("resume execution written: %s", e.AggregateID)
		}
	}
	if st := missionState(t, s, "mission-1"); st != "waiting_for_human" {
		t.Fatalf("mission %s", st)
	}
	ns, err := (memory.Service{Store: s}).Search(memory.Observation, "gate-resume", "")
	if err != nil || len(ns) != 1 || ns[0].MissionID != "mission-1" || !strings.Contains(ns[0].Content, "STOP") || !strings.Contains(ns[0].Content, gates[0].ID) {
		t.Fatalf("%+v %v", ns, err)
	}
}

// M1 through serve (the reviewer's probe): the real gate is rejected with
// STOP; a look-alike gate asked over the wire with the same correlation and
// mission is approved — no resume, no spawn, a refusal note.
func TestServeLauncherSpoofedGateFRRHZ124S2(t *testing.T) {
	s, f, _, srv, orig := slDeniedServe(t)
	gates := launcherGates(t, s)
	stop, _ := json.Marshal(map[string]string{"kind": "gate.reject", "gateId": gates[0].ID, "digest": gates[0].Digest, "reason": "STOP no", "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(stop)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	ask, _ := json.Marshal(map[string]string{"kind": "question.ask", "name": "please read README", "body": "approve me", "missionId": "mission-1", "correlationId": "launcher:" + orig, "actor": "mallory"})
	if out := postIntent(t, srv.URL, string(ask)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	id, _ := question.IDFor("please read README", "approve me", "")
	approve, _ := json.Marshal(map[string]string{"kind": "gate.approve", "gateId": id, "digest": question.Digest("please read README", "approve me", ""), "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(approve)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	if f.runs() != 1 || missionState(t, s, "mission-1") != "waiting_for_human" {
		t.Fatalf("runs=%d", f.runs())
	}
	for _, e := range s.All() {
		if e.AggregateType == "execution" && e.AggregateID != orig {
			t.Fatalf("resume execution written: %s", e.AggregateID)
		}
	}
	ns, _ := (memory.Service{Store: s}).Search(memory.Observation, "gate-resume", "")
	found := false
	for _, n := range ns {
		found = found || strings.Contains(n.Content, id+" is not the denial gate of "+orig)
	}
	if !found {
		t.Fatalf("%+v", ns)
	}
}

// slBusyBody: like slGateBody, plus an instruction BLOCKME that holds the
// slot until release3.
const slBusyBody = `n=$(wc -l < "$D/count" | tr -d ' ')
for a in "$@"; do printf '%s\0' "$a"; done > "$D/args.$n.tmp"
mv "$D/args.$n.tmp" "$D/args.$n"
printf '{"type":"system","subtype":"init","cwd":"%s"}\n' "$(pwd -P)"
case " $* " in
*" --resume "*)
  while [ ! -f "$D/release2" ]; do sleep 0.02; done
  cat <<'JSON'
` + slResult + `
JSON
  ;;
*BLOCKME*)
  while [ ! -f "$D/release3" ]; do sleep 0.02; done
  cat <<'JSON'
` + slResult + `
JSON
  ;;
*)
  cat <<'JSON'
` + slDenied + `
JSON
  ;;
esac`

// slBusyServe: maxConcurrent 1; mission-1 is denied and its gate approved
// while mission-2 (BLOCKME) holds the only slot, so the resume is refused
// as busy. Returns the gate and the original execution.
func slBusyServe(t *testing.T) (events.Port, slFakeEnv, *sessionlauncher.Launcher, string) {
	t.Helper()
	s := rhz092Store(t)
	if _, err := (mission.Service{Store: s}).Create("mission-2", "g", "block", "done"); err != nil {
		t.Fatal(err)
	}
	f := slFake(t, slBusyBody)
	var ledger map[string]any
	b, _ := os.ReadFile(f.cfg)
	_ = json.Unmarshal(b, &ledger)
	ledger["maxConcurrent"] = 1
	b, _ = json.Marshal(ledger)
	if err := os.WriteFile(f.cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sl := slLauncher(t, f)
	handler, _ := assembleServeWith(s, nil, "", "", nil, sl, io.Discard, false)
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { f.reap(); sl.Shutdown(); sl.Wait(); srv.Close() })
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1"}`)
	orig, _ := out["executionId"].(string)
	sl.Wait()
	if out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-2","instruction":"BLOCKME"}`); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	slArgs(t, f, 2)
	gates := launcherGates(t, s)
	if len(gates) != 1 {
		t.Fatalf("%+v", gates)
	}
	body, _ := json.Marshal(map[string]string{"kind": "gate.approve", "gateId": gates[0].ID, "digest": gates[0].Digest, "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(body)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	ns, _ := (memory.Service{Store: s}).Search(memory.Observation, "gate-resume", "")
	if f.runs() != 2 || missionState(t, s, "mission-1") != "waiting_for_human" || len(ns) != 1 || !strings.Contains(ns[0].Content, "session launcher busy") {
		t.Fatalf("runs=%d %+v", f.runs(), ns)
	}
	return s, f, sl, orig
}

// S1b: a resume refused as busy is retried from the journal when the slot
// frees; a later re-scan finds nothing owed.
func TestServeLauncherResumeRetriedOnSlotFreeFRRHZ124S2(t *testing.T) {
	s, f, sl, orig := slBusyServe(t)
	if err := os.WriteFile(filepath.Join(f.dir, "release3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := slArgs(t, f, 3)
	origRef, _ := execution.Replay(s.List("execution", orig))
	if !strings.Contains(strings.Join(args, "\x00"), "\x00--resume\x00"+origRef.ExternalID+"\x00") {
		t.Fatalf("%q", args)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	sl.Wait()
	if st := missionState(t, s, "mission-1"); st != "waiting_for_result" {
		t.Fatalf("mission %s", st)
	}
	before := len(s.All())
	launcherRescan(s, sl)
	sl.Wait()
	if f.runs() != 3 || len(s.All()) != before {
		t.Fatalf("runs=%d writes=%d", f.runs(), len(s.All())-before)
	}
}

// S1b: an owed resume left by a stopped serve is retried once at the next
// serve start (journal-derived; no in-memory queue survives).
func TestServeLauncherResumeRetriedAtStartFRRHZ124S2(t *testing.T) {
	s, f, sl, orig := slBusyServe(t)
	sl.Shutdown()
	sl.Wait()
	if f.runs() != 2 || missionState(t, s, "mission-1") != "waiting_for_human" {
		t.Fatalf("runs=%d", f.runs())
	}
	sl2 := slLauncher(t, f)
	t.Cleanup(func() { f.reap(); sl2.Shutdown(); sl2.Wait() })
	assembleServeWith(s, nil, "", "", nil, sl2, io.Discard, false)
	args := slArgs(t, f, 3)
	origRef, _ := execution.Replay(s.List("execution", orig))
	if !strings.Contains(strings.Join(args, "\x00"), "\x00--resume\x00"+origRef.ExternalID+"\x00") || missionState(t, s, "mission-1") != "running" {
		t.Fatalf("%q", args)
	}
	// X10: the rescan path (actor = the recorded, already-prefixed ActorRef)
	// records the same actor as the hook path: one prefix only.
	gates := launcherGates(t, s)
	if r, _ := execution.Replay(s.List("execution", sessionlauncher.ResumeExecutionID(gates[0].ID, "approve"))); r.Provenance == nil || r.Provenance.Actor != resumeActor {
		t.Fatalf("%+v", r.Provenance)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	sl2.Wait()
	if f.runs() != 3 {
		t.Fatalf("runs=%d", f.runs())
	}
}

// S1b: a resume whose spawn failed stays claimed-without-marker (S1
// semantics); the journal re-scan retries that same execution.
func TestServeLauncherResumeSpawnFailureRetriedFRRHZ124S2(t *testing.T) {
	s, f, sl, srv, orig := slDeniedServe(t)
	gates := launcherGates(t, s)
	if err := os.Chmod(f.bin, 0o600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"kind": "gate.approve", "gateId": gates[0].ID, "digest": gates[0].Digest, "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(body)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	rid := sessionlauncher.ResumeExecutionID(gates[0].ID, "approve")
	if r, _ := execution.Replay(s.List("execution", rid)); r.State != execution.DispatchClaimed || f.runs() != 1 || missionState(t, s, "mission-1") != "waiting_for_human" {
		t.Fatalf("%+v runs=%d", r, f.runs())
	}
	if err := os.Chmod(f.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	launcherRescan(s, sl)
	args := slArgs(t, f, 2)
	origRef, _ := execution.Replay(s.List("execution", orig))
	if r, _ := execution.Replay(s.List("execution", rid)); r.State != execution.Accepted || r.ExternalID != origRef.ExternalID || !strings.Contains(strings.Join(args, "\x00"), "\x00--resume\x00"+origRef.ExternalID+"\x00") {
		t.Fatalf("%+v %q", r, args)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	sl.Wait()
}

// resumeActor is the provenance actor of a resume approved by "eastside",
// whichever path (hook or rescan) started it.
const resumeActor = "unverified-local-operator:eastside"

// X7: a STOP-rejected gate is never resumed by the journal re-scan either:
// no spawn, no write (no note: Retry only reports through OnError).
func TestServeLauncherRescanStopFRRHZ124S2(t *testing.T) {
	s, f, sl, srv, _ := slDeniedServe(t)
	gates := launcherGates(t, s)
	body, _ := json.Marshal(map[string]string{"kind": "gate.reject", "gateId": gates[0].ID, "digest": gates[0].Digest, "reason": "STOP no", "actor": "eastside"})
	if out := postIntent(t, srv.URL, string(body)); out["Accepted"] != true {
		t.Fatalf("%v", out)
	}
	// A wrongful resume would not block (release2 pre-set): it fails fast.
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	before := len(s.All())
	launcherRescan(s, sl)
	sl.Wait()
	if len(s.All()) != before || f.runs() != 1 || missionState(t, s, "mission-1") != "waiting_for_human" {
		t.Fatalf("writes=%d runs=%d", len(s.All())-before, f.runs())
	}
}

// failClaimStore refuses the dispatch claim of one execution while armed.
type failClaimStore struct {
	*events.Store
	id    string
	armed *atomic.Bool
}

func (s *failClaimStore) Append(exp uint64, e events.Event) error {
	if s.armed.Load() && e.AggregateID == s.id && e.Type == "execution.dispatch_claimed" {
		return errors.New("injected claim fault")
	}
	return s.Store.Append(exp, e)
}

// X5: a resume execution left in intent (crash between intent and claim)
// converges through the re-scan into one accepted execution.
func TestServeLauncherRescanIntentConvergesFRRHZ124S2(t *testing.T) {
	base := rhz092Store(t)
	armed := &atomic.Bool{}
	s := &failClaimStore{Store: base, armed: armed}
	f := slFake(t, slGateBody)
	sl := slLauncher(t, f)
	handler, _ := assembleServeWith(s, nil, "", "", nil, sl, io.Discard, false)
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { f.reap(); sl.Shutdown(); sl.Wait(); srv.Close() })
	out := postIntent(t, srv.URL, `{"kind":"mission.start","missionId":"mission-1"}`)
	orig, _ := out["executionId"].(string)
	sl.Wait()
	gates := launcherGates(t, base)
	if len(gates) != 1 {
		t.Fatalf("%+v", gates)
	}
	rid := sessionlauncher.ResumeExecutionID(gates[0].ID, "approve")
	s.id = rid // set while nothing runs; read only by later appends
	armed.Store(true)
	body, _ := json.Marshal(map[string]string{"kind": "gate.approve", "gateId": gates[0].ID, "digest": gates[0].Digest, "actor": "eastside"})
	_ = postIntent(t, srv.URL, string(body))
	if r, _ := execution.Replay(base.List("execution", rid)); r.State != execution.Intent || f.runs() != 1 {
		t.Fatalf("%+v runs=%d", r, f.runs())
	}
	armed.Store(false)
	launcherRescan(s, sl)
	slArgs(t, f, 2)
	ids := map[string]bool{}
	for _, e := range base.All() {
		if e.AggregateType == "execution" && e.AggregateID != orig {
			ids[e.AggregateID] = true
		}
	}
	if r, _ := execution.Replay(base.List("execution", rid)); len(ids) != 1 || r.State != execution.Accepted || r.Provenance.Actor != resumeActor {
		t.Fatalf("%v %+v", ids, r)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	sl.Wait()
}

// X9 + N-a: launcherAskGate only returns a gate this session freshly raised —
// not one bound to another execution or mission, asked by someone else, or
// already decided (question.ask is content-idempotent).
func TestLauncherAskGateBindingFRRHZ124S2(t *testing.T) {
	s := rhz092Store(t)
	if _, err := (mission.Service{Store: s}).Create("mission-2", "g", "other", "done"); err != nil {
		t.Fatal(err)
	}
	pre := func(name, mission, corr, actor string) {
		res, err := workspace.RelayIntentHooks(s, workspace.Intent{Kind: "question.ask", Name: name, Body: "b " + name, MissionID: mission, CorrelationID: corr}, actor, trust.Authority{}, workspace.RelayHooks{})
		if err != nil || !res.Accepted {
			t.Fatal(res, err)
		}
	}
	g := func(name string) sessionlauncher.GateRequest {
		return sessionlauncher.GateRequest{MissionID: "mission-1", CorrelationID: "launcher:exec-x", Name: name, Body: "b " + name}
	}
	if id, err := launcherAskGate(s, g("fresh")); err != nil || id != mustID(t, "fresh") {
		t.Fatal(id, err)
	}
	pre("corr", "mission-1", "launcher:exec-other", sessionlauncher.Actor)
	pre("mission", "mission-2", "launcher:exec-x", sessionlauncher.Actor)
	pre("asker", "mission-1", "launcher:exec-x", "mallory")
	pre("decided", "mission-1", "launcher:exec-x", sessionlauncher.Actor)
	if _, err := (question.Service{Store: s}).Answer(mustID(t, "decided"), question.Approve, "", "mallory", question.Digest("decided", "b decided", "")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"corr": "another execution", "mission": "another execution", "asker": "not freshly raised", "decided": "not freshly raised"} {
		if id, err := launcherAskGate(s, g(name)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %q %v", name, id, err)
		}
	}
}

func mustID(t *testing.T, name string) string {
	t.Helper()
	id, err := question.IDFor(name, "b "+name, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// N-a end to end: an identical gate pre-created by someone else and
// pre-approved is not this session's gate — the mission falls back to
// waiting_for_result, nothing resumes.
func TestServeLauncherPreAnsweredSpoofFRRHZ124S2(t *testing.T) {
	f := slFake(t, slGateBody)
	// A wrongful resume would not block (release2 pre-set): it fails fast.
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	// Learn the exact gate text from a first serve on another journal (the
	// execution id is content-derived, the fake's output fixed).
	a := rhz092Store(t)
	slA := slLauncher(t, f)
	hA, _ := assembleServeWith(a, nil, "", "", nil, slA, io.Discard, false)
	srvA := httptest.NewServer(hA)
	t.Cleanup(func() { f.reap(); slA.Shutdown(); slA.Wait(); srvA.Close() })
	outA := postIntent(t, srvA.URL, `{"kind":"mission.start","missionId":"mission-1"}`)
	slA.Wait()
	real := launcherGates(t, a)
	if len(real) != 1 {
		t.Fatalf("%+v", real)
	}
	b := rhz092Store(t)
	if res, err := workspace.RelayIntentHooks(b, workspace.Intent{Kind: "question.ask", Name: real[0].Title, Body: real[0].Body, MissionID: "mission-1", CorrelationID: real[0].CorrelationID}, "mallory", trust.Authority{}, workspace.RelayHooks{}); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if _, err := (question.Service{Store: b}).Answer(real[0].ID, question.Approve, "", "mallory", real[0].Digest); err != nil {
		t.Fatal(err)
	}
	slB := slLauncher(t, f)
	var errs syncBuf
	hB, _ := assembleServeWith(b, nil, "", "", nil, slB, &errs, false)
	srvB := httptest.NewServer(hB)
	t.Cleanup(func() { slB.Shutdown(); slB.Wait(); srvB.Close() })
	outB := postIntent(t, srvB.URL, `{"kind":"mission.start","missionId":"mission-1"}`)
	slB.Wait()
	if outB["executionId"] != outA["executionId"] {
		t.Fatalf("%v %v", outA, outB)
	}
	if st := missionState(t, b, "mission-1"); st != "waiting_for_result" || f.runs() != 2 || !strings.Contains(errs.String(), "not freshly raised") {
		t.Fatalf("mission %s runs=%d errs=%s", st, f.runs(), errs.String())
	}
	for _, e := range b.All() {
		if e.AggregateType == "execution" && e.AggregateID != outB["executionId"] {
			t.Fatalf("resume execution written: %s", e.AggregateID)
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
