package janusadapter

// RHZ-092 (FR-RHZ-123): mission.start starter — ledger validation (K1),
// happy path (S1), ceiling intersection (S3), idempotent lookup (S4),
// post-claim failure semantics (S6) and the loop's D7 (S8). Fake Runner
// only: no hx, no tokens.

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
)

const startTrace = "fedcba9876543210fedcba9876543210"

func execConfigJSON(mut func(map[string]any)) []byte {
	m := map[string]any{"adapterId": "claudecode", "adapterVersion": "1.0.0", "profileId": "manual", "profileHash": strings.Repeat("ab", 32), "workspaceRef": "/workspace", "scope": "example/test", "sessionMode": "multiturn",
		"ceiling": map[string]any{"tokens": 200000, "timeMs": 600000, "maxDepth": 2}}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func execConfigFixture(t *testing.T) *ExecConfig {
	t.Helper()
	c, err := ParseExecConfig(execConfigJSON(nil))
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

// echoRunner answers "accepted" with the request's own key and fingerprint
// (what JANUS echoes), recording every request it saw.
type echoRunner struct {
	calls int
	reqs  []map[string]any
	err   error
	out   string // when set, replaces the echoed acceptance
}

func (r *echoRunner) run(cfg RunConfig, req []byte) (io.Reader, error) {
	r.calls++
	var m map[string]any
	if err := json.Unmarshal(req, &m); err != nil {
		return nil, err
	}
	r.reqs = append(r.reqs, m)
	if r.err != nil {
		return nil, r.err
	}
	if r.out != "" {
		return strings.NewReader(r.out), nil
	}
	c := map[string]any{"version": 1, "status": "accepted", "session_ref": map[string]any{"session_db": "/tmp/start.db", "trace_id": startTrace}, "idempotency_key": m["idempotency_key"], "request_fingerprint": m["request_fingerprint"], "policy_hash": "ph", "acceptance_seq": 1, "launch_claimed": true}
	b, _ := json.Marshal(c)
	return strings.NewReader(string(b) + "\n"), nil
}

func startStore(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "write the thing", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func starter(s *events.Store, r *echoRunner) Starter {
	return Starter{Store: s, Run: r.run, Cfg: runConfigFixture(), Exec: func() *ExecConfig { c, _ := ParseExecConfig(execConfigJSON(nil)); return &c }()}
}

func i64(v int64) *int64 { return &v }

func execTypes(s *events.Store) []string {
	out := []string{}
	for _, e := range s.All() {
		if e.AggregateType == "execution" {
			out = append(out, e.Type)
		}
	}
	return out
}

// K1: ledger validation — required fields, enum, hex, ceiling > 0, unknown
// (secret-looking) keys and unmappable ceilings are all refused.
func TestExecConfigValidationFRRHZ123(t *testing.T) {
	c := execConfigFixture(t)
	if c.Ceiling.Tokens != 200000 || c.Ceiling.TimeMs != 600000 || c.Ceiling.MaxDepth != 2 || c.Ceiling.Capabilities[0] != "fs:workspace" {
		t.Fatalf("%+v", c)
	}
	bad := map[string]func(map[string]any){
		"missing adapterId":    func(m map[string]any) { delete(m, "adapterId") },
		"bad adapter":          func(m map[string]any) { m["adapterId"] = "gpt" },
		"bad hash":             func(m map[string]any) { m["profileHash"] = "abc" },
		"bad sessionMode":      func(m map[string]any) { m["sessionMode"] = "forever" },
		"missing scope":        func(m map[string]any) { m["scope"] = " " },
		"zero tokens":          func(m map[string]any) { m["ceiling"].(map[string]any)["tokens"] = 0 },
		"negative time":        func(m map[string]any) { m["ceiling"].(map[string]any)["timeMs"] = -1 },
		"missing depth":        func(m map[string]any) { delete(m["ceiling"].(map[string]any), "maxDepth") },
		"secret-looking key":   func(m map[string]any) { m["apiToken"] = "TEST-NON-CREDENTIAL" },
		"unknown ceiling key":  func(m map[string]any) { m["ceiling"].(map[string]any)["secret"] = "x" },
		"unmapped capability":  func(m map[string]any) { m["ceiling"].(map[string]any)["capabilities"] = []string{"root:all"} },
		"egress without domai": func(m map[string]any) { m["ceiling"].(map[string]any)["capabilities"] = []string{"net:egress"} },
	}
	for name, mut := range bad {
		if _, err := ParseExecConfig(execConfigJSON(mut)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseExecConfig([]byte("{} trailing")); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := LoadExecConfig(t.TempDir() + "/absent.json"); err == nil {
		t.Error("absent file accepted")
	}
}

// S1: happy path — exactly intent → dispatch_claimed → accepted, Runner
// once, request = description + ceiling budget + ledger defaults.
func TestStartHappyPathFRRHZ123(t *testing.T) {
	s := startStore(t)
	r := &echoRunner{}
	st := starter(s, r)
	req := StartRequest{MissionID: "m", Instruction: "write the thing"}
	plan, reason, err := st.Prepare(req)
	if err != nil || reason != "" || plan.Existing != "" || !strings.HasPrefix(plan.ExecutionID, "exec-") {
		t.Fatalf("%+v %q %v", plan, reason, err)
	}
	if len(execTypes(s)) != 0 {
		t.Fatal("Prepare wrote")
	}
	id, reason, err := st.Start(req)
	if err != nil || reason != "" || id != plan.ExecutionID {
		t.Fatalf("%s %q %v", id, reason, err)
	}
	if got := execTypes(s); strings.Join(got, ",") != "execution.intent,execution.dispatch_claimed,execution.accepted" {
		t.Fatalf("journal: %v", got)
	}
	if r.calls != 1 {
		t.Fatalf("runner calls %d", r.calls)
	}
	q := r.reqs[0]
	b := q["budget"].(map[string]any)
	if q["task_ref"].(map[string]any)["instruction"] != "write the thing" || b["tokens"].(float64) != 200000 || b["time_ms"].(float64) != 600000 || b["max_depth"].(float64) != 2 || q["session_mode"] != "multiturn" || q["adapter_id"] != "claudecode" || q["profile_id"] != "manual" || q["workspace_ref"] != "/workspace" || q["scope"] != "example/test" || q["mission_id"] != "m" || q["rhizome_execution_id"] != id {
		t.Fatalf("request: %v", q)
	}
	ref, err := execution.Replay(s.List("execution", id))
	if err != nil || ref.State != execution.Accepted || ref.ExternalID != startTrace || ref.Policy.Budget != 200000 || ref.Policy.Timeout != 600000 || ref.Policy.MaxDepth != 2 {
		t.Fatalf("%+v %v", ref, err)
	}
}

// S3: budget override within the ceiling → effective = min (intersection);
// over the ceiling → POLICY_DENIED, ≤0 → BUDGET_INVALID, both with zero
// writes and no Runner contact.
func TestStartBudgetCeilingFRRHZ123(t *testing.T) {
	s := startStore(t)
	r := &echoRunner{}
	st := starter(s, r)
	req := StartRequest{MissionID: "m", Instruction: "x", Budget: BudgetOverride{Tokens: i64(1000), TimeMs: i64(5000), MaxDepth: i64(1)}}
	id, reason, err := st.Start(req)
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	ref, _ := execution.Replay(s.List("execution", id))
	b := r.reqs[0]["budget"].(map[string]any)
	if ref.Policy.Budget != 1000 || ref.Policy.Timeout != 5000 || ref.Policy.MaxDepth != 1 || b["tokens"].(float64) != 1000 || b["time_ms"].(float64) != 5000 || b["max_depth"].(float64) != 1 {
		t.Fatalf("effective policy %+v budget %v", ref.Policy, b)
	}
	before := s.All()
	cases := map[string]struct {
		b    BudgetOverride
		want string
	}{
		"tokens over":   {BudgetOverride{Tokens: i64(200001)}, "POLICY_DENIED: budget.tokens"},
		"time over":     {BudgetOverride{TimeMs: i64(600001)}, "POLICY_DENIED: budget.timeMs"},
		"depth over":    {BudgetOverride{MaxDepth: i64(3)}, "POLICY_DENIED: budget.maxDepth"},
		"tokens zero":   {BudgetOverride{Tokens: i64(0)}, "BUDGET_INVALID: budget.tokens"},
		"time negative": {BudgetOverride{TimeMs: i64(-5)}, "BUDGET_INVALID: budget.timeMs"},
		"depth zero":    {BudgetOverride{MaxDepth: i64(0)}, "BUDGET_INVALID: budget.maxDepth"},
	}
	for name, tc := range cases {
		rq := StartRequest{MissionID: "m", Instruction: "y", Budget: tc.b}
		_, reason, err := st.Prepare(rq)
		if err != nil || !strings.HasPrefix(reason, tc.want) {
			t.Errorf("%s Prepare: %q %v", name, reason, err)
		}
		if _, reason, err = st.Start(rq); err != nil || !strings.HasPrefix(reason, tc.want) {
			t.Errorf("%s Start: %q %v", name, reason, err)
		}
	}
	unchanged(t, before, s)
	if r.calls != 1 {
		t.Fatalf("runner contacted on rejection: %d", r.calls)
	}
	if _, reason, _ := st.Prepare(StartRequest{MissionID: "m", Instruction: "y", SessionMode: "forever"}); reason != "sessionMode must be oneshot|multiturn" {
		t.Fatal(reason)
	}
	// Not loaded: rejected before any write.
	none := Starter{Store: s, Run: r.run, Cfg: runConfigFixture()}
	if _, reason, err := none.Prepare(req); err != nil || reason != "execution config not loaded" {
		t.Fatal(reason, err)
	}
	if _, reason, err := none.Start(req); err != nil || reason != "execution config not loaded" {
		t.Fatal(reason, err)
	}
	unchanged(t, before, s)
}

// S4: re-submission of the same request is a lookup — Prepare reports the
// running execution, Start converges without a second intent, the key is
// the same across override-equals-ceiling and no-override.
func TestStartIdempotentFRRHZ123(t *testing.T) {
	s := startStore(t)
	r := &echoRunner{}
	st := starter(s, r)
	req := StartRequest{MissionID: "m", Instruction: "x"}
	id, _, _ := st.Start(req)
	before := s.All()
	same := StartRequest{MissionID: "m", Instruction: "x", Budget: BudgetOverride{Tokens: i64(200000), TimeMs: i64(600000), MaxDepth: i64(2)}, SessionMode: "multiturn"}
	plan, reason, err := st.Prepare(same)
	if err != nil || reason != "" || plan.ExecutionID != id || plan.Existing != "accepted" {
		t.Fatalf("%+v %q %v", plan, reason, err)
	}
	id2, reason, err := st.Start(same)
	if err != nil || reason != "" || id2 != id {
		t.Fatalf("%s %q %v", id2, reason, err)
	}
	unchanged(t, before, s)
	if r.calls != 2 {
		t.Fatalf("resubmission must be a Runner lookup (calls %d)", r.calls)
	}
	// A different instruction is a different execution.
	if p, _, _ := st.Prepare(StartRequest{MissionID: "m", Instruction: "z"}); p.ExecutionID == id || p.Existing != "" {
		t.Fatalf("%+v", p)
	}
	// Terminal under the same key: refused, nothing written, no spawn.
	es := execution.Service{Store: s}
	if _, err := es.Bind(id, execution.Binding{SessionDB: "/tmp/start.db", TraceID: startTrace, PolicyHash: "ph", RequestFingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.Observe(id, "0000000000000000001", "done", "/tmp/start.db", false); err != nil {
		t.Fatal(err)
	}
	before = s.All()
	if _, reason, err := st.Start(req); err != nil || !strings.HasPrefix(reason, "execution already ended: succeeded") {
		t.Fatal(reason, err)
	}
	unchanged(t, before, s)
	if r.calls != 2 {
		t.Fatal("spawned after terminal")
	}
}

// S6: failures after the durable claim. Runner error → execution.unknown
// (existing type), re-submission refused as unknown, no second spawn.
// JANUS "rejected" leaves dispatch_claimed; re-submission re-runs only the
// Runner lookup — intent/claim are never duplicated. A crash between intent
// and claim converges by appending the claim.
func TestStartPostClaimFailureFRRHZ123(t *testing.T) {
	s := startStore(t)
	r := &echoRunner{err: errors.New("exec: hx not found")}
	st := starter(s, r)
	req := StartRequest{MissionID: "m", Instruction: "x"}
	id, reason, err := st.Start(req)
	if err != nil || reason == "" || id == "" {
		t.Fatalf("%s %q %v", id, reason, err)
	}
	if got := execTypes(s); strings.Join(got, ",") != "execution.intent,execution.dispatch_claimed,execution.unknown" {
		t.Fatalf("journal: %v", got)
	}
	before := s.All()
	r.err = nil
	if _, reason, err := st.Prepare(req); err != nil || !strings.HasPrefix(reason, "execution unknown") {
		t.Fatal(reason, err)
	}
	if _, reason, err := st.Start(req); err != nil || !strings.HasPrefix(reason, "execution unknown") {
		t.Fatal(reason, err)
	}
	unchanged(t, before, s)
	if r.calls != 1 {
		t.Fatalf("spawned again after unknown: %d", r.calls)
	}

	// JANUS refusal: dispatch_claimed stays, resubmission converges.
	s = startStore(t)
	r = &echoRunner{out: `{"version":1,"status":"rejected","error":{"code":"POLICY_DENIED","retryable":false,"message":"fs_scope"}}` + "\n"}
	st = starter(s, r)
	id, reason, err = st.Start(req)
	if err != nil || !strings.Contains(reason, "POLICY_DENIED") {
		t.Fatalf("%q %v", reason, err)
	}
	if got := execTypes(s); strings.Join(got, ",") != "execution.intent,execution.dispatch_claimed" {
		t.Fatalf("journal: %v", got)
	}
	r.out = ""
	id2, reason, err := st.Start(req)
	if err != nil || reason != "" || id2 != id || r.calls != 2 {
		t.Fatalf("%s %q %v calls=%d", id2, reason, err, r.calls)
	}
	if got := execTypes(s); strings.Join(got, ",") != "execution.intent,execution.dispatch_claimed,execution.accepted" {
		t.Fatalf("journal: %v", got)
	}

	// Crash between intent and claim: the claim is appended, no second intent.
	s = startStore(t)
	r = &echoRunner{}
	st = starter(s, r)
	sp, _, _ := st.spec(req)
	if _, err := (execution.Service{Store: s}).IntentWithPolicy("m", sp.key, sp.requested, sp.ceiling); err != nil {
		t.Fatal(err)
	}
	if p, reason, _ := st.Prepare(req); reason != "" || p.Existing != "intent" {
		t.Fatalf("%+v %q", p, reason)
	}
	if _, reason, err := st.Start(req); err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	if got := execTypes(s); strings.Join(got, ",") != "execution.intent,execution.dispatch_claimed,execution.accepted" {
		t.Fatalf("journal: %v", got)
	}
}

// S8 (D7): the loop only observes — with a claimed and an accepted
// (unbound) execution present, Tick starts nothing and writes nothing.
func TestLoopNeverStartsFRRHZ123(t *testing.T) {
	s := startStore(t)
	r := &echoRunner{out: `{"version":1,"status":"rejected","error":{"code":"POLICY_DENIED","retryable":false,"message":"no"}}` + "\n"}
	st := starter(s, r)
	if _, reason, _ := st.Start(StartRequest{MissionID: "m", Instruction: "claimed only"}); reason == "" {
		t.Fatal("expected refusal")
	}
	r.out = ""
	if _, reason, _ := st.Start(StartRequest{MissionID: "m", Instruction: "accepted"}); reason != "" {
		t.Fatal(reason)
	}
	calls := r.calls
	before := s.All()
	loop := &Loop{ES: execution.Service{Store: s}, AS: approval.Service{Store: s},
		Replay: func(cfg RunConfig, db, tr string) (io.Reader, error) {
			t.Fatal("replay of an unbound execution")
			return nil, nil
		}}
	for i := 0; i < 3; i++ {
		loop.Tick()
	}
	unchanged(t, before, s)
	if r.calls != calls {
		t.Fatalf("loop started an execution: calls %d → %d", calls, r.calls)
	}
}

// request.json: session_mode is additive — absent stays byte-identical to
// the v1 golden (FR-RHZ-076 fixture unchanged), present is serialized and
// validated.
func TestRunRequestSessionModeFRRHZ123(t *testing.T) {
	_, r := runStore(t)
	base, err := BuildRunRequest(r, runParamsFixture(), runConfigFixture())
	if err != nil || strings.Contains(string(base), "session_mode") {
		t.Fatalf("%v %s", err, base)
	}
	p := runParamsFixture()
	p.SessionMode = "multiturn"
	b, err := BuildRunRequest(r, p, runConfigFixture())
	if err != nil || !strings.HasSuffix(string(b), `,"session_mode":"multiturn"}`) {
		t.Fatalf("%v %s", err, b)
	}
	p.SessionMode = "always"
	if _, err := BuildRunRequest(r, p, runConfigFixture()); err == nil {
		t.Fatal("invalid session_mode accepted")
	}
}

// RHZ-124 S1 (FR-RHZ-124-S1) L2: session-launcher fields reach the
// JANUS backend only to be refused — zero writes, no Runner contact.
func TestStarterRejectsLauncherFieldsFRRHZ124S1(t *testing.T) {
	s := startStore(t)
	r := &echoRunner{}
	st := starter(s, r)
	usd := 1.5
	before := len(s.All())
	for name, req := range map[string]StartRequest{
		"usd":     {MissionID: "m", Instruction: "write the thing", Budget: BudgetOverride{USD: &usd}},
		"workdir": {MissionID: "m", Instruction: "write the thing", Workdir: "rhizome"},
	} {
		if _, reason, err := st.Prepare(req); err != nil || !strings.Contains(reason, "not supported by janus backend") {
			t.Fatalf("%s prepare: %q %v", name, reason, err)
		}
		if _, reason, err := st.Start(req); err != nil || !strings.Contains(reason, "not supported by janus backend") {
			t.Fatalf("%s start: %q %v", name, reason, err)
		}
	}
	if len(s.All()) != before || r.calls != 0 {
		t.Fatalf("writes=%d calls=%d", len(s.All())-before, r.calls)
	}
}
