package events

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fixedTime removes the time.Now() zero-fill nondeterminism from differential
// comparisons (see I6-S3 for the one case that exercises the zero-fill branch).
var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// naiveStore is a reference STAND-IN, not a persistent service. It is a
// line-by-line copy of the pre-RHZ-054 O(n^2) Store logic, kept only so the
// indexed Store can be differential-tested against it (I6, FR-RHZ-084). It must
// never be presented as a real store.
type naiveStore struct {
	mu     sync.Mutex
	events []Event
}

func (s *naiveStore) Append(expected uint64, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current uint64
	for _, e := range s.events {
		if e.AggregateType == event.AggregateType && e.AggregateID == event.AggregateID && e.Revision > current {
			current = e.Revision
		}
	}
	if current != expected {
		return ErrRevisionConflict
	}
	if event.Revision != expected+1 {
		return ErrRevisionConflict
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	s.events = append(s.events, event)
	s.events[len(s.events)-1].Sequence = uint64(len(s.events))
	return nil
}

func (s *naiveStore) All() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	for i := range out {
		out[i].Payload = append(json.RawMessage(nil), out[i].Payload...)
	}
	return out
}

func (s *naiveStore) List(aggregateType, aggregateID string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0)
	for _, e := range s.events {
		if e.AggregateType == aggregateType && e.AggregateID == aggregateID {
			copyEvent := e
			copyEvent.Payload = append(json.RawMessage(nil), e.Payload...)
			out = append(out, copyEvent)
		}
	}
	return out
}

func (s *naiveStore) AppendRevision(event Event) error {
	if event.AggregateType == "" || event.AggregateID == "" || event.Type == "" {
		return ErrRevisionConflict
	}
	if event.Sequence == 0 {
		return ErrRevisionConflict
	}
	if event.Sequence != uint64(len(s.All())+1) {
		return ErrRevisionConflict
	}
	return s.Append(s.currentRevision(event.AggregateType, event.AggregateID), event)
}

func (s *naiveStore) currentRevision(t, id string) uint64 {
	l := s.List(t, id)
	if len(l) == 0 {
		return 0
	}
	return l[len(l)-1].Revision
}

// diffStore is the common surface the indexed Store and naiveStore share so the
// same op sequence can drive both.
type diffStore interface {
	Append(expected uint64, event Event) error
	AppendRevision(event Event) error
	All() []Event
	List(aggregateType, aggregateID string) []Event
}

var (
	_ diffStore = (*Store)(nil)
	_ diffStore = (*naiveStore)(nil)
)

func TestAppendUsesExpectedRevision(t *testing.T) {
	var s Store
	e := Event{ID: "e1", AggregateType: "mission", AggregateID: "m1", Revision: 1, Type: "mission.created", CorrelationID: "c1"}
	if err := s.Append(0, e); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(0, e); err != ErrRevisionConflict {
		t.Fatalf("err = %v", err)
	}
	if got := len(s.List("mission", "m1")); got != 1 {
		t.Fatalf("events = %d", got)
	}
}

func TestGlobalSequenceAndAll(t *testing.T) { // FR-RHZ-007
	var s Store
	for i, id := range []string{"a", "b"} {
		if err := s.Append(0, Event{AggregateType: "x", AggregateID: id, Revision: 1, Type: "created"}); err != nil {
			t.Fatal(err)
		}
		if s.All()[i].Sequence != uint64(i+1) {
			t.Fatalf("sequence mismatch")
		}
	}
	if got := s.All(); len(got) != 2 || got[0].AggregateID != "a" || got[1].AggregateID != "b" {
		t.Fatalf("wrong order: %#v", got)
	}
}

func TestConflictDoesNotConsumeSequence(t *testing.T) { // FR-RHZ-007
	var s Store
	_ = s.Append(0, Event{AggregateType: "x", AggregateID: "a", Revision: 1})
	if err := s.Append(0, Event{AggregateType: "x", AggregateID: "a", Revision: 1}); err == nil {
		t.Fatal("expected conflict")
	}
	_ = s.Append(0, Event{AggregateType: "x", AggregateID: "b", Revision: 1})
	if s.All()[1].Sequence != 2 {
		t.Fatal("conflict consumed sequence")
	}
}

