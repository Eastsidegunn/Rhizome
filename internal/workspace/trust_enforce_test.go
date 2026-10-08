package workspace

import (
	"testing"

	"rhizome/internal/question"
	"rhizome/internal/trust"
)

func TestEnforcedRejectsUnsignedAllowDenyFRRHZ151(t *testing.T) {
	for _, tc := range []struct {
		kind, reason string
	}{
		{kind: "gate.approve"},
		{kind: "gate.reject", reason: "no"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			s, pending := pendingGateFixture(t)
			before := len(s.All())
			res, err := RelayIntentHooks(s, Intent{Kind: tc.kind, GateID: pending.ID, Digest: pending.RequestDigest, Reason: tc.reason}, "operator", trust.Authority{}, RelayHooks{EnforceJANUS: true})
			if err != nil || res.Accepted || res.Reason != "verified decision required" {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			if len(s.All()) != before {
				t.Fatalf("unsigned %s wrote %d events", tc.kind, len(s.All())-before)
			}
		})
	}
}

func TestEnforcementLeavesInternalQuestionGatesUnaffectedFRRHZ151(t *testing.T) {
	s, _ := pendingGateFixture(t)
	q, err := (question.Service{Store: s}).Ask("internal", "body", "approve", "m", "", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := RelayIntentHooks(s, Intent{Kind: "gate.approve", GateID: q.ID, Digest: q.Digest}, "operator", trust.Authority{}, RelayHooks{EnforceJANUS: true})
	if err != nil || !res.Accepted {
		t.Fatalf("internal question blocked: %+v %v", res, err)
	}
}

func TestEnforcementAbsentPreservesUnsignedJANUSBehaviorFRRHZ151(t *testing.T) {
	s, pending := pendingGateFixture(t)
	res, err := RelayIntentHooks(s, Intent{Kind: "gate.approve", GateID: pending.ID, Digest: pending.RequestDigest}, "operator", trust.Authority{}, RelayHooks{})
	if err != nil || !res.Accepted {
		t.Fatalf("default policy changed: %+v %v", res, err)
	}
}
