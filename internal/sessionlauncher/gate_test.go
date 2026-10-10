package sessionlauncher

// RHZ-124 S2 (FR-RHZ-124-S2): permission denials → one internal gate →
// decision → resume of the same session; plus the S1 follow-ups (terminal
// label, ledger-change crash guard, modelUsage last resort). Fake claude
// only (no real binary, zero tokens); gate/note writes go to recording fakes
// (the relay path is covered in cmd/rhizome).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/projector"
)

// longInput is a tool input far over 512 bytes with multi-byte runes, so the
// cut must land on a rune boundary.
var longInput = strings.Repeat("한글", 400)

// denialResult has three denials (two distinct tools), a secret-looking
// command and a two-paragraph result text.
var denialResult = func() string {
	m := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "first paragraph\n\nI could not push; need Bash and Write.",
		"total_cost_usd": 0.25, "num_turns": 3, "duration_ms": 900, "terminal_reason": "completed",
		"permission_denials": []any{
			map[string]any{"tool_name": "Bash", "tool_use_id": "toolu_a", "tool_input": map[string]any{"command": "git push origin SECRET-INPUT-MARKER"}},
			map[string]any{"tool_name": "Write", "tool_use_id": "toolu_b", "tool_input": map[string]any{"file_path": "/x", "content": longInput}},
			map[string]any{"tool_name": "Bash", "tool_use_id": "toolu_c", "tool_input": map[string]any{"command": "rm -rf build"}},
		},
		"modelUsage": map[string]any{"claude-opus-5-5": map[string]any{"inputTokens": 1, "costUSD": 0.25}}}
	b, _ := json.Marshal(m)
	return string(b)
}()

// gateBody: the first run (no --resume) ends on denials; a resume run keeps
// its argv as args.<n> (written as .tmp then renamed: a poller never reads
// a partial file) and blocks until release2 (so the running state is
// observable), then succeeds. Both runs emit an init line with their cwd.
func gateBody(first string) string {
	return `n=$(wc -l < "$D/count" | tr -d ' ')
for a in "$@"; do printf '%s\0' "$a"; done > "$D/args.$n.tmp"
mv "$D/args.$n.tmp" "$D/args.$n"
printf '{"type":"system","subtype":"init","cwd":"%s"}\n' "$(pwd -P)"
case " $* " in
*" --resume "*)
  while [ ! -f "$D/release2" ]; do sleep 0.02; done
  cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"resumed and finished","total_cost_usd":0.1,"num_turns":1,"duration_ms":5,"terminal_reason":"completed","permission_denials":[],"modelUsage":{}}
JSON
  ;;
*)
  cat <<'JSON'
` + first + `
JSON
  ;;
esac`
}

type gateRec struct {
	mu    sync.Mutex
	asks  []GateRequest
	notes []string
	errs  []string
	fail  error
}

func (g *gateRec) ask(r GateRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail != nil {
		return "", g.fail
	}
	g.asks = append(g.asks, r)
	return fmt.Sprintf("q-fake-%d", len(g.asks)), nil
}

// id is the fake kernel's content id: the id ask assigned to an identical
// request ("" = never asked).
func (g *gateRec) id(r GateRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, a := range g.asks {
		if a.Name == r.Name && a.Body == r.Body {
			return fmt.Sprintf("q-fake-%d", i+1), nil
		}
	}
	return "", errors.New("no such gate")
}

func (g *gateRec) note(mission, content string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.notes = append(g.notes, mission+"|"+content)
	return nil
}

func (g *gateRec) onError(scope, id string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errs = append(g.errs, scope+" "+err.Error())
}

func gateLauncher(t *testing.T, body string) (*fakeEnv, *Launcher, *outcomes, *gateRec) {
	t.Helper()
	f := newFake(t, body)
	s := launchStore(t)
	o, g := &outcomes{}, &gateRec{}
	l := newLauncher(s, f.config(t, nil), o)
	l.AskGate, l.Note, l.OnError, l.GateID = g.ask, g.note, g.onError, g.id
	t.Cleanup(func() { f.reap(); l.Shutdown(); waitDone(t, l) })
	return f, l, o, g
}

func missionOf(t *testing.T, l *Launcher, id string) domain.MissionState {
	t.Helper()
	m, err := projector.ReplayMission(l.Store.List("mission", id))
	if err != nil {
		t.Fatal(err)
	}
	return m.State
}

func argsOf(t *testing.T, f *fakeEnv, n int) []string {
	t.Helper()
	b := waitFile(t, filepath.Join(f.dir, fmt.Sprintf("args.%d", n)))
	return strings.Split(strings.TrimSuffix(b, "\x00"), "\x00")
}

func flagValues(args []string, flag string) []string {
	for i, a := range args {
		if a == flag {
			out := []string{}
			for _, v := range args[i+1:] {
				if strings.HasPrefix(v, "--") {
					break
				}
				out = append(out, v)
			}
			return out
		}
	}
	return nil
}

