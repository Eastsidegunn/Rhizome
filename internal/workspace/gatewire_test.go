package workspace

// RHZ-047 gate wiring tests (FR-RHZ-078): pending projection and the
// gate.approve/reject relay. The end-to-end loop round trip lives in
// internal/janusadapter (S1/R6); here the relay and projection are the units.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/approval"
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"rhizome/internal/gaterequest"
	"rhizome/internal/mission"
)

const gwTrace = "0123456789abcdef0123456789abcdef"
const gwSpan = "0123456789abcdef"
const gwDigest = "hx-args-digest-v1:golden"

func pendingGate(t *testing.T, s *events.Store, requestID, digest string) gaterequest.Ref {
	t.Helper()
	key := approval.RequestKey{TraceID: gwTrace, SpanID: gwSpan, RequestID: requestID}
	id := approval.IDFor(key)
	if _, err := (decision.Service{Store: s}).CreateWithTickCorrelation(decision.Decision{
		ID: "dec-" + id, MissionID: "m", Reason: "approval_request " + requestID, Kind: decision.WaitHuman,
		Evidence: []decision.Evidence{{SourceType: "execution", SourceID: "exec-x"}, {SourceType: "approvalrequest", SourceID: id}},
	}, "exec-x:"+gwTrace+":1", id); err != nil {
		t.Fatal(err)
	}
	gr, err := (gaterequest.Service{Store: s}).Record(gaterequest.Ref{Key: key, Name: "tool", Reason: "why",
		RequestDigest: digest, PolicyHash: "ph", DisplaySummary: "do X", ExpiresAt: 123,
		MissionID: "m", ExecutionID: "exec-x", DecisionID: "dec-" + id})
	if err != nil {
		t.Fatal(err)
	}
	return gr
}

func pendingGateFixture(t *testing.T) (*events.Store, gaterequest.Ref) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, e := ms.CreateGoal("g", "목표", "done", "p"); e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Create("m", "g", "미션", "done"); e != nil {
		t.Fatal(e)
	}
	return s, pendingGate(t, s, "r1", gwDigest)
}

func gateByID(t *testing.T, s events.Port, id string) (Gate, int) {
	t.Helper()
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	count, found := 0, Gate{}
	for _, g := range p.Gates {
		if g.ID == id {
			count++
			found = g
		}
	}
	return found, count
}

// Plan Q1: a surfaced request appears as a pending gate under the identity
// the eventual human input will use, carrying the display material.
func TestSnapshotPendingGateFromRequestFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	g, n := gateByID(t, s, gr.ID)
	if n != 1 || g.State != "pending" || g.HumanDecision != "" || g.MissionID != "m" || g.Name != "tool" {
		t.Fatalf("pending gate: %+v (n=%d)", g, n)
	}
	if g.RequestDigest != gwDigest || g.DisplaySummary != "do X" || g.ExpiresAt != 123 {
		t.Fatalf("display material: %+v", g)
	}
}

// Plan Q2: after the human input lands, exactly one gate remains under the
// same ID — the pending entry merges into the approval-based gate, keeping
// digest and display material.
func TestSnapshotGateIdentityContinuityFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: gr.ID, Digest: gwDigest}, "alice", false)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	g, n := gateByID(t, s, gr.ID)
	if n != 1 {
		t.Fatalf("duplicate gates for one identity: %d", n)
	}
	if g.HumanDecision != "allow" || g.State != "pending" || g.RequestDigest != gwDigest || g.DisplaySummary != "do X" || g.ExpiresAt != 123 {
		t.Fatalf("continuity: %+v", g)
	}
}

