package janusadapter

// FR-RHZ-083 (T25 multi-turn): ProjectSessionEvents is the outbound execution
// emit — a read-only, contiguous projection of the JANUS session log with no
// synthesis and no gap inference. These tests reuse the replay_test helpers
// (replayLine, replayTrace).

import (
	"errors"
	"strings"
	"testing"
)

// usageLine carries the two usage counters JANUS records on a row.
func usageLine(seq int, kind string, in, out int64) string {
	return `{"actor":"child","kind":"` + kind + `","payload":{},"seq":` +
		itoa(seq) + `,"span_id":"` + replaySpan + `","trace_id":"` + replayTrace +
		`","ts":` + itoa(seq*10) + `,"usage_in":` + itoa64(in) + `,"usage_out":` + itoa64(out) + "}\n"
}
func itoa(n int) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// FR-RHZ-083: the whole log projects to contiguous SessionEvents that surface
// the kinds and per-call usage; the raw payload never crosses over.
func TestProjectSessionEventsSurfacesKindsAndUsageFRRHZ083(t *testing.T) {
	log := replayLine(1, "session/start", `{}`) +
		replayLine(2, "subagent/spawn", `{"secret":"do-not-copy"}`) +
		usageLine(3, "usage", 120, 45) +
		replayLine(4, "tool_call", `{}`) +
		replayLine(5, "done", `{}`)
	evs, err := ProjectSessionEvents(strings.NewReader(log), replayTrace)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("events: %+v", evs)
	}
	wantKinds := []string{"session/start", "subagent/spawn", "usage", "tool_call", "done"}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("non-contiguous seq at %d: %+v", i, e)
		}
		if e.Kind != wantKinds[i] {
			t.Fatalf("kind %d = %q want %q", i, e.Kind, wantKinds[i])
		}
	}
	// The usage row (seq 3) carries ts=30 and the two counters.
	if evs[2].TS != 30 || evs[2].UsageIn != 120 || evs[2].UsageOut != 45 {
		t.Fatalf("usage row not projected: %+v", evs[2])
	}
	// replayLine rows carry ts=1 and no usage.
	if evs[0].TS != 1 {
		t.Fatalf("ts not projected: %+v", evs[0])
	}
	if evs[0].UsageIn != 0 || evs[0].UsageOut != 0 {
		t.Fatalf("absent usage must project as zero: %+v", evs[0])
	}
}

// FR-RHZ-083: a seq gap is corruption, never gap-filled or inferred.
func TestProjectSessionEventsRejectsSeqGapFRRHZ083(t *testing.T) {
	log := replayLine(1, "session/start", `{}`) + replayLine(3, "done", `{}`)
	if _, err := ProjectSessionEvents(strings.NewReader(log), replayTrace); !errors.Is(err, ErrObservationCorrupt) {
		t.Fatalf("gap not rejected: %v", err)
	}
}

// FR-RHZ-083: a foreign trace_id is a replaced session, not a silent drop.
func TestProjectSessionEventsRejectsForeignTraceFRRHZ083(t *testing.T) {
	other := "ffffffffffffffffffffffffffffffff"
	log := `{"actor":"p","kind":"session/start","payload":{},"seq":1,"span_id":"` +
		replaySpan + `","trace_id":"` + other + `","ts":1}` + "\n"
	if _, err := ProjectSessionEvents(strings.NewReader(log), replayTrace); !errors.Is(err, ErrSessionReplaced) {
		t.Fatalf("foreign trace not rejected: %v", err)
	}
}

// FR-RHZ-083: an empty log is a valid empty projection (no synthesis).
func TestProjectSessionEventsEmptyFRRHZ083(t *testing.T) {
	evs, err := ProjectSessionEvents(strings.NewReader(""), replayTrace)
	if err != nil || len(evs) != 0 {
		t.Fatalf("empty projection: %v %+v", err, evs)
	}
}
