package workspace

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

const anchorJSON148 = `{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`

func rootKey149(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed, err := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

type FakeSigner struct{ private ed25519.PrivateKey }

func (s FakeSigner) decisionVerification(t *testing.T, journalID, consumer, gateID, digest, decision, reason, signedAt, nonce string) *question.Verification {
	t.Helper()
	keyID, err := trust.KeyID(s.private.Public())
	if err != nil {
		t.Fatal(err)
	}
	message := trust.DecisionMessage(journalID, consumer, gateID, digest, decision, reason, keyID, signedAt, nonce)
	return &question.Verification{Signature: &question.Signature{KeyID: keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, message))}}
}

func anchoredStore149(t *testing.T) (*events.Store, *trust.Verifier, trust.Anchor, ed25519.PrivateKey) {
	t.Helper()
	anchor, err := trust.ParseAnchor([]byte(anchorJSON148))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	store := &events.Store{Guard: verifier}
	if err := trust.EnsureGenesis(store, anchor); err != nil {
		t.Fatal(err)
	}
	return store, verifier, anchor, rootKey149(t)
}

func signedAt149() string {
	return time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

func domain149(t *testing.T, verifier *trust.Verifier, store events.View) string {
	t.Helper()
	summary, err := verifier.TrustSummary(store)
	if err != nil || summary == nil {
		t.Fatalf("trust summary: %+v %v", summary, err)
	}
	return summary.JournalID
}

func decisionVerification149(t *testing.T, private ed25519.PrivateKey, journalID, consumer, gateID, digest, decision, reason, signedAt, nonce string) *question.Verification {
	t.Helper()
	return (FakeSigner{private: private}).decisionVerification(t, journalID, consumer, gateID, digest, decision, reason, signedAt, nonce)
}

func intentVerification149(t *testing.T, verification *question.Verification) json.RawMessage {
	t.Helper()
	s := verification.Signature
	raw, err := json.Marshal(map[string]any{"signature": map[string]string{"keyId": s.KeyID, "signedAt": s.SignedAt, "nonce": s.Nonce, "sig": s.Sig}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func appendRevoke149(t *testing.T, store events.Port, verifier *trust.Verifier, private ed25519.PrivateKey, target, reason, signedAt, nonce string) error {
	t.Helper()
	signer, err := trust.KeyID(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	message := trust.RevokeMessage(domain149(t, verifier, store), target, reason, signer, signedAt, nonce)
	payload, _ := json.Marshal(struct {
		KeyID, Reason string
		Signature     question.Signature
	}{target, reason, question.Signature{KeyID: signer, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message))}})
	log := store.List(trust.TrustAggregateType, trust.TrustAggregateID)
	return store.Append(uint64(len(log)), events.Event{AggregateType: trust.TrustAggregateType, AggregateID: trust.TrustAggregateID, Revision: uint64(len(log) + 1), Type: trust.TrustRevokedType, Payload: payload})
}

func TestGenesisRestartIdempotentFRRHZ148(t *testing.T) {
	store, verifier, anchor, _ := anchoredStore149(t)
	before := domain149(t, verifier, store)
	if err := trust.EnsureGenesis(store, anchor); err != nil {
		t.Fatal(err)
	}
	if got := store.List(trust.TrustAggregateType, trust.TrustAggregateID); len(got) != 1 {
		t.Fatalf("genesis count = %d", len(got))
	}
	if after := domain149(t, verifier, store); after != before {
		t.Fatalf("journal id changed: %s -> %s", before, after)
	}
}

func addIntentWithSigner148(t *testing.T, store events.Port, verifier *trust.Verifier, addedPublic any, addedAlgorithm, signerID, signedAt, nonce string, sign func([]byte) []byte) (Intent, string) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(addedPublic)
	if err != nil {
		t.Fatal(err)
	}
	addedID, err := trust.KeyID(addedPublic)
	if err != nil {
		t.Fatal(err)
	}
	message := trust.AddMessage(domain149(t, verifier, store), addedID, addedAlgorithm, der, "H", trust.AssuranceKey, signerID, signedAt, nonce)
	raw, _ := json.Marshal(map[string]any{"keyId": addedID, "algorithm": addedAlgorithm, "publicKey": base64.StdEncoding.EncodeToString(der), "principal": "H", "assurance": trust.AssuranceKey, "signature": map[string]string{"keyId": signerID, "signedAt": signedAt, "nonce": nonce, "sig": base64.StdEncoding.EncodeToString(sign(message))}})
	return Intent{Kind: "trust.key.add", Trust: raw}, addedID
}

func addIntent148(t *testing.T, store events.Port, verifier *trust.Verifier, signer ed25519.PrivateKey, added ed25519.PublicKey, signedAt, nonce string) (Intent, string) {
	t.Helper()
	signerID, err := trust.KeyID(signer.Public())
	if err != nil {
		t.Fatal(err)
	}
	return addIntentWithSigner148(t, store, verifier, added, trust.AlgorithmEd25519, signerID, signedAt, nonce, func(message []byte) []byte {
		return ed25519.Sign(signer, message)
	})
}

func revokeIntent148(t *testing.T, store events.Port, verifier *trust.Verifier, signer ed25519.PrivateKey, target, signedAt, nonce string) Intent {
	t.Helper()
	signerID, _ := trust.KeyID(signer.Public())
	reason := "lost"
	message := trust.RevokeMessage(domain149(t, verifier, store), target, reason, signerID, signedAt, nonce)
	raw, _ := json.Marshal(map[string]any{"keyId": target, "reason": reason, "signature": map[string]string{"keyId": signerID, "signedAt": signedAt, "nonce": nonce, "sig": base64.StdEncoding.EncodeToString(ed25519.Sign(signer, message))}})
	return Intent{Kind: "trust.key.revoke", Trust: raw}
}

func mutateTrustIntent148(t *testing.T, intent Intent, mutate func(map[string]any)) Intent {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(intent.Trust, &object); err != nil {
		t.Fatal(err)
	}
	mutate(object)
	intent.Trust, _ = json.Marshal(object)
	return intent
}

func TestTrustKeyRuleTableFRRHZ148(t *testing.T) {
	store, verifier, _, root := anchoredStore149(t)
	second := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	add, secondID := addIntent148(t, store, verifier, root, second.Public().(ed25519.PublicKey), signedAt149(), "11111111111111111111111111111111")
	if result, err := RelayIntent(store, add, "ignored", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("valid add: %+v %v", result, err)
	}

	before := len(store.All())
	badSignature, _ := addIntent148(t, store, verifier, root, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize)).Public().(ed25519.PublicKey), signedAt149(), "51515151515151515151515151515151")
	badSignature = mutateTrustIntent148(t, badSignature, func(object map[string]any) {
		object["signature"].(map[string]any)["sig"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, ed25519.SignatureSize))
	})
	fieldBase, _ := addIntent148(t, store, verifier, root, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{10}, ed25519.SeedSize)).Public().(ed25519.PublicKey), signedAt149(), "56565656565656565656565656565656")
	malformed, _ := addIntent148(t, store, verifier, root, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey), signedAt149(), "53535353535353535353535353535353")
	malformed = mutateTrustIntent148(t, malformed, func(object map[string]any) { object["signature"].(map[string]any)["nonce"] = "NOT-HEX" })
	future, _ := addIntent148(t, store, verifier, root, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize)).Public().(ed25519.PublicKey), time.Now().UTC().Add(3*time.Minute).Truncate(time.Second).Format("2006-01-02T15:04:05Z"), "54545454545454545454545454545454")
	tcases := []struct {
		name   string
		intent Intent
	}{
		{"no re-add", add},
		{"unknown add key", Intent{Kind: "trust.key.add", Trust: append([]byte(nil), []byte(`{"keyId":"x","algorithm":"ed25519","publicKey":"x","principal":"H","assurance":"key","signature":{"keyId":"x","signedAt":"2026-10-08T00:00:00Z","nonce":"00000000000000000000000000000000","sig":"AA=="},"extra":1}`)...)}},
		{"stale", func() Intent {
			i, _ := addIntent148(t, store, verifier, root, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize)).Public().(ed25519.PublicKey), time.Now().UTC().Add(-16*time.Minute).Truncate(time.Second).Format("2006-01-02T15:04:05Z"), "22222222222222222222222222222222")
			return i
		}()},
		{"future", future},
		{"bad signature", badSignature},
		{"tampered keyId", mutateTrustIntent148(t, fieldBase, func(object map[string]any) {
			object["keyId"] = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		})},
		{"tampered algorithm", mutateTrustIntent148(t, fieldBase, func(object map[string]any) { object["algorithm"] = trust.AlgorithmECDSAP256 })},
		{"tampered publicKey", mutateTrustIntent148(t, fieldBase, func(object map[string]any) {
			object["publicKey"] = "MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="
		})},
		{"tampered principal", mutateTrustIntent148(t, fieldBase, func(object map[string]any) { object["principal"] = "X" })},
		{"tampered assurance", mutateTrustIntent148(t, fieldBase, func(object map[string]any) { object["assurance"] = "presence" })},
		{"tampered signer keyId", mutateTrustIntent148(t, fieldBase, func(object map[string]any) {
			object["signature"].(map[string]any)["keyId"] = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		})},
		{"tampered signedAt", mutateTrustIntent148(t, fieldBase, func(object map[string]any) {
			object["signature"].(map[string]any)["signedAt"] = time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Format("2006-01-02T15:04:05Z")
		})},
		{"tampered nonce", mutateTrustIntent148(t, fieldBase, func(object map[string]any) {
			object["signature"].(map[string]any)["nonce"] = "57575757575757575757575757575757"
		})},
		{"add signature unknown key", mutateTrustIntent148(t, fieldBase, func(object map[string]any) {
			object["signature"].(map[string]any)["extra"] = true
		})},
		{"malformed envelope", malformed},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := RelayIntent(store, tc.intent, "ignored", trust.Authority{})
			if err != nil || result.Accepted || len(store.All()) != before {
				t.Fatalf("accepted=%v reason=%q err=%v events=%d", result.Accepted, result.Reason, err, len(store.All()))
			}
		})
	}

	staleRevoke := revokeIntent148(t, store, verifier, root, secondID, time.Now().UTC().Add(-16*time.Minute).Truncate(time.Second).Format("2006-01-02T15:04:05Z"), "30303030303030303030303030303030")
	if result, err := RelayIntent(store, staleRevoke, "ignored", trust.Authority{}); err != nil || result.Accepted || len(store.All()) != before {
		t.Fatalf("stale revoke: %+v err=%v", result, err)
	}
	revoke := revokeIntent148(t, store, verifier, root, secondID, signedAt149(), "33333333333333333333333333333333")
	tamperedRevokeTarget := mutateTrustIntent148(t, revoke, func(object map[string]any) {
		object["keyId"] = "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9"
	})
	tamperedRevokeReason := mutateTrustIntent148(t, revoke, func(object map[string]any) { object["reason"] = "other" })
	unknownRevokeSignatureKey := mutateTrustIntent148(t, revoke, func(object map[string]any) {
		object["signature"].(map[string]any)["extra"] = true
	})
	for _, tc := range []struct {
		name   string
		intent Intent
	}{{"tampered revoke target", tamperedRevokeTarget}, {"tampered revoke reason", tamperedRevokeReason}, {"revoke signature unknown key", unknownRevokeSignatureKey}} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := RelayIntent(store, tc.intent, "ignored", trust.Authority{})
			if err != nil || result.Accepted || len(store.All()) != before {
				t.Fatalf("accepted=%v reason=%q err=%v events=%d", result.Accepted, result.Reason, err, len(store.All()))
			}
		})
	}
	if result, err := RelayIntent(store, revoke, "ignored", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("valid revoke: %+v %v", result, err)
	}
	before = len(store.All())
	for _, tc := range []struct {
		name   string
		intent Intent
	}{{"double revoke", revoke}, {"re-add after revoke", add}, {"unknown revoke key", Intent{Kind: "trust.key.revoke", Trust: json.RawMessage(`{"keyId":"x","reason":"lost","signature":{"keyId":"x","signedAt":"2026-10-08T00:00:00Z","nonce":"00000000000000000000000000000000","sig":"AA=="},"extra":1}`)}}} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := RelayIntent(store, tc.intent, "ignored", trust.Authority{})
			if err != nil || result.Accepted || len(store.All()) != before {
				t.Fatalf("accepted=%v reason=%q err=%v events=%d", result.Accepted, result.Reason, err, len(store.All()))
			}
		})
	}

	result, err := RelayIntent(store, Intent{Kind: "trust.key.genesis"}, "ignored", trust.Authority{})
	if err != nil || result.Accepted || len(store.All()) != before {
		t.Fatalf("genesis intent wrote: %+v err=%v", result, err)
	}
	anchorless := &events.Store{Guard: trust.NewAnchorless()}
	result, err = RelayIntent(anchorless, add, "ignored", trust.Authority{})
	if err != nil || result.Accepted || len(anchorless.All()) != 0 {
		t.Fatalf("anchorless trust intent wrote: %+v err=%v", result, err)
	}
}

