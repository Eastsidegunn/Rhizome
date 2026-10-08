package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"rhizome/internal/events"
	"sync"
	"testing"
	"time"
)

var jFixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type faultFile struct {
	journalFile
	writeErr error
	syncErr  error
	writes   int
}

func (f *faultFile) Write(p []byte) (int, error) {
	f.writes++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.journalFile.Write(p)
}

func (f *faultFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.journalFile.Sync()
}

// writeJournalLines serializes events as the on-disk ndjson format for replay
// tests. Test-local temp files only; not a contract fixture.
func writeJournalLines(t *testing.T, path string, evs []events.Event) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range evs {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRestartAndStrictValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.ndjson")
	j, err := openTestJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	e := events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: []byte(`{"id":"g"}`)}
	if err = j.Append(0, e); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j, err = openTestJournal(p)
	if err != nil || len(j.All()) != 1 {
		t.Fatalf("reopen: %v", err)
	}
	j.Close()
}

func TestConcurrentAppendSerializes(t *testing.T) {
	j, err := openTestJournal(filepath.Join(t.TempDir(), "j"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = j.Append(0, events.Event{AggregateType: "goal", AggregateID: fmt.Sprint(i), Revision: 1, Type: "created"})
		}(i)
	}
	wg.Wait()
	if len(j.All()) != 20 {
		t.Fatalf("events=%d", len(j.All()))
	}
}
func TestRejectsCorruptOrGap(t *testing.T) {
	for _, data := range []string{`{"sequence":2}`, `not-json\n`} {
		p := filepath.Join(t.TempDir(), "j")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := openTestJournal(p); err == nil {
			t.Fatal("accepted corrupt journal")
		}
	}
}

// FR-RHZ-144: an ambiguous write or fsync failure latches poison exactly
// once; later writers touch neither the file nor the readable memory state.
func TestJournalFailStopPoisonFRRHZ144(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeFail bool
	}{
		{name: "write", writeFail: true},
		{name: "fsync"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "journal.ndjson")
			j, err := openTestJournal(p)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			if err := j.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: json.RawMessage(`{"id":"g"}`)}); err != nil {
				t.Fatal(err)
			}
			beforeAll := j.All()
			cause := errors.New(tc.name + " failed")
			ff := &faultFile{journalFile: j.file}
			if tc.writeFail {
				ff.writeErr = cause
			} else {
				ff.syncErr = cause
			}
			j.file = ff
			var causes []error
			j.OnPoison = func(err error) { causes = append(causes, err) }

			err = j.Append(1, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 2, Type: "goal.updated"})
			if err != events.ErrPoisoned || !errors.Is(err, events.ErrPoisoned) || err.Error() != "journal poisoned: restart required" {
				t.Fatalf("first failure = %v, identical=%v", err, err == events.ErrPoisoned)
			}
			if !j.Poisoned() || len(causes) != 1 || causes[0] != cause {
				t.Fatalf("poisoned=%v causes=%v", j.Poisoned(), causes)
			}
			afterFailure, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			writes := ff.writes

			// A different writer path/aggregate cannot reuse the stale global
			// sequence after the ambiguous append.
			err = j.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created"})
			if err != events.ErrPoisoned || !errors.Is(err, events.ErrPoisoned) {
				t.Fatalf("later append = %v", err)
			}
			afterLater, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterLater, afterFailure) || ff.writes != writes {
				t.Fatalf("later append wrote bytes: writes %d -> %d", writes, ff.writes)
			}
			if !reflect.DeepEqual(j.All(), beforeAll) || !reflect.DeepEqual(j.List("goal", "g"), beforeAll) {
				t.Fatal("readable memory state changed after poison")
			}
			if len(causes) != 1 {
				t.Fatalf("OnPoison calls = %d, want 1", len(causes))
			}
		})
	}
}

// FR-RHZ-144: failures before the file write boundary remain ordinary errors
// and leave the journal available for a later valid append.
func TestJournalPreWriteFailuresDoNotPoisonFRRHZ144(t *testing.T) {
	j, err := openTestJournal(filepath.Join(t.TempDir(), "journal.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	calls := 0
	j.OnPoison = func(error) { calls++ }

	err = j.Append(1, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 2, Type: "goal.updated"})
	if !errors.Is(err, events.ErrRevisionConflict) || j.Poisoned() {
		t.Fatalf("revision conflict = %v, poisoned=%v", err, j.Poisoned())
	}
	err = j.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: json.RawMessage(`{`)})
	if err == nil || errors.Is(err, events.ErrPoisoned) || j.Poisoned() {
		t.Fatalf("marshal failure = %v, poisoned=%v", err, j.Poisoned())
	}
	if err := j.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created"}); err != nil {
		t.Fatalf("valid append after pre-write failures: %v", err)
	}
	if calls != 0 {
		t.Fatalf("OnPoison calls = %d, want 0", calls)
	}
}

// --- RHZ-054 (FR-RHZ-084) ---