// denied runs one denial session for m1 to its gate.
func denied(t *testing.T, l *Launcher, g *gateRec) (string, GateRequest) {
	t.Helper()
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "ship", Budget: BudgetOverride{USD: f64(2.5), TimeMs: i64(45000)}})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.asks) != 1 {
		t.Fatalf("%d gates asked: %v", len(g.asks), g.errs)
	}
	return id, g.asks[0]
}

func resumeExecs(t *testing.T, l *Launcher, orig string) []execution.Ref {
	t.Helper()
	out := []execution.Ref{}
	seen := map[string]bool{}
	for _, e := range l.Store.All() {
		if e.AggregateType != "execution" || e.AggregateID == orig || seen[e.AggregateID] {
			continue
		}
		seen[e.AggregateID] = true
		out = append(out, ref(t, l.Store, e.AggregateID))
	}
	return out
}

// S2-1: denials → exactly one gate (correlation launcher:<execId>), tool
// input compact and ≤512 bytes on a rune boundary, mission
// waiting_for_human, usage note without any tool input.
func TestDenialRaisesOneGateFRRHZ124S2(t *testing.T) {
	_, l, o, g := gateLauncher(t, gateBody(denialResult))
	id, gr := denied(t, l, g)
	if gr.CorrelationID != "launcher:"+id || gr.MissionID != "m1" || gr.ExecutionID != id || gr.Name != "RHZ 런처 승인 요청: Bash, Write" {
		t.Fatalf("%+v", gr)
	}
	if r := ref(t, l.Store, id); r.State != execution.Succeeded {
		t.Fatalf("%+v", r)
	}
	if st := missionOf(t, l, "m1"); st != domain.MissionWaitingHuman {
		t.Fatalf("mission %s", st)
	}
	lines := 0
	for _, line := range strings.Split(gr.Body, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		lines++
		_, input, _ := strings.Cut(line, ": ")
		input = strings.TrimSuffix(input, " …(잘림)")
		if len(input) > MaxToolInputBytes || !utf8.ValidString(input) {
			t.Fatalf("tool input %d bytes valid=%t", len(input), utf8.ValidString(input))
		}
	}
	if lines != 3 || !strings.Contains(gr.Body, `- Bash: {"command":"git push origin SECRET-INPUT-MARKER"}`) || !strings.Contains(gr.Body, " …(잘림)") ||
		!strings.Contains(gr.Body, "I could not push; need Bash and Write.") || strings.Contains(gr.Body, "first paragraph") || !strings.Contains(gr.Body, id) ||
		!strings.Contains(gr.Body, "다음 도구를 허용한다: Bash, Write") || !strings.Contains(gr.Body, "STOP") || len(gr.Body) > 16*1024 {
		t.Fatalf("body:\n%s", gr.Body)
	}
	if len(o.got) != 1 || o.got[0].GateID != "q-fake-1" {
		t.Fatalf("%+v", o.got)
	}
	note := o.got[0].NoteContent()
	if strings.Contains(note, "SECRET-INPUT-MARKER") || strings.Contains(note, "한글") || strings.Contains(note, "rm -rf") || strings.Contains(note, "tool_input") || !strings.Contains(note, `"permissionDenialsCount": 3`) {
		t.Fatalf("usage note leaks tool input:\n%s", note)
	}
	if len(g.errs) != 0 {
		t.Fatal(g.errs)
	}
}

// S2-1 (order): execution observed → gate asked → mission transition →
// usage note, seen from inside the AskGate and Report callbacks.
func TestDenialOrderFRRHZ124S2(t *testing.T) {
	_, l, _, g := gateLauncher(t, gateBody(denialResult))
	var steps []string
	ask := l.AskGate
	l.AskGate = func(r GateRequest) (string, error) {
		steps = append(steps, "ask exec="+string(ref(t, l.Store, r.ExecutionID).State)+" mission="+string(missionOf(t, l, r.MissionID)))
		return ask(r)
	}
	l.Report = func(o Outcome) {
		steps = append(steps, "report mission="+string(missionOf(t, l, o.MissionID))+" gate="+o.GateID)
	}
	denied(t, l, g)
	want := "ask exec=succeeded mission=running|report mission=waiting_for_human gate=q-fake-1"
	if got := strings.Join(steps, "|"); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
}