func TestTrustKeyAddUsesRegisteredSignerAlgorithmFRRHZ148(t *testing.T) {
	store, verifier, _, root := anchoredStore149(t)
	edSigner := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, ed25519.SeedSize))
	p256Signer, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edAdd, edID := addIntent148(t, store, verifier, root, edSigner.Public().(ed25519.PublicKey), signedAt149(), "52525252525252525252525252525252")
	if result, err := RelayIntent(store, edAdd, "ignored", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("register ed25519 signer: %+v err=%v", result, err)
	}
	rootID, _ := trust.KeyID(root.Public())
	p256Add, p256ID := addIntentWithSigner148(t, store, verifier, &p256Signer.PublicKey, trust.AlgorithmECDSAP256, rootID, signedAt149(), "53535353535353535353535353535353", func(message []byte) []byte {
		return ed25519.Sign(root, message)
	})
	if result, err := RelayIntent(store, p256Add, "ignored", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("register P-256 signer: %+v err=%v", result, err)
	}

	for _, tc := range []struct {
		name     string
		signerID string
		sign     func([]byte) []byte
		nonce    string
		seed     byte
	}{
		{
			name: "P-256 signature claiming ed25519 key ID", signerID: edID, nonce: "54545454545454545454545454545454", seed: 11,
			sign: func(message []byte) []byte {
				digest := sha256.Sum256(message)
				signature, err := ecdsa.SignASN1(cryptorand.Reader, p256Signer, digest[:])
				if err != nil {
					t.Fatal(err)
				}
				return signature
			},
		},
		{
			name: "ed25519 signature claiming P-256 key ID", signerID: p256ID, nonce: "55555555555555555555555555555555", seed: 12,
			sign: func(message []byte) []byte { return ed25519.Sign(edSigner, message) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{tc.seed}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
			intent, _ := addIntentWithSigner148(t, store, verifier, target, trust.AlgorithmEd25519, tc.signerID, signedAt149(), tc.nonce, tc.sign)
			before := len(store.All())
			result, err := RelayIntent(store, intent, "ignored", trust.Authority{})
			if err != nil || result.Accepted || len(store.All()) != before {
				t.Fatalf("accepted=%v reason=%q err=%v events=%d", result.Accepted, result.Reason, err, len(store.All()))
			}
		})
	}
}

