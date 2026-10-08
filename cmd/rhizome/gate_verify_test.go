package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/gaterequest"
	"rhizome/internal/journal"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

func signedGateJournal152(t *testing.T) (journalPath, anchorPath, gateID, handle, journalID, keyID string) {
	t.Helper()
	dir := t.TempDir()
	journalPath, anchorPath = filepath.Join(dir, "journal.ndjson"), filepath.Join(dir, "anchor.json")
	writeServeAnchor148(t, anchorPath, serveAnchorJSON148)
	anchor, err := trust.ParseAnchor([]byte(serveAnchorJSON148))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	j, err := journal.OpenGuarded(journalPath, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err = trust.EnsureGenesis(j, anchor); err != nil {
		t.Fatal(err)
	}
	q, err := (question.Service{Store: j}).Ask("verify", "signed gate", "approve", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := verifier.TrustSummary(j)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	private := ed25519.NewKeyFromSeed(seed)
	keyID, err = trust.KeyID(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	signedAt, nonce := time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), "000102030405060708090a0b0c0d0e0f"
	message := trust.DecisionMessage(summary.JournalID, "question", q.ID, q.Digest, "approve", "", keyID, signedAt, nonce)
	verification := &question.Verification{Signature: &question.Signature{KeyID: keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message))}}
	if _, err = (question.Service{Store: j}).Answer(q.ID, question.Approve, "", "signer", q.Digest, verification); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(q.ID))
	return journalPath, anchorPath, q.ID, "q-" + hex.EncodeToString(sum[:])[:8], summary.JournalID, keyID
}

func approvalGateJournal152(t *testing.T) (journalPath, anchorPath string, ids map[string]string) {
	t.Helper()
	dir := t.TempDir()
	journalPath, anchorPath = filepath.Join(dir, "journal.ndjson"), filepath.Join(dir, "anchor.json")
	writeServeAnchor148(t, anchorPath, serveAnchorJSON148)
	anchor, err := trust.ParseAnchor([]byte(serveAnchorJSON148))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	j, err := journal.OpenGuarded(journalPath, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err = trust.EnsureGenesis(j, anchor); err != nil {
		t.Fatal(err)
	}
	digest := "hx-args-digest-v1:gate-verify"
	pendingKey := approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "pending"}
	pending, err := (gaterequest.Service{Store: j}).Record(gaterequest.Ref{
		Key: pendingKey, RequestDigest: digest, MissionID: "mission", ExecutionID: "execution", DecisionID: "decision",
	})
	if err != nil {
		t.Fatal(err)
	}
	unsignedKey := approval.RequestKey{TraceID: pendingKey.TraceID, SpanID: pendingKey.SpanID, RequestID: "unsigned"}
	unsigned, err := (approval.Service{Store: j}).RecordInput(unsignedKey, approval.Allow, "", "response-unsigned", digest, "operator", "", "", trust.Authority{})
	if err != nil {
		t.Fatal(err)
	}
	verifiedKey := approval.RequestKey{TraceID: pendingKey.TraceID, SpanID: pendingKey.SpanID, RequestID: "verified"}
	verifiedID := approval.IDFor(verifiedKey)
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	private := ed25519.NewKeyFromSeed(seed)
	keyID, err := trust.KeyID(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := verifier.TrustSummary(j)
	if err != nil || summary == nil {
		t.Fatalf("trust summary: %+v %v", summary, err)
	}
	signedAt := time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	nonce := "101112131415161718191a1b1c1d1e1f"
	message := trust.DecisionMessage(summary.JournalID, "approval", verifiedID, digest, "allow", "", keyID, signedAt, nonce)
	verification := &question.Verification{Signature: &question.Signature{
		KeyID: keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message)),
	}}
	verified, err := (approval.Service{Store: j}).RecordInput(verifiedKey, approval.Allow, "", "response-verified", digest, "signer", "", "", trust.Authority{}, verification)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	return journalPath, anchorPath, map[string]string{"pending": pending.ID, "verified": verified.ID, "unverified": unsigned.ID}
}

func TestGateVerifyStatusTableFRRHZ152(t *testing.T) {
	journalPath, anchorPath, gateID, handle, journalID, keyID := signedGateJournal152(t)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
		want []string
	}{
		{name: "anchored verified id", args: []string{"-journal", journalPath, "-trust-anchor", anchorPath, "-show-domain", gateID}, code: 0,
			want: []string{"gate=" + gateID, "consumer=question", "decision=approve", "digestVersion=v2", "status=verified", "assurance=key", "keyId=" + keyID, "keyRevokedNow=false", "anchor=matched", "journalId=" + journalID, "genesisKeyId=" + keyID}},
		{name: "unanchored chain valid handle", args: []string{"-journal", journalPath, handle}, code: 3,
			want: []string{"gate=" + gateID, "status=chain-valid", "anchor=absent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := gateVerify(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout.String(), want+"\n") {
					t.Errorf("stdout missing %q: %q", want, stdout.String())
				}
			}
		})
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("gate verify changed journal bytes: %v", err)
	}
}

func TestGateVerifyMissingCreatesNothingFRRHZ152(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(string) []string
	}{
		{name: "journal", args: func(path string) []string { return []string{"-journal", path, "q-missing"} }},
		{name: "data-dir", args: func(path string) []string { return []string{"-data-dir", path, "q-missing"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			var stdout, stderr bytes.Buffer
			if code := gateVerify(tc.args(root), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "journal not found:") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("absent path was created: %v", err)
			}
			if _, err := os.Stat(root + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("lock was created: %v", err)
			}
		})
	}
}

