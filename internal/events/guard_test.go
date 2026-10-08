package events

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

type storeMutationGuard struct {
	cloned  chan struct{}
	release chan struct{}
	seen    []byte
	once    sync.Once
}

func (g *storeMutationGuard) CheckAppend(_ View, next Event, _ time.Time) error {
	g.seen = bytes.Clone(next.Payload)
	g.once.Do(func() { close(g.cloned) })
	<-g.release
	return nil
}

func (*storeMutationGuard) CheckReplay(View, Event) error { return nil }

func TestStoreGuardFRRHZ147(t *testing.T) {
	guard := &storeMutationGuard{cloned: make(chan struct{}), release: make(chan struct{})}
	store := &Store{Guard: guard}
	payload := []byte(`{"safe":true}`)
	done := make(chan error, 1)
	go func() {
		done <- store.Append(0, Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "x.created", Payload: payload})
	}()
	<-guard.cloned
	copy(payload, []byte(`{"evil":true}`))
	close(guard.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	payload[0] = '!'
	stored := store.List("x", "1")[0].Payload
	if !bytes.Equal(stored, guard.seen) || string(stored) != `{"safe":true}` {
		t.Fatalf("guard=%s stored=%s", guard.seen, stored)
	}

	reject := errors.New("rejected")
	store.Guard = rejectGuard{err: reject}
	before := store.Len()
	if err := store.Append(0, Event{AggregateType: "x", AggregateID: "2", Revision: 1, Type: "x.created"}); !errors.Is(err, reject) || store.Len() != before {
		t.Fatalf("rejection err=%v len=%d->%d", err, before, store.Len())
	}
}

type rejectGuard struct{ err error }

func (g rejectGuard) CheckAppend(View, Event, time.Time) error { return g.err }
func (g rejectGuard) CheckReplay(View, Event) error            { return g.err }

type storeRevokeRaceGuard struct{}

func (storeRevokeRaceGuard) check(prefix View, next Event) error {
	if next.Type == "decision" && len(prefix.List("trust", "root")) != 0 {
		return errors.New("key revoked")
	}
	return nil
}

func (g storeRevokeRaceGuard) CheckAppend(prefix View, next Event, _ time.Time) error {
	return g.check(prefix, next)
}

func (g storeRevokeRaceGuard) CheckReplay(prefix View, next Event) error {
	return g.check(prefix, next)
}

func TestStoreGuardRevokeRaceFRRHZ147(t *testing.T) {
	for i := 0; i < 40; i++ {
		store := &Store{Guard: storeRevokeRaceGuard{}}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			results <- store.Append(0, Event{AggregateType: "gate", AggregateID: "g", Revision: 1, Type: "decision"})
		}()
		go func() {
			<-start
			results <- store.Append(0, Event{AggregateType: "trust", AggregateID: "root", Revision: 1, Type: "revoke"})
		}()
		close(start)
		err1, err2 := <-results, <-results
		if err1 != nil && err2 != nil {
			t.Fatalf("both racing appends failed: %v, %v", err1, err2)
		}
		all := store.All()
		if len(all) == 0 || all[len(all)-1].Type != "revoke" {
			t.Fatalf("decision accepted after revoke: %v", all)
		}
		if len(all) == 2 && all[0].Type != "decision" {
			t.Fatalf("decision accepted after revoke: %v", all)
		}
	}
}

type storeSequenceGuard struct{ seen []uint64 }

func (g *storeSequenceGuard) CheckAppend(_ View, next Event, _ time.Time) error {
	g.seen = append(g.seen, next.Sequence)
	return nil
}

func (*storeSequenceGuard) CheckReplay(View, Event) error { return nil }

func TestStoreGuardObservesAssignedSequenceFRRHZ147(t *testing.T) {
	guard := &storeSequenceGuard{}
	store := &Store{Guard: guard}
	if err := store.Append(0, Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "x.created"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(1, Event{AggregateType: "x", AggregateID: "1", Revision: 2, Type: "x.updated"}); err != nil {
		t.Fatal(err)
	}
	if len(guard.seen) != 2 || guard.seen[0] != 1 || guard.seen[1] != 2 {
		t.Fatalf("guard observed sequences %v, want [1 2]", guard.seen)
	}
}