func TestTrustKeyAddSignedByRevokedKeyRejectedFRRHZ148(t *testing.T) {
	store, verifier, _, root := anchoredStore149(t)
	revokedSigner := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, ed25519.SeedSize))
	addSigner, revokedID := addIntent148(t, store, verifier, root, revokedSigner.Public().(ed25519.PublicKey), signedAt149(), "56565656565656565656565656565656")
	if result, err := RelayIntent(store, addSigner, "ignored", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("register signer: %+v err=%v", result, err)
	}
	revoke := revokeIntent148(t, store, verifier, root, revokedID, signedAt149(), "57575757575757575757575757575757")
	if result, err := RelayIntent(store, revoke, "ignored", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("revoke signer: %+v err=%v", result, err)
	}
	target := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{14}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	addTarget, _ := addIntent148(t, store, verifier, revokedSigner, target, signedAt149(), "58585858585858585858585858585858")
	before := len(store.All())
	result, err := RelayIntent(store, addTarget, "ignored", trust.Authority{})
	if err != nil || result.Accepted || !strings.Contains(result.Reason, "signing key is not active") || len(store.All()) != before {
		t.Fatalf("accepted=%v reason=%q err=%v events=%d", result.Accepted, result.Reason, err, len(store.All()))
	}
}