func TestPayloadIsolatedFromStore(t *testing.T) { // FR-RHZ-011
	var s Store
	p := json.RawMessage(`{"nested":{"value":"original"}}`)
	if err := s.Append(0, Event{AggregateType: "x", AggregateID: "a", Revision: 1, Payload: p}); err != nil {
		t.Fatal(err)
	}
	p[14] = 'X'
	a := s.All()
	a[0].Payload[14] = 'Y'
	l := s.List("x", "a")
	l[0].Payload[14] = 'Z'
	got := string(s.All()[0].Payload)
	if got != `{"nested":{"value":"original"}}` {
		t.Fatalf("stored payload mutated: %s", got)
	}
}

// --- RHZ-054 (FR-RHZ-084) index invariants on events.Store ---

var benchKeyTypes = []string{"goal", "mission"}
var benchKeyIDs = []string{"a", "b", "c"}

// I1-S3: revision of one aggregate key must not leak into another key's check.
func TestRevisionIndexIsPerKey(t *testing.T) { // FR-RHZ-084 / FR-RHZ-002 (I1)
	var s Store
	if err := s.Append(0, Event{AggregateType: "x", AggregateID: "a", Revision: 1, Type: "t", CreatedAt: fixedTime}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(1, Event{AggregateType: "x", AggregateID: "a", Revision: 2, Type: "t", CreatedAt: fixedTime}); err != nil {
		t.Fatal("expected append failed")
	}
	// Key b is fresh: expected=0 must succeed regardless of key a being at rev 2.
	if err := s.Append(0, Event{AggregateType: "x", AggregateID: "b", Revision: 1, Type: "t", CreatedAt: fixedTime}); err != nil {
		t.Fatalf("per-key revision leaked: %v", err)
	}
	if got := s.Revision("x", "a"); got != 2 {
		t.Fatalf("Revision(a)=%d want 2", got)
	}
	if got := s.Revision("x", "b"); got != 1 {
		t.Fatalf("Revision(b)=%d want 1", got)
	}
}

// I1-S4: sequential expected revisions succeed and are reflected in the index.
func TestSequentialAppendTracksRevision(t *testing.T) { // FR-RHZ-084 / FR-RHZ-002 (I1)
	var s Store
	for i := uint64(0); i < 3; i++ {
		if err := s.Append(i, Event{AggregateType: "x", AggregateID: "a", Revision: i + 1, Type: "t", CreatedAt: fixedTime}); err != nil {
			t.Fatalf("append rev %d: %v", i+1, err)
		}
		if got := s.Revision("x", "a"); got != i+1 {
			t.Fatalf("Revision after rev %d = %d", i+1, got)
		}
	}
	if l := s.List("x", "a"); len(l) != 3 || l[0].Revision != 1 || l[2].Revision != 3 {
		t.Fatalf("List revisions wrong: %#v", l)
	}
}

// I5-S3 (service mirror): a rejected append must not mutate index/List/All.
func TestConflictLeavesIndexUnchanged(t *testing.T) { // FR-RHZ-084 (I5)
	var s Store
	if err := s.Append(0, Event{AggregateType: "x", AggregateID: "a", Revision: 1, Type: "t", CreatedAt: fixedTime}); err != nil {
		t.Fatal(err)
	}
	beforeLen := s.Len()
	beforeRev := s.Revision("x", "a")
	beforeList := s.List("x", "a")
	beforeAll := s.All()
	// Conflicting append (wrong expected).
	if err := s.Append(5, Event{AggregateType: "x", AggregateID: "a", Revision: 6, Type: "t", CreatedAt: fixedTime}); err != ErrRevisionConflict {
		t.Fatalf("err=%v want conflict", err)
	}
	if s.Len() != beforeLen {
		t.Fatalf("Len changed on conflict: %d -> %d", beforeLen, s.Len())
	}
	if s.Revision("x", "a") != beforeRev {
		t.Fatalf("Revision changed on conflict")
	}
	if !reflect.DeepEqual(s.List("x", "a"), beforeList) {
		t.Fatalf("List changed on conflict")
	}
	if !reflect.DeepEqual(s.All(), beforeAll) {
		t.Fatalf("All changed on conflict")
	}
}

// I3-S3: List must deep-copy payloads even with the positions index.
func TestListPayloadIsolationWithIndex(t *testing.T) { // FR-RHZ-084 / FR-RHZ-011 (I3)
	var s Store
	for i := 0; i < 3; i++ {
		p := json.RawMessage(fmt.Sprintf(`{"v":%d}`, i))
		if err := s.Append(uint64(i), Event{AggregateType: "x", AggregateID: "a", Revision: uint64(i + 1), Type: "t", Payload: p, CreatedAt: fixedTime}); err != nil {
			t.Fatal(err)
		}
	}
	l := s.List("x", "a")
	for i := range l {
		l[i].Payload[0] = '!' // corrupt returned copies
	}
	again := s.List("x", "a")
	for i := range again {
		if again[i].Payload[0] != '{' {
			t.Fatalf("index shared payload with caller at %d: %s", i, again[i].Payload)
		}
	}
}

// genOp is one differential operation (I6).
type genOp struct {
	useRevision bool // call AppendRevision instead of Append
	expected    uint64
	event       Event
}

// genOps builds a deterministic mix of guaranteed-success and guaranteed-conflict
// ops. Successful ops advance the internal model so later "correct" ops stay
// valid; conflict ops are deliberately wrong and change no state, keeping the
// model exact. Returns the op list plus the success/conflict counts actually
// produced (adoption filter in the caller requires both > 0). I6-S1.
func genOps(seed int64, n int) (ops []genOp, wantOK, wantConflict int) {
	r := rand.New(rand.NewSource(seed))
	rev := map[aggKey]uint64{}
	total := uint64(0)
	for i := 0; i < n; i++ {
		kt := benchKeyTypes[r.Intn(len(benchKeyTypes))]
		kid := benchKeyIDs[r.Intn(len(benchKeyIDs))]
		key := aggKey{kt, kid}
		payload := json.RawMessage(fmt.Sprintf(`{"i":%d,"k":"%s/%s"}`, i, kt, kid))
		correct := r.Intn(2) == 0
		useRev := r.Intn(2) == 0
		e := Event{AggregateType: kt, AggregateID: kid, Type: "evt", Payload: payload, CreatedAt: fixedTime}
		op := genOp{useRevision: useRev}
		if correct {
			op.expected = rev[key]
			e.Revision = rev[key] + 1
			if useRev {
				e.Sequence = total + 1
			}
			rev[key]++
			total++
			wantOK++
		} else {
			// Deliberately wrong so it always conflicts and changes nothing.
			op.expected = rev[key] + 5
			e.Revision = rev[key] + 9
			if useRev {
				e.Sequence = total + 5
			}
			wantConflict++
		}
		op.event = e
		ops = append(ops, op)
	}
	return ops, wantOK, wantConflict
}

func applyOp(st diffStore, op genOp) error {
	if op.useRevision {
		return st.AppendRevision(op.event)
	}
	return st.Append(op.expected, op.event)
}

// allKeys enumerates the fixed key pool for List comparisons.
func allKeys() []aggKey {
	var out []aggKey
	for _, t := range benchKeyTypes {
		for _, id := range benchKeyIDs {
			out = append(out, aggKey{t, id})
		}
	}
	return out
}

// I6-S1: differential test. Same random op stream (success + conflict mixed) on
// the indexed Store and the naiveStore must agree on error, List, and All after
// every single op.
func TestDifferentialAgainstNaive(t *testing.T) { // FR-RHZ-084 (I6)
	for seed := int64(0); seed < 20; seed++ {
		ops, wantOK, wantConflict := genOps(seed, 300)
		if wantOK == 0 || wantConflict == 0 {
			continue // adoption filter: need both outcomes present
		}
		var idx Store
		naive := &naiveStore{}
		for step, op := range ops {
			eIdx := applyOp(&idx, op)
			eNaive := applyOp(naive, op)
			if (eIdx == nil) != (eNaive == nil) || (eIdx != nil && eIdx != eNaive) {
				t.Fatalf("seed=%d step=%d err mismatch idx=%v naive=%v", seed, step, eIdx, eNaive)
			}
			if !reflect.DeepEqual(idx.All(), naive.All()) {
				t.Fatalf("seed=%d step=%d All mismatch", seed, step)
			}
			for _, k := range allKeys() {
				if !reflect.DeepEqual(idx.List(k.t, k.id), naive.List(k.t, k.id)) {
					t.Fatalf("seed=%d step=%d List(%s/%s) mismatch", seed, step, k.t, k.id)
				}
			}
		}
	}
}

// I6-S2: deterministic all-success reinforcement stream (no conflicts).
func TestDifferentialAllSuccess(t *testing.T) { // FR-RHZ-084 (I6)
	var idx Store
	naive := &naiveStore{}
	for _, k := range allKeys() {
		for rev := uint64(1); rev <= 5; rev++ {
			e := Event{AggregateType: k.t, AggregateID: k.id, Revision: rev, Type: "evt",
				Payload: json.RawMessage(fmt.Sprintf(`{"r":%d}`, rev)), CreatedAt: fixedTime}
			if applyOp(&idx, genOp{expected: rev - 1, event: e}) != nil {
				t.Fatalf("idx append %s/%s rev %d", k.t, k.id, rev)
			}
			if applyOp(naive, genOp{expected: rev - 1, event: e}) != nil {
				t.Fatalf("naive append %s/%s rev %d", k.t, k.id, rev)
			}
		}
	}
	if !reflect.DeepEqual(idx.All(), naive.All()) {
		t.Fatal("All mismatch in all-success stream")
	}
	for _, k := range allKeys() {
		if !reflect.DeepEqual(idx.List(k.t, k.id), naive.List(k.t, k.id)) {
			t.Fatalf("List(%s/%s) mismatch", k.t, k.id)
		}
	}
}

// I6-S3: zero CreatedAt branch — both stores fill a non-zero time; all other
// fields stay equal (the time value itself is nondeterministic, so not compared).
func TestDifferentialZeroCreatedAt(t *testing.T) { // FR-RHZ-084 (I6)
	var idx Store
	naive := &naiveStore{}
	e := Event{AggregateType: "x", AggregateID: "a", Revision: 1, Type: "t", Payload: json.RawMessage(`{"z":1}`)}
	if err := idx.Append(0, e); err != nil {
		t.Fatal(err)
	}
	if err := naive.Append(0, e); err != nil {
		t.Fatal(err)
	}
	ai, an := idx.All()[0], naive.All()[0]
	if ai.CreatedAt.IsZero() || an.CreatedAt.IsZero() {
		t.Fatal("zero CreatedAt not filled")
	}
	ai.CreatedAt, an.CreatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(ai, an) {
		t.Fatalf("fields differ ignoring CreatedAt: %#v vs %#v", ai, an)
	}
}

// I7-2: concurrent Append / List / All must be race-clean. Run with -race.
func TestConcurrentAppendListAll(t *testing.T) { // FR-RHZ-084 (I7)
	var s Store
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := fmt.Sprintf("agg%d", w)
			for rev := uint64(1); rev <= 50; rev++ {
				_ = s.Append(rev-1, Event{AggregateType: "x", AggregateID: id, Revision: rev, Type: "t", CreatedAt: fixedTime})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = s.All()
				got := s.List("x", "agg0")
				for j := range got { // mutate returned copies; must not race internals
					got[j].Revision++
				}
			}
		}()
	}
	wg.Wait()
	if s.Len() != 8*50 {
		t.Fatalf("Len=%d want %d", s.Len(), 8*50)
	}
}

// --- RHZ-054 benchmarks (reviewer measures; no in-test assertions) ---

func prefillStore(s *Store, n int) {
	padding := make([]byte, 90)
	for i := range padding {
		padding[i] = 'a'
	}
	payload := json.RawMessage(`{"pad":"` + string(padding) + `"}`)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agg%d", i%200)
		exp := s.Revision("pre", id)
		_ = s.Append(exp, Event{AggregateType: "pre", AggregateID: id, Revision: exp + 1, Type: "t", Payload: payload, CreatedAt: fixedTime})
	}
}

// BenchmarkStoreAppend isolates the single-append cost at scale n, with no
// fsync in the path, to expose O(1) append vs the prior O(n). I6/§6.
func BenchmarkStoreAppend(b *testing.B) {
	for _, n := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			var s Store
			prefillStore(&s, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = s.Append(0, Event{AggregateType: "bench", AggregateID: fmt.Sprintf("x%d", i), Revision: 1, Type: "t", CreatedAt: fixedTime})
			}
		})
	}
}
