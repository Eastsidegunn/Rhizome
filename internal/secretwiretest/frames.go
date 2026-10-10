package secretwiretest

import (
	"encoding/json"
	"strings"

	"rhizome/internal/secretwire"
)

// FaultFrame returns the raw line (with '\n') of fault ff in place of a
// frame of type typ (secretwire.TypeHello or TypeBatch) from planeID. The
// sentinel is embedded only where §8.1 puts it (unknown key value, the
// first value of a duplicate key, next to a raw invalid byte); it is never
// stored by the fake.
func FaultFrame(ff FrameFault, typ, planeID, sentinel string) []byte {
	head := `{"v":1,"type":"` + typ + `"`
	tail := `,"entries":[]}`
	if typ == secretwire.TypeHello {
		head = `{"v":1,"type":"hello","role":"plane","version":1`
		tail = `}`
	}
	var line string
	switch ff {
	case FaultBrokenJSON:
		line = head + `,"planeId":"` + planeID + `"`
	case FaultDupKey:
		line = head + `,"planeId":"` + sentinel + `","planeId":"` + planeID + `"` + tail
	case FaultNull:
		line = head + `,"planeId":null` + tail
	case FaultUnknownValue:
		line = head + `,"planeId":"` + planeID + `","value":"` + sentinel + `"` + tail
	case FaultTooLarge:
		line = head + `,"planeId":"` + planeID + `"` + tail + strings.Repeat(" ", secretwire.MaxFrameBytes)
	case FaultNonUTF8:
		line = head + `,"planeId":"` + planeID + "\xff" + sentinel + `"` + tail
	case FaultVersionMismatch:
		if typ == secretwire.TypeHello {
			line = `{"v":1,"type":"hello","role":"plane","version":2,"planeId":"` + planeID + `"}`
		} else {
			line = `{"v":2,"type":"batch","planeId":"` + planeID + `","entries":[]}`
		}
	case FaultPlaneIDChange:
		line = head + `,"planeId":"` + otherPlane(planeID) + `"` + tail
	case FaultSeqGap:
		return rawBatch(1, planeID, []secretwire.Entry{ObservedEntry(1, secretwire.DeriveRef(zeroOp), 1),
			ObservedEntry(3, secretwire.DeriveRef(zeroOp), 1)})
	case FaultSeqNonIncreasing:
		return rawBatch(1, planeID, []secretwire.Entry{ObservedEntry(2, secretwire.DeriveRef(zeroOp), 1),
			ObservedEntry(2, secretwire.DeriveRef(zeroOp), 1)})
	case FaultTooManyEntries:
		var es []secretwire.Entry
		for i := 1; i <= secretwire.MaxEntries+1; i++ {
			es = append(es, ObservedEntry(uint64(i), secretwire.DeriveRef(zeroOp), 1))
		}
		return rawBatch(1, planeID, es)
	default:
		line = head + `,"planeId":"` + planeID + `"` + tail
	}
	return []byte(line + "\n")
}

const zeroOp = "op-00000000000000000000000000000000"

// otherPlane returns a valid planeId different from id.
func otherPlane(id string) string {
	if id == "pl-ffffffffffffffffffffffffffffffff" {
		return "pl-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	}
	return "pl-ffffffffffffffffffffffffffffffff"
}

// rawBatch encodes a batch without the encoder's own W9 re-check, so
// deliberately non-contiguous or oversized entry lists can be sent.
func rawBatch(v uint64, planeID string, entries []secretwire.Entry) []byte {
	if entries == nil {
		entries = []secretwire.Entry{}
	}
	b, err := json.Marshal(struct {
		V       uint64             `json:"v"`
		Type    string             `json:"type"`
		PlaneID string             `json:"planeId"`
		Entries []secretwire.Entry `json:"entries"`
	}{v, secretwire.TypeBatch, planeID, entries})
	if err != nil {
		return []byte("{}\n")
	}
	return append(b, '\n')
}

// ObservedAt is the fixed G13 time the helpers use.
const ObservedAt = "2026-10-10T01:02:03Z"

// OutcomeEntry is an outcome entry (seq is set by Append).
func OutcomeEntry(seq uint64, opID, ref string, version uint64, result string) secretwire.Entry {
	e := secretwire.Entry{Seq: seq, Fact: secretwire.FactOutcome, OpID: opID, Ref: ref, Version: version, Result: result}
	if result == secretwire.ResultFailed {
		e.FailureCode = secretwire.FailureBackendError
	}
	return e
}

// ObservedEntry is an observed entry with Present true.
func ObservedEntry(seq uint64, ref string, version uint64) secretwire.Entry {
	return secretwire.Entry{Seq: seq, Fact: secretwire.FactObserved, Ref: ref, Version: version, Present: true,
		PlaneObservedAt: ObservedAt}
}

// ReconciledEntry is a plane-direct reconciled entry for opID.
func ReconciledEntry(seq uint64, opID, name string) secretwire.Entry {
	return secretwire.Entry{Seq: seq, Fact: secretwire.FactReconciled, OpID: opID, Ref: secretwire.DeriveRef(opID),
		Name: name, RefKind: secretwire.RefKindProviderKey, Version: 1, Origin: secretwire.OriginPlaneDirect}
}

// PartialRejectScenario is the built-in "ack 10, accept 11, reject 12" P5
// scenario (§3.5) for a registered opID: seq 1 is the register ok outcome
// (version 1), seq 2–11 observe the ref at version 1, and seq 12 observes it
// at version 2, which Rhizome rejects (version-mismatch). Append the first
// ten, let Rhizome ack 10, then append the last two.
func PartialRejectScenario(opID string) []secretwire.Entry {
	ref := secretwire.DeriveRef(opID)
	out := []secretwire.Entry{OutcomeEntry(1, opID, ref, 1, secretwire.ResultOK)}
	for seq := uint64(2); seq <= 11; seq++ {
		out = append(out, ObservedEntry(seq, ref, 1))
	}
	return append(out, ObservedEntry(12, ref, 2))
}