func TestRecordedOldTrustEventReplayAcceptedFRRHZ148(t *testing.T) {
	store, verifier, _, root := anchoredStore149(t)
	old := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	added := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	intent, _ := addIntent148(t, store, verifier, root, added, old, "44444444444444444444444444444444")
	payload, err := trustIntentPayload(intent.Kind, intent.Trust)
	if err != nil {
		t.Fatal(err)
	}
	event := events.Event{Sequence: 2, AggregateType: trust.TrustAggregateType, AggregateID: trust.TrustAggregateID, Revision: 2, Type: trust.TrustAddedType, Payload: payload}
	if err := verifier.CheckReplay(store, event); err != nil {
		t.Fatalf("replay reapplied freshness: %v", err)
	}
}

func TestSignedDecisionRuleTableFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	q, err := (question.Service{Store: store}).Ask("signed question", "body", "", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	v := decisionVerification149(t, private, domain149(t, verifier, store), "question", q.ID, q.Digest, "approve", "", signedAt149(), "01010101010101010101010101010101")
	result, err := RelayIntent(store, Intent{Kind: "gate.approve", GateID: q.ID, Digest: q.Digest, Verification: intentVerification149(t, v)}, "signer", trust.Authority{})
	if err != nil || !result.Accepted {
		t.Fatalf("signed question rejected: %+v %v", result, err)
	}
	projection, err := Snapshot(store, verifier)
	if err != nil || projection.Gates[0].Verification == nil || projection.Gates[0].Verification.Status != "verified" || projection.Gates[0].Verification.Assurance != "key" || projection.Gates[0].Verification.KeyID != v.Signature.KeyID {
		t.Fatalf("question projection: %+v err=%v", projection.Gates, err)
	}

	key := approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "signed"}
	id := approval.IDFor(key)
	digest := "hx-args-digest-v1:signed"
	av := decisionVerification149(t, private, domain149(t, verifier, store), "approval", id, digest, "allow", "", signedAt149(), "02020202020202020202020202020202")
	if _, err := (approval.Service{Store: store}).RecordInput(key, approval.Allow, "", "response", digest, "signer", "", "", trust.Authority{}, av); err != nil {
		t.Fatal(err)
	}
	projection, err = Snapshot(store, verifier)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, gate := range projection.Gates {
		if gate.ID == id && gate.Verification != nil && gate.Verification.Status == "verified" {
			found = true
		}
	}
	if !found {
		t.Fatalf("signed approval not projected: %+v", projection.Gates)
	}

	tcases := []struct {
		name string
		make func(*events.Store, *trust.Verifier, ed25519.PrivateKey) (*question.Ref, *question.Verification)
	}{
		{"bad signature", func(s *events.Store, v *trust.Verifier, k ed25519.PrivateKey) (*question.Ref, *question.Verification) {
			q, _ := (question.Service{Store: s}).Ask("bad signature", "body", "", "", "", "asker", "")
			x := decisionVerification149(t, k, domain149(t, v, s), "question", q.ID, q.Digest, "approve", "", signedAt149(), "03030303030303030303030303030303")
			x.Signature.Sig = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, ed25519.SignatureSize))
			return &q, x
		}},
		{"stale", func(s *events.Store, v *trust.Verifier, k ed25519.PrivateKey) (*question.Ref, *question.Verification) {
			q, _ := (question.Service{Store: s}).Ask("stale", "body", "", "", "", "asker", "")
			at := time.Now().UTC().Add(-16 * time.Minute).Truncate(time.Second).Format("2006-01-02T15:04:05Z")
			return &q, decisionVerification149(t, k, domain149(t, v, s), "question", q.ID, q.Digest, "approve", "", at, "04040404040404040404040404040404")
		}},
		{"future", func(s *events.Store, v *trust.Verifier, k ed25519.PrivateKey) (*question.Ref, *question.Verification) {
			q, _ := (question.Service{Store: s}).Ask("future", "body", "", "", "", "asker", "")
			at := time.Now().UTC().Add(3 * time.Minute).Truncate(time.Second).Format("2006-01-02T15:04:05Z")
			return &q, decisionVerification149(t, k, domain149(t, v, s), "question", q.ID, q.Digest, "approve", "", at, "05050505050505050505050505050505")
		}},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			s, v, _, key := anchoredStore149(t)
			q, verification := tc.make(s, v, key)
			before := len(s.All())
			_, err := (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification)
			if err == nil || len(s.All()) != before {
				t.Fatalf("err=%v events=%d want=%d", err, len(s.All()), before)
			}
		})
	}

	anchorless := &events.Store{Guard: trust.NewAnchorless()}
	aq, _ := (question.Service{Store: anchorless}).Ask("anchorless", "body", "", "", "", "asker", "")
	av2 := decisionVerification149(t, private, domain149(t, verifier, store), "question", aq.ID, aq.Digest, "approve", "", signedAt149(), "06060606060606060606060606060606")
	before := len(anchorless.All())
	if _, err := (question.Service{Store: anchorless}).Answer(aq.ID, question.Approve, "", "signer", aq.Digest, av2); err == nil || len(anchorless.All()) != before {
		t.Fatalf("anchorless signed write err=%v events=%d", err, len(anchorless.All()))
	}

	inactive, inactiveVerifier, _, inactiveKey := anchoredStore149(t)
	inactiveID, _ := trust.KeyID(inactiveKey.Public())
	if err := appendRevoke149(t, inactive, inactiveVerifier, inactiveKey, inactiveID, "retired", signedAt149(), "61616161616161616161616161616161"); err != nil {
		t.Fatal(err)
	}
	iq, _ := (question.Service{Store: inactive}).Ask("inactive", "body", "", "", "", "asker", "")
	iv := decisionVerification149(t, inactiveKey, domain149(t, inactiveVerifier, inactive), "question", iq.ID, iq.Digest, "approve", "", signedAt149(), "62626262626262626262626262626262")
	before = len(inactive.All())
	if _, err := (question.Service{Store: inactive}).Answer(iq.ID, question.Approve, "", "signer", iq.Digest, iv); err == nil || len(inactive.All()) != before {
		t.Fatalf("inactive signing key err=%v events=%d", err, len(inactive.All()))
	}
}

func TestSignatureForGateARejectedOnGateBFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	gateA, err := (question.Service{Store: store}).Ask("gate A", "body A", "", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	gateB, err := (question.Service{Store: store}).Ask("gate B", "body B", "", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	verification := decisionVerification149(t, private, domain149(t, verifier, store), "question", gateA.ID, gateA.Digest, "approve", "", signedAt149(), "67676767676767676767676767676767")
	before := len(store.All())
	if _, err := (question.Service{Store: store}).Answer(gateB.ID, question.Approve, "", "signer", gateB.Digest, verification); err == nil || len(store.All()) != before {
		t.Fatalf("gate A signature used on gate B: err=%v events=%d", err, len(store.All()))
	}
}

func TestSecondSignedDecisionOnSameGateRejectedFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	gate, err := (question.Service{Store: store}).Ask("one decision", "body", "", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	journalID := domain149(t, verifier, store)
	first := decisionVerification149(t, private, journalID, "question", gate.ID, gate.Digest, "approve", "", signedAt149(), "68686868686868686868686868686868")
	if _, err := (question.Service{Store: store}).Answer(gate.ID, question.Approve, "", "signer", gate.Digest, first); err != nil {
		t.Fatal(err)
	}
	second := decisionVerification149(t, private, journalID, "question", gate.ID, gate.Digest, "reject", "second", signedAt149(), "69696969696969696969696969696969")
	before := len(store.All())
	if _, err := (question.Service{Store: store}).Answer(gate.ID, question.Reject, "second", "signer", gate.Digest, second); err == nil || !strings.Contains(err.Error(), "gate already has input") || len(store.All()) != before {
		t.Fatalf("second signed decision: err=%v events=%d", err, len(store.All()))
	}
}

func TestUnanchoredSignedDecisionMissingKeyRejectedFRRHZ149(t *testing.T) {
	anchor, err := trust.ParseAnchor([]byte(anchorJSON148))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewUnanchored()
	store := &events.Store{Guard: verifier}
	if err := trust.EnsureGenesis(store, anchor); err != nil {
		t.Fatal(err)
	}
	gate, err := (question.Service{Store: store}).Ask("missing key", "body", "", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	unknown := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{15}, ed25519.SeedSize))
	verification := decisionVerification149(t, unknown, domain149(t, verifier, store), "question", gate.ID, gate.Digest, "approve", "", signedAt149(), "70707070707070707070707070707070")
	before := len(store.All())
	if _, err := (question.Service{Store: store}).Answer(gate.ID, question.Approve, "", "signer", gate.Digest, verification); err == nil || !strings.Contains(err.Error(), "signing key is not active") || len(store.All()) != before {
		t.Fatalf("unanchored missing key: err=%v events=%d", err, len(store.All()))
	}
}

func TestSignedEnvelopeExactKeysAndClaimMixFRRHZ149(t *testing.T) {
	for _, mutation := range []struct {
		name string
		raw  func(*question.Signature) json.RawMessage
	}{
		{"claim mixed", func(s *question.Signature) json.RawMessage {
			raw, _ := json.Marshal(map[string]any{"claimKind": "relayed", "signature": map[string]string{"keyId": s.KeyID, "signedAt": s.SignedAt, "nonce": s.Nonce, "sig": s.Sig}})
			return raw
		}},
		{"signature missing key", func(s *question.Signature) json.RawMessage {
			raw, _ := json.Marshal(map[string]any{"signature": map[string]string{"keyId": s.KeyID, "signedAt": s.SignedAt, "sig": s.Sig}})
			return raw
		}},
		{"signature unknown key", func(s *question.Signature) json.RawMessage {
			raw, _ := json.Marshal(map[string]any{"signature": map[string]any{"keyId": s.KeyID, "signedAt": s.SignedAt, "nonce": s.Nonce, "sig": s.Sig, "extra": true}})
			return raw
		}},
		{"signature wrong casing", func(s *question.Signature) json.RawMessage {
			raw, _ := json.Marshal(map[string]any{"signature": map[string]string{"KeyID": s.KeyID, "signedAt": s.SignedAt, "nonce": s.Nonce, "sig": s.Sig}})
			return raw
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			store, verifier, _, private := anchoredStore149(t)
			q, _ := (question.Service{Store: store}).Ask(mutation.name, "body", "", "", "", "asker", "")
			valid := decisionVerification149(t, private, domain149(t, verifier, store), "question", q.ID, q.Digest, "approve", "", signedAt149(), "63636363636363636363636363636363")
			before := len(store.All())
			result, err := RelayIntent(store, Intent{Kind: "gate.approve", GateID: q.ID, Digest: q.Digest, Verification: mutation.raw(valid.Signature)}, "signer", trust.Authority{})
			if err != nil || result.Accepted || result.Reason != "invalid verification" || len(store.All()) != before {
				t.Fatalf("result=%+v err=%v events=%d", result, err, len(store.All()))
			}
		})
	}
}

