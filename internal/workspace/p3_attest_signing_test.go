package workspace

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"rhizome/internal/approval"
	"rhizome/internal/attest"
	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/gaterequest"
	"rhizome/internal/mission"
	"rhizome/internal/question"
	"rhizome/internal/trust"
	"strings"
	"testing"
)

func attestSignature164(t *testing.T, store events.View, verifier *trust.Verifier, private ed25519.PrivateKey, items []attest.Item, nonce string) trust.Signature {
	t.Helper()
	keyID, _ := trust.KeyID(private.Public())
	signedAt := signedAt149()
	message := trust.AttestMessage(domain149(t, verifier, store), trust.ManifestDigest(items), fmt.Sprint(len(items)), keyID, signedAt, nonce)
	return trust.Signature{KeyID: keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message))}
}

func unsignedQuestion164(t *testing.T, store events.Port, title string, decision question.Decision, reason string) (question.Ref, attest.Item) {
	t.Helper()
	q, err := (question.Service{Store: store}).Ask(title, "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	q, err = (question.Service{Store: store}).Answer(q.ID, decision, reason, "operator", q.Digest)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := storedDecision(store.List("question", q.ID), trust.QuestionAnsweredType)
	return q, attest.Item{Consumer: "question", GateID: q.ID, DecisionSequence: event.Sequence, Digest: q.Digest, Decision: string(q.Decision), Reason: q.Reason}
}

func TestRequestChangesEffectiveReasonSignedFRRHZ165(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	ms := mission.Service{Store: store}
	if _, err := ms.CreateGoal("goal-165", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-165", "goal-165", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	makeGate := func(title string) question.Ref {
		q, err := (question.Service{Store: store}).Ask(title, "body", "", "mission-165", "", "agent", "")
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	effective := "fix the output"
	q := makeGate("effective")
	verification := decisionVerification149(t, private, domain149(t, verifier, store), "question", q.ID, q.Digest, "requestChanges", effective, signedAt149(), "15151515151515151515151515151515")
	res, err := RelayIntent(store, Intent{Kind: "gate.requestChanges", GateID: q.ID, Digest: q.Digest, Instruction: effective, Verification: intentVerification149(t, verification)}, "signer", trust.Authority{})
	if err != nil || !res.Accepted {
		t.Fatalf("effective signature: %+v %v", res, err)
	}
	replayed, _ := (question.Service{Store: store}).Get(q.ID)
	if replayed.Reason != effective {
		t.Fatalf("stored reason = %q", replayed.Reason)
	}

	q = makeGate("empty")
	bad := decisionVerification149(t, private, domain149(t, verifier, store), "question", q.ID, q.Digest, "requestChanges", "", signedAt149(), "16161616161616161616161616161616")
	before := len(store.All())
	res, err = RelayIntent(store, Intent{Kind: "gate.requestChanges", GateID: q.ID, Digest: q.Digest, Instruction: effective, Verification: intentVerification149(t, bad)}, "signer", trust.Authority{})
	if err != nil || res.Accepted || !strings.Contains(res.Reason, "signature verification failed") || len(store.All()) != before {
		t.Fatalf("empty signature: %+v %v writes=%d", res, err, len(store.All())-before)
	}
}

func TestAttestedProjectionWorkspaceContextAndRevocationFRRHZ166(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	ms := mission.Service{Store: store}
	if _, err := ms.CreateGoal("goal-166", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-parent-166", "goal-166", "parent", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-step-166", "goal-166", "step", "done"); err != nil {
		t.Fatal(err)
	}
	// Minimal spawn edge payload through the kernel service is provided by the
	// existing procedure fixtures; here a direct edge declaration keeps this
	// projection test focused on the shared attest index.
	if _, err := (edge.Service{Store: store}).Create(edge.Spec{ID: "edge-spawn-166", From: edge.Endpoint{Type: "mission", ID: "mission-parent-166"}, To: edge.Endpoint{Type: "mission", ID: "mission-step-166"}, Kind: edge.Spawn, Actor: "operator", Correlation: "test"}, trust.Authority{}); err != nil {
		t.Fatal(err)
	}
	q, err := (question.Service{Store: store}).Ask("attested gate", "body", "", "mission-step-166", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	claim := &question.Verification{ClaimKind: "relayed", OriginClaim: "H", RelayChain: []string{"operator"}}
	q, err = (question.Service{Store: store}).Answer(q.ID, question.Approve, "", "operator", q.Digest, claim)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := storedDecision(store.List("question", q.ID), trust.QuestionAnsweredType)
	items := []attest.Item{{Consumer: "question", GateID: q.ID, DecisionSequence: event.Sequence, Digest: q.Digest, Decision: "approve", Reason: ""}}
	if err := (attest.Service{Store: store}).Create(trust.ManifestDigest(items), items, attestSignature164(t, store, verifier, private, items, "17171717171717171717171717171717")); err != nil {
		t.Fatal(err)
	}
	projection, err := Snapshot(store, verifier)
	if err != nil {
		t.Fatal(err)
	}
	var workspaceVerification *GateVerification
	for i := range projection.Gates {
		if projection.Gates[i].ID == q.ID {
			workspaceVerification = projection.Gates[i].Verification
			if projection.Gates[i].DecidedBy != "unverified-local-operator:operator" {
				t.Fatalf("attest changed actor: %q", projection.Gates[i].DecidedBy)
			}
		}
	}
	if workspaceVerification == nil || workspaceVerification.Status != "attested" || workspaceVerification.Assurance != "key" {
		t.Fatalf("workspace verification = %+v", workspaceVerification)
	}
	h := NewHTTP(store)
	h.Trust = verifier
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/context?task=mission-parent-166", nil)
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"status":"attested"`)) {
		t.Fatalf("context %d: %s", rec.Code, rec.Body.String())
	}
	keyID, _ := trust.KeyID(private.Public())
	if err := appendRevoke149(t, store, verifier, private, keyID, "rotated", signedAt149(), "18181818181818181818181818181818"); err != nil {
		t.Fatal(err)
	}
	projection, err = Snapshot(store, verifier)
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range projection.Gates {
		if gate.ID == q.ID && (gate.Verification == nil || !gate.Verification.KeyRevokedNow) {
			t.Fatalf("revoked projection = %+v", gate.Verification)
		}
	}

	copyStore := &events.Store{}
	for _, copied := range store.All() {
		if err := copyStore.AppendRevision(copied); err != nil {
			t.Fatal(err)
		}
	}
	anchorless, err := Snapshot(copyStore, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range anchorless.Gates {
		if gate.ID == q.ID && (gate.Verification == nil || gate.Verification.Status != "claimed") {
			t.Fatalf("anchorless attest changed original status: %+v", gate.Verification)
		}
	}
}

func TestSigningEndpointGoldensFRRHZ168(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	pending, err := (question.Service{Store: store}).Ask("pending title", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	approved, approvedItem := unsignedQuestion164(t, store, "approved title", question.Approve, "")
	changes, err := (question.Service{Store: store}).Ask("changes title", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	changes, err = (question.Service{Store: store}).Answer(changes.ID, question.RequestChanges, "revise", "operator", changes.Digest)
	if err != nil {
		t.Fatal(err)
	}
	key := approval.RequestKey{TraceID: "3123456789abcdef0123456789abcdef", SpanID: "3123456789abcdef", RequestID: "signing"}
	request, err := (gaterequest.Service{Store: store}).Record(gaterequest.Ref{Key: key, Name: "fallback", DisplaySummary: "Approval summary", RequestDigest: "hx-args-digest-v1:signing", MissionID: "m", ExecutionID: "e", DecisionID: "d"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := (approval.Service{Store: store}).RecordInput(key, approval.Allow, "approved", "response", request.RequestDigest, "operator", "", "", trust.Authority{})
	if err != nil {
		t.Fatal(err)
	}
	pendingApprovalKey := approval.RequestKey{TraceID: key.TraceID, SpanID: key.SpanID, RequestID: "pending-signing"}
	pendingApproval, err := (gaterequest.Service{Store: store}).Record(gaterequest.Ref{Key: pendingApprovalKey, Name: "Pending fallback", RequestDigest: "hx-args-digest-v1:pending", MissionID: "m", ExecutionID: "e", DecisionID: "d"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTP(store)
	h.Trust = verifier
	beforeReads := len(store.All())
	call := func(path, remote string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remote
		h.Handler().ServeHTTP(rec, req)
		return rec
	}
	journalID := domain149(t, verifier, store)
	for _, tc := range []struct{ id, want string }{
		{pending.ID, fmt.Sprintf(`{"journalId":%q,"consumer":"question","gateId":%q,"title":"pending title","requestDigest":%q,"state":"pending","verificationStatus":"none"}`+"\n", journalID, pending.ID, pending.Digest)},
		{approved.ID, fmt.Sprintf(`{"journalId":%q,"consumer":"question","gateId":%q,"title":"approved title","requestDigest":%q,"state":"approved","decisionSequence":%d,"decision":"approve","reason":"","verificationStatus":"none"}`+"\n", journalID, approved.ID, approved.Digest, approvedItem.DecisionSequence)},
		{changes.ID, fmt.Sprintf(`{"journalId":%q,"consumer":"question","gateId":%q,"title":"changes title","requestDigest":%q,"state":"changes_requested","decisionSequence":%d,"decision":"requestChanges","reason":"revise","verificationStatus":"none"}`+"\n", journalID, changes.ID, changes.Digest, store.List("question", changes.ID)[1].Sequence)},
		{input.ID, fmt.Sprintf(`{"journalId":%q,"consumer":"approval","gateId":%q,"title":"Approval summary","requestDigest":"hx-args-digest-v1:signing","state":"input_recorded","decisionSequence":%d,"decision":"allow","reason":"approved","verificationStatus":"none"}`+"\n", journalID, input.ID, store.List("approval", input.ID)[0].Sequence)},
		{pendingApproval.ID, fmt.Sprintf(`{"journalId":%q,"consumer":"approval","gateId":%q,"title":"Pending fallback","requestDigest":"hx-args-digest-v1:pending","state":"pending","verificationStatus":"none"}`+"\n", journalID, pendingApproval.ID)},
	} {
		rec := call("/v1/trust/signing?gate="+tc.id, "127.0.0.2:99")
		if rec.Code != 200 || rec.Body.String() != tc.want || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("gate %s status=%d body=%q want=%q", tc.id, rec.Code, rec.Body.String(), tc.want)
		}
	}
	for _, path := range []string{"/v1/trust/signing", "/v1/trust/signing?gate=", "/v1/trust/signing?gate=a&gate=b"} {
		if rec := call(path, "[::1]:9"); rec.Code != 400 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	if rec := call("/v1/trust/signing?gate=q-missing", "127.0.0.1:9"); rec.Code != 404 || rec.Body.String() != "gate not found\n" {
		t.Fatalf("404 = %d %q", rec.Code, rec.Body.String())
	}
	if rec := call("/v1/trust/signing?gate="+pending.ID, "10.0.0.1:9"); rec.Code != 403 || rec.Body.String() != "forbidden\n" {
		t.Fatalf("403 = %d %q", rec.Code, rec.Body.String())
	}

	anchorless := NewHTTP(store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/trust/signing?gate="+pending.ID, nil)
	req.RemoteAddr = "127.0.0.1:9"
	anchorless.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || bytes.Contains(rec.Body.Bytes(), []byte(`"journalId"`)) {
		t.Fatalf("anchorless = %d %s", rec.Code, rec.Body.String())
	}
	if len(store.All()) != beforeReads {
		t.Fatalf("signing reads wrote %d events", len(store.All())-beforeReads)
	}

	items := []attest.Item{approvedItem}
	if err := (attest.Service{Store: store}).Create(trust.ManifestDigest(items), items, attestSignature164(t, store, verifier, private, items, "19191919191919191919191919191919")); err != nil {
		t.Fatal(err)
	}
	rejected, rejectedItem := unsignedQuestion164(t, store, "rejected title", question.Reject, "no")
	rec = call("/v1/trust/signing?unverified=1", "127.0.0.1:9")
	wantList := fmt.Sprintf(`{"journalId":%q,"items":[{"consumer":"approval","gateId":%q,"decisionSequence":%d,"requestDigest":"hx-args-digest-v1:signing","decision":"allow","reason":"approved","title":"Approval summary"},{"consumer":"question","gateId":%q,"decisionSequence":%d,"requestDigest":%q,"decision":"reject","reason":"no","title":"rejected title"}]}`+"\n", journalID, input.ID, store.List("approval", input.ID)[0].Sequence, rejected.ID, rejectedItem.DecisionSequence, rejected.Digest)
	if rec.Code != 200 || rec.Body.String() != wantList {
		t.Fatalf("list = %d %q, want %q (changes_requested %s must be excluded)", rec.Code, rec.Body.String(), wantList, changes.ID)
	}

	poison := &poisonPortFRRHZ144{store: store}
	poison.poisoned.Store(true)
	hp := NewHTTP(poison)
	hp.Trust = verifier
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/trust/signing?gate="+input.ID, nil)
	req.RemoteAddr = "127.0.0.1:9"
	hp.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("poison signing = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAttestCreateHTTPExactIntentFRRHZ164(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	q, item := unsignedQuestion164(t, store, "http attest", question.Approve, "")
	items := []attest.Item{item}
	manifestDigest := trust.ManifestDigest(items)
	sig := attestSignature164(t, store, verifier, private, items, "20202020202020202020202020202020")
	body, _ := json.Marshal(map[string]any{"kind": "attest.create", "manifestDigest": manifestDigest, "actor": "signer", "items": []map[string]any{{"consumer": item.Consumer, "gateId": item.GateID, "decisionSequence": item.DecisionSequence, "digest": item.Digest, "decision": item.Decision, "reason": item.Reason}}, "signature": map[string]any{"keyId": sig.KeyID, "signedAt": sig.SignedAt, "nonce": sig.Nonce, "sig": sig.Sig}})
	h := NewHTTP(store)
	h.Trust = verifier
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/intent", bytes.NewReader(body)))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"Accepted":true`)) {
		t.Fatalf("intent = %d %s", rec.Code, rec.Body.String())
	}
	if got := store.List("attest", "attest-root"); len(got) != 1 || got[0].Type != "attest.recorded" {
		t.Fatalf("attest log = %+v", got)
	}
	var object map[string]any
	_ = json.Unmarshal(body, &object)
	object["actor"] = ""
	blank, _ := json.Marshal(object)
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/intent", bytes.NewReader(blank)))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"Reason":"invalid attest"`)) || len(store.List("attest", "attest-root")) != 1 {
		t.Fatalf("blank actor = %d %s", rec.Code, rec.Body.String())
	}
	object["actor"], object["manifestDigest"] = "signer", "sha256:"+strings.Repeat("0", 64)
	mismatch, _ := json.Marshal(object)
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/intent", bytes.NewReader(mismatch)))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"Reason":"attest manifest digest mismatch"`)) || len(store.List("attest", "attest-root")) != 1 {
		t.Fatalf("manifest mismatch = %d %s", rec.Code, rec.Body.String())
	}
	delete(object, "manifestDigest")
	missing, _ := json.Marshal(object)
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/intent", bytes.NewReader(missing)))
	if rec.Code != 400 || rec.Body.String() != "invalid attest\n" || len(store.List("attest", "attest-root")) != 1 {
		t.Fatalf("missing manifest digest = %d %s", rec.Code, rec.Body.String())
	}
	object["manifestDigest"], object["extra"] = manifestDigest, true
	extra, _ := json.Marshal(object)
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/intent", bytes.NewReader(extra)))
	if rec.Code != 400 || rec.Body.String() != "invalid attest\n" || len(store.List("attest", "attest-root")) != 1 {
		t.Fatalf("extra key = %d %s", rec.Code, rec.Body.String())
	}
	_ = q
}
