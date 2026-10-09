package attest_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"rhizome/internal/approval"
	"rhizome/internal/attest"
	"rhizome/internal/events"
	"rhizome/internal/question"
	"rhizome/internal/trust"
	"strings"
	"testing"
	"time"
)

const anchorJSON164 = `{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`

type fixture164 struct {
	store    *events.Store
	verifier *trust.Verifier
	private  ed25519.PrivateKey
	keyID    string
	journal  string
}

func newFixture164(t *testing.T) fixture164 {
	t.Helper()
	anchor, err := trust.ParseAnchor([]byte(anchorJSON164))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	store := &events.Store{Guard: verifier}
	if err := trust.EnsureGenesis(store, anchor); err != nil {
		t.Fatal(err)
	}
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	private := ed25519.NewKeyFromSeed(seed)
	keyID, _ := trust.KeyID(private.Public())
	summary, _ := verifier.TrustSummary(store)
	return fixture164{store, verifier, private, keyID, summary.JournalID}
}

func (f fixture164) question(t *testing.T, name string, decisions ...question.Decision) (question.Ref, []events.Event) {
	t.Helper()
	q, err := (question.Service{Store: f.store}).Ask(name, "body", "", "mission-x", "", "agent", "")
	if err != nil {
		// The question kernel validates a non-empty mission binding. Tests use
		// an unbound direct kernel question because attest validation only owns
		// decision facts, not relay binding policy.
		q, err = (question.Service{Store: f.store}).Ask(name, "body", "", "", "", "agent", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		reason := ""
		if decision != question.Approve {
			reason = "reason"
		}
		q, err = (question.Service{Store: f.store}).Answer(q.ID, decision, reason, "operator", q.Digest)
		if err != nil {
			t.Fatal(err)
		}
	}
	return q, f.store.List("question", q.ID)
}

func item164(q question.Ref, event events.Event) attest.Item {
	return attest.Item{Consumer: "question", GateID: q.ID, DecisionSequence: event.Sequence, Digest: q.Digest, Decision: string(q.Decision), Reason: q.Reason}
}

func (f fixture164) signature(items []attest.Item, signedAt, nonce string) trust.Signature {
	if signedAt == "" {
		signedAt = time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	}
	message := trust.AttestMessage(f.journal, trust.ManifestDigest(items), stringCount164(len(items)), f.keyID, signedAt, nonce)
	return trust.Signature{KeyID: f.keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, message))}
}

func stringCount164(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return fmt.Sprint(n)
}

func event164(f fixture164, items []attest.Item, sig trust.Signature, revision uint64) events.Event {
	payload, _ := json.Marshal(struct {
		ManifestDigest string
		Items          []attest.Item
		Signature      trust.Signature
	}{trust.ManifestDigest(items), items, sig})
	return events.Event{Sequence: uint64(len(f.store.All()) + 1), AggregateType: trust.AttestAggregateType, AggregateID: trust.AttestAggregateID, Revision: revision, Type: trust.AttestRecordedType, Payload: payload}
}

