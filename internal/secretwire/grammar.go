// Package secretwire is the single definition of the secret-management wire
// vocabulary (RHZ-107 G1, FR-RHZ-136): the closed field grammar G1–G17
// (SCP §0.1), DeriveRef, the strict JSON shape check V-J/V-K (§0.1a), and the
// channel wire v1 frames (§3.3, W3–W5, W9). It imports the standard library
// only (§7 R8) so that Rhizome and the secret plane share it verbatim.
//
// No surface here carries a secret value, and no error string echoes input
// bytes, key names or numbers (S2, S5): every error is a fixed enum code.
package secretwire

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"time"
)

// MaxSafeInt is 2^53−1, the upper bound of G11 and G16.
const MaxSafeInt = 1<<53 - 1

// MaxVersion is the upper bound of G12.
const MaxVersion = 1_000_000

// ActorRefPrefix is the payload prefix of G14 (ActorRef = prefix + actor).
const ActorRefPrefix = "unverified-local-operator:"

var (
	opIDRe       = regexp.MustCompile(`^op-[0-9a-f]{32}$`)                                        // G1
	refRe        = regexp.MustCompile(`^sref-[0-9a-f]{24}$`)                                      // G2
	nameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)                              // G3
	planeIDRe    = regexp.MustCompile(`^pl-[0-9a-f]{32}$`)                                        // G10
	observedAtRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`) // G13
)

// Enumerated values (G5–G9, G17).
const (
	RefKindProviderKey       = "provider-key"
	RefKindGatewayCredential = "gateway-credential"
	RefKindOther             = "other"

	ReasonUnused     = "unused"
	ReasonExposed    = "exposed"
	ReasonRotatedOut = "rotated-out"
	ReasonOther      = "other"

	ResultOK       = "ok"
	ResultFailed   = "failed"
	ResultRejected = "rejected"

	FailureCrashBeforeEffect = "crash-before-effect"
	FailureBackendError      = "backend-error"
	FailureRateLimited       = "rate-limited"
	FailureNoTTY             = "no-tty"
	FailureInvalidState      = "invalid-state"
	FailureQuarantined       = "quarantined"

	// OriginAttested is in G9 because SCP §11 B was approved.
	OriginPlaneDirect = "plane-direct"
	OriginAttested    = "attested"

	OpRegister = "register"
	OpRotate   = "rotate"
	OpRevoke   = "revoke"
)

func in(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

// ValidOpID reports G1.
func ValidOpID(s string) bool { return opIDRe.MatchString(s) }

// ValidRef reports the G2 format. V-A additionally requires the value to equal
// DeriveRef of the introducing opId; that check belongs to the caller.
func ValidRef(s string) bool { return refRe.MatchString(s) }

// ValidName reports G3.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidScopeTags reports G4 for a present list: 1–8 G3 elements, strictly
// ascending (sorted, no duplicates). An absent list is represented by an
// omitted key, never by an empty array.
func ValidScopeTags(tags []string) bool {
	if len(tags) < 1 || len(tags) > 8 {
		return false
	}
	for i, tag := range tags {
		if !ValidName(tag) || (i > 0 && tags[i-1] >= tag) {
			return false
		}
	}
	return true
}

// ValidRefKind reports G5.
func ValidRefKind(s string) bool {
	return in(s, RefKindProviderKey, RefKindGatewayCredential, RefKindOther)
}

// ValidReasonCode reports G6.
func ValidReasonCode(s string) bool {
	return in(s, ReasonUnused, ReasonExposed, ReasonRotatedOut, ReasonOther)
}

// ValidResult reports G7.
func ValidResult(s string) bool { return in(s, ResultOK, ResultFailed, ResultRejected) }

// ValidFailureCode reports G8. Presence (required iff Result=="failed") is a
// shape rule enforced by the callers' specs.
func ValidFailureCode(s string) bool {
	return in(s, FailureCrashBeforeEffect, FailureBackendError, FailureRateLimited,
		FailureNoTTY, FailureInvalidState, FailureQuarantined)
}

// ValidOrigin reports G9.
func ValidOrigin(s string) bool { return in(s, OriginPlaneDirect, OriginAttested) }

// ValidPlaneID reports G10.
func ValidPlaneID(s string) bool { return planeIDRe.MatchString(s) }

// ValidSeq reports G11.
func ValidSeq(n uint64) bool { return n >= 1 && n <= MaxSafeInt }

// ValidVersion reports G12 (the exact admissible value is fixed by R4–R6).
func ValidVersion(n uint64) bool { return n <= MaxVersion }

// ValidObservedAt reports G13: the fixed second-precision UTC form that
// time.RFC3339 also accepts. It is never compared with the wall clock.
func ValidObservedAt(s string) bool {
	if !observedAtRe.MatchString(s) {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// ValidActor reports G14 on the wire (G3 grammar).
func ValidActor(s string) bool { return ValidName(s) }

// ValidActorRef reports G14 in a payload: ActorRefPrefix + a G3 actor.
func ValidActorRef(s string) bool {
	return len(s) > len(ActorRefPrefix) && s[:len(ActorRefPrefix)] == ActorRefPrefix &&
		ValidActor(s[len(ActorRefPrefix):])
}

// ValidJournalSeq reports G16 (0 means none).
func ValidJournalSeq(n uint64) bool { return n <= MaxSafeInt }

// ValidOp reports G17.
func ValidOp(s string) bool { return in(s, OpRegister, OpRotate, OpRevoke) }

// refDomain is the DeriveRef domain separator (§0.1).
const refDomain = "rhz-secret-ref-v1"

// DeriveRef returns "sref-" + hex(sha256("rhz-secret-ref-v1" ‖ 0x00 ‖ opId))[:24]
// (§0.1). Rhizome and the plane both call this function.
func DeriveRef(opID string) string {
	h := sha256.New()
	h.Write([]byte(refDomain))
	h.Write([]byte{0})
	h.Write([]byte(opID))
	return "sref-" + hex.EncodeToString(h.Sum(nil))[:24]
}
