package workspace

// RHZ-096 (FR-RHZ-124): /v1/execution sessions[] gain additive `limits`
// derived from the intent's journaled provenance; legacy executions are
// byte-identical to v1. The relay threads its (unverified-prefixed) actor
// to the starter for the provenance record.

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/policy"
)

func provenanceExec(t *testing.T, s *events.Store, missionID, key string) execution.Ref {
	t.Helper()
	es := execution.Service{Store: s}
	ceil := policy.Policy{Capabilities: []string{"fs:workspace"}, Budget: 200000, Timeout: 600000, MaxDepth: 2, Units: "tokens-ms-v1"}
	req := ceil
	req.Budget, req.MaxDepth = 1000, 1
	prov := execution.Provenance{Ceiling: ceil, Requested: req, Effective: policy.Merge(req, ceil).Policy, ProfileID: "manual", ProfileHash: strings.Repeat("ab", 32), ExecConfigDigest: "sha256:" + strings.Repeat("0f", 32), Actor: "op"}
	r, err := es.IntentWithProvenance(missionID, key, req, ceil, prov)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr"); err != nil {
		t.Fatal(err)
	}
	if r, err = es.Accept(r.ID, execTrace); err != nil {
		t.Fatal(err)
	}
	return r
}

// P6: limits projected for a provenance execution (numbers only — no
// profile/hash/digest/actor on the wire), absent for a legacy execution
// whose session bytes stay exactly v1.
func TestExecutionLimitsProjectionFRRHZ124(t *testing.T) {
	s, _, srv := execHTTPFixture(t)
	acceptedExec(t, s, "m", "a-legacy")
	provenanceExec(t, s, "m", "b-prov")
	code, b := getExecution(t, srv.URL, "m")
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var env struct {
		Body struct {
			Sessions []json.RawMessage `json:"sessions"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &env); err != nil || len(env.Body.Sessions) != 2 {
		t.Fatalf("%s %v", b, err)
	}
	if got, want := string(env.Body.Sessions[0]), `{"id":"exec-a-legacy","taskId":"m","state":"running"}`; got != want {
		t.Errorf("legacy session changed:\n got %s\nwant %s", got, want)
	}
	if got, want := string(env.Body.Sessions[1]), `{"id":"exec-b-prov","taskId":"m","state":"running","limits":{"ceiling":{"tokens":200000,"timeMs":600000,"maxDepth":2},"effective":{"tokens":1000,"timeMs":600000,"maxDepth":1}}}`; got != want {
		t.Errorf("provenance session:\n got %s\nwant %s", got, want)
	}
	for _, leak := range []string{"profile", "sha256", "digest", "actor", strings.Repeat("ab", 32)} {
		if strings.Contains(string(b), leak) {
			t.Errorf("projection leaked %q: %s", leak, b)
		}
	}
}

// The relay hands the starter its actor, unverified-prefixed (provenance
// actor). The actor is not part of the request identity.
func TestStartRelayActorFRRHZ124(t *testing.T) {
	s := runningMission(t)
	f := &fakeStarter{s: s}
	if r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f); !r.Accepted {
		t.Fatalf("%+v", r)
	}
	if len(f.started) != 1 || f.started[0].Actor != "unverified-local-operator:op" || f.prepared[0].Actor != "unverified-local-operator:op" {
		t.Fatalf("%+v", f.started)
	}
}
