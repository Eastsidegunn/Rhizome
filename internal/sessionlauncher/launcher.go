package sessionlauncher

// RHZ-124 S1 (FR-RHZ-TBD(124-S1)): mission.start → one local claude -p
// session. Sequence (mirrors janusadapter.Starter): validate against the
// ledger and look the content-derived key up with zero writes (Prepare);
// then execution.intent → dispatch_claimed (durable) → spawn → accepted
// with the session uuid as the external id. The relay moves the mission to
// running only after Start reports success.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/policy"
)

const (
	// Target is the dispatch target and provenance profile id of this backend.
	Target = "claude-local"
	// Actor is the journal actor of launcher-originated writes (stop request,
	// usage note).
	Actor = "rhizome:session-launcher"
	// Units names the policy Budget unit of this backend.
	Units = "usd_cents"
)

// BudgetOverride is the per-mission budget request; nil axis = ceiling.
// Tokens and MaxDepth are JANUS axes and are rejected here (no silent
// ignoring).
type BudgetOverride struct {
	Tokens, TimeMs, MaxDepth *int64
	USD                      *float64
}

// StartRequest is one mission.start after the relay resolved the mission
// (instruction already defaulted to the mission description).
type StartRequest struct {
	MissionID, Instruction, SessionMode string
	// Workdir names a ledger workdirs entry; "" = defaultWorkdir.
	Workdir string
	Budget  BudgetOverride
	// Actor is journaled in the intent provenance only, never keyed.
	Actor string
}

// StartPlan is the zero-write preview of a start.
type StartPlan struct {
	ExecutionID string
	Existing    string
}

// Launcher runs mission.start against the execution kernel by spawning the
// configured claude binary. Use it by pointer: it tracks its children.
type Launcher struct {
	Store events.Port
	Cfg   *Config
	// Report receives every finished session's outcome (the composition
	// root records it as a usage note). nil = not reported.
	Report func(Outcome)
	// OnError receives asynchronous faults of the completion path.
	OnError func(scope, id string, err error)
	// Environ is the parent environment the child env is filtered from.
	// nil = os.Environ.
	Environ func() []string
	// KillGrace is the SIGTERM → SIGKILL grace on timeout (0 = 5s).
	KillGrace time.Duration
	// RunningWait bounds how long completion waits for the relay to move the
	// mission to running before skipping the waiting_for_result step (0 = 5s).
	RunningWait time.Duration
	// BoardURL is the board base URL named in the session preamble (serve
	// passes its -addr). "" = DefaultBoardURL.
	BoardURL string

	startMu sync.Mutex // serializes Prepare/Start/Shutdown: one spawn per key
	closed  bool       // set by Shutdown under startMu: no new spawns
	// beforeAccept, when set (tests only), runs between spawn and accept.
	beforeAccept func()
	mu           sync.Mutex // guards running
	running      map[string]*child
	wg           sync.WaitGroup
}

type child struct {
	execID, missionID, sessionID string
	cmd                          *exec.Cmd
	pgid                         int
	stdoutPath                   string
	timeout                      time.Duration
	stop                         chan struct{} // closed by Shutdown
	stopOnce                     sync.Once
}

// DefaultBoardURL is serve's default address.
const DefaultBoardURL = "http://127.0.0.1:8080"

type spec struct {
	key, execID         string
	requested, ceiling  policy.Policy
	prov                execution.Provenance
	workdir             string
	cents, timeMs       int64
	mode, mission, text string
}

// capabilities maps allowedTools into policy capabilities. The execution
// kernel requires a non-empty capability set, so an empty tool list is the
// explicit "tool:none".
func (c Config) capabilities() []string {
	if len(c.AllowedTools) == 0 {
		return []string{"tool:none"}
	}
	out := make([]string, 0, len(c.AllowedTools))
	for _, t := range c.AllowedTools {
		out = append(out, "tool:"+t)
	}
	return out
}