func TestJanusRequestChangesSignatureRejectedZeroWritesFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	missions := mission.Service{Store: store}
	if _, err := missions.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := missions.Create("m", "g", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	gate := pendingGate(t, store, "signed-request-changes", gwDigest)
	verification := decisionVerification149(t, private, domain149(t, verifier, store), "approval", gate.ID, gwDigest, "requestChanges", "fix", signedAt149(), "64646464646464646464646464646464")
	before := len(store.All())
	result, err := RelayIntent(store, Intent{Kind: "gate.requestChanges", GateID: gate.ID, Digest: gwDigest, Reason: "fix", Verification: intentVerification149(t, verification)}, "signer", trust.Authority{})
	if err != nil || result.Accepted || result.Reason != "invalid verification" || len(store.All()) != before {
		t.Fatalf("result=%+v err=%v events=%d", result, err, len(store.All()))
	}
}

func TestIndependentJournalReplayRejectedFRRHZ149(t *testing.T) {
	first, firstVerifier, _, private := anchoredStore149(t)
	second, _, _, _ := anchoredStore149(t)
	q1, _ := (question.Service{Store: first}).Ask("same", "body", "", "", "", "asker", "")
	q2, _ := (question.Service{Store: second}).Ask("same", "body", "", "", "", "asker", "")
	verification := decisionVerification149(t, private, domain149(t, firstVerifier, first), "question", q1.ID, q1.Digest, "approve", "", signedAt149(), "07070707070707070707070707070707")
	before := len(second.All())
	if _, err := (question.Service{Store: second}).Answer(q2.ID, question.Approve, "", "signer", q2.Digest, verification); err == nil || len(second.All()) != before {
		t.Fatalf("cross-journal replay err=%v events=%d", err, len(second.All()))
	}
}

func TestV1QuestionSignedVerifiedFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	if err := appendV1QuestionFRRHZ145(store, "legacy", "body", "", "", "", "asker", ""); err != nil {
		t.Fatal(err)
	}
	digest := digestV1ForTest("legacy", "body", "")
	id := "q-" + strings.TrimPrefix(digest, "rhz-question-v1:")[:24]
	verification := decisionVerification149(t, private, domain149(t, verifier, store), "question", id, digest, "approve", "", signedAt149(), "47474747474747474747474747474747")
	if _, err := (question.Service{Store: store}).Answer(id, question.Approve, "", "signer", digest, verification); err != nil {
		t.Fatal(err)
	}
	p, err := Snapshot(store, verifier)
	if err != nil || p.Gates[0].Verification == nil || p.Gates[0].Verification.Status != "verified" || p.Gates[0].RequestDigest != digest {
		t.Fatalf("v1 signed projection: %+v err=%v", p.Gates, err)
	}
}

