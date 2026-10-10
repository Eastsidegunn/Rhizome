package secretwire

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

const (
	testOpID    = "op-0123456789abcdef0123456789abcdef"
	testPlaneID = "pl-00112233445566778899aabbccddeeff"
)

// FR-RHZ-136: §0.1 G1–G17, each accepting and rejecting its boundary cases.
func TestGrammarFRRHZ136(t *testing.T) {
	hex32 := strings.Repeat("a", 32)
	str := []struct {
		name  string
		valid func(string) bool
		ok    []string
		bad   []string
	}{
		{"G1", ValidOpID, []string{"op-" + hex32}, []string{"", "op-" + hex32[:31], "op-" + strings.Repeat("A", 32), "op-" + hex32 + "0", "xop-" + hex32, "op-" + hex32 + "\n"}},
		{"G2", ValidRef, []string{"sref-" + hex32[:24]}, []string{"sref-" + hex32[:23], "sref-" + hex32[:25], "sref-" + strings.Repeat("G", 24)}},
		{"G3", ValidName, []string{"a", "0x", "a.b_c-d", "a" + strings.Repeat("z", 63)}, []string{"", ".a", "-a", "A", "a b", "a" + strings.Repeat("z", 64), "é"}},
		{"G5", ValidRefKind, []string{"provider-key", "gateway-credential", "other"}, []string{"", "Other", "key"}},
		{"G6", ValidReasonCode, []string{"unused", "exposed", "rotated-out", "other"}, []string{"", "rotated"}},
		{"G7", ValidResult, []string{"ok", "failed", "rejected"}, []string{"", "OK", "fail"}},
		{"G8", ValidFailureCode, []string{"crash-before-effect", "backend-error", "rate-limited", "no-tty", "invalid-state", "quarantined"}, []string{"", "timeout"}},
		{"G9", ValidOrigin, []string{"plane-direct", "attested"}, []string{"", "direct"}},
		{"G10", ValidPlaneID, []string{"pl-" + hex32}, []string{"pl-" + hex32[:31], "op-" + hex32}},
		{"G13", ValidObservedAt, []string{"2026-10-10T12:34:56Z", "2024-02-29T00:00:00Z"}, []string{"2026-10-10T12:34:56+00:00", "2026-10-10T12:34:56.1Z", "2026-13-10T12:34:56Z", "2025-02-29T00:00:00Z", "2026-10-10t12:34:56Z"}},
		{"G14", ValidActor, []string{"operator"}, []string{"", "Operator"}},
		{"G14 payload", ValidActorRef, []string{ActorRefPrefix + "operator"}, []string{ActorRefPrefix, "operator", "x" + ActorRefPrefix + "operator"}},
		{"G17", ValidOp, []string{"register", "rotate", "revoke"}, []string{"", "delete"}},
	}
	for _, c := range str {
		for _, s := range c.ok {
			if !c.valid(s) {
				t.Errorf("%s rejected a valid value", c.name)
			}
		}
		for _, s := range c.bad {
			if c.valid(s) {
				t.Errorf("%s accepted an invalid value", c.name)
			}
		}
	}
	tags := []struct {
		tags []string
		ok   bool
	}{
		{[]string{"a"}, true}, {[]string{"a", "b", "c", "d", "e", "f", "g", "h"}, true},
		{nil, false}, {[]string{}, false}, {[]string{"b", "a"}, false}, {[]string{"a", "a"}, false},
		{[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, false}, {[]string{"A"}, false},
	}
	for i, c := range tags {
		if ValidScopeTags(c.tags) != c.ok {
			t.Errorf("G4 case %d: want %v", i, c.ok)
		}
	}
	ints := []struct {
		name  string
		valid func(uint64) bool
		ok    []uint64
		bad   []uint64
	}{
		{"G11", ValidSeq, []uint64{1, MaxSafeInt}, []uint64{0, MaxSafeInt + 1}},
		{"G12", ValidVersion, []uint64{0, 1_000_000}, []uint64{1_000_001}},
		{"G16", ValidJournalSeq, []uint64{0, MaxSafeInt}, []uint64{MaxSafeInt + 1}},
	}
	for _, c := range ints {
		for _, n := range c.ok {
			if !c.valid(n) {
				t.Errorf("%s rejected a valid value", c.name)
			}
		}
		for _, n := range c.bad {
			if c.valid(n) {
				t.Errorf("%s accepted an invalid value", c.name)
			}
		}
	}
}

// FR-RHZ-136: DeriveRef fixed vector — recomputed from the §0.1 formula and
// pinned as a literal so a formula drift cannot pass silently.
func TestDeriveRefVectorFRRHZ136(t *testing.T) {
	sum := sha256.Sum256([]byte("rhz-secret-ref-v1\x00" + testOpID))
	fromFormula := "sref-" + hex.EncodeToString(sum[:])[:24]
	const pinned = "sref-8fa71e2e7eef2abc88a2c302"
	if got := DeriveRef(testOpID); got != fromFormula || got != pinned {
		t.Fatalf("DeriveRef drifted from §0.1")
	}
	if !ValidRef(DeriveRef(testOpID)) {
		t.Fatal("DeriveRef output violates G2")
	}
}
