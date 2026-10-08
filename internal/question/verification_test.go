package question_test

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/question"
)

func TestAnsweredPayloadWithoutVerificationByteIdenticalFRRHZ129(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "operator", q.Digest); err != nil {
		t.Fatal(err)
	}
	want := `{"Decision":"approve","Reason":"","ActorRef":"unverified-local-operator:operator","Digest":"` + q.Digest + `"}`
	if got := string(s.List("question", q.ID)[1].Payload); got != want {
		t.Fatalf("legacy payload changed\n got: %s\nwant: %s", got, want)
	}
}

func TestVerificationPayloadRoundTripBothClaimKindsFRRHZ129(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim *question.Verification
	}{
		{"relayed", &question.Verification{ClaimKind: "relayed", OriginClaim: "H", OriginChannel: "board", RelayChain: []string{"ops"}, ObservedAt: "2026-10-08T03:12:00Z"}},
		{"session-direct", &question.Verification{ClaimKind: "session-direct", OriginClaim: "H", OriginChannel: "dev-session", SessionRef: "session:example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &events.Store{}
			q, err := (question.Service{Store: s}).Ask(tc.name, "body", "recommend", "", "", "operator", "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "ops", q.Digest, tc.claim)
			if err != nil {
				t.Fatal(err)
			}
			if got.Verification == nil || got.Verification.ClaimKind != tc.name {
				t.Fatalf("claim not replayed: %+v", got.Verification)
			}
			if tc.name == "relayed" && got.Verification.RelayChain[0] != "unverified-local-operator:ops" {
				t.Fatalf("relay chain not normalized: %+v", got.Verification)
			}
			if !strings.Contains(string(s.List("question", q.ID)[1].Payload), `"Verification":{"ClaimKind":"`+tc.name+`"`) {
				t.Fatalf("payload key casing/shape: %s", s.List("question", q.ID)[1].Payload)
			}
		})
	}
}

func TestVerificationReplayRejectsEachRuleViolationFRRHZ129(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "ops", q.Digest, &question.Verification{ClaimKind: "relayed", OriginClaim: "H", OriginChannel: "board", RelayChain: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	base := s.List("question", q.ID)
	cases := map[string]string{
		"malformed non-object":    `[]`,
		"V1 missing claimKind":    `{"OriginClaim":"H","RelayChain":["unverified-local-operator:ops"]}`,
		"V2 non-H originClaim":    `{"ClaimKind":"relayed","OriginClaim":"operator","RelayChain":["unverified-local-operator:ops"]}`,
		"V3 empty relayChain":     `{"ClaimKind":"relayed","OriginClaim":"H","RelayChain":[]}`,
		"V4 blank sessionRef":     `{"ClaimKind":"session-direct","OriginClaim":"H","SessionRef":" "}`,
		"V5 wrong channel":        `{"ClaimKind":"session-direct","OriginClaim":"H","OriginChannel":"ops-session","SessionRef":"session:x"}`,
		"V6 malformed observedAt": `{"ClaimKind":"session-direct","OriginClaim":"H","SessionRef":"session:x","ObservedAt":"yesterday"}`,
		"V7 unknown key":          `{"ClaimKind":"session-direct","OriginClaim":"H","SessionRef":"session:x","Status":"verified"}`,
		"V7 wrong key casing":     `{"claimKind":"session-direct","OriginClaim":"H","SessionRef":"session:x"}`,
	}
	for name, verification := range cases {
		t.Run(name, func(t *testing.T) {
			log := append([]events.Event(nil), base...)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(log[1].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			payload["Verification"] = json.RawMessage(verification)
			log[1].Payload, _ = json.Marshal(payload)
			if _, err := question.Replay(log); err == nil {
				t.Fatal("malformed verification replayed")
			}
		})
	}
}

func TestVerificationReplayRejectsMismatchedLastRelayChainEntryFRRHZ129(t *testing.T) {
	log := answeredLogWithoutVerificationFRRHZ129(t)
	setReplayVerificationFRRHZ129(t, log, `{"ClaimKind":"relayed","OriginClaim":"H","RelayChain":["unverified-local-operator:someone-else"]}`)
	if _, err := question.Replay(log); err == nil {
		t.Fatal("mismatched final relay actor replayed")
	}
}

func TestVerificationReplayRejectsFieldsFromOtherClaimKindFRRHZ129(t *testing.T) {
	for name, raw := range map[string]string{
		"relayed with sessionRef":        `{"ClaimKind":"relayed","OriginClaim":"H","RelayChain":["unverified-local-operator:ops"],"SessionRef":"session:x"}`,
		"session-direct with relayChain": `{"ClaimKind":"session-direct","OriginClaim":"H","SessionRef":"session:x","RelayChain":["unverified-local-operator:ops"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			log := answeredLogWithoutVerificationFRRHZ129(t)
			setReplayVerificationFRRHZ129(t, log, raw)
			if _, err := question.Replay(log); err == nil {
				t.Fatal("claim-kind-incompatible fields replayed")
			}
		})
	}
}

func TestVerificationReplayRejectsNullFRRHZ129(t *testing.T) {
	log := answeredLogWithoutVerificationFRRHZ129(t)
	setReplayVerificationFRRHZ129(t, log, `null`)
	if _, err := question.Replay(log); err == nil {
		t.Fatal("null Verification replayed")
	}
}

func TestAnswerInvalidVerificationZeroJournalGrowthFRRHZ129(t *testing.T) {
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	_, err = (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "ops", q.Digest, &question.Verification{
		ClaimKind: "relayed", OriginClaim: "H", RelayChain: []string{"someone-else"},
	})
	if err == nil {
		t.Fatal("invalid verification accepted")
	}
	if after := len(s.All()); after != before {
		t.Fatalf("rejected Answer grew journal: before=%d after=%d", before, after)
	}
}

func answeredLogWithoutVerificationFRRHZ129(t *testing.T) []events.Event {
	t.Helper()
	s := &events.Store{}
	q, err := (question.Service{Store: s}).Ask("title", "body", "recommend", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "ops", q.Digest); err != nil {
		t.Fatal(err)
	}
	return s.List("question", q.ID)
}

func setReplayVerificationFRRHZ129(t *testing.T, log []events.Event, raw string) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(log[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["Verification"] = json.RawMessage(raw)
	var err error
	log[1].Payload, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
}
