package workspace

// RHZ-124 S2 (FR-RHZ-124-S2): RelayHooks.GateDecided — called once, after
// the internal gate decision is durable, only for launcher-correlated gates
// and only for approve/reject; never for requestChanges, other gates or a
// rejected decision. The hook cannot change the relay result. Plus the
// additive budgetUnits on /v1/execution limits (3a).

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/policy"
	"rhizome/internal/question"
)

func askCorr124(t *testing.T, s events.Port, name, corr string) (string, string) {
	t.Helper()
	res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: name, Body: "body " + name, MissionID: "mission-s2", CorrelationID: corr}, "rhizome:session-launcher", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("ask %s: %v %+v", name, err, res)
	}
	return mustQuestionID(name, "body "+name, ""), question.Digest(name, "body "+name, "")
}

// S2-5 (and the relay half of S2-6).
func TestGateDecidedHookFRRHZ124S2(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-s2", domain.MissionReady, domain.MissionRunning)
	var got []GateDecision
	var answeredAtCall int
	hooks := RelayHooks{GateDecided: func(d GateDecision) {
		got = append(got, d)
		// Decision durable before the hook runs.
		answeredAtCall = 0
		for _, e := range s.List("question", d.GateID) {
			if e.Type == "question.answered" {
				answeredAtCall++
			}
		}
	}}
	plain, plainDigest := askCorr124(t, s, "plain", "other:exec-1")
	none, noneDigest := askCorr124(t, s, "none", "")
	lg, lgDigest := askCorr124(t, s, "launcher gate", "launcher:exec-abc")
	lr, lrDigest := askCorr124(t, s, "launcher reject", "launcher:exec-def")
	for _, c := range []struct{ id, digest string }{{plain, plainDigest}, {none, noneDigest}} {
		if res, err := RelayIntentHooks(s, Intent{Kind: "gate.approve", GateID: c.id, Digest: c.digest}, "alice", noAuthority(), hooks); err != nil || !res.Accepted {
			t.Fatal(res, err)
		}
	}
	if res, err := RelayIntentHooks(s, Intent{Kind: "gate.requestChanges", GateID: lg, Digest: lgDigest, Reason: "shorter"}, "alice", noAuthority(), hooks); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	// A decision the kernel refuses (wrong digest) never reaches the hook.
	if res, _ := RelayIntentHooks(s, Intent{Kind: "gate.approve", GateID: lg, Digest: plainDigest}, "alice", noAuthority(), hooks); res.Accepted {
		t.Fatal("wrong digest accepted")
	}
	if len(got) != 0 {
		t.Fatalf("hook called for %+v", got)
	}
	if res, err := RelayIntentHooks(s, Intent{Kind: "gate.approve", GateID: lg, Digest: lgDigest, Reason: "go"}, "alice", noAuthority(), hooks); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if len(got) != 1 || got[0] != (GateDecision{GateID: lg, CorrelationID: "launcher:exec-abc", MissionID: "mission-s2", Decision: "approve", Reason: "go", Actor: "alice"}) || answeredAtCall != 2 {
		t.Fatalf("%+v answered=%d", got, answeredAtCall)
	}
	// Re-delivery of the same decision: the kernel refuses it, no second call.
	if res, _ := RelayIntentHooks(s, Intent{Kind: "gate.approve", GateID: lg, Digest: lgDigest}, "alice", noAuthority(), hooks); res.Accepted || len(got) != 1 {
		t.Fatalf("%+v %d", res, len(got))
	}
	if res, err := RelayIntentHooks(s, Intent{Kind: "gate.reject", GateID: lr, Digest: lrDigest, Reason: "STOP no"}, "bob", noAuthority(), hooks); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if len(got) != 2 || got[1].Decision != "reject" || got[1].Reason != "STOP no" || got[1].CorrelationID != "launcher:exec-def" {
		t.Fatalf("%+v", got)
	}
	// The HTTP surface wires ExecGateDecided into the relay.
	h := NewHTTP(s)
	called := 0
	h.ExecGateDecided = func(GateDecision) { called++ }
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	lh, lhDigest := askCorr124(t, s, "http gate", "launcher:exec-http")
	body, _ := json.Marshal(map[string]string{"kind": "gate.approve", "gateId": lh, "digest": lhDigest, "actor": "carol"})
	resp, err := srv.Client().Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if called != 1 {
		t.Fatalf("http hook calls %d", called)
	}
}

// 3a: a launcher execution (policy Units usd_cents) projects
// limits.budgetUnits; token-unit executions keep the FR-RHZ-124 shape.
func TestExecutionLimitsBudgetUnitsFRRHZ124S2(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "m", domain.MissionReady, domain.MissionRunning)
	ceil := policy.Policy{Capabilities: []string{"tool:Read"}, Domains: []string{}, Budget: 500, Timeout: 60000, MaxDepth: 1, Units: "usd_cents"}
	req := ceil
	req.Budget = 250
	eff := policy.Merge(req, ceil).Policy
	digest := "sha256:" + strings.Repeat("a", 64)
	if _, err := (execution.Service{Store: s}).IntentWithProvenance("m", "k-usd", req, ceil, execution.Provenance{Ceiling: ceil, Requested: req, Effective: eff, ProfileID: "claude-local", ProfileHash: strings.Repeat("a", 64), ExecConfigDigest: digest}); err != nil {
		t.Fatal(err)
	}
	es := execution.Service{Store: s}
	if _, err := es.ClaimDispatch("exec-k-usd", "claude-local", "mission.start:m"); err != nil {
		t.Fatal(err)
	}
	if _, err := es.Accept("exec-k-usd", "00000000-0000-4000-8000-000000000000"); err != nil {
		t.Fatal(err)
	}
	p, err := ExecutionSnapshot(s, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(toExecutionDTO(p).Sessions[0])
	if want := `"limits":{"ceiling":{"tokens":500,"timeMs":60000,"maxDepth":1},"effective":{"tokens":250,"timeMs":60000,"maxDepth":1},"budgetUnits":"usd_cents"}`; !strings.Contains(string(b), want) {
		t.Fatalf("%s", b)
	}
	if l := limitsOf(execution.Ref{Provenance: &execution.Provenance{Effective: policy.Policy{Units: "tokens-ms-v1"}}}); l.BudgetUnits != "" {
		t.Fatal(l)
	}
}