// S2-2: approve → resume execution: --resume <same uuid>, ledger tools ∪
// denied tool names (this execution only), same budget/time, claim
// correlation gate.resume:<gateId>, mission running → after completion
// waiting_for_result with resumeOf in the usage note.
func TestApproveResumesSameSessionFRRHZ124S2(t *testing.T) {
	f, l, o, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	origRef := ref(t, l.Store, orig)
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Reason: "ok, push allowed", Actor: "alice"})
	waitRuns(t, f, 2)
	args := argsOf(t, f, 2)
	if v := flagValues(args, "--resume"); len(v) != 1 || v[0] != origRef.ExternalID {
		t.Fatalf("--resume %v want %s", v, origRef.ExternalID)
	}
	if flagValues(args, "--session-id") != nil {
		t.Fatal("resume must not start a fresh session id")
	}
	if got := strings.Join(flagValues(args, "--allowedTools"), ","); got != "Read,Edit,Bash(git log:*),Bash,Write" {
		t.Fatalf("allowedTools %s", got)
	}
	if v := flagValues(args, "--max-budget-usd"); len(v) != 1 || v[0] != "2.50" {
		t.Fatalf("budget %v", v)
	}
	if !strings.Contains(args[1], "승인됨: ok, push allowed") || !strings.Contains(args[1], "/v1/context?task=m1") || !strings.Contains(args[1], "git push 금지") {
		t.Fatalf("prompt %q", args[1])
	}
	if st := missionOf(t, l, "m1"); st != domain.MissionRunning {
		t.Fatalf("mission %s", st)
	}
	rs := resumeExecs(t, l, orig)
	if len(rs) != 1 {
		t.Fatalf("%d resume executions", len(rs))
	}
	r := rs[0]
	p := r.Provenance
	if r.State != execution.Accepted || r.ExternalID != origRef.ExternalID || r.ClaimCorrelation != "gate.resume:q-fake-1" || r.Target != Target || r.MissionID != "m1" ||
		p == nil || p.Effective.Budget != 250 || p.Effective.Timeout != 45000 || strings.Join(p.Effective.Capabilities, ",") != "tool:Bash,tool:Bash(git log:*),tool:Edit,tool:Read,tool:Write" ||
		strings.Join(p.Ceiling.Capabilities, ",") != strings.Join(p.Effective.Capabilities, ",") || p.Actor != "unverified-local-operator:alice" || p.ExecConfigDigest != l.Cfg.Digest() {
		t.Fatalf("%+v %+v", r, p)
	}
	// The widening is scoped to this execution: the ledger is untouched and
	// the next fresh start runs with the ledger tools only.
	if strings.Join(l.Cfg.AllowedTools, ",") != "Read,Edit,Bash(git log:*)" || strings.Join(l.Cfg.ceilingPolicy().Capabilities, ",") != "tool:Bash(git log:*),tool:Edit,tool:Read" {
		t.Fatalf("ledger mutated: %v", l.Cfg.AllowedTools)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitDone(t, l)
	if st := missionOf(t, l, "m1"); st != domain.MissionWaitingResult {
		t.Fatalf("mission %s", st)
	}
	if len(o.got) != 2 || o.got[1].ResumeOf != orig || o.got[1].ExecutionID != r.ID || o.got[1].SessionID != origRef.ExternalID {
		t.Fatalf("%+v", o.got)
	}
	if note := o.got[1].NoteContent(); !strings.Contains(note, `"resumeOf": "`+orig+`"`) || !strings.Contains(note, "Gate resume of "+orig) {
		t.Fatalf("%s", note)
	}
	if r := ref(t, l.Store, r.ID); r.State != execution.Succeeded {
		t.Fatalf("%+v", r)
	}
	// Fresh start on m2 after the resume: ledger tools only.
	if _, reason, err := l.Start(StartRequest{MissionID: "m2", Instruction: "next"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l)
	if got := strings.Join(flagValues(argsOf(t, f, 3), "--allowedTools"), ","); got != "Read,Edit,Bash(git log:*)" {
		t.Fatalf("fresh start tools %s", got)
	}
	if len(g.errs) != 0 || len(g.notes) != 0 {
		t.Fatal(g.errs, g.notes)
	}
}

// S2-3: reject (first word not STOP) → resume without extra tools and the
// reject prompt with the reason verbatim.
func TestRejectResumesWithoutToolsFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "reject", Reason: "STOPPING is not the word; do it locally", Actor: "alice"})
	waitRuns(t, f, 2)
	args := argsOf(t, f, 2)
	if got := strings.Join(flagValues(args, "--allowedTools"), ","); got != "Read,Edit,Bash(git log:*)" {
		t.Fatalf("allowedTools %s", got)
	}
	if !strings.Contains(args[1], "거부됨: STOPPING is not the word; do it locally — 이 도구 없이 마무리하고 끝내라") || flagValues(args, "--resume") == nil {
		t.Fatalf("argv %q", args)
	}
	rs := resumeExecs(t, l, orig)
	if len(rs) != 1 || strings.Join(rs[0].Provenance.Effective.Capabilities, ",") != "tool:Bash(git log:*),tool:Edit,tool:Read" || missionOf(t, l, "m1") != domain.MissionRunning {
		t.Fatalf("%+v", rs)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	waitDone(t, l)
}