func revokeRoot164(t *testing.T, f fixture164) {
	t.Helper()
	signedAt, nonce := time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	message := trust.RevokeMessage(f.journal, f.keyID, "retired", f.keyID, signedAt, nonce)
	payload, _ := json.Marshal(struct {
		KeyID, Reason string
		Signature     trust.Signature
	}{f.keyID, "retired", trust.Signature{KeyID: f.keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, message))}})
	log := f.store.List(trust.TrustAggregateType, trust.TrustAggregateID)
	if err := f.store.Append(uint64(len(log)), events.Event{AggregateType: trust.TrustAggregateType, AggregateID: trust.TrustAggregateID, Revision: uint64(len(log) + 1), Type: trust.TrustRevokedType, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestAttestGuardRuleTableFRRHZ164(t *testing.T) {
	t.Run("VA1 anchorless append", func(t *testing.T) {
		f := newFixture164(t)
		q, log := f.question(t, "va1", question.Approve)
		items := []attest.Item{item164(q, log[len(log)-1])}
		store := &events.Store{Guard: trust.NewAnchorless()}
		before := len(store.All())
		err := (attest.Service{Store: store}).Create(trust.ManifestDigest(items), items, f.signature(items, "", "01010101010101010101010101010101"))
		if err == nil || !strings.Contains(err.Error(), "trust anchor") || len(store.All()) != before {
			t.Fatalf("err=%v writes=%d", err, len(store.All())-before)
		}
	})

	tests := []struct {
		name, want string
		mutate     func(fixture164, question.Ref, []events.Event, []attest.Item) []attest.Item
	}{
		{"VA3 missing gate", "gate not found", func(f fixture164, q question.Ref, log []events.Event, items []attest.Item) []attest.Item {
			items[0].GateID = "q-missing"
			return items
		}},
		{"VA4 zero sequence", "item order", func(f fixture164, q question.Ref, log []events.Event, items []attest.Item) []attest.Item {
			items[0].DecisionSequence = 0
			return items
		}},
		{"VA4 future sequence", "sequence mismatch", func(f fixture164, q question.Ref, log []events.Event, items []attest.Item) []attest.Item {
			items[0].DecisionSequence += 100
			return items
		}},
		{"VA5 digest mismatch", "decision mismatch", func(f fixture164, q question.Ref, log []events.Event, items []attest.Item) []attest.Item {
			items[0].Digest += "x"
			return items
		}},
		{"VA5 reason mismatch", "decision mismatch", func(f fixture164, q question.Ref, log []events.Event, items []attest.Item) []attest.Item {
			items[0].Reason = "other"
			return items
		}},
		{"VA5 decision mismatch", "decision mismatch", func(f fixture164, q question.Ref, log []events.Event, items []attest.Item) []attest.Item {
			items[0].Decision = "reject"
			return items
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture164(t)
			q, log := f.question(t, tc.name, question.Approve)
			items := tc.mutate(f, q, log, []attest.Item{item164(q, log[len(log)-1])})
			before := len(f.store.All())
			err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, f.signature(items, "", "02020202020202020202020202020202"))
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(f.store.All()) != before {
				t.Fatalf("err=%v writes=%d", err, len(f.store.All())-before)
			}
		})
	}
}

func TestAttestVA2ShapesAndOrderingFRRHZ164(t *testing.T) {
	f := newFixture164(t)
	q1, log1 := f.question(t, "shape-a", question.Approve)
	q2, log2 := f.question(t, "shape-b", question.Reject)
	a, b := item164(q1, log1[len(log1)-1]), item164(q2, log2[len(log2)-1])
	for _, tc := range []struct {
		name, want string
		items      []attest.Item
	}{
		{"empty", "item count", nil},
		{"unsorted", "item order", []attest.Item{b, a}},
		{"duplicate gate", "duplicate attest gate", []attest.Item{a, {Consumer: a.Consumer, GateID: a.GateID, DecisionSequence: b.DecisionSequence, Digest: a.Digest, Decision: a.Decision, Reason: a.Reason}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(tc.items), tc.items, f.signature(tc.items, "", "03030303030303030303030303030303"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	tooMany := make([]attest.Item, 257)
	for i := range tooMany {
		tooMany[i] = attest.Item{Consumer: "question", GateID: fmt.Sprintf("q-%024d", i), DecisionSequence: uint64(i + 1), Digest: "d", Decision: "approve"}
	}
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(tooMany), tooMany, f.signature(tooMany, "", "04040404040404040404040404040404")); err == nil || !strings.Contains(err.Error(), "item count") {
		t.Fatalf("N>256 err=%v", err)
	}
}

func TestAttestVA2ExactKeysVocabularyAndVA3TerminalFRRHZ164(t *testing.T) {
	f := newFixture164(t)
	pending, err := (question.Service{Store: f.store}).Ask("pending-va3", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	pendingItem := []attest.Item{{Consumer: "question", GateID: pending.ID, DecisionSequence: f.store.List("question", pending.ID)[0].Sequence, Digest: pending.Digest, Decision: "approve"}}
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(pendingItem), pendingItem, f.signature(pendingItem, "", "41414141414141414141414141414141")); err == nil || !strings.Contains(err.Error(), "not terminal") {
		t.Fatalf("pending err=%v", err)
	}
	q, log := f.question(t, "vocabulary", question.Approve)
	invalidDecision := []attest.Item{item164(q, log[len(log)-1])}
	invalidDecision[0].Decision = "ALLOW"
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(invalidDecision), invalidDecision, f.signature(invalidDecision, "", "42424242424242424242424242424242")); err == nil || !strings.Contains(err.Error(), "invalid attest decision") {
		t.Fatalf("vocabulary err=%v", err)
	}
	valid := []attest.Item{item164(q, log[len(log)-1])}
	event := event164(f, valid, f.signature(valid, "", "43434343434343434343434343434343"), 1)
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)
	signature := payload["Signature"].(map[string]any)
	signature["keyId"] = signature["KeyID"]
	delete(signature, "KeyID")
	event.Payload, _ = json.Marshal(payload)
	if err := f.verifier.CheckAppend(f.store, event, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("signature casing err=%v", err)
	}
}

func TestAttestLatestVerifiedFreshnessAndSignatureRulesFRRHZ164(t *testing.T) {
	t.Run("latest not earlier requestChanges", func(t *testing.T) {
		f := newFixture164(t)
		q, log := f.question(t, "latest", question.RequestChanges, question.Approve)
		items := []attest.Item{item164(q, log[1])}
		items[0].Decision, items[0].Reason = "requestChanges", "reason"
		err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, f.signature(items, "", "05050505050505050505050505050505"))
		if err == nil || !strings.Contains(err.Error(), "sequence mismatch") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("VA6 already verified", func(t *testing.T) {
		f := newFixture164(t)
		q, _ := f.question(t, "verified")
		signedAt := time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
		nonce := "06060606060606060606060606060606"
		message := trust.DecisionMessage(f.journal, "question", q.ID, q.Digest, "approve", "", f.keyID, signedAt, nonce)
		verification := &question.Verification{Signature: &question.Signature{KeyID: f.keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, message))}}
		q, err := (question.Service{Store: f.store}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification)
		if err != nil {
			t.Fatal(err)
		}
		log := f.store.List("question", q.ID)
		items := []attest.Item{item164(q, log[len(log)-1])}
		err = (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, f.signature(items, "", "07070707070707070707070707070707"))
		if err == nil || !strings.Contains(err.Error(), "already verified") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("VA7 bad signature and stale append", func(t *testing.T) {
		f := newFixture164(t)
		q, log := f.question(t, "signature", question.Approve)
		items := []attest.Item{item164(q, log[len(log)-1])}
		bad := f.signature(items, "", "08080808080808080808080808080808")
		bad.Sig = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, bad); err == nil || !strings.Contains(err.Error(), "verification failed") {
			t.Fatalf("bad sig err=%v", err)
		}
		stale := f.signature(items, "2026-10-09T00:00:00Z", "09090909090909090909090909090909")
		if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, stale); err == nil || !strings.Contains(err.Error(), "freshness") {
			t.Fatalf("stale err=%v", err)
		}
		event := event164(f, items, stale, 1)
		if err := f.verifier.CheckReplay(f.store, event); err != nil {
			t.Fatalf("stale replay: %v", err)
		}
	})
	t.Run("VA7 unknown and revoked signing key", func(t *testing.T) {
		f := newFixture164(t)
		q, log := f.question(t, "inactive", question.Approve)
		items := []attest.Item{item164(q, log[len(log)-1])}
		malformed := f.signature(items, "", "47474747474747474747474747474747")
		malformed.KeyID = "sha256:" + strings.Repeat("A", 64)
		if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, malformed); err == nil || !strings.Contains(err.Error(), "signature envelope") {
			t.Fatalf("uppercase key err=%v", err)
		}
		unknown := f.signature(items, "", "44444444444444444444444444444444")
		unknown.KeyID = "sha256:" + strings.Repeat("1", 64)
		if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, unknown); err == nil || !strings.Contains(err.Error(), "not active") {
			t.Fatalf("unknown err=%v", err)
		}
		revokeRoot164(t, f)
		if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, f.signature(items, "", "45454545454545454545454545454545")); err == nil || !strings.Contains(err.Error(), "not active") {
			t.Fatalf("revoked err=%v", err)
		}
	})
}