// Plan Q3: camelCase golden for the additive gate fields — present only where
// values exist, existing keys unchanged.
func TestSnapshotGateDTOSchemaFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	// A smoke-style input without any surfaced record: digest yes, display no.
	smokeKey := approval.RequestKey{TraceID: gwTrace, SpanID: gwSpan, RequestID: "r-smoke"}
	smoke, err := (approval.Service{Store: s}).RecordInputWithGate(smokeKey, approval.Allow, "", "resp-manual", "hx-args-digest-v1:smoke", "test-operator", "smoke", "", true, approval.GateFields{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	defer resp.Body.Close()
	var env struct {
		Body struct {
			Gates []map[string]json.RawMessage `json:"gates"`
		} `json:"body"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, g := range env.Body.Gates {
		var id string
		if json.Unmarshal(g["id"], &id) != nil {
			t.Fatal("gate without id")
		}
		byID[id] = g
	}
	pending := byID[gr.ID]
	for _, k := range []string{"id", "state", "superseded", "requestDigest", "displaySummary", "expiresAt", "missionId", "name"} {
		if _, ok := pending[k]; !ok {
			t.Fatalf("pending gate missing %q: %v", k, pending)
		}
	}
	sm := byID[smoke.ID]
	if _, ok := sm["requestDigest"]; !ok {
		t.Fatalf("input gate missing requestDigest: %v", sm)
	}
	for _, k := range []string{"displaySummary", "expiresAt"} {
		if _, ok := sm[k]; ok {
			t.Fatalf("absent value not omitted (%q): %v", k, sm)
		}
	}
}

// Plan R1: gate.approve records the human input against the observed request
// — key, digest, decision link and gate fields all from the pending record,
// response_id deterministic, actor marked unverified.
func TestGateApproveRecordsInputFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: gr.ID, Digest: gwDigest}, "alice", false)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	a, err := (approval.Service{Store: s}).Get(gr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != approval.InputRecorded || a.HumanDecision != approval.Allow || a.Key != gr.Key {
		t.Fatalf("input: %+v", a)
	}
	if a.RequestDigest != gwDigest || a.DecisionID != gr.DecisionID || a.ResponseID != approval.ResponseIDFor(gr.ID) {
		t.Fatalf("provenance: %+v", a)
	}
	if a.ActorRef != "unverified-local-operator:alice" || a.ActorVerified {
		t.Fatalf("actor marking: %+v", a)
	}
	if a.GateName != "tool" || a.GateType != "approval" {
		t.Fatalf("gate fields: %+v", a)
	}
}

// Plan R2: reject requires a reason and preserves it verbatim.
func TestGateRejectRequiresReasonVerbatimFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "gate.reject", GateID: gr.ID, Digest: gwDigest}, "alice", false)
	if err != nil || res.Accepted || res.Reason != "reason required" || len(s.All()) != before {
		t.Fatal(res, err)
	}
	res, err = RelayIntent(s, Intent{Kind: "gate.reject", GateID: gr.ID, Digest: gwDigest, Reason: "위험한 작업"}, "alice", false)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	a, err := (approval.Service{Store: s}).Get(gr.ID)
	if err != nil || a.HumanDecision != approval.Deny || a.Reason != "위험한 작업" {
		t.Fatal(a, err)
	}
}

// Plan R3: no observed request, no human decision (FR-RHZ-078) — and no
// journal write.
func TestGateApproveWithoutRequestRejectedFRRHZ078(t *testing.T) {
	s := &events.Store{}
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: "appr-nonexistent", Digest: gwDigest}, "alice", false)
	if err != nil || res.Accepted || res.Reason != "관측된 승인 요청 없음" || len(s.All()) != before {
		t.Fatal(res, err)
	}
}

// Plan R4: the digest the human saw is checked with VerifyDigest semantics —
// a mismatch, a missing digest, and the mutation-detecting case (equal
// strings without the contract prefix, which literal comparison would pass)
// are all refused without writes.
func TestGateApproveDigestMismatchRejectedFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	noPrefix := pendingGate(t, s, "r-noprefix", "raw-equal-digest")
	before := len(s.All())
	cases := []struct{ gate, digest, reason string }{
		{gr.ID, "hx-args-digest-v1:other", "digest mismatch"},
		{gr.ID, "", "digest required"},
		{noPrefix.ID, "raw-equal-digest", "digest mismatch"}, // equal but unprefixed: VerifyDigest refuses
	}
	for _, c := range cases {
		res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: c.gate, Digest: c.digest}, "alice", false)
		if err != nil || res.Accepted || res.Reason != c.reason {
			t.Fatalf("%+v -> %+v %v", c, res, err)
		}
	}
	if len(s.All()) != before {
		t.Fatalf("refused approve wrote: %d->%d", before, len(s.All()))
	}
}

// Plan R5: a gate that already has durable input refuses further input with
// the established wording.
func TestGateApproveAlreadyInputRejectedFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	if res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: gr.ID, Digest: gwDigest}, "alice", false); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: gr.ID, Digest: gwDigest}, "bob", false)
	if err != nil || res.Accepted || res.Reason != "gate already has input" || len(s.All()) != before {
		t.Fatal(res, err)
	}
}

// Plan R7: requestChanges works from a pending gate through its decision
// link, and keeps working after the input lands (approval branch).
func TestGateRequestChangesPendingFRRHZ078(t *testing.T) {
	s, gr := pendingGateFixture(t)
	countInstruct := func() int {
		n := 0
		for _, e := range s.All() {
			if e.AggregateType == "surface" && strings.Contains(string(e.Payload), "고쳐주세요") {
				n++
			}
		}
		return n
	}
	res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: gr.ID, Instruction: "고쳐주세요"}, "alice", false)
	if err != nil || !res.Accepted || countInstruct() != 1 {
		t.Fatal(res, err, countInstruct())
	}
	if res, err = RelayIntent(s, Intent{Kind: "gate.approve", GateID: gr.ID, Digest: gwDigest}, "alice", false); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	res, err = RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: gr.ID, Instruction: "고쳐주세요"}, "alice", false)
	if err != nil || !res.Accepted || countInstruct() != 2 {
		t.Fatal(res, err, countInstruct())
	}
}

// Plan R8: the derived response_id is deterministic per gate (idempotent
// resubmission, contract §4) and distinct across gates.
func TestGateApproveResponseIDDeterministicFRRHZ078(t *testing.T) {
	a, b := approval.ResponseIDFor("appr-x"), approval.ResponseIDFor("appr-x")
	if a != b || !strings.HasPrefix(a, "resp-") || len(a) != len("resp-")+24 {
		t.Fatal(a, b)
	}
	if approval.ResponseIDFor("appr-y") == a {
		t.Fatal("response id not distinct per gate")
	}
}