// S2-4: reject "STOP …" → no execution, a mission note, mission stays
// waiting_for_human. Lower-case "stop" is not STOP.
func TestRejectStopNoResumeFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	before := len(l.Store.All())
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "reject", Reason: "  STOP\tthis is enough", Actor: "alice"})
	if len(l.Store.All()) != before || runs(f) != 1 || l.Running() != 0 {
		t.Fatalf("writes=%d runs=%d", len(l.Store.All())-before, runs(f))
	}
	if st := missionOf(t, l, "m1"); st != domain.MissionWaitingHuman {
		t.Fatalf("mission %s", st)
	}
	if len(g.notes) != 1 || !strings.HasPrefix(g.notes[0], "m1|") || !strings.Contains(g.notes[0], "STOP") || !strings.Contains(g.notes[0], "재개하지 않았다") || !strings.Contains(g.notes[0], "q-fake-1") {
		t.Fatalf("%v", g.notes)
	}
	// A gate without the launcher prefix and an unknown decision: ignored.
	l.GateDecided(GateDecision{GateID: "q-x", CorrelationID: "other:" + orig, MissionID: "m1", Decision: "approve"})
	l.GateDecided(GateDecision{GateID: "q-x", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "requestChanges", Reason: "x"})
	if len(l.Store.All()) != before || runs(f) != 1 || len(g.notes) != 1 {
		t.Fatalf("ignored decisions wrote: %d %d %v", len(l.Store.All())-before, runs(f), g.notes)
	}
	// lower-case stop resumes.
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "reject", Reason: "stop pushing, finish locally", Actor: "alice"})
	waitRuns(t, f, 2)
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	waitDone(t, l)
}

// S2-6: the same decision delivered twice → one resume execution, one spawn,
// no refusal note.
func TestSameDecisionTwiceOneResumeFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	d := GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Actor: "alice"}
	l.GateDecided(d)
	waitRuns(t, f, 2)
	before := len(l.Store.All())
	l.GateDecided(d)
	if len(l.Store.All()) != before || len(resumeExecs(t, l, orig)) != 1 {
		t.Fatalf("second delivery wrote %d", len(l.Store.All())-before)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	waitDone(t, l)
	// Re-delivered after the resume ended: still nothing new.
	before = len(l.Store.All())
	l.GateDecided(d)
	waitDone(t, l)
	if len(l.Store.All()) != before || runs(f) != 2 || len(g.notes) != 0 || len(g.errs) != 0 {
		t.Fatalf("writes=%d runs=%d notes=%v errs=%v", len(l.Store.All())-before, runs(f), g.notes, g.errs)
	}
}

// S2-7: the original log no longer matches the journaled digest → refused
// with a note, no spawn, no execution written, mission waiting_for_human.
// Missing log and a foreign/unknown correlation are refused the same way.
func TestLogDigestMismatchRefusedFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	logPath := filepath.Join(f.logs, orig+".ndjson")
	b, _ := os.ReadFile(logPath)
	tampered := strings.Replace(string(b), `"tool_name":"Bash"`, `"tool_name":"Bash(*)"`, 1)
	if err := os.WriteFile(logPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(l.Store.All())
	d := GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Actor: "alice"}
	l.GateDecided(d)
	if len(l.Store.All()) != before || runs(f) != 1 || missionOf(t, l, "m1") != domain.MissionWaitingHuman {
		t.Fatalf("writes=%d runs=%d", len(l.Store.All())-before, runs(f))
	}
	if len(g.notes) != 1 || !strings.Contains(g.notes[0], "digest mismatch") || len(g.errs) != 1 || !strings.HasPrefix(g.errs[0], "resume ") {
		t.Fatalf("%v %v", g.notes, g.errs)
	}
	// X6: the re-scan path (Retry) refuses through OnError only, no note.
	l.Retry(d)
	if len(g.notes) != 1 || len(g.errs) != 2 || len(l.Store.All()) != before || runs(f) != 1 {
		t.Fatalf("retry: %v %v", g.notes, g.errs)
	}
	_ = os.Remove(logPath)
	l.GateDecided(d)
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:exec-nope", MissionID: "m1", Decision: "approve", Actor: "alice"})
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m2", Decision: "approve", Actor: "alice"})
	if len(l.Store.All()) != before || runs(f) != 1 || len(g.notes) != 4 || !strings.Contains(g.notes[1], "log unreadable") || !strings.Contains(g.notes[2], "not found") || !strings.Contains(g.notes[3], "does not match") {
		t.Fatalf("%v", g.notes)
	}
}

