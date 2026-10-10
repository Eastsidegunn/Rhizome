package sessionlauncher

// RHZ-124 S1 (FR-RHZ-124-S1): session launcher — ledger strict decode,
// event order, ceiling ∩ request, workdir allowlist, argv, env allowlist,
// usage outcome, timeout group kill, maxConcurrent, idempotency, spawn
// failure. A shell-script fake claude only: no real binary, no tokens.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

// fakeEnv is one fake-claude installation: the script records its argv
// (NUL-separated), environment and cwd under dir, counts its runs, then runs
// body (which emits the scripted stream-json).
type fakeEnv struct {
	dir, bin, work, other, logs string
}

func newFake(t *testing.T, body string) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	f := &fakeEnv{dir: filepath.Join(root, "rec"), bin: filepath.Join(root, "claude"), work: filepath.Join(root, "work"), other: filepath.Join(root, "other"), logs: filepath.Join(root, "logs")}
	for _, d := range []string{f.dir, f.work, f.other, f.logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.setBody(t, body)
	// FR-RHZ-124-S2 (3e): no fake claude (or its process group) outlives the
	// test, even when the test fails before releasing it.
	t.Cleanup(func() { f.reap() })
	return f
}

// reap releases a blocked fake and SIGKILLs every process group the fake
// started (each run records its pid = pgid: the launcher spawns with Setpgid)
// plus a recorded grandchild.
func (f *fakeEnv) reap() {
	_ = os.WriteFile(filepath.Join(f.dir, "release"), nil, 0o600)
	b, _ := os.ReadFile(filepath.Join(f.dir, "pids"))
	for _, line := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "grandchild")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func (f *fakeEnv) setBody(t *testing.T, body string) {
	t.Helper()
	script := "#!/bin/sh\nD='" + f.dir + "'\necho $$ >> \"$D/pids\"\n" +
		"for a in \"$@\"; do printf '%s\\0' \"$a\"; done > \"$D/args\"\n" +
		"env > \"$D/env\"\npwd -P > \"$D/cwd\"\necho run >> \"$D/count\"\n" + body + "\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeEnv) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// blockBody waits for release before emitting the result line.
const blockBody = `while [ ! -f "$D/release" ]; do sleep 0.02; done
cat <<'JSON'
{"type":"system","subtype":"init","model":"claude-opus-5-5"}
{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.5,"num_turns":1,"duration_ms":10,"terminal_reason":"completed","permission_denials":[],"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":1,"costUSD":0.5}}}
JSON`

// twoModelResult is a result line with two models and full-precision numbers.
const twoModelResult = `{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"ignored","total_cost_usd":0.123456789012345678,"num_turns":7,"duration_ms":45678,"terminal_reason":"completed","permission_denials":[{"tool_name":"Bash","tool_use_id":"toolu_01","tool_input":{"command":"git push"}}],"modelUsage":{"claude-opus-5-5":{"inputTokens":1234,"outputTokens":567,"cacheReadInputTokens":89012,"cacheCreationInputTokens":3456,"costUSD":0.1000000000000000055,"contextWindow":200000},"claude-haiku-4-5":{"inputTokens":11,"outputTokens":22,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.023456789012345678}}}`

func emit(lines ...string) string {
	return "cat <<'JSON'\n" + strings.Join(lines, "\n") + "\nJSON"
}

func (f *fakeEnv) ledger(mut func(map[string]any)) []byte {
	m := map[string]any{"claudePath": f.bin, "workdirs": map[string]any{"rhizome": f.work, "other": f.other}, "defaultWorkdir": "rhizome", "model": "claude-opus-5-5",
		"permissionMode": "acceptEdits", "allowedTools": []any{"Read", "Edit", "Bash(git log:*)"}, "logDir": f.logs, "maxConcurrent": 1,
		"ceiling": map[string]any{"usd": 5, "timeMs": 60000}}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func (f *fakeEnv) config(t *testing.T, mut func(map[string]any)) *Config {
	t.Helper()
	c, err := Parse(f.ledger(mut))
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

// launchStore holds goal g and missions m1, m2 already running (package
// tests stand in for the relay, which moves a mission to running after a
// successful Start).
func launchStore(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2"} {
		if _, err := ms.Create(id, "g", "do "+id, "done"); err != nil {
			t.Fatal(err)
		}
		toRunning(t, s, id)
	}
	return s
}

func toRunning(t *testing.T, s events.Port, id string) {
	t.Helper()
	ms := mission.Service{Store: s}
	for _, to := range []domain.MissionState{domain.MissionReady, domain.MissionRunning} {
		m, _ := projector.ReplayMission(s.List("mission", id))
		if _, err := ms.Transition(id, m.Revision, to); err != nil {
			t.Fatal(err)
		}
	}
}

func execTypes(s *events.Store) []string {
	out := []string{}
	for _, e := range s.All() {
		if e.AggregateType == "execution" {
			out = append(out, e.Type)
		}
	}
	return out
}

func readArgs(t *testing.T, f *fakeEnv) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// waitRuns blocks until the fake has started n times (it records argv, env
// and cwd before counting the run).
func waitRuns(t *testing.T, f *fakeEnv, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for runs(f) < n {
		if time.Now().After(deadline) {
			t.Fatalf("fake started %d times, want %d", runs(f), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runs(f *fakeEnv) int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "count"))
	return strings.Count(string(b), "run\n")
}

func ref(t *testing.T, s events.Port, id string) execution.Ref {
	t.Helper()
	r, err := execution.Replay(s.List("execution", id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

type outcomes struct{ got []Outcome }

func (o *outcomes) report(x Outcome) { o.got = append(o.got, x) }

func newLauncher(s events.Port, c *Config, o *outcomes) *Launcher {
	return &Launcher{Store: s, Cfg: c, Report: o.report, KillGrace: 200 * time.Millisecond, RunningWait: 100 * time.Millisecond}
}

// Ledger strict decode: unknown key, bypassPermissions, relative paths,
// trailing data, non-executable binary and every bound are refused; the
// digest ignores key order.
func TestLedgerStrictDecodeFRRHZ124S1(t *testing.T) {
	f := newFake(t, "")
	c := f.config(t, nil)
	if c.MaxConcurrent != 1 || c.Ceiling.USD != 5 || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(c.Digest()) {
		t.Fatalf("%+v %s", c, c.Digest())
	}
	noexec := filepath.Join(t.TempDir(), "claude")
	_ = os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o600)
	groupWritable := filepath.Join(t.TempDir(), "claude")
	_ = os.WriteFile(groupWritable, []byte("#!/bin/sh\n"), 0o700)
	if err := os.Chmod(groupWritable, 0o770); err != nil {
		t.Fatal(err)
	}
	linkToWritable := filepath.Join(t.TempDir(), "claude-link")
	if err := os.Symlink(groupWritable, linkToWritable); err != nil {
		t.Fatal(err)
	}
	// Every listed mode except bypassPermissions/default parses; a symlink to
	// a safe binary is fine; timeMs exactly one day is the cap.
	for _, mode := range []string{"acceptEdits", "auto", "manual", "dontAsk", "plan"} {
		if _, err := Parse(f.ledger(func(m map[string]any) { m["permissionMode"] = mode })); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	safeLink := filepath.Join(t.TempDir(), "claude-ok")
	_ = os.Symlink(f.bin, safeLink)
	if _, err := Parse(f.ledger(func(m map[string]any) {
		m["claudePath"] = safeLink
		m["ceiling"].(map[string]any)["timeMs"] = MaxTimeMs
	})); err != nil {
		t.Fatal(err)
	}
	// A group- or world-writable ledger file is refused by Load.
	for _, mode := range []os.FileMode{0o620, 0o602} {
		p := filepath.Join(t.TempDir(), "launcher.json")
		_ = os.WriteFile(p, f.ledger(nil), 0o600)
		_ = os.Chmod(p, mode)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Errorf("mode %o: %v", mode, err)
		}
	}
	okFile := filepath.Join(t.TempDir(), "launcher.json")
	_ = os.WriteFile(okFile, f.ledger(nil), 0o600)
	if _, err := Load(okFile); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(map[string]any){
		"unknown key":          func(m map[string]any) { m["apiKey"] = "TEST-NON-CREDENTIAL" },
		"unknown ceiling key":  func(m map[string]any) { m["ceiling"].(map[string]any)["tokens"] = 1 },
		"bypassPermissions":    func(m map[string]any) { m["permissionMode"] = "bypassPermissions" },
		"unknown mode":         func(m map[string]any) { m["permissionMode"] = "yolo" },
		"default mode":         func(m map[string]any) { m["permissionMode"] = "default" },
		"timeMs over a day":    func(m map[string]any) { m["ceiling"].(map[string]any)["timeMs"] = MaxTimeMs + 1 },
		"writable claudePath":  func(m map[string]any) { m["claudePath"] = groupWritable },
		"symlink to writable":  func(m map[string]any) { m["claudePath"] = linkToWritable },
		"relative claudePath":  func(m map[string]any) { m["claudePath"] = "bin/claude" },
		"non-executable":       func(m map[string]any) { m["claudePath"] = noexec },
		"relative workdir":     func(m map[string]any) { m["workdirs"] = map[string]any{"rhizome": "work"} },
		"missing workdir dir":  func(m map[string]any) { m["workdirs"] = map[string]any{"rhizome": f.work + "/absent"} },
		"empty workdirs":       func(m map[string]any) { m["workdirs"] = map[string]any{} },
		"default not a key":    func(m map[string]any) { m["defaultWorkdir"] = "nope" },
		"empty model":          func(m map[string]any) { m["model"] = " " },
		"relative logDir":      func(m map[string]any) { m["logDir"] = "logs" },
		"zero maxConcurrent":   func(m map[string]any) { m["maxConcurrent"] = 0 },
		"zero usd":             func(m map[string]any) { m["ceiling"].(map[string]any)["usd"] = 0 },
		"sub-cent usd":         func(m map[string]any) { m["ceiling"].(map[string]any)["usd"] = 0.001 },
		"zero timeMs":          func(m map[string]any) { m["ceiling"].(map[string]any)["timeMs"] = 0 },
		"flag-shaped tool":     func(m map[string]any) { m["allowedTools"] = []any{"--dangerously-skip-permissions"} },
		"missing claudePath":   func(m map[string]any) { delete(m, "claudePath") },
		"missing permissionMd": func(m map[string]any) { delete(m, "permissionMode") },
	}
	for name, mut := range bad {
		if _, err := Parse(f.ledger(mut)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, tail := range []string{" {}", " }", " x"} {
		if _, err := Parse(append(f.ledger(nil), tail...)); err == nil {
			t.Errorf("trailing %q accepted", tail)
		}
	}
	// allowedTools may be empty.
	if _, err := Parse(f.ledger(func(m map[string]any) { m["allowedTools"] = []any{} })); err != nil {
		t.Fatal(err)
	}
	// Digest: key order / whitespace independent, value sensitive.
	var m map[string]any
	_ = json.Unmarshal(f.ledger(nil), &m)
	pretty, _ := json.MarshalIndent(m, "", "    ")
	c2, err := Parse(pretty)
	if err != nil || c2.Digest() != c.Digest() {
		t.Fatalf("digest moved with formatting: %v", err)
	}
	if c3 := f.config(t, func(m map[string]any) { m["model"] = "claude-other" }); c3.Digest() == c.Digest() {
		t.Fatal("digest ignores model")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("absent file accepted")
	}
}

// FR-RHZ-124-S1-L1 + FR-RHZ-124-S1-L5: intent → dispatch_claimed → accepted; the accepted external id
// is the --session-id uuid; argv is exact.
func TestStartEventOrderAndArgvFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	o := &outcomes{}
	l := newLauncher(s, f.config(t, nil), o)
	l.BoardURL = "http://127.0.0.1:9911"
	req := StartRequest{MissionID: "m1", Instruction: "build it", Budget: BudgetOverride{USD: f64(2.5)}, Actor: "tester"}
	plan, reason, err := l.Prepare(req)
	if err != nil || reason != "" || plan.Existing != "" || !strings.HasPrefix(plan.ExecutionID, "exec-") {
		t.Fatalf("%+v %q %v", plan, reason, err)
	}
	if len(execTypes(s)) != 0 {
		t.Fatal("prepare wrote")
	}
	id, reason, err := l.Start(req)
	if err != nil || reason != "" || id != plan.ExecutionID {
		t.Fatalf("%s %q %v", id, reason, err)
	}
	want := []string{"execution.intent", "execution.dispatch_claimed", "execution.accepted"}
	if got := execTypes(s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", got)
	}
	r := ref(t, s, id)
	if r.Target != Target || r.ClaimCorrelation != "mission.start:m1" || r.State != execution.Accepted {
		t.Fatalf("%+v", r)
	}
	waitRuns(t, f, 1)
	args := readArgs(t, f)
	uuidRE := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRE.MatchString(r.ExternalID) {
		t.Fatalf("external %q", r.ExternalID)
	}
	wantArgs := []string{"-p", Prompt("http://127.0.0.1:9911", "m1", "build it"), "--output-format", "stream-json", "--verbose", "--session-id", r.ExternalID, "--model", "claude-opus-5-5",
		"--permission-mode", "acceptEdits", "--allowedTools", "Read", "Edit", "Bash(git log:*)", "--max-budget-usd", "2.50"}
	if strings.Join(args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("argv\n got %q\nwant %q", args, wantArgs)
	}
	if !strings.Contains(args[1], "curl -s 'http://127.0.0.1:9911/v1/context?task=m1'") || !strings.Contains(args[1], "POST http://127.0.0.1:9911/v1/intent") || strings.Contains(args[1], "8790") || !strings.HasSuffix(args[1], "\n\nbuild it") || !strings.Contains(args[1], "git push") {
		t.Fatalf("prompt %q", args[1])
	}
	// Provenance: usd cents / ms / depth 1 / tool capabilities / ledger digest.
	p := r.Provenance
	if p == nil || p.Effective.Budget != 250 || p.Ceiling.Budget != 500 || p.Effective.Timeout != 60000 || p.Effective.MaxDepth != 1 || p.Effective.Units != Units ||
		strings.Join(p.Effective.Capabilities, ",") != "tool:Bash(git log:*),tool:Edit,tool:Read" || p.ProfileID != Target ||
		p.ExecConfigDigest != l.Cfg.Digest() || "sha256:"+p.ProfileHash != l.Cfg.Digest() || p.Actor != "tester" {
		t.Fatalf("%+v", p)
	}
	f.release(t)
	l.Wait()
	if r := ref(t, s, id); r.State != execution.Succeeded || r.Summary != "done" || !strings.HasPrefix(r.SourceRef, "sha256:") {
		t.Fatalf("%+v", r)
	}
	if m, _ := projector.ReplayMission(s.List("mission", "m1")); m.State != domain.MissionWaitingResult {
		t.Fatalf("mission %s", m.State)
	}
	if len(o.got) != 1 || o.got[0].SessionID != r.ExternalID {
		t.Fatalf("%+v", o.got)
	}
	// Terminal and durable: the spawn marker is gone.
	if _, err := os.Stat(filepath.Join(f.logs, id+".pid")); !os.IsNotExist(err) {
		t.Fatalf("marker kept after terminal state: %v", err)
	}
	// --max-budget-usd formatting and the empty tool list.
	for cents, want := range map[int64]string{1: "0.01", 10: "0.10", 500: "5.00", 1234: "12.34"} {
		if got := formatUSD(cents); got != want {
			t.Errorf("%d → %s", cents, got)
		}
	}
	c := f.config(t, func(m map[string]any) { m["allowedTools"] = []any{} })
	a := c.Args("p", "sid", 100)
	if strings.Contains(strings.Join(a, " "), "--allowedTools") || a[len(a)-1] != "1.00" || strings.Join(c.ceilingPolicy().Capabilities, ",") != "tool:none" {
		t.Fatalf("%q %v", a, c.ceilingPolicy())
	}
}

// FR-RHZ-124-S1-L2 + FR-RHZ-124-S1-L3: ceiling ∩ request on usd/timeMs; JANUS axes, other session
// modes and unknown workdirs are refused — all with zero writes and no spawn.
func TestBudgetAndWorkdirRejectionsFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	cases := map[string]struct {
		req  StartRequest
		want string
	}{
		"usd over ceiling":    {StartRequest{Budget: BudgetOverride{USD: f64(5.01)}}, "POLICY_DENIED: budget.usd 5.01 exceeds ceiling 5.00"},
		"usd zero":            {StartRequest{Budget: BudgetOverride{USD: f64(0)}}, "BUDGET_INVALID: budget.usd must be > 0"},
		"usd negative":        {StartRequest{Budget: BudgetOverride{USD: f64(-1)}}, "BUDGET_INVALID"},
		"usd sub-cent":        {StartRequest{Budget: BudgetOverride{USD: f64(0.005)}}, "BUDGET_INVALID: budget.usd must have at most 2 decimals"},
		"timeMs over ceiling": {StartRequest{Budget: BudgetOverride{TimeMs: i64(60001)}}, "POLICY_DENIED: budget.timeMs 60001 exceeds ceiling 60000"},
		"timeMs zero":         {StartRequest{Budget: BudgetOverride{TimeMs: i64(0)}}, "BUDGET_INVALID: budget.timeMs must be > 0"},
		"tokens":              {StartRequest{Budget: BudgetOverride{Tokens: i64(10)}}, "budget.tokens not supported by session launcher"},
		"maxDepth":            {StartRequest{Budget: BudgetOverride{MaxDepth: i64(1)}}, "budget.maxDepth not supported by session launcher"},
		"multiturn":           {StartRequest{SessionMode: "multiturn"}, "sessionMode must be oneshot"},
		"unknown workdir":     {StartRequest{Workdir: "elsewhere"}, `POLICY_DENIED: workdir "elsewhere" not in ledger`},
		"path as workdir":     {StartRequest{Workdir: f.work}, "POLICY_DENIED: workdir"},
		"no instruction":      {StartRequest{Instruction: " "}, "missionId and instruction required"},
	}
	before := len(s.All())
	for name, c := range cases {
		c.req.MissionID = "m1"
		if c.req.Instruction == "" {
			c.req.Instruction = "x"
		}
		if _, reason, err := l.Prepare(c.req); err != nil || !strings.HasPrefix(reason, c.want) {
			t.Errorf("%s prepare: %q %v", name, reason, err)
		}
		if _, reason, err := l.Start(c.req); err != nil || !strings.HasPrefix(reason, c.want) {
			t.Errorf("%s start: %q %v", name, reason, err)
		}
	}
	if len(s.All()) != before || runs(f) != 0 {
		t.Fatalf("writes=%d runs=%d", len(s.All())-before, runs(f))
	}
	// Within the ceiling: effective = request; the named workdir is the cwd.
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x", Workdir: "other", Budget: BudgetOverride{USD: f64(5), TimeMs: i64(30000)}})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	p := ref(t, s, id).Provenance
	if p.Effective.Budget != 500 || p.Effective.Timeout != 30000 || p.Requested.Timeout != 30000 || p.Ceiling.Timeout != 60000 {
		t.Fatalf("%+v", p)
	}
	waitRuns(t, f, 1)
	cwd, _ := os.ReadFile(filepath.Join(f.dir, "cwd"))
	wantDir, _ := filepath.EvalSymlinks(f.other)
	if strings.TrimSpace(string(cwd)) != wantDir {
		t.Fatalf("cwd %q want %q", cwd, wantDir)
	}
	f.release(t)
	l.Wait()
	// A nil ledger rejects everything before any write.
	if _, reason, _ := (&Launcher{Store: s}).Prepare(StartRequest{MissionID: "m1", Instruction: "x"}); reason != "session launcher config not loaded" {
		t.Fatal(reason)
	}
}

// FR-RHZ-124-S1-L6: the child environment is the allowlist only, secrets are never passed
// even when allowlisted, TERM=dumb is set.
func TestEnvAllowlistFRRHZ124S1(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "TEST-NON-CREDENTIAL")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "TEST-NON-CREDENTIAL")
	t.Setenv("GITHUB_TOKEN", "TEST-NON-CREDENTIAL")
	t.Setenv("RHZ_UNLISTED_VAR", "1")
	t.Setenv("LANG", "C")
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	if _, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	f.release(t)
	l.Wait()
	b, err := os.ReadFile(filepath.Join(f.dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	// sh itself exports PWD/SHLVL/_/OLDPWD; nothing else may appear.
	allowed := map[string]bool{"TERM": true, "PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
	for _, n := range EnvAllowlist {
		allowed[n] = true
	}
	names := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		n, v, _ := strings.Cut(line, "=")
		names[n] = v
		if !allowed[n] {
			t.Errorf("child saw %s", n)
		}
	}
	if names["TERM"] != "dumb" || names["LANG"] != "C" || names["PATH"] == "" {
		t.Fatalf("%v", names)
	}
	// Secret-shaped names are dropped even when allowlisted.
	env := BuildEnv([]string{"ANTHROPIC_BASE_URL=x", "CLAUDE_CODE_OAUTH_TOKEN=x", "DB_PASSWORD=x", "X_SECRET=x", "API_KEY=x", "GH_TOKEN=x", "HOME=/h", "PATH=/bin", "HOME=/dup", "TERM=xterm"},
		[]string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "DB_PASSWORD", "X_SECRET", "API_KEY", "GH_TOKEN", "HOME", "PATH"})
	if strings.Join(env, " ") != "HOME=/h PATH=/bin TERM=dumb" {
		t.Fatalf("%q", env)
	}
}

// FR-RHZ-124-S1-L4: per-model usage preserved byte-exact (two models), denials recorded
// without tool input; is_error → Failed.
func TestUsageOutcomeFRRHZ124S1(t *testing.T) {
	f := newFake(t, emit(`{"type":"assistant","message":{"content":[]}}`, twoModelResult))
	s := launchStore(t)
	o := &outcomes{}
	l := newLauncher(s, f.config(t, nil), o)
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	l.Wait()
	if len(o.got) != 1 {
		t.Fatalf("%d outcomes", len(o.got))
	}
	x := o.got[0]
	if x.ExecutionID != id || x.State != execution.Succeeded || !x.UsageAvailable || x.TotalCostUSD.String() != "0.123456789012345678" || x.NumTurns.String() != "7" || x.DurationMs.String() != "45678" || *x.TerminalReason != "completed" || *x.IsError {
		t.Fatalf("%+v", x)
	}
	if string(x.ModelUsage["claude-opus-5-5"]) != `{"inputTokens":1234,"outputTokens":567,"cacheReadInputTokens":89012,"cacheCreationInputTokens":3456,"costUSD":0.1000000000000000055,"contextWindow":200000}` ||
		string(x.ModelUsage["claude-haiku-4-5"]) != `{"inputTokens":11,"outputTokens":22,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.023456789012345678}` || len(x.ModelUsage) != 2 {
		t.Fatalf("%s", x.ModelUsage)
	}
	if len(x.PermissionDenials) != 1 || x.PermissionDenials[0] != (Denial{"Bash", "toolu_01"}) {
		t.Fatalf("%+v", x.PermissionDenials)
	}
	logBytes, _ := os.ReadFile(filepath.Join(f.logs, id+".ndjson"))
	_, digest := parseLog(logBytes)
	if x.LogDigest != digest || ref(t, s, id).SourceRef != digest || ref(t, s, id).Summary != "all done" {
		t.Fatalf("%s %s", x.LogDigest, digest)
	}
	note := x.NoteContent()
	body := note[strings.Index(note, "```json\n")+8 : strings.LastIndex(note, "```")]
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err, note)
	}
	keys := []string{}
	for k := range parsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "durationMs,executionId,isError,logDigest,model,modelUsage,numTurns,permissionDenials,permissionDenialsCount,resumeOf,sessionId,terminalReason,totalCostUsd" {
		t.Fatal(keys)
	}
	if string(parsed["resumeOf"]) != "null" || string(parsed["totalCostUsd"]) != "0.123456789012345678" || !strings.Contains(body, "0.1000000000000000055") || !strings.Contains(body, "0.023456789012345678") ||
		strings.Contains(note, "git push") || !strings.Contains(string(parsed["permissionDenials"]), `"toolUseId": "toolu_01"`) {
		t.Fatalf("%s", note)
	}
	// is_error → Failed.
	f2 := newFake(t, emit(strings.Replace(twoModelResult, `"is_error":false`, `"is_error":true`, 1)))
	o2 := &outcomes{}
	l2 := newLauncher(s, f2.config(t, nil), o2)
	id2, reason, err := l2.Start(StartRequest{MissionID: "m2", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	l2.Wait()
	if ref(t, s, id2).State != execution.Failed || o2.got[0].State != execution.Failed || !*o2.got[0].IsError {
		t.Fatalf("%+v", o2.got)
	}
}

// FR-RHZ-124-S1-L8: no result line → execution Failed, usage unavailable with nulls.
func TestNoResultLineFRRHZ124S1(t *testing.T) {
	f := newFake(t, emit(`{"type":"system","subtype":"init"}`, `not json`)+"\nexit 1")
	s := launchStore(t)
	o := &outcomes{}
	l := newLauncher(s, f.config(t, nil), o)
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	l.Wait()
	if r := ref(t, s, id); r.State != execution.Failed || r.Summary != "no result line in session log" {
		t.Fatalf("%+v", r)
	}
	if len(o.got) != 1 || o.got[0].UsageAvailable {
		t.Fatalf("%+v", o.got)
	}
	note := o.got[0].NoteContent()
	for _, want := range []string{"usage unavailable", `"modelUsage": null`, `"totalCostUsd": null`, `"numTurns": null`, `"durationMs": null`, `"isError": null`, `"permissionDenials": null`, `"permissionDenialsCount": null`, `"resumeOf": null`, `"logDigest": "sha256:`} {
		if !strings.Contains(note, want) {
			t.Fatalf("missing %s in %s", want, note)
		}
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// FR-RHZ-124-S1-L9: timeout → durable stop request, then the whole process group dies
// (a grandchild that never exits on its own is gone too).
func TestTimeoutKillsProcessGroupFRRHZ124S1(t *testing.T) {
	f := newFake(t, `sleep 1000 &
echo $! > "$D/grandchild.tmp"
mv "$D/grandchild.tmp" "$D/grandchild"
wait`)
	s := launchStore(t)
	o := &outcomes{}
	l := newLauncher(s, f.config(t, nil), o)
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x", Budget: BudgetOverride{TimeMs: i64(1500)}})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.dir, "grandchild")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake never forked its grandchild")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { l.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("completion did not finish")
	}
	r := ref(t, s, id)
	if !r.StopRequested || r.StopReason != "budget_exceeded" || r.StopActor != Actor || r.State != execution.Cancelled || !strings.HasPrefix(r.Summary, "stopped: timeout after 1500ms") {
		t.Fatalf("%+v", r)
	}
	if len(o.got) != 1 || !o.got[0].TimedOut || o.got[0].State != execution.Cancelled {
		t.Fatalf("%+v", o.got)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "grandchild"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if l.Running() != 0 {
		t.Fatal("child still tracked")
	}
}

// FR-RHZ-124-S1-L11: maxConcurrent — a new start beyond the limit is refused with zero
// writes; once the slot frees it starts.
func TestMaxConcurrentFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	if _, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitRuns(t, f, 1)
	before := len(s.All())
	req := StartRequest{MissionID: "m2", Instruction: "y"}
	if _, reason, err := l.Prepare(req); err != nil || reason != "session launcher busy: 1 running (maxConcurrent 1)" {
		t.Fatal(reason, err)
	}
	if _, reason, err := l.Start(req); err != nil || !strings.HasPrefix(reason, "session launcher busy") {
		t.Fatal(reason, err)
	}
	if len(s.All()) != before || runs(f) != 1 {
		t.Fatalf("writes=%d runs=%d", len(s.All())-before, runs(f))
	}
	f.release(t)
	l.Wait()
	if _, reason, err := l.Start(req); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	l.Wait()
}

// FR-RHZ-124-S1-L12: an identical re-submission finds the same execution: no second
// spawn, no write; after the end it is refused like the JANUS backend.
func TestIdempotentResubmitFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, func(m map[string]any) { m["maxConcurrent"] = 3 }), &outcomes{})
	req := StartRequest{MissionID: "m1", Instruction: "x", Budget: BudgetOverride{USD: f64(1)}}
	id, reason, err := l.Start(req)
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitRuns(t, f, 1)
	before := len(s.All())
	plan, reason, err := l.Prepare(req)
	if err != nil || reason != "" || plan.ExecutionID != id || plan.Existing != "accepted" {
		t.Fatalf("%+v %q %v", plan, reason, err)
	}
	id2, reason, err := l.Start(req)
	if err != nil || reason != "" || id2 != id || len(s.All()) != before || runs(f) != 1 {
		t.Fatalf("%s %q %v writes=%d runs=%d", id2, reason, err, len(s.All())-before, runs(f))
	}
	// A different budget is a different request (different key).
	if p, _, _ := l.Prepare(StartRequest{MissionID: "m1", Instruction: "x", Budget: BudgetOverride{USD: f64(2)}}); p.ExecutionID == id {
		t.Fatal("budget not keyed")
	}
	f.release(t)
	l.Wait()
	if _, reason, _ := l.Start(req); !strings.HasPrefix(reason, "execution already ended: succeeded") || runs(f) != 1 {
		t.Fatalf("%q runs=%d", reason, runs(f))
	}
	// A fresh launcher (serve restart) over an accepted execution with no
	// live child: lookup only, nothing killed or re-run, no panic.
	f2 := newFake(t, blockBody)
	s2 := launchStore(t)
	l2 := newLauncher(s2, f2.config(t, nil), &outcomes{})
	if _, reason, err := l2.Start(req); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	waitRuns(t, f2, 1)
	restarted := newLauncher(s2, l2.Cfg, &outcomes{})
	before = len(s2.All())
	if p, reason, err := restarted.Prepare(req); err != nil || reason != "" || p.Existing != "accepted" || restarted.Running() != 0 {
		t.Fatalf("%+v %q %v", p, reason, err)
	}
	if _, reason, err := restarted.Start(req); err != nil || reason != "" || len(s2.All()) != before || runs(f2) != 1 {
		t.Fatalf("%q %v", reason, err)
	}
	f2.release(t)
	l2.Wait()
}

// FR-RHZ-124-S1-L1 (failure half): a spawn failure leaves the execution claimed, never
// accepted; the marker is dropped so the same request converges later.
func TestSpawnFailureFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	if err := os.Chmod(f.bin, 0o600); err != nil {
		t.Fatal(err)
	}
	req := StartRequest{MissionID: "m1", Instruction: "x"}
	id, reason, err := l.Start(req)
	if err != nil || !strings.HasPrefix(reason, "session spawn failed") || id == "" {
		t.Fatalf("%s %q %v", id, reason, err)
	}
	if got := execTypes(s); strings.Join(got, ",") != "execution.intent,execution.dispatch_claimed" {
		t.Fatalf("%v", got)
	}
	if _, err := os.Stat(filepath.Join(f.logs, id+".pid")); !os.IsNotExist(err) {
		t.Fatal("marker left behind", err)
	}
	if l.Running() != 0 {
		t.Fatal("tracked a failed spawn")
	}
	_ = os.Chmod(f.bin, 0o700)
	if p, reason, _ := l.Prepare(req); reason != "" || p.Existing != "dispatch_claimed" {
		t.Fatalf("%+v %q", p, reason)
	}
	if id2, reason, err := l.Start(req); err != nil || reason != "" || id2 != id || ref(t, s, id).State != execution.Accepted {
		t.Fatalf("%q %v", reason, err)
	}
	pidFile, _ := os.ReadFile(filepath.Join(f.logs, id+".pid"))
	if _, err := strconv.Atoi(strings.TrimSpace(string(pidFile))); err != nil {
		t.Fatalf("pid file %q", pidFile)
	}
	f.release(t)
	l.Wait()
	// A claimed execution whose spawn marker exists (crash between spawn and
	// accept) is never respawned.
	s2 := launchStore(t)
	l2 := newLauncher(s2, f.config(t, nil), &outcomes{})
	_ = os.Chmod(f.bin, 0o600)
	id3, _, _ := l2.Start(StartRequest{MissionID: "m2", Instruction: "z"})
	_ = os.Chmod(f.bin, 0o700)
	if err := os.WriteFile(filepath.Join(f.logs, id3+".pid"), []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(s2.All())
	if _, reason, _ := l2.Start(StartRequest{MissionID: "m2", Instruction: "z"}); !strings.HasPrefix(reason, "session spawn outcome unknown") || len(s2.All()) != before {
		t.Fatalf("%q", reason)
	}
}

// The mission is moved only from running; an operator-moved mission is left
// alone without error.
func TestCompletionSkipsNonRunningMissionFRRHZ124S1(t *testing.T) {
	f := newFake(t, blockBody)
	s := launchStore(t)
	var faults bytes.Buffer
	l := newLauncher(s, f.config(t, nil), &outcomes{})
	l.OnError = func(scope, id string, err error) { faults.WriteString(scope + " " + err.Error() + "\n") }
	if _, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"}); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	m, _ := projector.ReplayMission(s.List("mission", "m1"))
	if _, err := (mission.Service{Store: s}).Transition("m1", m.Revision, domain.MissionBlocked); err != nil {
		t.Fatal(err)
	}
	f.release(t)
	l.Wait()
	if m, _ := projector.ReplayMission(s.List("mission", "m1")); m.State != domain.MissionBlocked || faults.Len() != 0 {
		t.Fatalf("%s %s", m.State, faults.String())
	}
}

func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