// ceilingPolicy is the ledger ceiling as a policy: Budget in USD cents,
// Timeout in ms, MaxDepth 1 (a session never spawns a tracked child), no
// egress domains. Merged with itself so capabilities are in the kernel's
// normal form (sorted, unique) and Ceiling == Merge(Ceiling, Ceiling).
func (c Config) ceilingPolicy() policy.Policy {
	cents, _ := usdCents("ceiling.usd", c.Ceiling.USD)
	p := policy.Policy{Capabilities: c.capabilities(), Domains: []string{}, Budget: cents, Timeout: c.Ceiling.TimeMs, MaxDepth: 1, Units: Units}
	return policy.Merge(p, p).Policy
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// spec derives everything deterministic from the request and the ledger.
// Pure: no store access. A non-empty string is the rejection verbatim.
func (l *Launcher) spec(req StartRequest) (spec, string) {
	if l.Cfg == nil {
		return spec{}, "session launcher config not loaded"
	}
	c := l.Cfg
	if strings.TrimSpace(req.MissionID) == "" || strings.TrimSpace(req.Instruction) == "" {
		return spec{}, "missionId and instruction required"
	}
	if req.Budget.Tokens != nil {
		return spec{}, "budget.tokens not supported by session launcher"
	}
	if req.Budget.MaxDepth != nil {
		return spec{}, "budget.maxDepth not supported by session launcher"
	}
	mode := req.SessionMode
	if mode == "" {
		mode = "oneshot"
	}
	if mode != "oneshot" {
		return spec{}, "sessionMode must be oneshot (session launcher)"
	}
	wd := req.Workdir
	if wd == "" {
		wd = c.DefaultWorkdir
	}
	dir, ok := c.Workdirs[wd]
	if !ok {
		return spec{}, fmt.Sprintf("POLICY_DENIED: workdir %q not in ledger", wd)
	}
	ceiling := c.ceilingPolicy()
	requested := ceiling
	if req.Budget.USD != nil {
		v, err := usdCents("budget.usd", *req.Budget.USD)
		if err != nil {
			return spec{}, "BUDGET_INVALID: " + err.Error()
		}
		if v > ceiling.Budget {
			return spec{}, fmt.Sprintf("POLICY_DENIED: budget.usd %s exceeds ceiling %s", formatUSD(v), formatUSD(ceiling.Budget))
		}
		requested.Budget = v
	}
	if v := req.Budget.TimeMs; v != nil {
		if *v <= 0 {
			return spec{}, "BUDGET_INVALID: budget.timeMs must be > 0"
		}
		if *v > ceiling.Timeout {
			return spec{}, fmt.Sprintf("POLICY_DENIED: budget.timeMs %d exceeds ceiling %d", *v, ceiling.Timeout)
		}
		requested.Timeout = *v
	}
	merged := policy.Merge(requested, ceiling)
	if merged.Invalid || merged.Empty || merged.Policy.Units != Units {
		return spec{}, "POLICY_DENIED: effective policy empty"
	}
	eff := merged.Policy
	// The key is a function of the request content (re-submission finds the
	// same execution, never spawns twice). Target tags the key space so it
	// can never coincide with a JANUS-backend key.
	key := hashOf("mission.start", Target, req.MissionID, req.Instruction, wd, fmt.Sprint(eff.Budget), fmt.Sprint(eff.Timeout), mode)
	digest := c.Digest()
	prov := execution.Provenance{Ceiling: ceiling, Requested: requested, Effective: eff, ProfileID: Target, ProfileHash: strings.TrimPrefix(digest, "sha256:"), ExecConfigDigest: digest, Actor: req.Actor}
	return spec{key: key, execID: "exec-" + key, requested: requested, ceiling: ceiling, prov: prov, workdir: dir, cents: eff.Budget, timeMs: eff.Timeout, mode: mode, mission: req.MissionID, text: req.Instruction}, ""
}

func (l *Launcher) existing(execID, key string) (execution.Ref, bool, error) {
	if l.Store == nil {
		return execution.Ref{}, false, fmt.Errorf("nil event store")
	}
	log := l.Store.List("execution", execID)
	if len(log) == 0 {
		return execution.Ref{}, false, nil
	}
	r, err := execution.Replay(log)
	if err != nil {
		return execution.Ref{}, false, err
	}
	if r.IdempotencyKey != key {
		return execution.Ref{}, false, fmt.Errorf("execution %s carries a foreign idempotency key", execID)
	}
	return r, true, nil
}

func (l *Launcher) markerPath(execID string) string {
	return filepath.Join(l.Cfg.LogDir, execID+".pid")
}

// existingReason classifies an execution found under the request's key: ""
// = proceed (accepted/observing → lookup; intent → converge; claimed with no
// spawn marker → the earlier spawn definitely failed, retry it).
func (l *Launcher) existingReason(r execution.Ref) string {
	switch r.State {
	case execution.Intent, execution.Accepted, execution.Observing:
		return ""
	case execution.DispatchClaimed:
		if _, err := os.Lstat(l.markerPath(r.ID)); err == nil {
			return "session spawn outcome unknown: human resolution required (" + r.ID + ")"
		}
		return ""
	case execution.Unknown:
		return "execution unknown: human resolution required (" + r.ID + ")"
	}
	return "execution already ended: " + string(r.State) + " (" + r.ID + ")"
}

// Running is the number of live children of this launcher.
func (l *Launcher) Running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.running)
}

