package approval

import (
	"encoding/json"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/question"
)

func TestApprovalVerificationPayloadReplayAndV8FRRHZ129(t *testing.T) {
	k := RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "claim"}
	s := &events.Store{}
	claim := &question.Verification{ClaimKind: "relayed", OriginClaim: "H", OriginChannel: "ops-session", RelayChain: []string{"ops"}}
	r, err := (Service{Store: s}).RecordInput(k, Allow, "", "resp", "digest", "ops", "corr", "", noAuthority(), claim)
	if err != nil || r.Verification == nil || r.Verification.ClaimKind != "relayed" {
		t.Fatalf("claim round trip: %+v %v", r, err)
	}
	log := s.List("approval", r.ID)
	if _, err := Replay(log); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(log[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["ActorVerified"] = json.RawMessage("true")
	log[0].Payload, _ = json.Marshal(payload)
	if _, err := Replay(log); err == nil {
		t.Fatal("V8 claim plus ActorVerified replayed")
	}

}

func TestApprovalReplayRejectsUnknownVerificationKeyFRRHZ129(t *testing.T) {
	k := RequestKey{TraceID: "1123456789abcdef0123456789abcdef", SpanID: "1123456789abcdef", RequestID: "unknown"}
	s := &events.Store{}
	r, err := (Service{Store: s}).RecordInput(k, Allow, "", "resp", "digest", "ops", "", "", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	log := s.List("approval", r.ID)
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(log[0].Payload, &payload)
	payload["Verification"] = json.RawMessage(`{"ClaimKind":"relayed","OriginClaim":"H","RelayChain":["unverified-local-operator:ops"],"assurance":"key"}`)
	log[0].Payload, _ = json.Marshal(payload)
	if _, err := Replay(log); err == nil {
		t.Fatal("unknown lowerCamel payload key replayed")
	}
}
