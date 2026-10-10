package journal

// RHZ-133 (FR-RHZ-173): legacy journals (zero envelope created_at on disk)
// open deterministically as the Unix epoch, and appending to them never
// rewrites old bytes; new lines carry the same non-zero time on disk and in
// memory.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/trust"
)

func TestLegacyZeroCreatedAtAppendOnlyFRRHZ173(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.ndjson")
	var legacy bytes.Buffer
	for i, typ := range []string{"note.created", "note.updated"} {
		raw, err := json.Marshal(events.Event{Sequence: uint64(i + 1), ID: "e" + string(rune('1'+i)), AggregateType: "note", AggregateID: "n-1", Revision: uint64(i + 1), Type: typ, Payload: json.RawMessage(`{"v":1}`), CorrelationID: "c"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(raw, []byte(`"created_at":"0001-01-01T00:00:00Z"}`)) {
			t.Fatalf("fixture is not a legacy zero-time line: %s", raw)
		}
		legacy.Write(raw)
		legacy.WriteByte('\n')
	}
	old := legacy.Bytes()
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}

	epoch := time.Unix(0, 0).UTC()
	j, err := OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatalf("legacy open: %v", err)
	}
	for _, e := range j.All() {
		if !e.CreatedAt.Equal(epoch) {
			t.Fatalf("legacy line %d loaded as %v, want epoch", e.Sequence, e.CreatedAt)
		}
	}
	if err := j.Append(2, events.Event{ID: "e3", AggregateType: "note", AggregateID: "n-1", Revision: 3, Type: "note.updated", Payload: json.RawMessage(`{"v":2}`), CorrelationID: "c"}); err != nil {
		t.Fatal(err)
	}
	inMemory := j.All()[2].CreatedAt
	if inMemory.IsZero() || !inMemory.After(epoch) {
		t.Fatalf("appended event time %v", inMemory)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(disk, old) {
		t.Fatalf("legacy bytes rewritten:\nold %s\nnow %s", old, disk)
	}
	tail := bytes.TrimSuffix(disk[len(old):], []byte("\n"))
	if bytes.Count(tail, []byte("\n")) != 0 || len(tail) == 0 {
		t.Fatalf("expected exactly one appended line, got %q", tail)
	}
	var appended events.Event
	if err := json.Unmarshal(tail, &appended); err != nil {
		t.Fatal(err)
	}
	if appended.CreatedAt.IsZero() || !appended.CreatedAt.Equal(inMemory) {
		t.Fatalf("durable created_at %v, in-memory %v", appended.CreatedAt, inMemory)
	}

	// Reopen twice: legacy lines stay the epoch, the new line keeps its time.
	for round := 0; round < 2; round++ {
		r, err := OpenReadOnly(path, trust.NewAnchorless())
		if err != nil {
			t.Fatal(err)
		}
		all := r.All()
		if len(all) != 3 || !all[0].CreatedAt.Equal(epoch) || !all[1].CreatedAt.Equal(epoch) || !all[2].CreatedAt.Equal(inMemory) {
			t.Fatalf("round %d replay times: %v %v %v", round, all[0].CreatedAt, all[1].CreatedAt, all[2].CreatedAt)
		}
		r.Close()
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, disk) {
		t.Fatal("reopen modified the journal file")
	}
}