func (l *Launcher) busyReason() string {
	if n := l.Running(); n >= l.Cfg.MaxConcurrent {
		return fmt.Sprintf("session launcher busy: %d running (maxConcurrent %d)", n, l.Cfg.MaxConcurrent)
	}
	return ""
}

// Prepare validates the request and looks the key up. It never writes.
func (l *Launcher) Prepare(req StartRequest) (StartPlan, string, error) {
	sp, reason := l.spec(req)
	if reason != "" {
		return StartPlan{}, reason, nil
	}
	// Under startMu: a concurrent identical Start mid-spawn (claimed, marker
	// present, not yet accepted) is never misread as an unknown outcome.
	l.startMu.Lock()
	defer l.startMu.Unlock()
	if l.closed {
		return StartPlan{ExecutionID: sp.execID}, "session launcher shutting down", nil
	}
	r, ok, err := l.existing(sp.execID, sp.key)
	if err != nil {
		return StartPlan{}, "", err
	}
	plan := StartPlan{ExecutionID: sp.execID}
	if ok {
		plan.Existing = string(r.State)
		if reason := l.existingReason(r); reason != "" {
			return plan, reason, nil
		}
		if r.State == execution.Accepted || r.State == execution.Observing {
			return plan, "", nil
		}
	}
	if reason := l.busyReason(); reason != "" {
		return plan, reason, nil
	}
	return plan, "", nil
}

// Start appends execution.intent and dispatch_claimed (durable before the
// child exists), spawns the session and appends accepted with the session
// uuid. A spawn failure is a run-level refusal: the reason is returned, the
// execution stays claimed and the mission is not moved (the relay only
// transitions on success). err is reserved for store faults.
func (l *Launcher) Start(req StartRequest) (string, string, error) {
	sp, reason := l.spec(req)
	if reason != "" {
		return "", reason, nil
	}
	l.startMu.Lock()
	defer l.startMu.Unlock()
	if l.closed {
		return "", "session launcher shutting down", nil
	}
	es := execution.Service{Store: l.Store}
	r, ok, err := l.existing(sp.execID, sp.key)
	if err != nil {
		return "", "", err
	}
	if ok {
		if reason := l.existingReason(r); reason != "" {
			return r.ID, reason, nil
		}
		if r.State == execution.Accepted || r.State == execution.Observing {
			return r.ID, "", nil
		}
	}
	if reason := l.busyReason(); reason != "" {
		return sp.execID, reason, nil
	}
	if !ok {
		if r, err = es.IntentWithProvenance(sp.mission, sp.key, sp.requested, sp.ceiling, sp.prov); err != nil {
			return "", "", err
		}
	}
	id := r.ID
	if r.State == execution.Intent {
		if _, err = es.ClaimDispatch(id, Target, "mission.start:"+sp.mission); err != nil {
			return id, "", err
		}
	}
	sid, err := newUUID()
	if err != nil {
		return id, "session spawn failed: " + err.Error(), nil
	}
	ch, err := l.spawn(sp, sid)
	if err != nil {
		return id, "session spawn failed: " + err.Error(), nil
	}
	if l.beforeAccept != nil {
		l.beforeAccept()
	}
	if _, err = es.Accept(id, sid); err != nil {
		// The child must not run without a journaled acceptance. The spawn
		// marker stays, so a re-submission asks for human resolution.
		ch.signal(syscall.SIGKILL)
		_ = ch.cmd.Wait()
		return id, "", err
	}
	l.mu.Lock()
	if l.running == nil {
		l.running = map[string]*child{}
	}
	l.running[id] = ch
	l.mu.Unlock()
	l.wg.Add(1)
	go l.complete(ch)
	return id, "", nil
}

// Wait blocks until every completion goroutine started so far has finished
// (tests, orderly shutdown).
func (l *Launcher) Wait() { l.wg.Wait() }