// Resume refused while busy / mission not waiting / shutting down: note, no
// spawn.
func TestResumeRefusalsFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	d := GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Actor: "alice"}
	// m2 occupies the only slot (a blocking session without denials).
	f.setBody(t, blockBody)
	if _, reason, err := l.Start(StartRequest{MissionID: "m2", Instruction: "busy"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitRuns(t, f, 2)
	before := len(l.Store.All())
	l.GateDecided(d)
	if len(l.Store.All()) != before || len(g.notes) != 1 || !strings.Contains(g.notes[0], "session launcher busy") {
		t.Fatalf("%v", g.notes)
	}
	f.release(t)
	waitDone(t, l)
	f.setBody(t, gateBody(denialResult))
	l.Shutdown()
	l.GateDecided(d)
	if len(g.notes) != 2 || !strings.Contains(g.notes[1], "shutting down") || missionOf(t, l, "m1") != domain.MissionWaitingHuman {
		t.Fatalf("%v", g.notes)
	}
}

// S2-8: gate creation fails → mission falls back to waiting_for_result, the
// fault is reported, the usage note is still written.
func TestGateAskFailureFallsBackFRRHZ124S2(t *testing.T) {
	_, l, o, g := gateLauncher(t, gateBody(denialResult))
	g.fail = errors.New("relay said no")
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "ship"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l)
	if st := missionOf(t, l, "m1"); st != domain.MissionWaitingResult {
		t.Fatalf("mission %s", st)
	}
	if len(g.errs) != 1 || !strings.HasPrefix(g.errs[0], "gate ") || !strings.Contains(g.errs[0], "relay said no") {
		t.Fatalf("%v", g.errs)
	}
	if len(o.got) != 1 || o.got[0].ExecutionID != id || o.got[0].GateID != "" {
		t.Fatalf("%+v", o.got)
	}
	// A session stopped by the launcher never raises a gate, even with denials.
	g.fail = nil
	f2, l2, _, g2 := gateLauncher(t, `trap 'cat "$D/result.json"; exit 0' TERM
sleep 1000 &
wait`)
	_ = os.WriteFile(filepath.Join(f2.dir, "result.json"), []byte(denialResult+"\n"), 0o600)
	if _, reason, err := l2.Start(StartRequest{MissionID: "m1", Instruction: "x", Budget: BudgetOverride{TimeMs: i64(800)}}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l2)
	if len(g2.asks) != 0 || missionOf(t, l2, "m1") != domain.MissionWaitingResult {
		t.Fatalf("%v %s", g2.asks, missionOf(t, l2, "m1"))
	}
}

