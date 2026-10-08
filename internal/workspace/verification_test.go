package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/question"
)

func rawVerification(s string) json.RawMessage { return json.RawMessage(s) }

func journalImage105(t *testing.T, s events.Port) []byte {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRelayVerificationRuleTableZeroWriteFRRHZ130(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("verify", "body", "recommend", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	validRelayed := `{"claimKind":"relayed","originClaim":"H","originChannel":"board","relayChain":["ops"]}`
	cases := []struct {
		name     string
		kind     string
		verified bool
		raw      string
	}{
		{"V1 claimKind missing", "gate.approve", false, `{"originClaim":"H","relayChain":["ops"]}`},
		{"V1 claimKind unknown", "gate.approve", false, `{"claimKind":"signed","originClaim":"H","relayChain":["ops"]}`},
		{"V2 originClaim missing", "gate.approve", false, `{"claimKind":"relayed","relayChain":["ops"]}`},
		{"V2 originClaim blank", "gate.approve", false, `{"claimKind":"relayed","originClaim":"","relayChain":["ops"]}`},
		{"V2 originClaim not H", "gate.approve", false, `{"claimKind":"relayed","originClaim":"human","relayChain":["ops"]}`},
		{"V3 relayChain empty", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":[]}`},
		{"V3 relayChain last mismatch", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":["other"]}`},
		{"V4 sessionRef missing", "gate.approve", false, `{"claimKind":"session-direct","originClaim":"H"}`},
		{"V4 sessionRef blank", "gate.approve", false, `{"claimKind":"session-direct","originClaim":"H","sessionRef":"  "}`},
		{"V5 originChannel unknown", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","originChannel":"chat","relayChain":["ops"]}`},
		{"V5 relayed dev channel", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","originChannel":"dev-session","relayChain":["ops"]}`},
		{"V5 direct ops channel", "gate.approve", false, `{"claimKind":"session-direct","originClaim":"H","originChannel":"ops-session","sessionRef":"session:x"}`},
		{"V6 observedAt malformed", "gate.approve", false, `{"claimKind":"session-direct","originClaim":"H","sessionRef":"session:x","observedAt":"2026-10-08"}`},
		{"V7 status", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":["ops"],"status":"verified"}`},
		{"V7 assurance", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":["ops"],"assurance":"key"}`},
		{"V7 verified", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":["ops"],"verified":true}`},
		{"V7 principal", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":["ops"],"principal":"H"}`},
		{"V7 wrong casing", "gate.approve", false, `{"ClaimKind":"relayed","originClaim":"H","relayChain":["ops"]}`},
		{"V7 direct cross relayChain", "gate.approve", false, `{"claimKind":"session-direct","originClaim":"H","sessionRef":"session:x","relayChain":[]}`},
		{"V7 relayed cross sessionRef", "gate.approve", false, `{"claimKind":"relayed","originClaim":"H","relayChain":["ops"],"sessionRef":"session:x"}`},
		{"V8 disallowed intent kind", "question.ask", false, validRelayed},
		{"V8 bare verified conflict", "gate.approve", true, validRelayed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := journalImage105(t, s)
			in := Intent{Kind: tc.kind, GateID: q.ID, Digest: q.Digest, Verification: rawVerification(tc.raw)}
			res, err := RelayIntent(s, in, "ops", tc.verified)
			if err != nil || res.Accepted || res.Reason != "invalid verification" {
				t.Fatalf("accepted or echoing/noncanonical rejection: %+v err=%v", res, err)
			}
			if after := journalImage105(t, s); !bytes.Equal(before, after) {
				t.Fatalf("V9 journal changed\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestRelayVerificationRelayedAndSessionDirectRecordedFRRHZ130(t *testing.T) {
	s := &events.Store{}
	claims := []struct {
		name, actor, raw string
	}{
		{"relayed", "ops", `{"claimKind":"relayed","originClaim":"H","originChannel":"ops-session","relayChain":["first","ops"],"observedAt":"2026-10-08T03:12:00Z"}`},
		{"session-direct", "dev", `{"claimKind":"session-direct","originClaim":"H","originChannel":"dev-session","sessionRef":"session:example"}`},
	}
	for _, tc := range claims {
		q, err := (question.Service{Store: s}).Ask(tc.name, "body", "recommend", "", "", "asker", "")
		if err != nil {
			t.Fatal(err)
		}
		res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: q.ID, Digest: q.Digest, Verification: rawVerification(tc.raw)}, tc.actor, false)
		if err != nil || !res.Accepted {
			t.Fatalf("%s rejected: %+v %v", tc.name, res, err)
		}
		got, err := (question.Service{Store: s}).Get(q.ID)
		if err != nil || got.Verification == nil || got.Verification.ClaimKind != tc.name {
			t.Fatalf("%s not recorded: %+v %v", tc.name, got.Verification, err)
		}
		gate, n := gateByID(t, s, q.ID)
		if n != 1 || gate.Verification == nil || gate.Verification.Status != "claimed" || gate.Verification.ClaimKind != tc.name {
			t.Fatalf("%s not projected: %+v", tc.name, gate)
		}
	}
}

func TestGateVerificationDerivationTableFRRHZ131(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	// Internal pending: omitted.
	pending, _ := (question.Service{Store: s}).Ask("pending", "body", "r", "", "", "asker", "")
	if g, _ := gateByID(t, s, pending.ID); g.Verification != nil {
		t.Fatalf("pending internal gate has verification: %+v", g)
	}
	// Internal unclaimed answer: omitted.
	unclaimed, _ := (question.Service{Store: s}).Ask("unclaimed", "body", "r", "", "", "asker", "")
	if _, err := (question.Service{Store: s}).Answer(unclaimed.ID, question.Approve, "", "actor", unclaimed.Digest); err != nil {
		t.Fatal(err)
	}
	if g, _ := gateByID(t, s, unclaimed.ID); g.Verification != nil {
		t.Fatalf("unclaimed answer has verification: %+v", g)
	}
	// The latest answer wins, including clearing an earlier changes_requested claim.
	latest, _ := (question.Service{Store: s}).Ask("latest", "body", "r", "", "", "asker", "")
	claim := &question.Verification{ClaimKind: "session-direct", OriginClaim: "H", SessionRef: "session:x"}
	if _, err := (question.Service{Store: s}).Answer(latest.ID, question.RequestChanges, "fix", "dev", latest.Digest, claim); err != nil {
		t.Fatal(err)
	}
	if g, _ := gateByID(t, s, latest.ID); g.State != "changes_requested" || g.Verification == nil || g.Verification.Status != "claimed" {
		t.Fatalf("changes_requested claim missing: %+v", g)
	}
	if _, err := (question.Service{Store: s}).Answer(latest.ID, question.Approve, "", "dev", latest.Digest); err != nil {
		t.Fatal(err)
	}
	if g, _ := gateByID(t, s, latest.ID); g.Verification != nil {
		t.Fatalf("old claim carried into latest answer: %+v", g)
	}
	// JANUS request only: omitted; input claim: claimed even while state is pending.
	gr := pendingGate(t, s, "verification-pending", gwDigest)
	if g, _ := gateByID(t, s, gr.ID); g.Verification != nil {
		t.Fatalf("request-only JANUS gate has verification: %+v", g)
	}
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: gr.ID, Digest: gwDigest, Verification: rawVerification(`{"claimKind":"relayed","originClaim":"H","originChannel":"board","relayChain":["ops"]}`)}, "ops", false)
	if err != nil || !res.Accepted {
		t.Fatalf("JANUS claim rejected: %+v %v", res, err)
	}
	if g, _ := gateByID(t, s, gr.ID); g.State != "pending" || g.Verification == nil || g.Verification.Status != "claimed" || g.Verification.ClaimKind != "relayed" {
		t.Fatalf("JANUS claim projection: %+v", g)
	}
	// P1 must never serialize verified or assurance.
	wire, err := json.Marshal(toDTO(mustSnapshot105(t, s)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"status":"verified"`)) || bytes.Contains(wire, []byte(`"assurance"`)) {
		t.Fatalf("P1 serialized verified/assurance: %s", wire)
	}
	again, err := json.Marshal(toDTO(mustSnapshot105(t, s)))
	if err != nil || !bytes.Equal(wire, again) {
		t.Fatalf("verification projection nondeterministic\nfirst=%s\nagain=%s err=%v", wire, again, err)
	}
}

func mustSnapshot105(t *testing.T, s events.Port) Projection {
	t.Helper()
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContextGateVerificationClaimedFRRHZ131(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-verification", procedure.Step{ID: "a", Action: "act-a", NeedsGate: true})
	if res, err := RelayIntent(s, Intent{Kind: "procedure.run", ID: "proc-verification", Name: "verification", GoalID: "goal-dev"}, "op", false); err != nil || !res.Accepted {
		t.Fatalf("procedure.run: %+v %v", res, err)
	}
	qID := question.IDFor("verification · a 게이트", "act-a", "")
	q, err := (question.Service{Store: s}).Get(qID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: q.ID, Digest: q.Digest, Reason: "fix", Verification: rawVerification(`{"claimKind":"session-direct","originClaim":"H","sessionRef":"session:ctx"}`)}, "dev", false)
	if err != nil || !res.Accepted {
		t.Fatalf("requestChanges: %+v %v", res, err)
	}
	code, body := getContext(t, NewHTTP(s).Handler(), "?task=mission-verification")
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"verification":{"status":"claimed","claimKind":"session-direct"}`)) {
		t.Fatalf("context verification missing: code=%d body=%s", code, body)
	}
}

func TestWorkspaceGoldenLiteralUnchangedWithoutVerificationFRRHZ131(t *testing.T) {
	s := goldenJournalWithoutVerificationFRRHZ131(t)
	h := NewHTTP(s).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	if got, want := rec.Body.Bytes(), []byte(goldenWorkspaceWithoutVerificationFRRHZ131); !bytes.Equal(got, want) {
		t.Fatalf("workspace pre-change golden differs\n got: %s\nwant: %s", got, want)
	}

	for _, tc := range []struct {
		name, task, want string
	}{
		{"gated procedure", "mission-bc", goldenGatedContextWithoutVerificationFRRHZ131},
		{"plain task", "mission-x", goldenPlainContextWithoutVerificationFRRHZ131},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := getContext(t, h, "?task="+tc.task)
			if code != http.StatusOK || !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("context pre-change golden differs code=%d\n got: %s\nwant: %s", code, got, tc.want)
			}
		})
	}

	if got, want := firstWorkspaceSnapshotFrameFRRHZ131(t, h), []byte(goldenWorkspaceStreamWithoutVerificationFRRHZ131); !bytes.Equal(got, want) {
		t.Fatalf("SSE pre-change golden differs\n got: %s\nwant: %s", got, want)
	}
}

func goldenJournalWithoutVerificationFRRHZ131(t *testing.T) *events.Store {
	t.Helper()
	source := fixture068(t)
	if _, err := (mission.Service{Store: source}).Create("m", "goal-dev", "mission m", "done"); err != nil {
		t.Fatal(err)
	}
	defineProc(t, source, "proc-bc",
		procedure.Step{ID: "a", Action: "act-a", NeedsGate: true},
		procedure.Step{ID: "b", Action: "act-b", After: []string{"a"}, NeedsGate: true},
	)
	if res, err := RelayIntent(source, Intent{Kind: "procedure.run", ID: "proc-bc", Name: "bc", GoalID: "goal-dev"}, "op", false); err != nil || !res.Accepted {
		t.Fatalf("procedure.run: %+v %v", res, err)
	}
	qa, err := (question.Service{Store: source}).Get(question.IDFor("bc · a 게이트", "act-a", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := RelayIntent(source, Intent{Kind: "gate.requestChanges", GateID: qa.ID, Digest: qa.Digest, Reason: "fix it"}, "dev", false); err != nil || !res.Accepted {
		t.Fatalf("request changes: %+v %v", res, err)
	}
	approved, err := (question.Service{Store: source}).Ask("approve-me", "b", "r", "mission-x", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := RelayIntent(source, Intent{Kind: "gate.approve", GateID: approved.ID, Digest: approved.Digest}, "ops", false); err != nil || !res.Accepted {
		t.Fatalf("approve: %+v %v", res, err)
	}
	rejected, err := (question.Service{Store: source}).Ask("reject-me", "b", "r", "", "goal-dev", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := RelayIntent(source, Intent{Kind: "gate.reject", GateID: rejected.ID, Digest: rejected.Digest, Reason: "no"}, "ops", false); err != nil || !res.Accepted {
		t.Fatalf("reject: %+v %v", res, err)
	}
	if _, err := (question.Service{Store: source}).Ask("pending", "b", "r", "", "", "asker", ""); err != nil {
		t.Fatal(err)
	}
	janusApproved := pendingGate(t, source, "bc-1", gwDigest)
	if res, err := RelayIntent(source, Intent{Kind: "gate.approve", GateID: janusApproved.ID, Digest: gwDigest}, "ops", false); err != nil || !res.Accepted {
		t.Fatalf("JANUS approve: %+v %v", res, err)
	}
	pendingGate(t, source, "bc-2", gwDigest)
	key := approval.RequestKey{TraceID: "abcdef0123456789abcdef0123456789", SpanID: "abcdef0123456789", RequestID: "bc-3"}
	if _, err := (approval.Service{Store: source}).RecordInput(key, approval.Deny, "nah", "resp", "digest", "operator", "", "", false); err != nil {
		t.Fatal(err)
	}

	// Domain helpers use wall time. Re-append their semantic journal with fixed
	// event and embedded request timestamps so the fixture is deterministic.
	stable := &events.Store{}
	base := time.Date(2026, 10, 8, 3, 35, 32, 0, time.UTC)
	for i, event := range source.All() {
		event.ID = ""
		event.CreatedAt = base.Add(time.Duration(i) * time.Second)
		if event.Type == "approval.input_recorded" {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if request, ok := payload["Request"].(map[string]any); ok && request["RequestedAt"] != "" {
				request["RequestedAt"] = base.Format(time.RFC3339)
			}
			event.Payload, _ = json.Marshal(payload)
		}
		if err := stable.Append(stable.Revision(event.AggregateType, event.AggregateID), event); err != nil {
			t.Fatal(err)
		}
	}
	return stable
}

func firstWorkspaceSnapshotFrameFRRHZ131(t *testing.T, h http.Handler) []byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(&cancelOnFlushRecorder{ResponseRecorder: rec, cancel: cancel}, req)
	return append([]byte(nil), rec.Body.Bytes()...)
}

type cancelOnFlushRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (r *cancelOnFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.cancel()
}

// These literals were generated once by running the equivalent deterministic
// journal against the pre-RHZ-105 tree and copied byte-for-byte from
// copied byte-for-byte from the pre-RHZ-105 output. They must not be regenerated from current DTOs.
const goldenWorkspaceWithoutVerificationFRRHZ131 = `{"revision":26,"body":{"missions":[{"id":"goal-dev","name":"dev goal","attention":false,"state":"active","success":"done","handle":"g-ea3f48f9"}],"tasks":[{"id":"m","missionId":"goal-dev","name":"mission m","state":"queued","hasProgress":false,"attention":false,"handle":"m-62c66a7a"},{"id":"mission-bc","missionId":"goal-dev","name":"bc","state":"queued","hasProgress":false,"attention":false,"handle":"m-411a80f9"},{"id":"mission-bc-a","missionId":"goal-dev","name":"act-a","state":"queued","hasProgress":false,"attention":false,"handle":"m-8f43e5dd"},{"id":"mission-bc-b","missionId":"goal-dev","name":"act-b","state":"queued","hasProgress":false,"attention":false,"handle":"m-a474674b"},{"id":"mission-x","missionId":"goal-dev","name":"plain mission","state":"queued","hasProgress":false,"attention":false,"handle":"m-cf10b7f0"}],"gates":[{"id":"appr-6c79cadb475ba4cb15622b7a","state":"pending","superseded":false,"missionId":"m","name":"tool","requestDigest":"hx-args-digest-v1:golden","displaySummary":"do X","expiresAt":123,"source":"janus","handle":"a-48b40ecc"},{"id":"appr-9b714a4cfbb59e517f9aed12","state":"pending","humanDecision":"deny","superseded":false,"requestDigest":"digest","source":"janus","handle":"a-c43a07ca"},{"id":"appr-9b7f71c10f9807599ef820cb","state":"pending","humanDecision":"allow","superseded":false,"missionId":"m","name":"tool","requestDigest":"hx-args-digest-v1:golden","displaySummary":"do X","expiresAt":123,"source":"janus","handle":"a-fe7a4d3c"},{"id":"q-4a120422c1ef102c065ad2c6","state":"pending","superseded":false,"name":"pending","requestDigest":"rhz-question-v1:4a120422c1ef102c065ad2c6451df2ce5354278d2240fe8431b1a95baeed7291","source":"internal","body":"b","recommendation":"r","handle":"q-c8bf3fb8"},{"id":"q-7066210b847ad93732441581","state":"approved","superseded":false,"missionId":"mission-x","name":"approve-me","requestDigest":"rhz-question-v1:7066210b847ad937324415816a6bf346409f274196900ddddf815f511431b6d0","source":"internal","body":"b","recommendation":"r","decidedBy":"unverified-local-operator:ops","handle":"q-5d9cd54b"},{"id":"q-71204592b6ebe79215ec9c36","state":"rejected","superseded":false,"goalId":"goal-dev","name":"reject-me","requestDigest":"rhz-question-v1:71204592b6ebe79215ec9c36475989c751449a75042aea5fd5e7b17b32a0c747","source":"internal","body":"b","recommendation":"r","decisionReason":"no","decidedBy":"unverified-local-operator:ops","handle":"q-33811317"},{"id":"q-a7b41f2471516c3f6538d091","state":"changes_requested","superseded":false,"missionId":"mission-bc-a","name":"bc · a 게이트","requestDigest":"rhz-question-v1:a7b41f2471516c3f6538d091f0405528fa029133ba777545972b21a2fedd1f1d","source":"internal","body":"act-a","decisionReason":"fix it","decidedBy":"unverified-local-operator:dev","handle":"q-c2fb4dd1"},{"id":"q-a82b7cd8e5328a8f86e855b9","state":"pending","superseded":false,"missionId":"mission-bc-b","name":"bc · b 게이트","requestDigest":"rhz-question-v1:a82b7cd8e5328a8f86e855b913b38ff539e56786247187fc61ba143ba53dbe71","source":"internal","body":"act-b","handle":"q-fdb1934f"}],"deliverables":[],"edges":[{"id":"edge-dep-bc-b-a","from":{"id":"mission-bc-b","type":"mission"},"to":{"id":"mission-bc-a","type":"mission"},"edgeKind":"dependency","actor":"unverified-local-operator:op","correlation":"procedure.run:bc"},{"id":"edge-spawn-bc-a","from":{"id":"mission-bc","type":"mission"},"to":{"id":"mission-bc-a","type":"mission"},"edgeKind":"spawn","actor":"unverified-local-operator:op","correlation":"procedure.run:bc"},{"id":"edge-spawn-bc-b","from":{"id":"mission-bc","type":"mission"},"to":{"id":"mission-bc-b","type":"mission"},"edgeKind":"spawn","actor":"unverified-local-operator:op","correlation":"procedure.run:bc"}],"counts":{"running":0,"needsYou":2,"blocked":0},"attention":[],"capabilities":{"m":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-bc":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-bc-a":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-bc-b":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-x":{"pause":"disabled","resume":"enabled","instruct":"enabled"}},"gateCapabilities":{"q-4a120422c1ef102c065ad2c6":{"approve":"enabled","reject":"enabled","requestChanges":"hidden"},"q-7066210b847ad93732441581":{"approve":"hidden","reject":"hidden","requestChanges":"hidden"},"q-71204592b6ebe79215ec9c36":{"approve":"hidden","reject":"hidden","requestChanges":"hidden"},"q-a7b41f2471516c3f6538d091":{"approve":"enabled","reject":"enabled","requestChanges":"hidden"},"q-a82b7cd8e5328a8f86e855b9":{"approve":"enabled","reject":"enabled","requestChanges":"enabled"}}}}
`

const goldenGatedContextWithoutVerificationFRRHZ131 = `{"task":{"id":"mission-bc","name":"bc","state":"queued","hasProgress":false},"goal":{"id":"goal-dev","description":"dev goal","state":"active"},"memories":[],"knowledge":[],"relations":[],"steps":[{"id":"mission-bc-a","name":"act-a","state":"queued","after":[],"gates":[{"id":"q-a7b41f2471516c3f6538d091","name":"bc · a 게이트","state":"changes_requested","decisionReason":"fix it","decidedBy":"unverified-local-operator:dev"}]},{"id":"mission-bc-b","name":"act-b","state":"queued","after":["mission-bc-a"],"gates":[{"id":"q-a82b7cd8e5328a8f86e855b9","name":"bc · b 게이트","state":"pending"}]}]}
`

const goldenPlainContextWithoutVerificationFRRHZ131 = `{"task":{"id":"mission-x","name":"plain mission","state":"queued","hasProgress":false},"goal":{"id":"goal-dev","description":"dev goal","state":"active"},"memories":[],"knowledge":[],"relations":[],"steps":[]}
`

const goldenWorkspaceStreamWithoutVerificationFRRHZ131 = `event: snapshot
data: {"revision":26,"body":{"missions":[{"id":"goal-dev","name":"dev goal","attention":false,"state":"active","success":"done","handle":"g-ea3f48f9"}],"tasks":[{"id":"m","missionId":"goal-dev","name":"mission m","state":"queued","hasProgress":false,"attention":false,"handle":"m-62c66a7a"},{"id":"mission-bc","missionId":"goal-dev","name":"bc","state":"queued","hasProgress":false,"attention":false,"handle":"m-411a80f9"},{"id":"mission-bc-a","missionId":"goal-dev","name":"act-a","state":"queued","hasProgress":false,"attention":false,"handle":"m-8f43e5dd"},{"id":"mission-bc-b","missionId":"goal-dev","name":"act-b","state":"queued","hasProgress":false,"attention":false,"handle":"m-a474674b"},{"id":"mission-x","missionId":"goal-dev","name":"plain mission","state":"queued","hasProgress":false,"attention":false,"handle":"m-cf10b7f0"}],"gates":[{"id":"appr-6c79cadb475ba4cb15622b7a","state":"pending","superseded":false,"missionId":"m","name":"tool","requestDigest":"hx-args-digest-v1:golden","displaySummary":"do X","expiresAt":123,"source":"janus","handle":"a-48b40ecc"},{"id":"appr-9b714a4cfbb59e517f9aed12","state":"pending","humanDecision":"deny","superseded":false,"requestDigest":"digest","source":"janus","handle":"a-c43a07ca"},{"id":"appr-9b7f71c10f9807599ef820cb","state":"pending","humanDecision":"allow","superseded":false,"missionId":"m","name":"tool","requestDigest":"hx-args-digest-v1:golden","displaySummary":"do X","expiresAt":123,"source":"janus","handle":"a-fe7a4d3c"},{"id":"q-4a120422c1ef102c065ad2c6","state":"pending","superseded":false,"name":"pending","requestDigest":"rhz-question-v1:4a120422c1ef102c065ad2c6451df2ce5354278d2240fe8431b1a95baeed7291","source":"internal","body":"b","recommendation":"r","handle":"q-c8bf3fb8"},{"id":"q-7066210b847ad93732441581","state":"approved","superseded":false,"missionId":"mission-x","name":"approve-me","requestDigest":"rhz-question-v1:7066210b847ad937324415816a6bf346409f274196900ddddf815f511431b6d0","source":"internal","body":"b","recommendation":"r","decidedBy":"unverified-local-operator:ops","handle":"q-5d9cd54b"},{"id":"q-71204592b6ebe79215ec9c36","state":"rejected","superseded":false,"goalId":"goal-dev","name":"reject-me","requestDigest":"rhz-question-v1:71204592b6ebe79215ec9c36475989c751449a75042aea5fd5e7b17b32a0c747","source":"internal","body":"b","recommendation":"r","decisionReason":"no","decidedBy":"unverified-local-operator:ops","handle":"q-33811317"},{"id":"q-a7b41f2471516c3f6538d091","state":"changes_requested","superseded":false,"missionId":"mission-bc-a","name":"bc · a 게이트","requestDigest":"rhz-question-v1:a7b41f2471516c3f6538d091f0405528fa029133ba777545972b21a2fedd1f1d","source":"internal","body":"act-a","decisionReason":"fix it","decidedBy":"unverified-local-operator:dev","handle":"q-c2fb4dd1"},{"id":"q-a82b7cd8e5328a8f86e855b9","state":"pending","superseded":false,"missionId":"mission-bc-b","name":"bc · b 게이트","requestDigest":"rhz-question-v1:a82b7cd8e5328a8f86e855b913b38ff539e56786247187fc61ba143ba53dbe71","source":"internal","body":"act-b","handle":"q-fdb1934f"}],"deliverables":[],"edges":[{"id":"edge-dep-bc-b-a","from":{"id":"mission-bc-b","type":"mission"},"to":{"id":"mission-bc-a","type":"mission"},"edgeKind":"dependency","actor":"unverified-local-operator:op","correlation":"procedure.run:bc"},{"id":"edge-spawn-bc-a","from":{"id":"mission-bc","type":"mission"},"to":{"id":"mission-bc-a","type":"mission"},"edgeKind":"spawn","actor":"unverified-local-operator:op","correlation":"procedure.run:bc"},{"id":"edge-spawn-bc-b","from":{"id":"mission-bc","type":"mission"},"to":{"id":"mission-bc-b","type":"mission"},"edgeKind":"spawn","actor":"unverified-local-operator:op","correlation":"procedure.run:bc"}],"counts":{"running":0,"needsYou":2,"blocked":0},"attention":[],"capabilities":{"m":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-bc":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-bc-a":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-bc-b":{"pause":"disabled","resume":"enabled","instruct":"enabled"},"mission-x":{"pause":"disabled","resume":"enabled","instruct":"enabled"}},"gateCapabilities":{"q-4a120422c1ef102c065ad2c6":{"approve":"enabled","reject":"enabled","requestChanges":"hidden"},"q-7066210b847ad93732441581":{"approve":"hidden","reject":"hidden","requestChanges":"hidden"},"q-71204592b6ebe79215ec9c36":{"approve":"hidden","reject":"hidden","requestChanges":"hidden"},"q-a7b41f2471516c3f6538d091":{"approve":"enabled","reject":"enabled","requestChanges":"hidden"},"q-a82b7cd8e5328a8f86e855b9":{"approve":"enabled","reject":"enabled","requestChanges":"enabled"}}}}

`

func TestBareActorVerifiedProjectsLegacyAssertedFRRHZ132(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("legacy-g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("legacy-m", "legacy-g", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	_, contextBefore := getContext(t, NewHTTP(s).Handler(), "?task=legacy-m")
	k := approval.RequestKey{TraceID: "abcdef0123456789abcdef0123456789", SpanID: "abcdef0123456789", RequestID: "legacy"}
	r, err := (approval.Service{Store: s}).RecordInput(k, approval.Allow, "", "resp", "digest", "operator", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	g, n := gateByID(t, s, r.ID)
	if n != 1 || g.State != "pending" || g.DecidedBy != "" || g.Verification == nil || g.Verification.Status != "legacy-asserted" || g.Verification.ClaimKind != "" {
		t.Fatalf("legacy projection changed state/actor or status: %+v", g)
	}
	_, contextAfter := getContext(t, NewHTTP(s).Handler(), "?task=legacy-m")
	if !bytes.Equal(contextBefore, contextAfter) {
		t.Fatalf("JANUS legacy input changed context\nbefore=%s\nafter=%s", contextBefore, contextAfter)
	}

	// The public HTTP verified bit remains ignored and cannot produce this status.
	internal, _ := (question.Service{Store: s}).Ask("http", "body", "r", "", "", "asker", "")
	body := `{"kind":"gate.approve","gateId":"` + internal.ID + `","digest":"` + internal.Digest + `","actor":"http-actor","verified":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	response, _ := io.ReadAll(rec.Result().Body)
	if rec.Code != http.StatusOK || !bytes.Contains(response, []byte(`"Accepted":true`)) {
		t.Fatalf("HTTP decision failed: code=%d body=%s", rec.Code, response)
	}
	if q, err := (question.Service{Store: s}).Get(internal.ID); err != nil || q.ActorRef != "unverified-local-operator:http-actor" || q.Verification != nil {
		t.Fatalf("HTTP verified bit affected provenance: %+v %v", q, err)
	}
}
