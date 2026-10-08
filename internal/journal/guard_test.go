package journal

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"rhizome/internal/events"
	"sync"
	"testing"
	"time"
)

type mutationGuard struct {
	cloned  chan struct{}
	release chan struct{}
	seen    []byte
	once    sync.Once
}

func (g *mutationGuard) CheckAppend(_ events.View, next events.Event, _ time.Time) error {
	g.seen = bytes.Clone(next.Payload)
	g.once.Do(func() { close(g.cloned) })
	<-g.release
	return nil
}
func (*mutationGuard) CheckReplay(events.View, events.Event) error { return nil }

func TestGuardPayloadMutationAfterCloneFRRHZ147(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	guard := &mutationGuard{cloned: make(chan struct{}), release: make(chan struct{})}
	j, err := OpenGuarded(path, guard)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"safe":true}`)
	done := make(chan error, 1)
	go func() {
		done <- j.Append(0, events.Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "x.created", Payload: payload})
	}()
	<-guard.cloned
	copy(payload, []byte(`{"evil":true}`))
	close(guard.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	payload[0] = '!'
	stored := j.List("x", "1")[0].Payload
	if !bytes.Equal(stored, guard.seen) || string(stored) != `{"safe":true}` {
		t.Fatalf("guard=%s stored=%s", guard.seen, stored)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenGuarded(path, replayPayloadGuard{want: guard.seen})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
}

type replayPayloadGuard struct{ want []byte }

func (g replayPayloadGuard) CheckAppend(events.View, events.Event, time.Time) error { return nil }
func (g replayPayloadGuard) CheckReplay(_ events.View, next events.Event) error {
	if !bytes.Equal(next.Payload, g.want) {
		return errors.New("payload changed")
	}
	return nil
}

type alwaysRejectGuard struct{ err error }

func (g alwaysRejectGuard) CheckAppend(events.View, events.Event, time.Time) error { return g.err }
func (g alwaysRejectGuard) CheckReplay(events.View, events.Event) error            { return g.err }

func TestOpenGuardedNilRejectedFRRHZ147(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	if _, err := OpenGuarded(path, nil); err == nil {
		t.Fatal("nil guard accepted")
	}
	if _, err := OpenReadOnly(path, nil); err == nil {
		t.Fatal("nil read-only guard accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nil guard created journal: %v", err)
	}

	parsed, err := parser.ParseFile(token.NewFileSet(), "journal.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range parsed.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "Open" {
			t.Fatal("exported journal.Open still exists")
		}
	}
}

func TestGuardRejectionWritesZeroAndDoesNotPoisonFRRHZ147(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	rejected := errors.New("guard rejected")
	j, err := OpenGuarded(path, alwaysRejectGuard{err: rejected})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	before, _ := os.ReadFile(path)
	err = j.Append(0, events.Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "x.created", Payload: []byte(`{}`)})
	after, _ := os.ReadFile(path)
	if !errors.Is(err, rejected) || !bytes.Equal(before, after) || j.Poisoned() || len(j.All()) != 0 {
		t.Fatalf("err=%v bytes=%d/%d poisoned=%v events=%d", err, len(before), len(after), j.Poisoned(), len(j.All()))
	}
}

type revokeRaceGuard struct{}

func (revokeRaceGuard) check(prefix events.View, next events.Event) error {
	if next.Type != "decision" {
		return nil
	}
	if len(prefix.List("trust", "root")) != 0 {
		return errors.New("key revoked")
	}
	return nil
}
func (g revokeRaceGuard) CheckAppend(prefix events.View, next events.Event, _ time.Time) error {
	return g.check(prefix, next)
}
func (g revokeRaceGuard) CheckReplay(prefix events.View, next events.Event) error {
	return g.check(prefix, next)
}

func TestGuardRevokeRaceFRRHZ147(t *testing.T) {
	for i := 0; i < 40; i++ {
		path := filepath.Join(t.TempDir(), "journal.ndjson")
		j, err := OpenGuarded(path, revokeRaceGuard{})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			results <- j.Append(0, events.Event{AggregateType: "gate", AggregateID: "g", Revision: 1, Type: "decision"})
		}()
		go func() {
			<-start
			results <- j.Append(0, events.Event{AggregateType: "trust", AggregateID: "root", Revision: 1, Type: "revoke"})
		}()
		close(start)
		err1, err2 := <-results, <-results
		if err1 != nil && err2 != nil {
			t.Fatalf("both racing appends failed: %v, %v", err1, err2)
		}
		all := j.All()
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenGuarded(path, revokeRaceGuard{})
		if err != nil {
			t.Fatalf("accepted journal failed replay (events=%v): %v", all, err)
		}
		reopened.Close()
		if len(all) == 2 && (all[0].Type != "decision" || all[1].Type != "revoke") {
			t.Fatalf("decision accepted after revoke: %v", all)
		}
	}
}

type journalSequenceGuard struct{ seen []uint64 }

func (g *journalSequenceGuard) CheckAppend(_ events.View, next events.Event, _ time.Time) error {
	g.seen = append(g.seen, next.Sequence)
	return nil
}

func (*journalSequenceGuard) CheckReplay(events.View, events.Event) error { return nil }

func TestJournalGuardObservesAssignedSequenceFRRHZ147(t *testing.T) {
	guard := &journalSequenceGuard{}
	j, err := OpenGuarded(filepath.Join(t.TempDir(), "journal.ndjson"), guard)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.Append(0, events.Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "x.created"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(1, events.Event{AggregateType: "x", AggregateID: "1", Revision: 2, Type: "x.updated"}); err != nil {
		t.Fatal(err)
	}
	if len(guard.seen) != 2 || guard.seen[0] != 1 || guard.seen[1] != 2 {
		t.Fatalf("guard observed sequences %v, want [1 2]", guard.seen)
	}
}

func TestOpenReadOnlyAppendRejectedFRRHZ147(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.ndjson")
	if _, err := OpenReadOnly(missing, revokeRaceGuard{}); err == nil {
		t.Fatal("read-only open created a missing journal")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read-only open created file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	j, err := OpenGuarded(path, revokeRaceGuard{})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(path, revokeRaceGuard{})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := readOnly.Append(0, events.Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "x"}); err == nil {
		t.Fatal("read-only append accepted")
	}
}

func TestGuardReplayFailureFRRHZ147(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.ndjson")
	j, err := OpenGuarded(path, revokeRaceGuard{})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(0, events.Event{AggregateType: "x", AggregateID: "1", Revision: 1, Type: "forged", Payload: []byte(`{"signature":"bad"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGuarded(path, alwaysRejectGuard{err: errors.New("forged signature")}); err == nil {
		t.Fatal("replay guard accepted planted forged event")
	}
}
