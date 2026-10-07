package execution

// RHZ-096 (FR-RHZ-124): additive provenance on the execution.intent payload.

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/policy"
)

// smokeIntentBytes is the exact intent payload for the legacy direct caller
// policy as written at RHZ-092 (pinned: provenance must not alter it).
const smokeIntentBytes = `{"MissionID":"m1","IdempotencyKey":"smoke-key","ExternalID":"","Cursor":"","Summary":"","SourceRef":"","State":"intent","Policy":{"Capabilities":["fs:workspace"],"Domains":[],"Budget":200000,"Timeout":600000,"MaxDepth":2,"Units":"tokens-ms-v1","Unlimited":false}}`

func smokePolicy() policy.Policy {
	return policy.Policy{Capabilities: []string{"fs:workspace"}, Budget: 200000, Timeout: 600000, MaxDepth: 2, Units: "tokens-ms-v1"}
}

func provFixture(req, ceil policy.Policy) Provenance {
	return Provenance{Ceiling: ceil, Requested: req, Effective: policy.Merge(req, ceil).Policy, ProfileID: "manual", ProfileHash: strings.Repeat("ab", 32), ExecConfigDigest: "sha256:" + strings.Repeat("0f", 32), Actor: "op"}
}

// P2: a legacy intent (hand-written, no provenance) replays exactly as
// before with Provenance nil, and IntentWithPolicy writes the old bytes.
func TestLegacyIntentReplayFRRHZ124(t *testing.T) {
	legacy := `{"MissionID":"m1","IdempotencyKey":"k","ExternalID":"","Cursor":"","Summary":"","SourceRef":"","State":"intent","Policy":{"Capabilities":["run"],"Domains":null,"Budget":1,"Timeout":1,"MaxDepth":0,"Units":"","Unlimited":false}}`
	log := []events.Event{{AggregateType: "execution", AggregateID: "exec-k", Revision: 1, Type: "execution.intent", Payload: json.RawMessage(legacy)}}
	r, err := Replay(log)
	if err != nil || r.Provenance != nil || r.MissionID != "m1" || r.IdempotencyKey != "k" || r.State != Intent || r.Policy.Budget != 1 || r.Revision != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	s := Service{Store: missionStore()}
	p := smokePolicy()
	ref, err := s.IntentWithPolicy("m1", "smoke-key", p, p)
	if err != nil || ref.Provenance != nil {
		t.Fatalf("%+v %v", ref, err)
	}
	got := string(s.Store.List("execution", ref.ID)[0].Payload)
	if got != smokeIntentBytes {
		t.Fatalf("IntentWithPolicy bytes changed:\n got %s\nwant %s", got, smokeIntentBytes)
	}
}

// Write-time validation: provenance must describe exactly this intent;
// replay rejects provenance on any later event (no re-stamping).
func TestProvenanceWriteValidationFRRHZ124(t *testing.T) {
	ceil := smokePolicy()
	req := ceil
	req.Budget = 1000
	s := Service{Store: missionStore()}
	bad := map[string]func(*Provenance){
		"effective mismatch": func(p *Provenance) { p.Effective.Budget = 200000 },
		"ceiling mismatch":   func(p *Provenance) { p.Ceiling.Timeout = 1 },
		"requested mismatch": func(p *Provenance) { p.Requested.MaxDepth = 1 },
		"no profile id":      func(p *Provenance) { p.ProfileID = " " },
		"no profile hash":    func(p *Provenance) { p.ProfileHash = "" },
		"raw digest":         func(p *Provenance) { p.ExecConfigDigest = strings.Repeat("0f", 32) },
	}
	for name, mut := range bad {
		p := provFixture(req, ceil)
		mut(&p)
		if _, err := s.IntentWithProvenance("m1", "k-"+name, req, ceil, p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if n := len(s.Store.All()); n != 1 {
		t.Fatalf("rejections wrote: %d events", n)
	}
	ref, err := s.IntentWithProvenance("m1", "k", req, ceil, provFixture(req, ceil))
	if err != nil || ref.Provenance == nil || ref.Provenance.Effective.Budget != 1000 || ref.Policy.Budget != 1000 || ref.ID != "exec-k" || ref.IdempotencyKey != "k" {
		t.Fatalf("%+v %v", ref, err)
	}
	log := s.Store.List("execution", ref.ID)
	inj, _ := json.Marshal(map[string]any{"MissionID": "m1", "IdempotencyKey": "k", "Target": "janus", "CorrelationID": "c", "ScopedKey": scoped("janus", "k"), "provenance": provFixture(req, ceil)})
	forged := append(append([]events.Event(nil), log...), events.Event{AggregateType: "execution", AggregateID: ref.ID, Revision: 2, Type: "execution.dispatch_claimed", Payload: inj})
	if _, err := Replay(forged); err == nil {
		t.Fatal("provenance on a later event accepted")
	}
}