// Shutdown refuses new starts and stops every live child (durable stop
// request, reason "user", then SIGTERM group → grace → SIGKILL group). Each
// completion still journals its outcome, moves the mission and reports
// usage; call Wait afterwards, before the journal closes.
func (l *Launcher) Shutdown() {
	l.startMu.Lock()
	l.closed = true
	l.startMu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ch := range l.running {
		ch.stopOnce.Do(func() { close(ch.stop) })
	}
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// preamble is the fixed instruction prefix of every launched session.
func preamble(board, missionID string) string {
	return "너는 Rhizome 미션 " + missionID + " 의 실행 세션이다. 다음을 지켜라.\n" +
		"1. 일을 시작하기 전에 보드 맥락을 먼저 읽어라: curl -s '" + board + "/v1/context?task=" + url.QueryEscape(missionID) + "'\n" +
		"2. 이 미션의 범위 안에서만 일하라.\n" +
		"3. 끝나면 보드에 완료 note를 남겨라 (POST " + board + "/v1/intent, kind note.create, missionId " + missionID + ").\n" +
		"4. git push 금지. 서비스 재시작 금지."
}

// Prompt is the full -p argument for a mission instruction; board is the
// board base URL ("" = DefaultBoardURL).
func Prompt(board, missionID, instruction string) string {
	if board == "" {
		board = DefaultBoardURL
	}
	return preamble(strings.TrimSuffix(board, "/"), missionID) + "\n\n" + instruction
}

// BoardURLFromAddr turns a serve -addr into the URL a local session uses: an
// unspecified or wildcard host becomes loopback.
func BoardURLFromAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return DefaultBoardURL
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Args is the exact claude argv (without argv[0]) for one session.
func (c Config) Args(prompt, sessionID string, cents int64) []string {
	a := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--session-id", sessionID, "--model", c.Model, "--permission-mode", c.PermissionMode}
	if len(c.AllowedTools) > 0 {
		a = append(a, "--allowedTools")
		a = append(a, c.AllowedTools...)
	}
	return append(a, "--max-budget-usd", formatUSD(cents))
}

// EnvAllowlist is the only parent environment that reaches a session.
var EnvAllowlist = []string{"HOME", "USER", "LOGNAME", "PATH", "LANG", "TMPDIR", "SHELL"}

// secretName reports names that must never reach a session, even when
// allowlisted: harness credentials and anything shaped like a secret.
func secretName(name string) bool {
	u := strings.ToUpper(name)
	if strings.HasPrefix(u, "ANTHROPIC_") || strings.HasPrefix(u, "CLAUDE_CODE_OAUTH") {
		return true
	}
	for _, suf := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD"} {
		if strings.HasSuffix(u, suf) {
			return true
		}
	}
	return false
}

// BuildEnv filters environ down to allow (minus secret-shaped names) and
// appends TERM=dumb.
func BuildEnv(environ, allow []string) []string {
	ok := map[string]bool{}
	for _, n := range allow {
		if !secretName(n) {
			ok[n] = true
		}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, kv := range environ {
		name, _, found := strings.Cut(kv, "=")
		if !found || !ok[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, kv)
	}
	return append(out, "TERM=dumb")
}

func (l *Launcher) spawn(sp spec, sid string) (*child, error) {
	c := l.Cfg
	marker := l.markerPath(sp.execID)
	mf, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spawn marker: %w", err)
	}
	defer mf.Close()
	fail := func(err error) (*child, error) {
		// The child never started: drop the marker so a re-submission may
		// retry the spawn.
		mf.Close()
		_ = os.Remove(marker)
		return nil, err
	}
	if _, err := mf.WriteString("starting\n"); err != nil {
		return fail(err)
	}
	outPath := filepath.Join(c.LogDir, sp.execID+".ndjson")
	stdout, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(c.LogDir, sp.execID+".stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	defer stderr.Close()
	environ := os.Environ
	if l.Environ != nil {
		environ = l.Environ
	}
	cmd := exec.Command(c.ClaudePath, c.Args(Prompt(l.BoardURL, sp.mission, sp.text), sid, sp.cents)...)
	cmd.Dir = sp.workdir
	cmd.Env = BuildEnv(environ(), EnvAllowlist)
	cmd.Stdin = nil // os/exec connects /dev/null
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	if err := mf.Truncate(0); err == nil {
		_, _ = mf.WriteAt([]byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0)
	}
	return &child{execID: sp.execID, missionID: sp.mission, sessionID: sid, cmd: cmd, pgid: cmd.Process.Pid, stdoutPath: outPath, timeout: time.Duration(sp.timeMs) * time.Millisecond, stop: make(chan struct{})}, nil
}

// signal delivers sig to the child's whole process group.
func (ch *child) signal(sig syscall.Signal) {
	_ = syscall.Kill(-ch.pgid, sig)
}
