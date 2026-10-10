package secretchannel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/peercred"
	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
	"rhizome/internal/secretwiretest"
)

var t0 = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

func opN(n int) string     { return fmt.Sprintf("op-%032x", n) }
func planeN(n int) string  { return fmt.Sprintf("pl-%032x", n) }
func ref(op string) string { return secretwire.DeriveRef(op) }

const actor = "tester"

// sentinel is the §8.1 ZV value: "rhzsntl-" + 32 hex from crypto/rand.
func sentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

func assertNoSentinel(t *testing.T, where string, data []byte, s string) {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	for _, f := range []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]} {
		if strings.Contains(string(data), f) {
			t.Fatalf("%s contains a sentinel form", where)
		}
	}
}

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

// poisonStore wraps a real journal; once armed, the next Append poisons it
// (the ambiguous-append case) and every Append after returns ErrPoisoned.
type poisonStore struct {
	*journal.Journal
	arm, latched atomic.Bool
}

func (p *poisonStore) Append(exp uint64, e events.Event) error {
	if p.latched.Load() {
		return events.ErrPoisoned
	}
	if p.arm.Load() {
		p.latched.Store(true)
		return events.ErrPoisoned
	}
	return p.Journal.Append(exp, e)
}

func (p *poisonStore) Poisoned() bool { return p.latched.Load() }

// env is one Rhizome (journal + writer + channel) and one fake plane.
type env struct {
	t        *testing.T
	path     string
	store    *poisonStore
	svc      *secretref.Service
	fake     *secretwiretest.FakeSecretPlane
	ch       *Channel
	verifies atomic.Int64
	appends  atomic.Int64
	dials    atomic.Int64 // successful dials across Rhizome restarts

	mu      sync.Mutex
	reports []string
	polls   int // fake polls already checked

	live     atomic.Pointer[poisonStore] // the current journal, for fake hooks
	sendSkew atomic.Int64                // acks that differed from the journal at receipt
	notices  int
}

type opt func(*Config)

func newEnv(t *testing.T, opts ...opt) *env {
	t.Helper()
	fake, err := secretwiretest.Start(planeN(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	e := &env{t: t, path: filepath.Join(t.TempDir(), "journal.ndjson"), fake: fake}
	e.open(opts...)
	return e
}

// open (re)opens the journal and builds a fresh writer and channel over it:
// a Rhizome restart.
func (e *env) open(opts ...opt) {
	e.t.Helper()
	e.store = &poisonStore{Journal: openJournal(e.t, e.path)}
	e.live.Store(e.store)
	e.svc = secretref.New(e.store, secretref.WithClock(func() time.Time { return t0 }))
	cfg := Config{SocketPath: e.fake.SocketPath(), Service: e.svc, Latch: e.store,
		Verify: func(c *net.UnixConn) (peercred.Verified, error) {
			e.verifies.Add(1)
			return peercred.Verify(c)
		},
		OnAppend: func() { e.appends.Add(1) },
		OnError: func(s string) {
			e.mu.Lock()
			e.reports = append(e.reports, s)
			e.mu.Unlock()
		}}
	for _, o := range opts {
		o(&cfg)
	}
	ch, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	ch.ioTimeout = 3 * time.Second
	dial := ch.dial
	ch.dial = func(ctx context.Context, path string, timeout time.Duration) (*net.UnixConn, error) {
		c, err := dial(ctx, path, timeout)
		if err == nil {
			e.dials.Add(1)
		}
		return c, err
	}
	e.ch = ch
	e.installHooks()
}

// restart closes the journal and reopens everything over the same file.
func (e *env) restart(opts ...opt) {
	e.t.Helper()
	e.store.Journal.Close()
	e.open(opts...)
}

func (e *env) reportsCopy() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.reports...)
}

func (e *env) lastReport() string {
	r := e.reportsCopy()
	if len(r) == 0 {
		return ""
	}
	return r[len(r)-1]
}

func (e *env) project() *secretref.Projection {
	e.t.Helper()
	p, err := secretref.Project(e.store.All())
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) bytes() []byte {
	e.t.Helper()
	b, err := os.ReadFile(e.path)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	return b
}