// I5-1 + I5-2: a failed file write (closed fd) must not advance the in-memory
// index/List/All, and must not reach disk. White-box: close j.file directly so
// the write path (not the j.file==nil guard) is exercised.
func TestWriteFailureLeavesStateUnchanged(t *testing.T) { // FR-RHZ-084 (I5)
	p := filepath.Join(t.TempDir(), "j")
	j, err := openTestJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: []byte(`{"id":"g"}`), CreatedAt: jFixedTime}); err != nil {
		t.Fatal(err)
	}
	beforeAll := j.All()
	beforeList := j.List("goal", "g")

	j.file.Close() // force the next Write/Sync to fail, but keep j.file non-nil

	err = j.Append(1, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 2, Type: "goal.updated", Payload: []byte(`{"id":"g"}`), CreatedAt: jFixedTime})
	if err == nil {
		t.Fatal("expected write failure")
	}
	if !reflect.DeepEqual(j.All(), beforeAll) {
		t.Fatal("All changed after failed write")
	}
	if !reflect.DeepEqual(j.List("goal", "g"), beforeList) {
		t.Fatal("List changed after failed write")
	}
	// I5-2: the failed event must not be on disk either.
	j2, err := openTestJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if n := len(j2.All()); n != 1 {
		t.Fatalf("disk has %d events after failed write, want 1", n)
	}
}

// I6-J2: replaying the real file must reproduce All/List exactly (symmetry of
// the index across the service and replay paths).
func TestReplayReproducesState(t *testing.T) { // FR-RHZ-084 (I6)
	p := filepath.Join(t.TempDir(), "j")
	j, err := openTestJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		exp  uint64
		t, i string
		rev  uint64
	}
	steps := []step{
		{0, "goal", "g", 1}, {0, "mission", "m", 1}, {1, "mission", "m", 2},
		{0, "goal", "h", 1}, {2, "mission", "m", 3}, {1, "goal", "g", 2},
	}
	for _, s := range steps {
		e := events.Event{AggregateType: s.t, AggregateID: s.i, Revision: s.rev, Type: "evt",
			Payload: []byte(fmt.Sprintf(`{"r":%d}`, s.rev)), CreatedAt: jFixedTime}
		if err = j.Append(s.exp, e); err != nil {
			t.Fatalf("append %s/%s rev %d: %v", s.t, s.i, s.rev, err)
		}
	}
	beforeAll := j.All()
	beforeMission := j.List("mission", "m")
	beforeGoalG := j.List("goal", "g")
	j.Close()

	j2, err := openTestJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if !reflect.DeepEqual(j2.All(), beforeAll) {
		t.Fatal("replay All mismatch")
	}
	if !reflect.DeepEqual(j2.List("mission", "m"), beforeMission) {
		t.Fatal("replay List(mission/m) mismatch")
	}
	if !reflect.DeepEqual(j2.List("goal", "g"), beforeGoalG) {
		t.Fatal("replay List(goal/g) mismatch")
	}
}

// I1-R1 / I4-4: contiguous sequence but a discontinuous revision must fail Open.
func TestReplayRejectsRevisionGap(t *testing.T) { // FR-RHZ-084 (I1/I4)
	p := filepath.Join(t.TempDir(), "j")
	writeJournalLines(t, p, []events.Event{
		{Sequence: 1, AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", CreatedAt: jFixedTime},
		{Sequence: 2, AggregateType: "goal", AggregateID: "g", Revision: 3, Type: "goal.updated", CreatedAt: jFixedTime}, // skips rev 2
	})
	if _, err := openTestJournal(p); err == nil {
		t.Fatal("accepted revision gap on replay")
	}
}

// I4-1: a truncated (no trailing newline) final line must fail Open.
func TestReplayRejectsTruncatedLine(t *testing.T) { // FR-RHZ-084 (I4)
	p := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(p, []byte(`{"sequence":1,"aggregate_type":"goal","aggregate_id":"g","revision":1,"type":"goal.created"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openTestJournal(p); err == nil {
		t.Fatal("accepted truncated final line")
	}
}

// --- RHZ-054 benchmarks (reviewer measures; no in-test assertions) ---

func buildJournal(b *testing.B, path string, n int) {
	j, err := openTestJournal(path)
	if err != nil {
		b.Fatal(err)
	}
	payload := []byte(`{"pad":"` + string(bytes.Repeat([]byte("a"), 90)) + `"}`)
	rev := map[string]uint64{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agg%d", i%200)
		exp := rev[id]
		if err = j.Append(exp, events.Event{AggregateType: "pre", AggregateID: id, Revision: exp + 1, Type: "t", Payload: payload, CreatedAt: jFixedTime}); err != nil {
			b.Fatal(err)
		}
		rev[id] = exp + 1
	}
	j.Close()
}

// BenchmarkJournalOpen measures full replay cost at n events. Target: n=16000
// under 1s wall clock (baseline 6.46s). §6.
func BenchmarkJournalOpen(b *testing.B) {
	for _, n := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "j")
			buildJournal(b, path, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				j, err := openTestJournal(path)
				if err != nil {
					b.Fatal(err)
				}
				j.Close()
			}
		})
	}
}

// BenchmarkJournalAppend measures one append at scale n. Note: fsync dominates
// here, so BenchmarkStoreAppend is the clean O(1) signal; this guards that the
// structural cost does not regress. §6.
func BenchmarkJournalAppend(b *testing.B) {
	for _, n := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "j")
			buildJournal(b, path, n)
			j, err := openTestJournal(path)
			if err != nil {
				b.Fatal(err)
			}
			defer j.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = j.Append(0, events.Event{AggregateType: "bench", AggregateID: fmt.Sprintf("x%d", i), Revision: 1, Type: "t", CreatedAt: jFixedTime})
			}
		})
	}
}