// 3b: a result line without result text is labelled with the harness's
// terminal_reason and subtype.
func TestTerminalLabelFRRHZ124S2(t *testing.T) {
	f := newFake(t, emit(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"terminal_reason":"budget_exhausted","total_cost_usd":5.01,"num_turns":9,"permission_denials":[],"modelUsage":{}}`))
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l)
	if r := ref(t, s, id); r.Summary != "terminal: budget_exhausted (error_max_budget_usd)" || r.State != execution.Failed {
		t.Fatalf("%+v", r)
	}
	tr := "x"
	for want, res := range map[string]resultLine{
		"terminal: x":                     {TerminalReason: &tr},
		"terminal: error_during":          {Subtype: "error_during"},
		"result line without result text": {},
	} {
		if got := terminalLabel(&res); got != want {
			t.Errorf("%q want %q", got, want)
		}
	}
}

// 3c: a claimed execution with no spawn marker under a ledger whose digest
// differs from the claim's is never respawned (the marker may live in the
// old logDir) — human resolution required, zero writes.
func TestLedgerChangeCrashGuardFRRHZ124S2(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	if err := os.Chmod(f.bin, 0o600); err != nil {
		t.Fatal(err)
	}
	req := StartRequest{MissionID: "m1", Instruction: "x"}
	id, reason, _ := l.Start(req)
	if !strings.HasPrefix(reason, "session spawn failed") || ref(t, s, id).State != execution.DispatchClaimed {
		t.Fatalf("%q", reason)
	}
	_ = os.Chmod(f.bin, 0o700)
	newLogs := t.TempDir()
	moved := newLauncher(s, f.config(t, func(m map[string]any) { m["logDir"] = newLogs }), &outcomes{})
	before := len(s.All())
	for _, step := range []func() (string, error){
		func() (string, error) { _, r, e := moved.Prepare(req); return r, e },
		func() (string, error) { _, r, e := moved.Start(req); return r, e },
	} {
		r, err := step()
		if err != nil || !strings.Contains(r, "ledger changed") || !strings.Contains(r, "human resolution required") {
			t.Fatalf("%q %v", r, err)
		}
	}
	if len(s.All()) != before || runs(f) != 0 {
		t.Fatalf("writes=%d runs=%d", len(s.All())-before, runs(f))
	}
	// The same ledger still converges (S1 behavior kept).
	if _, reason, err := l.Start(req); err != nil || reason != "" || ref(t, s, id).State != execution.Accepted {
		t.Fatal(reason, err)
	}
	f.release(t)
	waitDone(t, l)
}

// 3d: modelUsage too large even without the denial list → replaced by null
// with an explicit marker and digest; totals stay.
func TestUsageNoteModelUsageLastResortFRRHZ124S2(t *testing.T) {
	cost, turns := json.Number("1.5"), json.Number("4")
	mu := map[string]json.RawMessage{}
	for i := 0; i < 40; i++ {
		mu[fmt.Sprintf("model-%02d", i)] = json.RawMessage(fmt.Sprintf(`{"inputTokens":%d,"pad":"%s"}`, i, strings.Repeat("p", 600)))
	}
	o := Outcome{MissionID: "m1", ExecutionID: "exec-x", SessionID: "s", Model: "claude-opus-5-5", State: execution.Succeeded, UsageAvailable: true, ModelUsage: mu, TotalCostUSD: &cost, NumTurns: &turns, PermissionDenials: []Denial{}}
	note := o.NoteContent()
	b, _ := json.Marshal(mu)
	sum := sha256.Sum256(b)
	if len(note) > MaxNoteBytes || !strings.Contains(note, `"modelUsage":null`) || !strings.Contains(note, `"modelUsageOmitted":true`) ||
		!strings.Contains(note, `"modelUsageDigest":"sha256:`+hex.EncodeToString(sum[:])+`"`) || !strings.Contains(note, `"totalCostUsd":1.5`) || !strings.Contains(note, `"numTurns":4`) {
		t.Fatalf("%d bytes: %.400s", len(note), note)
	}
	// A note that fits never carries the marker.
	small := o
	small.ModelUsage = map[string]json.RawMessage{"m": json.RawMessage(`{"a":1}`)}
	if n := small.NoteContent(); strings.Contains(n, "modelUsageOmitted") || strings.Contains(n, "modelUsageDigest") {
		t.Fatal(n)
	}
}

// Helper bounds: rune-safe cut, first word, tool-name validity.
func TestGateHelpersFRRHZ124S2(t *testing.T) {
	if s, cut := truncBytes("가나다", 4); s != "가" || !cut {
		t.Fatalf("%q", s)
	}
	for in, want := range map[string]string{"STOP now": "STOP", "\n STOP": "STOP", "": "", "STOPx y": "STOPx"} {
		if got := firstWord(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	for name, ok := range map[string]bool{"Bash": true, "mcp__x__y": true, "a.b:c-d_e9": true, "Bash(git push:*)": false, "Read Bash": false, "Read,Bash": false, "-x": false, "--allowedTools": false, " ": false, "": false, "a\nb": false, "한글": false, strings.Repeat("n", 129): false, strings.Repeat("n", 128): true} {
		if validTool(name) != ok {
			t.Errorf("%q", name)
		}
	}
	names, skipped := deniedTools([]rawDenial{{ToolName: "A"}, {ToolName: "--evil"}, {ToolName: "A"}, {ToolName: "B"}, {ToolName: "Read Bash"}, {ToolName: "Read,Bash"}})
	if strings.Join(names, ",") != "A,B" || skipped != 3 {
		t.Fatal(names, skipped)
	}
	// S3: at most MaxDenials distinct names are ever granted.
	many := []rawDenial{}
	for i := 0; i < MaxDenials+5; i++ {
		many = append(many, rawDenial{ToolName: fmt.Sprintf("T%02d", i)})
	}
	if names, _ := deniedTools(many); len(names) != MaxDenials || names[MaxDenials-1] != fmt.Sprintf("T%02d", MaxDenials-1) {
		t.Fatal(names)
	}
}

// N1 + N3 + determinism: an invalid (raw) tool name is quoted in the body so
// it cannot forge a denial line; the STOP rule is spelled out; the same
// original always renders the same request (the gate binding relies on it).
func TestGateBodyFRRHZ124S2(t *testing.T) {
	text := "done"
	res := &resultLine{Result: &text, RawDenials: []rawDenial{{ToolName: "Evil\n- Bash: {}", ToolInput: json.RawMessage(`{"a":1}`)}, {ToolName: "Read", ToolInput: json.RawMessage(`{}`)}}}
	gr := gateRequest("m1", "exec-1", res)
	if strings.Contains(gr.Body, "\n- Bash: {}") || !strings.Contains(gr.Body, `- "Evil\n- Bash: {}": {"a":1}`) || gr.Name != "RHZ 런처 승인 요청: Read" {
		t.Fatalf("%q", gr.Body)
	}
	if !strings.Contains(gr.Body, `"STOP:"·"stop"은 해당하지 않는다`) {
		t.Fatalf("%s", gr.Body)
	}
	if again := gateRequest("m1", "exec-1", res); again != gr {
		t.Fatal("gateRequest not deterministic")
	}
}

// M1: the reviewer's probe — the real gate is rejected with STOP, then a
// look-alike gate (same correlation and mission, other content) is approved:
// no resume, no spawn, a refusal note. The real gate id still binds.
func TestSpoofedGateRefusedFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	orig, _ := denied(t, l, g)
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "reject", Reason: "STOP no", Actor: "alice"})
	before := len(l.Store.All())
	l.GateDecided(GateDecision{GateID: "q-spoof", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Reason: "please read README", Actor: "mallory"})
	waitDone(t, l)
	if len(l.Store.All()) != before || runs(f) != 1 || missionOf(t, l, "m1") != domain.MissionWaitingHuman {
		t.Fatalf("writes=%d runs=%d", len(l.Store.All())-before, runs(f))
	}
	if len(g.notes) != 2 || !strings.Contains(g.notes[1], "q-spoof is not the denial gate of "+orig) || len(g.errs) != 1 {
		t.Fatalf("%v %v", g.notes, g.errs)
	}
	// Without the binding seam every resume is refused.
	l.GateID = nil
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Actor: "alice"})
	if runs(f) != 1 || !strings.Contains(g.notes[2], "gate binding unavailable") {
		t.Fatalf("%v", g.notes)
	}
}

// S1a: a decision delivered while the original's completion is still
// running (here: from inside AskGate) waits for that completion and then
// resumes — not refused because the mission is still running.
func TestDecisionDuringCompletionWaitsFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	// The decision is delivered and AskGate then holds the completion for
	// 500ms, so an immediate resume would find the mission still running.
	// The wait's bound is RunningWait + grace (≈30s here): a resume within
	// the 10s poll of argsOf can only come from the done signal of the
	// original's completion (no wall-clock threshold: stable under load).
	l.RunningWait = 30 * time.Second
	l.AskGate = func(r GateRequest) (string, error) {
		id, err := g.ask(r)
		l.GateDecided(GateDecision{GateID: id, CorrelationID: r.CorrelationID, MissionID: r.MissionID, Decision: "approve", Actor: "alice"})
		time.Sleep(500 * time.Millisecond)
		return id, err
	}
	if _, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "ship"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	args := argsOf(t, f, 2)
	if flagValues(args, "--resume") == nil || missionOfEventually(t, l, "m1", domain.MissionRunning) != domain.MissionRunning {
		t.Fatalf("%q", args)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	waitDone(t, l)
	if len(g.notes) != 0 || len(g.errs) != 0 {
		t.Fatal(g.notes, g.errs)
	}
}

func missionOfEventually(t *testing.T, l *Launcher, id string, want domain.MissionState) domain.MissionState {
	t.Helper()
	var st domain.MissionState
	for i := 0; i < 500; i++ {
		if st = missionOf(t, l, id); st == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	return st
}

// failObserve refuses every execution.observed append.
type failObserve struct{ *events.Store }

func (s failObserve) Append(exp uint64, e events.Event) error {
	if e.Type == "execution.observed" {
		return errors.New("disk full")
	}
	return s.Store.Append(exp, e)
}

// S2: no gate unless the observation (and so the log digest the gate binds
// to) is durable: waiting_for_result + OnError, usage note still reported.
func TestObserveFailureNoGateFRRHZ124S2(t *testing.T) {
	f := newFake(t, gateBody(denialResult))
	s := failObserve{launchStore(t)}
	o, g := &outcomes{}, &gateRec{}
	l := newLauncher(s, f.config(t, nil), o)
	l.AskGate, l.Note, l.OnError, l.GateID = g.ask, g.note, g.onError, g.id
	if _, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "ship"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitDone(t, l)
	if len(g.asks) != 0 || missionOf(t, l, "m1") != domain.MissionWaitingResult || len(o.got) != 1 || o.got[0].GateID != "" {
		t.Fatalf("asks=%d mission=%s", len(g.asks), missionOf(t, l, "m1"))
	}
	if len(g.errs) != 1 || !strings.HasPrefix(g.errs[0], "observe ") {
		t.Fatal(g.errs)
	}
}

// S3: resume guards on the original execution — each refused with a note,
// no spawn, no write.
func TestResumeGuardsFRRHZ124S2(t *testing.T) {
	f, l, _, g := gateLauncher(t, gateBody(denialResult))
	es := execution.Service{Store: l.Store}
	pol := l.Cfg.ceilingPolicy()
	prov := execution.Provenance{Ceiling: pol, Requested: pol, Effective: pol, ProfileID: Target, ProfileHash: strings.TrimPrefix(l.Cfg.Digest(), "sha256:"), ExecConfigDigest: l.Cfg.Digest(), Actor: "unverified-local-operator:t"}
	mk := func(key, target, ext string, terminal bool) string {
		r, err := es.IntentWithProvenance("m1", key, pol, pol, prov)
		if err == nil {
			_, err = es.ClaimDispatch(r.ID, target, "t")
		}
		if err == nil {
			_, err = es.Accept(r.ID, ext)
		}
		if err == nil && terminal {
			_, err = es.ObserveState(r.ID, "", "s", "sha256:x", execution.Succeeded)
		}
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	uuid := "5d624285-309c-4fa6-bbb0-4eba29a74d21"
	cases := map[string]string{
		mk("k-live", Target, uuid, false):                  "not terminal: accepted",
		mk("k-janus", "janus", uuid, true):                 "is not a claude-local execution",
		mk("k-ext", Target, "not-a-uuid", true):            "has no session uuid",
		mk("k-upper", Target, strings.ToUpper(uuid), true): "has no session uuid",
	}
	for id, want := range cases {
		before, n := len(l.Store.All()), len(g.notes)
		l.GateDecided(GateDecision{GateID: "q-x", CorrelationID: "launcher:" + id, MissionID: "m1", Decision: "approve", Actor: "alice"})
		if len(l.Store.All()) != before || runs(f) != 0 || len(g.notes) != n+1 || !strings.Contains(g.notes[n], want) {
			t.Fatalf("%s: %v", id, g.notes)
		}
	}
}

// S3 (security): the original log's init cwd must be a ledger workdir — a
// resume never runs claude in a directory the ledger does not name.
func TestResumeCwdNotInLedgerFRRHZ124S2(t *testing.T) {
	body := strings.Replace(gateBody(denialResult), `"$(pwd -P)"`, `"/nonexistent/elsewhere"`, 1)
	f, l, _, g := gateLauncher(t, body)
	orig, _ := denied(t, l, g)
	before := len(l.Store.All())
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Actor: "alice"})
	if len(l.Store.All()) != before || runs(f) != 1 || len(g.notes) != 1 || !strings.Contains(g.notes[0], "workdir unknown or not in ledger") {
		t.Fatalf("%v", g.notes)
	}
}

// N2: approve where no denied name is grantable → refused, never a resume
// with an empty grant. Reject still resumes (no grant needed).
func TestApproveNothingGrantableFRRHZ124S2(t *testing.T) {
	res := strings.Replace(strings.Replace(denialResult, `"tool_name":"Bash"`, `"tool_name":"Bash(git push:*)"`, -1), `"tool_name":"Write"`, `"tool_name":"Read Bash"`, 1)
	f, l, _, g := gateLauncher(t, gateBody(res))
	orig, gr := denied(t, l, g)
	if gr.Name != "RHZ 런처 승인 요청: (없음)" {
		t.Fatal(gr.Name)
	}
	before := len(l.Store.All())
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "approve", Actor: "alice"})
	if len(l.Store.All()) != before || runs(f) != 1 || len(g.notes) != 1 || !strings.Contains(g.notes[0], "no grantable tool name") {
		t.Fatalf("%v", g.notes)
	}
	l.GateDecided(GateDecision{GateID: "q-fake-1", CorrelationID: "launcher:" + orig, MissionID: "m1", Decision: "reject", Reason: "finish without", Actor: "alice"})
	if got := strings.Join(flagValues(argsOf(t, f, 2), "--allowedTools"), ","); got != "Read,Edit,Bash(git log:*)" {
		t.Fatal(got)
	}
	_ = os.WriteFile(filepath.Join(f.dir, "release2"), nil, 0o600)
	waitDone(t, l)
}

// N-d: the gate wording is pinned. Changing it changes the content id and
// invalidates every launcher gate still open across an upgrade — update this
// golden only on purpose.
func TestGateRequestGoldenFRRHZ124S2(t *testing.T) {
	text := "first\n\nlast paragraph"
	res := &resultLine{Result: &text, RawDenials: []rawDenial{{ToolName: "Bash", ToolInput: json.RawMessage(`{"command": "ls -la"}`)}, {ToolName: "Read Bash", ToolInput: json.RawMessage(`{}`)}, {ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"pwd"}`)}}}
	gr := gateRequest("m1", "exec-abc", res)
	const body = `세션 런처 실행 exec-abc (미션 m1)이 권한 거부로 끝났다. 거부 3건:
- Bash: {"command":"ls -la"}
- "Read Bash": {}
- Bash: {"command":"pwd"}

결과 마지막 문단:
last paragraph

승인 = 같은 세션을 재개하며 이번 재개 1회에 한해 다음 도구를 허용한다: Bash
(형식이 잘못된 도구 이름 1개는 승인해도 허용되지 않는다.)
거부 = 이 도구 없이 같은 세션을 재개해 마무리시킨다. 거부 사유의 첫 단어가 STOP이면 재개하지 않는다.
(STOP 규칙: 공백으로 나눈 첫 단어가 정확히 대문자 STOP일 때만 — "STOP" 단독 또는 "STOP 사유…". "STOP:"·"stop"은 해당하지 않는다.)`
	want := GateRequest{MissionID: "m1", ExecutionID: "exec-abc", CorrelationID: "launcher:exec-abc", Name: "RHZ 런처 승인 요청: Bash", Body: body}
	if gr != want {
		t.Fatalf("gate text changed:\n%q\nwant\n%q", gr, want)
	}
}
