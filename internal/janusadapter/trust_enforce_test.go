package janusadapter

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

const enforcementAnchorJSON151 = `{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`

func signedApproval151(t *testing.T, decision approval.Decision, reason string) (*events.Store, *trust.Verifier, approval.Ref) {
	t.Helper()
	anchor, err := trust.ParseAnchor([]byte(enforcementAnchorJSON151))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	store := &events.Store{Guard: verifier}
	if err = trust.EnsureGenesis(store, anchor); err != nil {
		t.Fatal(err)
	}
	key := approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "verified-" + string(decision)}
	id := approval.IDFor(key)
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	private := ed25519.NewKeyFromSeed(seed)
	keyID, err := trust.KeyID(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := verifier.TrustSummary(store)
	if err != nil || summary == nil {
		t.Fatalf("trust summary: %+v %v", summary, err)
	}
	signedAt := time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	nonce := "202122232425262728292a2b2c2d2e2f"
	message := trust.DecisionMessage(summary.JournalID, "approval", id, gateDigest, string(decision), reason, keyID, signedAt, nonce)
	verification := &question.Verification{Signature: &question.Signature{
		KeyID: keyID, SignedAt: signedAt, Nonce: nonce, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message)),
	}}
	a, err := (approval.Service{Store: store}).RecordInput(key, decision, reason, "resp-verified-"+string(decision), gateDigest, "signer", "corr", "", trust.Authority{}, verification)
	if err != nil {
		t.Fatal(err)
	}
	return store, verifier, a
}

func TestLoopFailClosedPreDispatchAndPreSubmitFRRHZ151(t *testing.T) {
	for _, state := range []approval.State{approval.InputRecorded, approval.Dispatched} {
		t.Run(string(state), func(t *testing.T) {
			s := &events.Store{}
			key := approval.RequestKey{TraceID: replayTrace, SpanID: replaySpan, RequestID: "enforced-" + string(state)}
			a, err := (approval.Service{Store: s}).RecordInput(key, approval.Allow, "", "resp-enforced", gateDigest, "operator", "corr", "", noAuthority())
			if err != nil {
				t.Fatal(err)
			}
			if state == approval.Dispatched {
				if _, err = (approval.Service{Store: s}).Dispatch(a.ID, a.ResponseID); err != nil {
					t.Fatal(err)
				}
			}
			dials := 0
			l, reports := newTestLoop(s, emptyReplay, hookedFake(func() { dials++ }, func(map[string]any) map[string]any { return nil }))
			l.Trust = trust.NewAnchorless()
			l.EnforceAll = true
			l.Tick()
			got := approvalRef(t, s, a.ID)
			if got.State != state || dials != 0 {
				t.Fatalf("state=%s want=%s dials=%d", got.State, state, dials)
			}
			if reports.count() != 1 || reports.all()[0] != "approval|"+a.ID+"|verified decision required" {
				t.Fatalf("reports=%v", reports.all())
			}
		})
	}
}

func TestLoopEnforceAllSubmitsVerifiedApprovalFRRHZ151(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision approval.Decision
		reason   string
	}{
		{name: "allow reaches dispatched", decision: approval.Allow},
		{name: "deny reaches submit", decision: approval.Deny, reason: "policy says no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, verifier, a := signedApproval151(t, tc.decision, tc.reason)
			dials := 0
			l, reports := newTestLoop(s, emptyReplay, hookedFake(func() { dials++ }, func(q map[string]any) map[string]any {
				if q["op"] != "submit" || q["decision"] != string(tc.decision) || q["reason"] != tc.reason {
					t.Errorf("submit wire: %v", q)
				}
				return map[string]any{"status": "decided", "decision": string(tc.decision), "reason": tc.reason}
			}))
			l.Trust = verifier
			l.EnforceAll = true
			l.Tick()
			got := approvalRef(t, s, a.ID)
			if got.State != approval.Dispatched || dials != 1 {
				t.Fatalf("state=%s want=%s dials=%d", got.State, approval.Dispatched, dials)
			}
			if reports.count() != 0 {
				t.Fatalf("reports=%v", reports.all())
			}
		})
	}
}
