package secretwiretest

import (
	"errors"
	"strings"
	"testing"

	"rhizome/internal/secretwire"
)

// Every fault frame is refused by Rhizome's decoder with the §3.6 frame code
// the fault stands for.
func TestFaultFramesFRRHZ137(t *testing.T) {
	const s = "rhzsntl-00112233445566778899aabbccddeeff"
	P := "pl-0123456789abcdef0123456789abcdef"
	cases := []struct {
		typ  string
		f    FrameFault
		want error
	}{
		{secretwire.TypeHello, FaultBrokenJSON, secretwire.ErrFrameMalformed},
		{secretwire.TypeHello, FaultDupKey, secretwire.ErrFrameMalformed},
		{secretwire.TypeHello, FaultNull, secretwire.ErrFrameMalformed},
		{secretwire.TypeHello, FaultUnknownValue, secretwire.ErrFrameUnknownField},
		{secretwire.TypeHello, FaultTooLarge, secretwire.ErrFrameTooLarge},
		{secretwire.TypeHello, FaultNonUTF8, secretwire.ErrFrameMalformed},
		{secretwire.TypeHello, FaultVersionMismatch, secretwire.ErrFrameBadVersion},
		{secretwire.TypeBatch, FaultBrokenJSON, secretwire.ErrFrameMalformed},
		{secretwire.TypeBatch, FaultUnknownValue, secretwire.ErrFrameUnknownField},
		{secretwire.TypeBatch, FaultVersionMismatch, secretwire.ErrFrameBadVersion},
		{secretwire.TypeBatch, FaultSeqGap, secretwire.ErrFrameSeqOrder},
		{secretwire.TypeBatch, FaultSeqNonIncreasing, secretwire.ErrFrameSeqOrder},
		{secretwire.TypeBatch, FaultTooManyEntries, secretwire.ErrFrameTooManyEntries},
	}
	for _, c := range cases {
		b := FaultFrame(c.f, c.typ, P, s)
		if _, err := secretwire.DecodeFromPlane(b, c.typ, 1); !errors.Is(err, c.want) {
			t.Fatalf("%s fault %d: %v", c.typ, c.f, err)
		}
	}
	fr, err := secretwire.DecodeFromPlane(FaultFrame(FaultPlaneIDChange, secretwire.TypeBatch, P, s), secretwire.TypeBatch, 1)
	if err != nil || secretwire.CheckBatchPlane(fr.Batch, P) == nil {
		t.Fatal("planeId change not detected")
	}
	for _, f := range []FrameFault{FaultBrokenJSON, FaultNull, FaultVersionMismatch, FaultPlaneIDChange, FaultSeqGap} {
		if strings.Contains(string(FaultFrame(f, secretwire.TypeBatch, P, s)), s) {
			t.Fatalf("fault %d carries the sentinel where §8.1 does not put it", f)
		}
	}
}

// The built-in P5 scenario: 1–11 are valid, 12 is the rejected entry.
func TestPartialRejectScenarioFRRHZ137(t *testing.T) {
	es := PartialRejectScenario("op-00000000000000000000000000000001")
	if len(es) != 12 || es[0].Fact != secretwire.FactOutcome || es[11].Version != 2 {
		t.Fatal("scenario shape")
	}
	if _, err := secretwire.EncodeBatch(secretwire.Batch{V: 1, PlaneID: "pl-0123456789abcdef0123456789abcdef", Entries: es}); err != nil {
		t.Fatal(err)
	}
}