func TestAttestVA6VA8AcrossManifestsAppendAndReplayFRRHZ164(t *testing.T) {
	f := newFixture164(t)
	qa, la := f.question(t, "manifest-a", question.Approve)
	qb, lb := f.question(t, "manifest-b", question.Approve)
	qc, lc := f.question(t, "manifest-c", question.Reject)
	a, b, c := item164(qa, la[len(la)-1]), item164(qb, lb[len(lb)-1]), item164(qc, lc[len(lc)-1])
	first := []attest.Item{a, b}
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(first), first, f.signature(first, "", "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a")); err != nil {
		t.Fatal(err)
	}
	before := len(f.store.All())
	second := []attest.Item{a, c}
	sig := f.signature(second, "", "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(second), second, sig); err == nil || !strings.Contains(err.Error(), "already recorded") || len(f.store.All()) != before {
		t.Fatalf("append err=%v writes=%d", err, len(f.store.All())-before)
	}
	event := event164(f, second, sig, 2)
	if err := f.verifier.CheckReplay(f.store, event); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("replay err=%v", err)
	}
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(first), first, f.signature(first, "", "0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c")); err == nil || !strings.Contains(err.Error(), "manifest already") {
		t.Fatalf("same digest err=%v", err)
	}
}

func TestAttestWrongRevisionDigestShapeAndCopiedReplayFRRHZ164(t *testing.T) {
	f := newFixture164(t)
	q, log := f.question(t, "raw", question.Approve)
	items := []attest.Item{item164(q, log[len(log)-1])}
	sig := f.signature(items, "", "0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d")
	before := len(f.store.All())
	if err := (attest.Service{Store: f.store}).Create("sha256:"+strings.Repeat("0", 64), items, sig); err == nil || err.Error() != "attest manifest digest mismatch" || len(f.store.All()) != before {
		t.Fatalf("service digest mismatch err=%v writes=%d", err, len(f.store.All())-before)
	}
	event := event164(f, items, sig, 2)
	if err := f.verifier.CheckAppend(f.store, event, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("revision err=%v", err)
	}
	event.Revision = 1
	var object map[string]any
	_ = json.Unmarshal(event.Payload, &object)
	object["ManifestDigest"] = "sha256:" + strings.Repeat("0", 64)
	event.Payload, _ = json.Marshal(object)
	if err := f.verifier.CheckAppend(f.store, event, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("digest err=%v", err)
	}
	object["Extra"] = true
	event.Payload, _ = json.Marshal(object)
	if err := f.verifier.CheckAppend(f.store, event, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("shape err=%v", err)
	}
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, sig); err != nil {
		t.Fatal(err)
	}
	copyStore := &events.Store{}
	for _, copied := range f.store.All() {
		if err := f.verifier.CheckReplay(copyStore, copied); err != nil {
			t.Fatalf("copied replay: %v", err)
		}
		if err := copyStore.AppendRevision(copied); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttestApprovalAllowWithReasonFRRHZ164(t *testing.T) {
	f := newFixture164(t)
	key := approval.RequestKey{TraceID: "4123456789abcdef0123456789abcdef", SpanID: "4123456789abcdef", RequestID: "attest-approval"}
	a, err := (approval.Service{Store: f.store}).RecordInput(key, approval.Allow, "operator allowed", "response", "hx-args-digest-v1:attest", "operator", "", "", trust.Authority{})
	if err != nil {
		t.Fatal(err)
	}
	event := f.store.List("approval", a.ID)[0]
	items := []attest.Item{{Consumer: "approval", GateID: a.ID, DecisionSequence: event.Sequence, Digest: a.RequestDigest, Decision: "allow", Reason: a.Reason}}
	if err := (attest.Service{Store: f.store}).Create(trust.ManifestDigest(items), items, f.signature(items, "", "46464646464646464646464646464646")); err != nil {
		t.Fatal(err)
	}
	index, err := f.verifier.Attestations(f.store)
	if err != nil || index[a.ID].DecisionSequence != event.Sequence {
		t.Fatalf("index=%+v err=%v", index, err)
	}
}