// cycle runs one channel cycle and waits for the fake to finish the
// connection. Every poll ack the fake received must equal the ack recomputed
// from the journal before the cycle, and every notice ack the one after it
// (W8: acks are durable high-water marks).
func (e *env) cycle() {
	e.t.Helper()
	before := e.project()
	e.ch.Cycle(context.Background())
	if !e.fake.WaitHandled(int(e.dials.Load()), 10*time.Second) {
		e.t.Fatal("fake did not finish the connection")
	}
	after := e.project()
	polls, notices := e.fake.Polls(), e.fake.Notices()
	for _, p := range polls[e.polls:] {
		if p.Poll.Ack != before.Ack(p.PlaneID) {
			e.t.Fatalf("poll ack %+v, journal says %+v", p.Poll.Ack, before.Ack(p.PlaneID))
		}
		if want := before.LastRejected(p.PlaneID); fmt.Sprint(want) != fmt.Sprint(p.Poll.LastRejected) {
			e.t.Fatalf("poll lastRejected %v, journal says %v", p.Poll.LastRejected, want)
		}
	}
	for _, n := range notices[e.notices:] {
		if *n.Notice.Ack != after.Ack(n.PlaneID) {
			e.t.Fatalf("notice ack %+v, journal says %+v", *n.Notice.Ack, after.Ack(n.PlaneID))
		}
	}
	e.polls, e.notices = len(polls), len(notices)
	if n := e.sendSkew.Load(); n != 0 {
		e.t.Fatalf("%d sent acks differed from the durable hw at receipt", n)
	}
	if v := e.fake.Violations(); len(v) != 0 {
		e.t.Fatalf("fake violations %v", v)
	}
}

func (e *env) register(op, name string) {
	e.t.Helper()
	r, err := e.svc.RequestRegister(secretref.RegisterRequest{OpID: op, Name: name,
		RefKind: secretwire.RefKindProviderKey, Actor: actor})
	if err != nil || !r.Accepted {
		e.t.Fatalf("register %+v %v", r, err)
	}
}

func (e *env) state(op string) (string, int64) {
	e.t.Helper()
	v, ok := e.project().Ref(ref(op))
	if !ok {
		e.t.Fatal("ref missing")
	}
	return v.State, v.Version
}

func (e *env) count(typ string) int {
	n := 0
	for _, ev := range e.store.All() {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// hw builds plane P's high water n through the fake: op 1 registered and
// active (seq 1), then observed facts up to seq n.
func (e *env) hw(n int) {
	e.t.Helper()
	e.register(opN(1), "alpha")
	for _, en := range secretwiretest.PartialRejectScenario(opN(1))[:n] {
		e.fake.Append(en)
	}
	e.cycle()
	if got := e.project().HighWater(e.fake.PlaneID()); got != uint64(n) {
		e.t.Fatalf("hw %d", got)
	}
}

func (e *env) lastNotice() secretwire.ErrorFrame {
	e.t.Helper()
	ns := e.fake.Notices()
	if len(ns) == 0 {
		e.t.Fatal("no notice")
	}
	return ns[len(ns)-1].Notice
}

func (e *env) lastPoll() secretwire.Poll {
	e.t.Helper()
	ps := e.fake.Polls()
	if len(ps) == 0 {
		e.t.Fatal("no poll")
	}
	return ps[len(ps)-1].Poll
}

// installHooks makes the fake re-project the journal when a poll or notice
// arrives: W8 requires the sent ack to equal the durable hw at send time,
// and while the fake holds the frame Rhizome is blocked on the connection.
func (e *env) installHooks() {
	ackNow := func(planeID string) secretwire.Ack {
		p, err := secretref.Project(e.live.Load().All())
		if err != nil {
			return secretwire.Ack{}
		}
		return p.Ack(planeID)
	}
	e.fake.OnPoll(func(rp secretwiretest.RecordedPoll) {
		if rp.Poll.Ack != ackNow(rp.PlaneID) {
			e.sendSkew.Add(1)
		}
	})
	e.fake.OnNotice(func(n secretwiretest.RecordedNotice) {
		if n.Notice.Ack == nil || *n.Notice.Ack != ackNow(n.PlaneID) {
			e.sendSkew.Add(1)
		}
	})
}
