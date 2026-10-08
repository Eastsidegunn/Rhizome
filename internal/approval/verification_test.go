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
	r, err := (Service{Store: s}).RecordInput(k, Allow, "", "resp", "digest", "ops", "corr", "", false, claim)
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

	before := len(s.All())
	if _, err := (Service{Store: s}).RecordInput(RequestKey{TraceID: k.TraceID, SpanID: k.SpanID, RequestID: "v8-write"}, Allow, "", "resp2", "digest", "ops", "", "", true, claim); err == nil || len(s.All()) != before {
		t.Fatalf("V8 write not rejected atomically: err=%v events=%d->%d", err, before, len(s.All()))
	}
}

func TestApprovalReplayRejectsUnknownVerificationKeyFRRHZ129(t *testing.T) {
	k := RequestKey{TraceID: "1123456789abcdef0123456789abcdef", SpanID: "1123456789abcdef", RequestID: "unknown"}
	s := &events.Store{}
	r, err := (Service{Store: s}).RecordInput(k, Allow, "", "resp", "digest", "ops", "", "", false)
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