func TestGateVerifyLockHeldExit3FRRHZ152(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireJournalLock(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var stdout, stderr bytes.Buffer
	if code := gateVerify([]string{"-journal", path, "q-any"}, &stdout, &stderr); code != 3 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	want := "journal in use: stop serve, or read the serve's derived status from GET /v1/workspace\n"
	if stdout.String() != want {
		t.Fatalf("stdout=%q want=%q", stdout.String(), want)
	}
}

func TestGateVerifyStrictOpenFRRHZ152(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{name: "torn tail", content: `{"sequence":1`, want: "journal truncated line"},
		{name: "corrupt line", content: "not-json\n", want: "decode journal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.ndjson")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := gateVerify([]string{"-journal", path, "q-any"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestGateVerifyPendingAndNotVerifiedFRRHZ152(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	j, err := journal.OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := (question.Service{Store: j}).Ask("pending", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := (question.Service{Store: j}).Ask("unsigned", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (question.Service{Store: j}).Answer(unsigned.ID, question.Approve, "", "operator", unsigned.Digest); err != nil {
		t.Fatal(err)
	}
	claimed, err := (question.Service{Store: j}).Ask("claimed", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	claim := &question.Verification{ClaimKind: "relayed", OriginClaim: "H", OriginChannel: "board", RelayChain: []string{"operator"}}
	if _, err = (question.Service{Store: j}).Answer(claimed.ID, question.Approve, "", "operator", claimed.Digest, claim); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, status string }{{pending.ID, "pending"}, {unsigned.ID, "unverified"}, {claimed.ID, "claimed"}} {
		var stdout, stderr bytes.Buffer
		if code := gateVerify([]string{"-journal", path, tc.id}, &stdout, &stderr); code != 3 || !strings.Contains(stdout.String(), "status="+tc.status+"\n") {
			t.Fatalf("id=%s exit=%d stdout=%q stderr=%q", tc.id, code, stdout.String(), stderr.String())
		}
	}
}

func TestGateVerifyApprovalHXV1StatusesFRRHZ152(t *testing.T) {
	journalPath, anchorPath, ids := approvalGateJournal152(t)
	for _, tc := range []struct {
		name, decision, status string
		code                   int
	}{
		{name: "pending", status: "pending", code: 3},
		{name: "verified", decision: "allow", status: "verified", code: 0},
		{name: "unverified", decision: "allow", status: "unverified", code: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := gateVerify([]string{"-journal", journalPath, "-trust-anchor", anchorPath, ids[tc.name]}, &stdout, &stderr)
			if code != tc.code || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			for _, want := range []string{"consumer=approval", "decision=" + tc.decision, "digestVersion=hx-v1", "status=" + tc.status, "anchor=matched"} {
				if !strings.Contains(stdout.String(), want+"\n") {
					t.Errorf("stdout missing %q: %q", want, stdout.String())
				}
			}
		})
	}
}

func TestGateVerifyAnchoredRequiresTrustGenesisFRRHZ152(t *testing.T) {
	dir := t.TempDir()
	journalPath, anchorPath := filepath.Join(dir, "journal.ndjson"), filepath.Join(dir, "anchor.json")
	writeServeAnchor148(t, anchorPath, serveAnchorJSON148)
	j, err := journal.OpenGuarded(journalPath, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	q, err := (question.Service{Store: j}).Ask("anchorless", "body", "", "", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := gateVerify([]string{"-journal", journalPath, "-trust-anchor", anchorPath, "-show-domain", q.ID}, &stdout, &stderr); code != 1 || stderr.String() != "trust genesis missing\n" || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestGateVerifyFailureRowsFRRHZ152(t *testing.T) {
	journalPath, _, gateID, _, _, _ := signedGateJournal152(t)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(trust.Anchor{Format: trust.AnchorFormat, Principal: "H", Algorithm: trust.AlgorithmEd25519, Assurance: trust.AssuranceKey, PublicKey: base64.StdEncoding.EncodeToString(der)})
	otherPath := filepath.Join(filepath.Dir(journalPath), "other-anchor.json")
	if err = os.WriteFile(otherPath, other, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := gateVerify([]string{"-journal", journalPath, "-trust-anchor", otherPath, gateID}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "trust anchor does not match genesis") {
		t.Fatalf("mismatch exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := gateVerify([]string{"-journal", journalPath, "q-not-found"}, &stdout, &stderr); code != 1 || stderr.String() != "gate not found\n" {
		t.Fatalf("missing exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	lockFailure := filepath.Join(t.TempDir(), "journal.ndjson")
	if err = os.WriteFile(lockFailure, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(lockFailure+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := gateVerify([]string{"-journal", lockFailure, "q-any"}, &stdout, &stderr); code != 1 {
		t.Fatalf("lock error exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestGateVerifyUsageExit2FRRHZ152(t *testing.T) {
	for _, args := range [][]string{nil, {"one", "two"}, {"-journal", "x", "-data-dir", "y", "q"}, {"-trust-anchor=", "q"}} {
		if code := gateVerify(args, io.Discard, io.Discard); code != 2 {
			t.Fatalf("args=%v exit=%d", args, code)
		}
	}
	if code := run([]string{"gate"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("gate without verify exit=%d", code)
	}
}
