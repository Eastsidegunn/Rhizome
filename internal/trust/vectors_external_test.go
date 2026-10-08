package trust_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/question"
	"rhizome/internal/trust"
	"testing"
)

func TestDecisionMessageVectorFRRHZ146(t *testing.T) {
	message := trust.DecisionMessage(
		"00112233445566778899aabbccddeeff", "question", "q-92caa77d4e8cb1f9a861ff35",
		"rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61",
		"approve", "", "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9",
		"2026-10-08T00:00:00Z", "000102030405060708090a0b0c0d0e0f",
	)
	if len(message) != 336 {
		t.Fatalf("message length = %d", len(message))
	}
	sum := sha256.Sum256(message)
	if got := hex.EncodeToString(sum[:]); got != "9bebf41811aa4a1f080514aebf2a9fc87afe3edd17e90cc7581feba3a3a07570" {
		t.Fatalf("message hash = %s", got)
	}
	spki, _ := base64.StdEncoding.DecodeString("MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=")
	publicKey, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := trust.KeyID(publicKey)
	if err != nil || keyID != "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9" {
		t.Fatalf("key id = %q err=%v", keyID, err)
	}
	signature, _ := base64.StdEncoding.DecodeString("XgejPn6v24YZC4qg5UIjm7XQxkVq0Ua24bunxnJ0zbgVsAApWNxlK00m9EMiHg7kozMOBxeaSvXSHvy9lEdmBw==")
	if err := trust.VerifySignature(trust.AlgorithmEd25519, publicKey, message, signature); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), signature...)
	bad[0] ^= 1
	if trust.VerifySignature(trust.AlgorithmEd25519, publicKey, message, bad) == nil {
		t.Fatal("mutated Ed25519 signature accepted")
	}
}

func TestP256FixedVectorFRRHZ146(t *testing.T) {
	d := new(big.Int).SetInt64(1)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256()}
	pub.X, pub.Y = pub.Curve.ScalarBaseMult(d.Bytes())
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(der); got != "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaxfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li/hp/m47n60p8D54WK84zV2sxXs7LtkBoN79R9Q==" {
		t.Fatalf("P-256 public key = %s", got)
	}
	message := []byte("rhz-p256-fixed-vector")
	// Generated once with the fixed d=1 public key and pinned as ASN.1 DER.
	signature, err := hex.DecodeString("3046022100801c3752bbdd7072e1d254d33b6fb1b87f897d2a7c8fb6cde2283363c419fe4b022100f63195e32f165b978b22feeec651507225d890d1a7ae03698528741d0c92fb8f")
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.VerifySignature(trust.AlgorithmECDSAP256, pub, message, signature); err != nil {
		t.Fatal(err)
	}
	if trust.VerifySignature(trust.AlgorithmEd25519, ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)), message, signature) == nil {
		t.Fatal("algorithm mismatch accepted")
	}
}

func TestTrustKeyMessagesAndWireConstantsFRRHZ146(t *testing.T) {
	add := trust.AddMessage("00112233445566778899aabbccddeeff", "sha256:new", "ed25519", []byte{1, 2, 3}, "H", "key", "sha256:signer", "2026-10-08T00:00:00Z", "000102030405060708090a0b0c0d0e0f")
	if got := hex.EncodeToString(add); got != "0000001072687a2d74727573742d6b65792d7631000000203030313132323333343435353636373738383939616162626363646465656666000000036164640000000a7368613235363a6e65770000000765643235353139000000030102030000000148000000036b65790000000d7368613235363a7369676e657200000014323032362d31302d30385430303a30303a30305a000000203030303130323033303430353036303730383039306130623063306430653066" {
		t.Fatalf("add message = %s", got)
	}
	revoke := trust.RevokeMessage("00112233445566778899aabbccddeeff", "sha256:target", "lost", "sha256:signer", "2026-10-08T00:00:00Z", "000102030405060708090a0b0c0d0e0f")
	if got := hex.EncodeToString(revoke); got != "0000001072687a2d74727573742d6b65792d7631000000203030313132323333343435353636373738383939616162626363646465656666000000067265766f6b650000000d7368613235363a746172676574000000046c6f73740000000d7368613235363a7369676e657200000014323032362d31302d30385430303a30303a30305a000000203030303130323033303430353036303730383039306130623063306430653066" {
		t.Fatalf("revoke message = %s", got)
	}

	store := &events.Store{}
	q, err := (question.Service{Store: store}).Ask("t", "b", "", "", "", "actor", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (question.Service{Store: store}).Answer(q.ID, question.Approve, "", "actor", q.Digest); err != nil {
		t.Fatal(err)
	}
	if got := store.List("question", q.ID)[1].Type; got != trust.QuestionAnsweredType {
		t.Fatalf("question event type = %q", got)
	}
	key := approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "wire"}
	a, err := (approval.Service{Store: store}).RecordInput(key, approval.Allow, "", "resp", "digest", "actor", "", "", trust.Authority{})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.List("approval", a.ID)[0].Type; got != trust.ApprovalInputRecordedType {
		t.Fatalf("approval event type = %q", got)
	}
}

func TestApprovalV8VerifiedPlusVerificationZeroWritesFRRHZ149(t *testing.T) {
	store := &events.Store{}
	key := approval.RequestKey{TraceID: "2123456789abcdef0123456789abcdef", SpanID: "2123456789abcdef", RequestID: "v8-write"}
	claim := &question.Verification{ClaimKind: "relayed", OriginClaim: "H", RelayChain: []string{"ops"}}
	auth := trust.AuthorityForTest()
	before := len(store.All())
	if _, err := (approval.Service{Store: store}).RecordInput(key, approval.Allow, "", "response", "digest", "ops", "", "", auth, claim); err == nil || len(store.All()) != before {
		t.Fatalf("V8 claim write err=%v events=%d", err, len(store.All()))
	}
	signed := &question.Verification{Signature: &question.Signature{KeyID: "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9", SignedAt: "2026-10-08T00:00:00Z", Nonce: "000102030405060708090a0b0c0d0e0f", Sig: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}}
	if _, err := (approval.Service{Store: store}).RecordInput(key, approval.Allow, "", "response", "digest", "ops", "", "", auth, signed); err == nil || len(store.All()) != before {
		t.Fatalf("V8 signed write err=%v events=%d", err, len(store.All()))
	}
}
