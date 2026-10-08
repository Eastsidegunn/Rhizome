package janusadapter

// RHZ-096 (FR-RHZ-124): policy provenance on the execution.intent payload
// (additive field, no new event type). Fake Runner only: no hx, no tokens.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

var secretKeyRE = regexp.MustCompile(`(?i)token|secret|password|passwd|apikey|api_key|credential|bearer|path|filename`)

func walkKeys(t *testing.T, v any, at string) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if secretKeyRE.MatchString(k) {
				t.Errorf("secret/path-like key %q at %s", k, at)
			}
			walkKeys(t, c, at+"."+k)
		}
	case []any:
		for _, c := range x {
			walkKeys(t, c, at+"[]")
		}
	}
}

// P1: mission.start always journals the full provenance — ceiling from the
// ledger, requested = ceiling + overrides, effective, profile id/hash, the
// ledger digest and the actor — with no path or secret-looking key.
func TestStartIntentProvenanceFRRHZ124(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exec-ledger.json")
	if err := os.WriteFile(path, execConfigJSON(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadExecConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s := startStore(t)
	r := &echoRunner{}
	st := Starter{Store: s, Run: r.run, Cfg: runConfigFixture(), Exec: &cfg}
	id, reason, err := st.Start(StartRequest{MissionID: "m", Instruction: "write the thing", Budget: BudgetOverride{TimeMs: i64(5000)}, Actor: "unverified-local-operator:op"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	log := s.List("execution", id)
	if log[0].Type != "execution.intent" {
		t.Fatal(log[0].Type)
	}
	raw := log[0].Payload
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	pv, ok := m["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("provenance absent on mission.start intent: %s", raw)
	}
	for _, k := range []string{"ceiling", "requested", "effective", "profileId", "profileHash", "execConfigDigest", "actor"} {
		if _, ok := pv[k]; !ok {
			t.Errorf("provenance.%s missing", k)
		}
	}
	ref, err := execution.Replay(log)
	if err != nil || ref.Provenance == nil {
		t.Fatalf("%+v %v", ref, err)
	}
	p := ref.Provenance
	if p.Ceiling.Budget != 200000 || p.Ceiling.Timeout != 600000 || p.Ceiling.MaxDepth != 2 || p.Ceiling.Capabilities[0] != "fs:workspace" || p.Ceiling.Units != "tokens-ms-v1" {
		t.Errorf("ceiling %+v", p.Ceiling)
	}
	if p.Requested.Budget != 200000 || p.Requested.Timeout != 5000 || p.Requested.MaxDepth != 2 {
		t.Errorf("requested %+v", p.Requested)
	}
	if p.Effective.Budget != 200000 || p.Effective.Timeout != 5000 || p.Effective.MaxDepth != 2 || p.Effective.Timeout != ref.Policy.Timeout {
		t.Errorf("effective %+v policy %+v", p.Effective, ref.Policy)
	}
	if p.ProfileID != "manual" || p.ProfileHash != strings.Repeat("ab", 32) || p.ExecConfigDigest != cfg.Digest() || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(p.ExecConfigDigest) || p.Actor != "unverified-local-operator:op" {
		t.Errorf("identity %+v", p)
	}
	// Digests and numbers only: no path at all (ledger location, run config
	// paths, workspaceRef) and no secret/path-like key anywhere.
	if bytes.Contains(raw, []byte("/")) || bytes.Contains(raw, []byte(dir)) || bytes.Contains(raw, []byte("exec-ledger")) {
		t.Errorf("path leaked into the journal: %s", raw)
	}
	walkKeys(t, m, "payload")
	// Only the intent carries provenance.
	for _, e := range log[1:] {
		if bytes.Contains(e.Payload, []byte("provenance")) {
			t.Errorf("%s carries provenance", e.Type)
		}
	}
}

// P3: effective = policy.Merge(ceiling, requested) — budget minimum for an
// override below the ceiling, ceiling axis where no override was given.
func TestStartEffectiveIsMergeFRRHZ124(t *testing.T) {
	s := startStore(t)
	st := starter(s, &echoRunner{})
	id, reason, err := st.Start(StartRequest{MissionID: "m", Instruction: "x", Budget: BudgetOverride{Tokens: i64(1234), MaxDepth: i64(1)}, Actor: "op"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	ref, _ := execution.Replay(s.List("execution", id))
	p := ref.Provenance
	want := policy.Merge(p.Ceiling, p.Requested)
	wb, _ := json.Marshal(want.Policy)
	eb, _ := json.Marshal(p.Effective)
	sb, _ := json.Marshal(*ref.Policy)
	if want.Invalid || want.Empty || string(wb) != string(eb) || string(eb) != string(sb) {
		t.Fatalf("effective %s merge %s snapshot %s", eb, wb, sb)
	}
	if p.Effective.Budget != 1234 || p.Ceiling.Budget != 200000 || p.Requested.Budget != 1234 || p.Effective.MaxDepth != 1 || p.Effective.Timeout != 600000 {
		t.Fatalf("%+v", p)
	}
}

// P4: the ledger digest is over the canonical validated ledger — a ceiling
// value change moves it, a whitespace/key-order reformat of the file does not.
func TestExecConfigDigestCanonicalFRRHZ124(t *testing.T) {
	dir := t.TempDir()
	load := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadExecConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		return c.Digest()
	}
	base := execConfigJSON(nil)
	d0 := load("a.json", base)
	var ind bytes.Buffer
	if err := json.Indent(&ind, base, "  ", "\t  "); err != nil {
		t.Fatal(err)
	}
	reordered := []byte(`{"ceiling":{"maxDepth":2,"timeMs":600000,"tokens":200000},"sessionMode":"multiturn","scope":"example/test","workspaceRef":"/workspace","profileHash":"` + strings.Repeat("ab", 32) + `","profileId":"manual","adapterVersion":"1.0.0","adapterId":"claudecode"}` + "\n\n")
	if d := load("b.json", ind.Bytes()); d != d0 {
		t.Errorf("whitespace reformat changed digest %s != %s", d, d0)
	}
	if d := load("c.json", reordered); d != d0 {
		t.Errorf("key reorder changed digest %s != %s", d, d0)
	}
	d1 := load("d.json", execConfigJSON(func(m map[string]any) { m["ceiling"].(map[string]any)["tokens"] = 199999 }))
	if d1 == d0 {
		t.Error("ceiling change kept digest")
	}
	if d := load("e.json", execConfigJSON(func(m map[string]any) { m["profileHash"] = strings.Repeat("cd", 32) })); d == d0 || d == d1 {
		t.Error("profileHash change kept digest")
	}
	var hand ExecConfig
	_ = json.Unmarshal(base, &hand)
	hand.Ceiling.Capabilities = append([]string(nil), DefaultExecCapabilities...) // the default validate applies
	if hand.Digest() != d0 {
		t.Error("on-demand digest differs from load-time digest")
	}
}

// P5: provenance never feeds the idempotency key — the execution id for the
// same request is the RHZ-092 value (pinned constants computed at RHZ-092)
// regardless of actor or ledger digest.
func TestStartKeyUnchangedFRRHZ124(t *testing.T) {
	const wantPlain = "exec-9e1329e2e8c823b2d34e7046299de0e7e12c953ebccfbe5a77e5a7373e48557f"
	const wantOverride = "exec-e9c59da2419766f6572ce9a48f209a15943d0b66cf145db718f7d51623a7c054"
	for _, actor := range []string{"", "op", "unverified-local-operator:someone-else"} {
		st := starter(startStore(t), &echoRunner{})
		plan, reason, err := st.Prepare(StartRequest{MissionID: "m", Instruction: "write the thing", Actor: actor})
		if err != nil || reason != "" || plan.ExecutionID != wantPlain {
			t.Errorf("actor %q plain: %s %q %v", actor, plan.ExecutionID, reason, err)
		}
		id, reason, err := st.Start(StartRequest{MissionID: "m", Instruction: "write the thing", Budget: BudgetOverride{Tokens: i64(1000), TimeMs: i64(5000), MaxDepth: i64(1)}, Actor: actor})
		if err != nil || reason != "" || id != wantOverride {
			t.Errorf("actor %q override: %s %q %v", actor, id, reason, err)
		}
	}
}

// R1: the provenance survives the NDJSON journal (close + reopen) intact.
func TestProvenanceJournalRoundTripFRRHZ124(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.ndjson")
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "write the thing", "done"); err != nil {
		t.Fatal(err)
	}
	st := Starter{Store: j, Run: (&echoRunner{}).run, Cfg: runConfigFixture(), Exec: execConfigFixture(t)}
	id, reason, err := st.Start(StartRequest{MissionID: "m", Instruction: "write the thing", Budget: BudgetOverride{Tokens: i64(77)}, Actor: "op"})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	before, err := execution.Replay(j.List("execution", id))
	if err != nil || before.Provenance == nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	after, err := execution.Replay(j2.List("execution", id))
	if err != nil || after.Provenance == nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before.Provenance)
	b, _ := json.Marshal(after.Provenance)
	if string(a) != string(b) || after.Provenance.Effective.Budget != 77 || after.Provenance.ExecConfigDigest != execConfigFixture(t).Digest() {
		t.Fatalf("round trip %s != %s", a, b)
	}
}