func TestCopiedJournalSameTrustDomainFRRHZ149(t *testing.T) {
	anchor, err := trust.ParseAnchor([]byte(anchorJSON148))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	sourcePath := filepath.Join(t.TempDir(), "source.ndjson")
	source, err := journal.OpenGuarded(sourcePath, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.EnsureGenesis(source, anchor); err != nil {
		t.Fatal(err)
	}
	private := rootKey149(t)
	q, _ := (question.Service{Store: source}).Ask("copy", "body", "", "", "", "asker", "")
	verification := decisionVerification149(t, private, domain149(t, verifier, source), "question", q.ID, q.Digest, "approve", "", signedAt149(), "08080808080808080808080808080808")
	if _, err := (question.Service{Store: source}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.ndjson")
	if err := os.WriteFile(copyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	copyVerifier := trust.NewAnchored(anchor)
	copyStore, err := journal.OpenGuarded(copyPath, copyVerifier)
	if err != nil {
		t.Fatalf("copied replay rejected: %v", err)
	}
	defer copyStore.Close()
	projection, err := Snapshot(copyStore, copyVerifier)
	if err != nil || projection.Gates[0].Verification == nil || projection.Gates[0].Verification.Status != "verified" {
		t.Fatalf("copied projection: %+v %v", projection.Gates, err)
	}
}

func TestReplayFailClosedBadSignedEventsFRRHZ149(t *testing.T) {
	source, verifier, _, private := anchoredStore149(t)
	q, _ := (question.Service{Store: source}).Ask("forged replay", "body", "", "", "", "asker", "")
	verification := decisionVerification149(t, private, domain149(t, verifier, source), "question", q.ID, q.Digest, "approve", "", signedAt149(), "45454545454545454545454545454545")
	if _, err := (question.Service{Store: source}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification); err != nil {
		t.Fatal(err)
	}
	all := source.All()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(all[len(all)-1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	var verificationObject map[string]json.RawMessage
	_ = json.Unmarshal(payload["Verification"], &verificationObject)
	var signatureObject map[string]json.RawMessage
	_ = json.Unmarshal(verificationObject["Signature"], &signatureObject)
	signatureObject["Sig"] = json.RawMessage(`"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="`)
	verificationObject["Signature"], _ = json.Marshal(signatureObject)
	payload["Verification"], _ = json.Marshal(verificationObject)
	all[len(all)-1].Payload, _ = json.Marshal(payload)
	prefix := &events.Store{}
	for i, event := range all {
		if err := verifier.CheckReplay(prefix, event); i == len(all)-1 {
			if err == nil {
				t.Fatal("forged signed event replayed")
			}
		} else if err != nil {
			t.Fatalf("valid prefix rejected: %v", err)
		}
		if i < len(all)-1 {
			if err := prefix.AppendRevision(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	forged := &events.Store{}
	for _, event := range all {
		if err := forged.AppendRevision(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Snapshot(forged, verifier); err == nil {
		t.Fatal("projection downgraded forged signature")
	}
}

func TestGuardRevokeRaceFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	q, _ := (question.Service{Store: store}).Ask("race", "body", "", "", "", "asker", "")
	keyID, _ := trust.KeyID(private.Public())
	verification := decisionVerification149(t, private, domain149(t, verifier, store), "question", q.ID, q.Digest, "approve", "", signedAt149(), "09090909090909090909090909090909")
	revoke := revokeIntent148(t, store, verifier, private, keyID, signedAt149(), "10101010101010101010101010101010")
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := (question.Service{Store: store}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification)
		results <- err == nil
	}()
	go func() {
		defer wg.Done()
		result, err := RelayIntent(store, revoke, "ignored", trust.Authority{})
		results <- err == nil && result.Accepted
	}()
	wg.Wait()
	close(results)
	successes := 0
	for accepted := range results {
		if accepted {
			successes++
		}
	}
	if successes < 1 || successes > 2 {
		t.Fatalf("successful appends = %d", successes)
	}
	for _, event := range store.All() {
		if err := verifier.CheckReplay(prefixBefore149(store.All(), event.Sequence), event); err != nil {
			t.Fatalf("accepted race result does not replay: %v", err)
		}
	}
}

type eventPrefix149 struct{ events []events.Event }

func (p eventPrefix149) List(kind, id string) []events.Event {
	var out []events.Event
	for _, event := range p.events {
		if event.AggregateType == kind && event.AggregateID == id {
			out = append(out, event)
		}
	}
	return out
}

func prefixBefore149(all []events.Event, sequence uint64) eventPrefix149 {
	var before []events.Event
	for _, event := range all {
		if event.Sequence < sequence {
			before = append(before, event)
		}
	}
	return eventPrefix149{before}
}

func TestV8BareVerifiedPlusClaimZeroWriteFRRHZ149(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	key := approval.RequestKey{TraceID: "1123456789abcdef0123456789abcdef", SpanID: "1123456789abcdef", RequestID: "v8"}
	id := approval.IDFor(key)
	digest := "hx-args-digest-v1:v8"
	verification := (FakeSigner{private: private}).decisionVerification(t, domain149(t, verifier, store), "approval", id, digest, "allow", "", signedAt149(), "000102030405060708090a0b0c0d0e0f")
	payload, err := json.Marshal(map[string]any{
		"decision":      "allow",
		"Reason":        "",
		"RequestDigest": digest,
		"ActorVerified": true,
		"Verification":  verification,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := len(store.All())
	err = store.Append(0, events.Event{AggregateType: "approval", AggregateID: id, Revision: 1, Type: trust.ApprovalInputRecordedType, Payload: payload})
	if err == nil || len(store.All()) != before {
		t.Fatalf("bare verified + signature accepted: err=%v", err)
	}
}

type streamRecorder150 struct {
	header http.Header
	body   bytes.Buffer
	cancel context.CancelFunc
	once   sync.Once
}

func (r *streamRecorder150) Header() http.Header         { return r.header }
func (r *streamRecorder150) WriteHeader(int)             {}
func (r *streamRecorder150) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *streamRecorder150) Flush()                      { r.once.Do(r.cancel) }

func TestAnchorlessWorkspaceBytesNoTrustKeyFRRHZ150(t *testing.T) {
	store := &events.Store{Guard: trust.NewAnchorless()}
	handler := NewHTTP(store).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	wantGET := `{"revision":0,"body":{"missions":[],"tasks":[],"gates":[],"deliverables":[],"edges":[],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{},"gateCapabilities":{}}}` + "\n"
	if recorder.Body.String() != wantGET || bytes.Contains(recorder.Body.Bytes(), []byte(`"trust"`)) {
		t.Fatalf("GET bytes changed\n got: %s\nwant: %s", recorder.Body.String(), wantGET)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &streamRecorder150{header: http.Header{}, cancel: cancel}
	handler.ServeHTTP(stream, httptest.NewRequest(http.MethodGet, "/v1/workspace/stream", nil).WithContext(ctx))
	wantSSE := "event: snapshot\ndata: " + wantGET[:len(wantGET)-1] + "\n\n"
	if stream.body.String() != wantSSE || bytes.Contains(stream.body.Bytes(), []byte(`"trust"`)) {
		t.Fatalf("SSE bytes changed\n got: %s\nwant: %s", stream.body.String(), wantSSE)
	}
}

func TestAnchorlessExistingJournalExactGETAndSSEBytesFRRHZ150(t *testing.T) {
	store := goldenJournalWithoutVerificationFRRHZ131(t)
	handler := NewHTTP(store).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	if got := recorder.Body.String(); got != goldenWorkspaceWithoutVerificationFRRHZ131 || strings.Contains(got, `"trust"`) {
		t.Fatalf("GET bytes changed\n got: %s\nwant: %s", got, goldenWorkspaceWithoutVerificationFRRHZ131)
	}
	if got := string(firstWorkspaceSnapshotFrameFRRHZ131(t, handler)); got != goldenWorkspaceStreamWithoutVerificationFRRHZ131 || strings.Contains(got, `"trust"`) {
		t.Fatalf("SSE bytes changed\n got: %s\nwant: %s", got, goldenWorkspaceStreamWithoutVerificationFRRHZ131)
	}
}

func TestGateVerifiedDerivationTableFRRHZ150(t *testing.T) {
	store, verifier, _, private := anchoredStore149(t)
	q, _ := (question.Service{Store: store}).Ask("projection", "body", "", "", "", "asker", "")
	verification := decisionVerification149(t, private, domain149(t, verifier, store), "question", q.ID, q.Digest, "approve", "", signedAt149(), "11121314151617181920212223242526")
	if _, err := (question.Service{Store: store}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification); err != nil {
		t.Fatal(err)
	}
	p, err := Snapshot(store, verifier)
	if err != nil || p.Trust == nil || p.Gates[0].Verification == nil || p.Gates[0].Verification.KeyRevokedNow {
		t.Fatalf("before revoke: %+v err=%v", p, err)
	}
	anchorlessCopy := &events.Store{}
	for _, event := range store.All() {
		if err := anchorlessCopy.AppendRevision(event); err != nil {
			t.Fatal(err)
		}
	}
	anchorlessProjection, err := Snapshot(anchorlessCopy, trust.NewAnchorless())
	if err != nil || anchorlessProjection.Gates[0].Verification != nil {
		t.Fatalf("anchorless projected verification: %+v err=%v", anchorlessProjection.Gates, err)
	}
	keyID, _ := trust.KeyID(private.Public())
	if err := appendRevoke149(t, store, verifier, private, keyID, "retired", signedAt149(), "27282930313233343536373839404142"); err != nil {
		t.Fatal(err)
	}
	p, err = Snapshot(store, verifier)
	if err != nil || p.Gates[0].Verification == nil || p.Gates[0].Verification.Status != "verified" || !p.Gates[0].Verification.KeyRevokedNow {
		t.Fatalf("after revoke: %+v err=%v", p.Gates, err)
	}
	encoded, _ := json.Marshal(toDTO(p))
	if !bytes.Contains(encoded, []byte(`"verification":{"status":"verified","assurance":"key","keyId":"`)) || !bytes.Contains(encoded, []byte(`"keyRevokedNow":true`)) || !bytes.Contains(encoded, []byte(`"trust":{"journalId":"`)) {
		t.Fatalf("verified DTO missing: %s", encoded)
	}
}

func TestContextGateVerifiedDTOFRRHZ150(t *testing.T) {
	store := fixture068(t)
	anchor, _ := trust.ParseAnchor([]byte(anchorJSON148))
	verifier := trust.NewAnchored(anchor)
	store.Guard = verifier
	if err := trust.EnsureGenesis(store, anchor); err != nil {
		t.Fatal(err)
	}
	defineProc(t, store, "proc-signedctx", procedure.Step{ID: "a", Action: "act-a", NeedsGate: true})
	if result, err := RelayIntent(store, Intent{Kind: "procedure.run", ID: "proc-signedctx", Name: "signedctx", GoalID: "goal-dev"}, "operator", trust.Authority{}); err != nil || !result.Accepted {
		t.Fatalf("procedure run: %+v err=%v", result, err)
	}
	qID := mustQuestionID("signedctx · a 게이트", "act-a", "")
	q, err := (question.Service{Store: store}).Get(qID)
	if err != nil {
		t.Fatal(err)
	}
	verification := decisionVerification149(t, rootKey149(t), domain149(t, verifier, store), "question", q.ID, q.Digest, "approve", "", signedAt149(), "65656565656565656565656565656565")
	if _, err := (question.Service{Store: store}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification); err != nil {
		t.Fatal(err)
	}
	httpServer := NewHTTP(store)
	httpServer.Trust = verifier
	code, body := getContext(t, httpServer.Handler(), "?task=mission-signedctx")
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"verification":{"status":"verified","assurance":"key","keyId":"`)) {
		t.Fatalf("context verified DTO: code=%d body=%s", code, body)
	}
}

func TestSignatureMessageLiteralStillPinnedFRRHZ149(t *testing.T) {
	message := trust.DecisionMessage("00112233445566778899aabbccddeeff", "question", "q-92caa77d4e8cb1f9a861ff35", "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61", "approve", "", "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9", "2026-10-08T00:00:00Z", "000102030405060708090a0b0c0d0e0f")
	digest := sha256.Sum256(message)
	if len(message) != 336 || hex.EncodeToString(digest[:]) != "9bebf41811aa4a1f080514aebf2a9fc87afe3edd17e90cc7581feba3a3a07570" {
		t.Fatalf("message changed: len=%d sha=%x", len(message), digest)
	}
}
