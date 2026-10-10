package secretref

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/peercred"
	"rhizome/internal/secretwire"
)

var t0 = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return t0 } }

func opN(n int) string    { return fmt.Sprintf("op-%032x", n) }
func planeN(n int) string { return fmt.Sprintf("pl-%032x", n) }

const actor = "tester"
const actorRef = secretwire.ActorRefPrefix + actor

// sentinel is the §8.1 ZV value: "rhzsntl-" + 32 hex from crypto/rand.
func sentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

// sentinelForms are the §8.1 search forms of s.
func sentinelForms(s string) []string {
	sum := sha256.Sum256([]byte(s))
	return []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
}

func assertNoSentinel(t *testing.T, where string, data []byte, s string) {
	t.Helper()
	for _, f := range sentinelForms(s) {
		if strings.Contains(string(data), f) {
			t.Fatalf("%s contains a sentinel form", where)
		}
	}
}

// verified returns a real peercred.Verified for the current user.
func verified(t *testing.T) peercred.Verified {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	fa, fb := os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
	defer fa.Close()
	defer fb.Close()
	ca, err := net.FileConn(fa)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := net.FileConn(fb)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	v, err := peercred.Verify(ca.(*net.UnixConn))
	if err != nil || !v.SameUser() {
		t.Fatalf("peercred: %v", err)
	}
	return v
}

// --- stores ---

type allowGuard struct{}

func (allowGuard) CheckAppend(events.View, events.Event, time.Time) error { return nil }
func (allowGuard) CheckReplay(events.View, events.Event) error            { return nil }

func openJournal(t *testing.T, path string) *journal.Journal {
	t.Helper()
	j, err := journal.OpenGuarded(path, allowGuard{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

// faultStore counts Append calls, can fail them, and exposes a poison latch.
type faultStore struct {
	events.Store
	mu       sync.Mutex
	failWith error
	appends  int
	poison   bool
}

func (f *faultStore) Append(expected uint64, e events.Event) error {
	f.mu.Lock()
	f.appends++
	err := f.failWith
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.Store.Append(expected, e)
}

func (f *faultStore) Poisoned() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.poison }

func (f *faultStore) appendCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.appends }

// --- event log builder for replay tests ---

type logb struct {
	evs  []events.Event
	revs map[aggKey]uint64
}

func newLog() *logb { return &logb{revs: map[aggKey]uint64{}} }

func envelope(payload any) (typ, aggType, aggID, corr string) {
	switch v := payload.(type) {
	case registerRequested:
		return TypeRegisterRequested, AggregateOp, v.OpID, v.OpID
	case rotateRequested:
		return TypeRotateRequested, AggregateOp, v.OpID, v.OpID
	case revokeRequested:
		return TypeRevokeRequested, AggregateOp, v.OpID, v.OpID
	case opOutcome:
		return TypeOpOutcome, AggregateOp, v.OpID, v.OpID
	case reconciled:
		return TypeReconciled, AggregateRef, v.Ref, v.OpID
	case observed:
		return TypeObserved, AggregateRef, v.Ref, v.PlaneID
	case entryRejected:
		return TypeEntryRejected, AggregateChan, v.PlaneID, v.PlaneID
	}
	panic("unknown payload")
}

// add appends payload with the correct envelope; mod may tamper with it.
func (b *logb) add(payload any, mod ...func(*events.Event)) *logb {
	typ, aggType, aggID, corr := envelope(payload)
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	k := aggKey{aggType, aggID}
	b.revs[k]++
	e := events.Event{Sequence: uint64(len(b.evs) + 1), AggregateType: aggType, AggregateID: aggID,
		Revision: b.revs[k], Type: typ, Payload: raw, CorrelationID: corr,
		CreatedAt: t0.Add(time.Duration(len(b.evs)) * time.Second)}
	for _, m := range mod {
		m(&e)
	}
	b.evs = append(b.evs, e)
	return b
}

func e1(op, name string) registerRequested {
	return registerRequested{OpID: op, Name: name, Kind: secretwire.RefKindOther, ActorRef: actorRef}
}
func e2(op, ref string) rotateRequested {
	return rotateRequested{OpID: op, Ref: ref, ActorRef: actorRef}
}
func e3(op, ref string) revokeRequested {
	return revokeRequested{OpID: op, Ref: ref, ReasonCode: secretwire.ReasonUnused, ActorRef: actorRef}
}
func e4(op, ref string, ver int64, result, plane string, seq uint64) opOutcome {
	o := opOutcome{OpID: op, Ref: ref, Version: ver, Result: result, PlaneID: plane, PlaneEntrySeq: seq}
	if result == secretwire.ResultFailed {
		o.FailureCode = secretwire.FailureBackendError
	}
	return o
}
func e5(op, name, plane string, seq uint64) reconciled {
	return reconciled{Ref: secretwire.DeriveRef(op), OpID: op, Name: name, Kind: secretwire.RefKindProviderKey,
		Version: 1, Origin: secretwire.OriginPlaneDirect, PlaneID: plane, PlaneEntrySeq: seq}
}
func e6(ref string, ver int64, plane string, seq uint64) observed {
	return observed{Ref: ref, Version: ver, Present: true, PlaneObservedAt: "2026-10-10T01:02:03Z",
		PlaneID: plane, PlaneEntrySeq: seq}
}
func e7(plane string, rseq uint64, reason string, ack uint64) entryRejected {
	return entryRejected{PlaneID: plane, RejectedSeq: rseq, Reason: reason, AckSeq: ack}
}

func ref(op string) string { return secretwire.DeriveRef(op) }

// replayCode returns the ReplayError code of Project(log), "" if it passes.
func replayCode(t *testing.T, log []events.Event) string {
	t.Helper()
	_, err := Project(log)
	if err == nil {
		return ""
	}
	re, ok := err.(*ReplayError)
	if !ok {
		t.Fatalf("non-replay error type %T", err)
	}
	if err.Error() != "secret replay: "+re.Code {
		t.Fatalf("error text not fixed")
	}
	return re.Code
}

// --- entries ---

func outcomeEntry(seq uint64, op, r string, ver uint64, result string) secretwire.Entry {
	e := secretwire.Entry{Seq: seq, Fact: secretwire.FactOutcome, OpID: op, Ref: r, Version: ver, Result: result}
	if result == secretwire.ResultFailed {
		e.FailureCode = secretwire.FailureBackendError
	}
	return e
}

func reconciledEntry(seq uint64, op, name string) secretwire.Entry {
	return secretwire.Entry{Seq: seq, Fact: secretwire.FactReconciled, OpID: op, Ref: ref(op), Name: name,
		RefKind: secretwire.RefKindProviderKey, Version: 1, Origin: secretwire.OriginPlaneDirect}
}

func observedEntry(seq uint64, r string, ver uint64) secretwire.Entry {
	return secretwire.Entry{Seq: seq, Fact: secretwire.FactObserved, Ref: r, Version: ver, Present: true,
		PlaneObservedAt: "2026-10-10T01:02:03Z"}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func journalPath(t *testing.T) string { return filepath.Join(t.TempDir(), "journal.ndjson") }
