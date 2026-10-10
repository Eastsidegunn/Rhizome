package secretchannel

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rhizome/internal/peercred"
	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
	"rhizome/internal/secretwiretest"
)

const (
	rOK = secretwire.ResultOK
)

func TestConfigFRRHZ137(t *testing.T) {
	svc := secretref.New(nil)
	for _, cfg := range []Config{
		{SocketPath: "/x/p.sock"},
		{SocketPath: "rel/p.sock", Service: svc},
		{SocketPath: "/x/../p.sock", Service: svc},
		{SocketPath: "/x/p.sock", Service: svc, Interval: 199 * time.Millisecond},
		{SocketPath: "/x/p.sock", Service: svc, ExpectedPlaneID: "pl-1"},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrConfig) {
			t.Fatalf("config %+v accepted", cfg)
		}
	}
	c, err := New(Config{SocketPath: "/x/p.sock", Service: svc})
	if err != nil || c.interval != DefaultInterval || c.dialTimeout != 2*time.Second || c.ioTimeout != 10*time.Second {
		t.Fatalf("defaults %v", err)
	}
	if c, err := New(Config{SocketPath: "/x/p.sock", Service: svc, Interval: MinInterval}); err != nil || c.interval != MinInterval {
		t.Fatal("min interval refused")
	}
}

// Register → active(1), rotate → active(2), revoke → revoked, driven by the
// fake; pending carries each request; OnAppend fires per appending batch.
func TestLifecycleFRRHZ137(t *testing.T) {
	e := newEnv(t)
	op1, op2, op3 := opN(1), opN(2), opN(3)
	r1 := ref(op1)
	e.register(op1, "alpha")
	e.cycle() // nothing to deliver yet
	if p := e.lastPoll(); len(p.Pending) != 1 || p.Pending[0].OpID != op1 || p.Pending[0].Op != secretwire.OpRegister {
		t.Fatalf("pending %+v", p.Pending)
	}
	e.fake.Append(secretwiretest.OutcomeEntry(0, op1, r1, 1, rOK))
	e.cycle()
	if s, v := e.state(op1); s != secretref.StateActive || v != 1 {
		t.Fatalf("state %s %d", s, v)
	}
	if e.appends.Load() != 1 {
		t.Fatalf("OnAppend %d", e.appends.Load())
	}

	if r, err := e.svc.RequestRotate(secretref.RotateRequest{OpID: op2, Ref: r1, Actor: actor}); err != nil || !r.Accepted {
		t.Fatal("rotate refused")
	}
	e.cycle()
	if p := e.lastPoll(); len(p.Pending) != 1 || p.Pending[0].Op != secretwire.OpRotate || p.Ack.PlaneEntrySeq != 1 {
		t.Fatalf("poll %+v", p)
	}
	e.fake.Append(secretwiretest.OutcomeEntry(0, op2, r1, 2, rOK))
	e.cycle()
	if s, v := e.state(op1); s != secretref.StateActive || v != 2 {
		t.Fatalf("state %s %d", s, v)
	}

	if r, err := e.svc.RequestRevoke(secretref.RevokeRequest{OpID: op3, Ref: r1, ReasonCode: secretwire.ReasonUnused,
		Actor: actor}); err != nil || !r.Accepted {
		t.Fatal("revoke refused")
	}
	e.cycle()
	if p := e.lastPoll(); len(p.Pending) != 1 || p.Pending[0].Op != secretwire.OpRevoke || p.Pending[0].ReasonCode != secretwire.ReasonUnused {
		t.Fatalf("poll %+v", p)
	}
	e.fake.Append(secretwiretest.OutcomeEntry(0, op3, r1, 2, rOK))
	e.cycle()
	if s, _ := e.state(op1); s != secretref.StateRevoked {
		t.Fatalf("state %s", s)
	}
	e.cycle()
	if p := e.lastPoll(); len(p.Pending) != 0 || p.Ack.PlaneEntrySeq != 3 {
		t.Fatalf("final poll %+v", p)
	}
	if e.appends.Load() != 3 || len(e.reportsCopy()) != 0 {
		t.Fatalf("appends %d reports %v", e.appends.Load(), e.reportsCopy())
	}
}

// W2: the verifier runs once per connection; a failing or non-same-user
// verification closes before any frame with zero writes.
func TestVerifierPerConnectionFRRHZ137(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.cycle()
	if n := e.verifies.Load(); n != 2 {
		t.Fatalf("verifier calls %d", n)
	}
	e.register(opN(1), "alpha")
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(1), ref(opN(1)), 1, rOK))
	before := e.bytes()
	for _, vf := range []func(*net.UnixConn) (peercred.Verified, error){
		func(*net.UnixConn) (peercred.Verified, error) { return peercred.Verified{}, errors.New("x") },
		func(*net.UnixConn) (peercred.Verified, error) { return peercred.Verified{}, nil },
	} {
		e.ch.cfg.Verify = vf
		frames := len(e.fake.Received())
		e.cycle()
		if e.lastReport() != secretwire.ReportPeerRejected || len(e.fake.Received()) != frames {
			t.Fatalf("report %q", e.lastReport())
		}
		if !bytes.Equal(before, e.bytes()) {
			t.Fatal("journal changed")
		}
	}
}

// §3.1: a hold or close at any stage before the batch is complete writes
// nothing and leaves the ack unchanged.
func TestStageFaultsFRRHZ137(t *testing.T) {
	stages := []secretwiretest.Stage{secretwiretest.StageBeforeHello, secretwiretest.StageAfterHello,
		secretwiretest.StageAfterPoll, secretwiretest.StageMidBatch}
	for _, st := range stages {
		for _, act := range []secretwiretest.Action{secretwiretest.ActionHold, secretwiretest.ActionClose} {
			e := newEnv(t)
			e.ch.ioTimeout = 300 * time.Millisecond
			e.register(opN(1), "alpha")
			e.fake.Append(secretwiretest.OutcomeEntry(0, opN(1), ref(opN(1)), 1, rOK))
			ack := e.project().Ack(planeN(1))
			before := e.bytes()
			e.fake.SetStageFault(st, act)
			start := time.Now()
			e.cycle()
			if !bytes.Equal(before, e.bytes()) || e.project().Ack(planeN(1)) != ack || e.appends.Load() != 0 {
				t.Fatalf("stage %d action %d wrote", st, act)
			}
			rep := e.lastReport()
			if act == secretwiretest.ActionHold && rep != secretwire.ReportTimeout {
				t.Fatalf("stage %d hold report %q", st, rep)
			}
			if act == secretwiretest.ActionClose && rep != secretwire.ErrFrameMalformed.Error() && rep != secretwire.ReportDialFailed {
				t.Fatalf("stage %d close report %q", st, rep)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatal("deadline not applied")
			}
			// Recovery: the next clean cycle delivers.
			e.fake.ClearFaults()
			e.cycle()
			if s, _ := e.state(opN(1)); s != secretref.StateActive {
				t.Fatalf("stage %d no recovery", st)
			}
		}
	}
}

// §3.1: repeated timeouts do not grow goroutines; Run and Stop are clean.
func TestRepeatedTimeoutsGoroutinesFRRHZ137(t *testing.T) {
	e := newEnv(t)
	e.ch.ioTimeout = 100 * time.Millisecond
	e.fake.SetStageFault(secretwiretest.StageAfterHello, secretwiretest.ActionHold)
	e.cycle()
	settle := func() int {
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
		return runtime.NumGoroutine()
	}
	base := settle()
	for i := 0; i < 15; i++ {
		e.cycle()
	}
	if n := settle(); n > base+1 {
		t.Fatalf("goroutines %d -> %d", base, n)
	}
	if r := e.reportsCopy(); len(r) != 1 || r[0] != secretwire.ReportTimeout {
		t.Fatalf("reports not deduped: %v", r)
	}

	e.ch.interval = MinInterval
	done := make(chan error, 1)
	go func() { done <- e.ch.Run(context.Background()) }()
	time.Sleep(700 * time.Millisecond)
	e.ch.Stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := settle(); n > base+1 {
		t.Fatalf("goroutines after Run %d -> %d", base, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- e.ch.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// W10(a): a redelivered batch is a no-op.
func TestRedeliveryFRRHZ137(t *testing.T) {
	e := newEnv(t)
	e.hw(5)
	batch := secretwiretest.PartialRejectScenario(opN(1))[:5]
	before := e.bytes()
	e.fake.SetBatch(batch)
	e.cycle()
	e.cycle()
	if !bytes.Equal(before, e.bytes()) || len(e.fake.Notices()) != 0 || e.appends.Load() != 1 {
		t.Fatal("redelivery wrote")
	}
}

// W11: seq-conflict, seq-gap and redundant-outcome stall the channel; the
// next poll carries ack = hw and lastRejected; the same entry repeated gives
// the same rejection with zero writes.
func TestStallFRRHZ137(t *testing.T) {
	cases := []struct {
		reason string
		entry  func() secretwire.Entry
	}{
		{secretwire.EntrySeqConflict, func() secretwire.Entry { return secretwiretest.ObservedEntry(2, ref(opN(1)), 1) }},
		{secretwire.EntrySeqGap, func() secretwire.Entry { return secretwiretest.ObservedEntry(5, ref(opN(1)), 1) }},
		{secretwire.EntryRedundantOutcome, func() secretwire.Entry {
			return secretwiretest.OutcomeEntry(4, opN(1), ref(opN(1)), 1, rOK)
		}},
	}
	for _, c := range cases {
		e := newEnv(t)
		e.hw(3)
		en := c.entry()
		if c.reason == secretwire.EntrySeqConflict {
			en.Present = false // differs from the recorded seq 2
		}
		e.fake.SetBatch([]secretwire.Entry{en})
		e.cycle()
		n := e.lastNotice()
		if n.Reason != c.reason || n.RejectedSeq != en.Seq || n.Ack.PlaneEntrySeq != 3 || e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatalf("%s: notice %+v", c.reason, n)
		}
		after := e.bytes()
		for i := 0; i < 2; i++ {
			e.cycle()
			p := e.lastPoll()
			if p.Ack.PlaneEntrySeq != 3 || p.LastRejected == nil || *p.LastRejected != (secretwire.Rejected{Seq: en.Seq, Reason: c.reason}) {
				t.Fatalf("%s: poll %+v", c.reason, p)
			}
			if m := e.lastNotice(); m.Reason != c.reason || m.RejectedSeq != en.Seq {
				t.Fatalf("%s: repeated notice %+v", c.reason, m)
			}
		}
		if !bytes.Equal(after, e.bytes()) {
			t.Fatalf("%s: repeated rejection wrote", c.reason)
		}
		if r := e.reportsCopy(); len(r) != 1 || r[0] != secretwire.EntryRejectedPrefix+c.reason {
			t.Fatalf("%s: reports %v", c.reason, r)
		}
		if len(e.fake.Notices()) != 3 {
			t.Fatal("notice not repeated")
		}
	}
}

// The three FR-RHZ-137 counterexamples, with R7b checked by the fake.
func TestCounterexamplesFRRHZ137(t *testing.T) {
	t.Run("i", func(t *testing.T) {
		e := newEnv(t)
		e.hw(10)
		for _, en := range secretwiretest.PartialRejectScenario(opN(1))[10:] {
			e.fake.Append(en)
		}
		e.cycle()
		n := e.lastNotice()
		if n.Ack.PlaneEntrySeq != 11 || n.RejectedSeq != 12 || n.Reason != secretwire.EntryVersionMismatch ||
			e.count(secretref.TypeEntryRejected) != 1 || e.project().HighWater(planeN(1)) != 11 {
			t.Fatalf("notice %+v", n)
		}
		before := e.bytes()
		e.cycle()
		if !bytes.Equal(before, e.bytes()) || e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatal("repeat wrote")
		}
	})
	t.Run("ii", func(t *testing.T) {
		e := newEnv(t)
		e.hw(10)
		e.fake.SetBatch([]secretwire.Entry{secretwiretest.ObservedEntry(12, ref(opN(1)), 1)})
		e.cycle()
		n := e.lastNotice()
		if n.Reason != secretwire.EntrySeqGap || n.RejectedSeq != 12 || n.Ack.PlaneEntrySeq != 10 || e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatalf("notice %+v", n)
		}
		e.cycle()
		if e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatal("repeat wrote")
		}
	})
	t.Run("iii", func(t *testing.T) {
		e := newEnv(t)
		e.hw(10)
		en := secretwiretest.ObservedEntry(9, ref(opN(1)), 1)
		en.Present = false
		e.fake.SetBatch([]secretwire.Entry{en})
		e.cycle()
		n := e.lastNotice()
		if n.Reason != secretwire.EntrySeqConflict || n.RejectedSeq != 9 || n.Ack.PlaneEntrySeq != 10 || e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatalf("notice %+v", n)
		}
		e.cycle()
		if e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatal("repeat wrote")
		}
	})
}

// W11: a lost notice is recovered by the next poll's lastRejected.
func TestLostNoticeFRRHZ137(t *testing.T) {
	e := newEnv(t)
	e.hw(10)
	e.fake.DropNotices(true)
	for _, en := range secretwiretest.PartialRejectScenario(opN(1))[10:] {
		e.fake.Append(en)
	}
	e.cycle()
	if len(e.fake.Notices()) != 0 || e.count(secretref.TypeEntryRejected) != 1 {
		t.Fatal("notice not dropped")
	}
	e.fake.DropNotices(false)
	e.cycle()
	p := e.lastPoll()
	if p.LastRejected == nil || *p.LastRejected != (secretwire.Rejected{Seq: 12, Reason: secretwire.EntryVersionMismatch}) || p.Ack.PlaneEntrySeq != 11 {
		t.Fatalf("poll %+v", p)
	}
}

// countingLatch trips on its n-th Poisoned call.
type countingLatch struct {
	trip int64
	n    atomic.Int64
}

func (c *countingLatch) Poisoned() bool { return c.n.Add(1) >= c.trip }

func assertPoisonedNext(t *testing.T, e *env) {
	t.Helper()
	frames := len(e.fake.Received())
	e.cycle()
	got := e.fake.Frames()
	rec := e.fake.Received()
	if len(rec) != frames+1 {
		t.Fatalf("next connection sent %d frames %v conns %d", len(rec)-frames, e.reportsCopy(), e.fake.Connections())
	}
	f := got[len(got)-1]
	if f.Type != secretwire.TypeError || f.Error.Code != secretwire.CodeJournalPoisoned || f.Error.Ack != nil ||
		strings.Contains(string(rec[len(rec)-1]), "ack") {
		t.Fatalf("first frame %+v", f)
	}
	if e.lastReport() != secretwire.ReportJournalPoisoned {
		t.Fatalf("report %q", e.lastReport())
	}
}

// §1.8 poison: (a) no frame prepared after poison is sent, (b) the
// channel's own poisoned append closes with nothing sent, (c) a poll prepared
// before an unrelated poison carries the pre-poison durable ack; the next
// connection's first frame is the ack-less journal-poisoned.
func TestPoisonFRRHZ137(t *testing.T) {
	t.Run("a-before-poll", func(t *testing.T) {
		e := newEnv(t)
		e.register(opN(1), "alpha")
		l := &countingLatch{trip: 3} // start, before hello, then trips before the poll
		e.ch.cfg.Latch = l
		e.cycle()
		if fs := e.fake.Frames(); len(fs) != 1 || fs[0].Type != secretwire.TypeHello || len(e.fake.Polls()) != 0 {
			t.Fatalf("frames %+v", fs)
		}
		assertPoisonedNext(t, e)
	})
	t.Run("a-before-notice", func(t *testing.T) {
		e := newEnv(t)
		e.hw(10)
		for _, en := range secretwiretest.PartialRejectScenario(opN(1))[10:] {
			e.fake.Append(en)
		}
		l := &countingLatch{trip: 5} // start, hello, poll prep, poll send, notice
		e.ch.cfg.Latch = l
		e.cycle()
		if len(e.fake.Notices()) != 0 || e.count(secretref.TypeEntryRejected) != 1 {
			t.Fatal("notice sent after poison")
		}
		assertPoisonedNext(t, e)
	})
	t.Run("b-own-append", func(t *testing.T) {
		e := newEnv(t)
		e.register(opN(1), "alpha")
		e.fake.Append(secretwiretest.OutcomeEntry(0, opN(1), ref(opN(1)), 1, rOK))
		e.fake.Append(secretwiretest.ObservedEntry(0, ref(opN(1)), 2)) // would be rejected
		before := e.bytes()
		e.store.arm.Store(true)
		frames := len(e.fake.Received())
		e.cycle()
		if len(e.fake.Received()) != frames+2 || len(e.fake.Notices()) != 0 { // hello, poll only
			t.Fatal("frame sent after own poisoned append")
		}
		if !bytes.Equal(before, e.bytes()) || e.lastReport() != secretwire.ReportJournalPoisoned {
			t.Fatalf("wrote or report %q", e.lastReport())
		}
		assertPoisonedNext(t, e)
	})
	t.Run("c-unrelated", func(t *testing.T) {
		e := newEnv(t)
		e.hw(4)
		e.fake.Append(secretwiretest.PartialRejectScenario(opN(1))[4])
		e.fake.OnPoll(func(secretwiretest.RecordedPoll) { e.store.latched.Store(true) })
		before := e.bytes()
		frames := len(e.fake.Received())
		e.cycle()
		e.installHooks()
		if p := e.lastPoll(); p.Ack.PlaneEntrySeq != 4 || len(e.fake.Received()) != frames+2 {
			t.Fatalf("poll %+v", p)
		}
		if !bytes.Equal(before, e.bytes()) {
			t.Fatal("wrote after poison")
		}
		assertPoisonedNext(t, e)
	})
}

// P5 with the fake: resume as P′, P's entries never return, a quarantined
// outcome's op stays pending, P retires after P′'s first event, P′'s first
// poll has ack 0, a stall clears once.
func TestEpochFRRHZ137(t *testing.T) {
	e := newEnv(t)
	P, P2 := planeN(1), planeN(2)
	e.register(opN(2), "beta")
	e.hw(10)
	for _, en := range secretwiretest.PartialRejectScenario(opN(1))[10:] {
		e.fake.Append(en)
	}
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(2), ref(opN(2)), 1, rOK)) // seq 13
	e.cycle()
	if e.project().HighWater(P) != 11 {
		t.Fatal("hw")
	}
	e.fake.Freeze(P2)
	e.cycle() // cutoff poll on P, empty batch: the stalled P repeats its notice
	eps := e.fake.Epochs()
	if len(eps) != 1 || eps[0].Cutoff != 11 || eps[0].LastRejected == nil || eps[0].LastRejected.Seq != 12 ||
		len(eps[0].Quarantined) != 2 || eps[0].Quarantined[0].Seq != 12 || eps[0].Quarantined[1].Seq != 13 {
		t.Fatalf("epoch %+v", eps)
	}
	e.cycle() // P′
	p := e.lastPoll()
	if p.Ack != (secretwire.Ack{PlaneID: P2}) || p.LastRejected != nil || len(p.Pending) != 1 || p.Pending[0].OpID != opN(2) {
		t.Fatalf("P' first poll %+v", p)
	}
	if !slices.Contains(e.reportsCopy(), secretwire.ReportStallCleared) {
		t.Fatalf("reports %v", e.reportsCopy())
	}
	if e.project().Retired(P) {
		t.Fatal("P retired before any P' event")
	}
	e.fake.Append(secretwiretest.ObservedEntry(0, ref(opN(1)), 1))
	e.cycle()
	pr := e.project()
	if !pr.Retired(P) || pr.HighWater(P2) != 1 || pr.HighWater(P) != 11 { // P's 12, 13 never arrive
		t.Fatal("P not retired")
	}
	if pend := pr.Pending(10); len(pend) != 1 || pend[0].OpID != opN(2) {
		t.Fatalf("quarantined op not pending: %+v", pend)
	}
	before := e.bytes()
	e.fake.AnnouncePlaneID(P)
	e.cycle()
	fs := e.fake.Frames()
	if f := fs[len(fs)-1]; f.Type != secretwire.TypeError || f.Error.Code != secretwire.CodeRetiredPlane {
		t.Fatalf("frame %+v", f)
	}
	if e.lastReport() != secretwire.ReportRetiredPlane || !bytes.Equal(before, e.bytes()) {
		t.Fatal("retired-plane wrote or not reported")
	}
	n := 0
	for _, r := range e.reportsCopy() {
		if r == secretwire.ReportStallCleared {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("stall-cleared %d", n)
	}
}

// R7a: a plane whose only event is an E7 retires after P′'s first E7.
func TestEpochE7OnlyFRRHZ137(t *testing.T) {
	e := newEnv(t)
	P, P2 := planeN(1), planeN(2)
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(9), ref(opN(9)), 1, rOK)) // unknown-op at hw 0
	e.cycle()
	if n := e.lastNotice(); n.Reason != secretwire.EntryUnknownOp || n.RejectedSeq != 1 || n.Ack.PlaneEntrySeq != 0 {
		t.Fatalf("notice %+v", n)
	}
	if e.appends.Load() != 1 { // W12: an E7-only batch also pushes
		t.Fatalf("OnAppend %d after E7-only batch", e.appends.Load())
	}
	e.cycle() // stalled repeat: no write, no push
	if e.appends.Load() != 1 {
		t.Fatal("OnAppend without a write")
	}
	e.fake.Freeze(P2)
	e.cycle()
	e.cycle()
	if p := e.lastPoll(); p.Ack != (secretwire.Ack{PlaneID: P2}) {
		t.Fatalf("P' first poll %+v", p)
	}
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(8), ref(opN(8)), 1, rOK)) // E7 on P′
	e.cycle()
	if !e.project().Retired(P) {
		t.Fatal("P not retired by P' E7")
	}
	e.fake.AnnouncePlaneID(P)
	e.cycle()
	if e.lastReport() != secretwire.ReportRetiredPlane {
		t.Fatalf("report %q", e.lastReport())
	}
}

// W1: the socket's parent must be a real 0700-class directory of this UID.
func TestSocketDirFRRHZ137(t *testing.T) {
	e := newEnv(t)
	dir := e.fake.Dir()
	defer os.Chmod(dir, 0o700)
	for _, m := range []os.FileMode{0o700, 0o500} {
		os.Chmod(dir, m)
		n := e.fake.Connections()
		e.cycle()
		if e.fake.Connections() != n+1 {
			t.Fatalf("mode %o did not dial", m)
		}
	}
	for _, m := range []os.FileMode{0o710, 0o755, 0o701, 0o720} {
		os.Chmod(dir, m)
		n := e.fake.Connections()
		e.cycle()
		if e.fake.Connections() != n || e.lastReport() != secretwire.ReportSocketDirUnsafe {
			t.Fatalf("mode %o dialled", m)
		}
	}
	os.Chmod(dir, 0o700)

	link := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	e.ch.cfg.SocketPath = filepath.Join(link, filepath.Base(e.fake.SocketPath()))
	n := e.fake.Connections()
	e.ch.Cycle(context.Background())
	if e.fake.Connections() != n || e.lastReport() != secretwire.ReportSocketDirUnsafe {
		t.Fatal("symlinked dir dialled")
	}

	e.ch.cfg.SocketPath = e.fake.SocketPath()
	e.ch.uid = os.Getuid() + 1 // another owner
	e.ch.Cycle(context.Background())
	if e.fake.Connections() != n {
		t.Fatal("foreign-owned dir dialled")
	}
	e.ch.uid = os.Getuid()
	e.cycle()
	if e.fake.Connections() != n+1 {
		t.Fatal("no dial after restore")
	}
}

// W7/W8: after a Rhizome restart (a new writer over the same journal) the
// pending request is resent and the result recovered; ack is unchanged.
func TestRestartFRRHZ137(t *testing.T) {
	e := newEnv(t)
	e.register(opN(1), "alpha")
	e.register(opN(2), "beta")
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(1), ref(opN(1)), 1, rOK))
	e.cycle()
	ack := e.project().Ack(planeN(1))
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(2), ref(opN(2)), 1, rOK))
	e.restart()
	e.cycle()
	p := e.lastPoll()
	if p.Ack != ack || len(p.Pending) != 1 || p.Pending[0].OpID != opN(2) {
		t.Fatalf("poll after restart %+v", p)
	}
	if s, v := e.state(opN(2)); s != secretref.StateActive || v != 1 {
		t.Fatalf("state %s %d", s, v)
	}
	if e.appends.Load() != 2 {
		t.Fatal("OnAppend")
	}
}

// Approval Q3: a pinned planeId refuses another plane with peer-rejected.
func TestPinnedPlaneFRRHZ137(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.ExpectedPlaneID = planeN(7) })
	e.register(opN(1), "alpha")
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(1), ref(opN(1)), 1, rOK))
	before := e.bytes()
	e.cycle()
	fs := e.fake.Frames()
	if len(fs) != 2 || fs[1].Type != secretwire.TypeError || fs[1].Error.Code != secretwire.CodePeerRejected ||
		len(e.fake.Polls()) != 0 {
		t.Fatalf("frames %+v", fs)
	}
	if !bytes.Equal(before, e.bytes()) || e.lastReport() != secretwire.ReportPeerRejected {
		t.Fatal("pinned mismatch wrote or not reported")
	}
	e.ch.cfg.ExpectedPlaneID = planeN(1)
	e.cycle()
	if s, _ := e.state(opN(1)); s != secretref.StateActive {
		t.Fatal("matching pin refused")
	}
}

// §3.5 frame faults: each is refused with its fixed §3.6 report and zero
// writes; ZV over the journal, the reports and every frame Rhizome sent.
func TestFrameFaultsZVFRRHZ137(t *testing.T) {
	s := sentinel(t)
	e := newEnv(t)
	e.register(opN(1), "alpha")
	e.fake.Append(secretwiretest.OutcomeEntry(0, opN(1), ref(opN(1)), 1, rOK))
	before := e.bytes()
	malformed := secretwire.ErrFrameMalformed.Error()
	cases := []struct {
		hello bool
		f     secretwiretest.FrameFault
		want  string
	}{
		{true, secretwiretest.FaultBrokenJSON, malformed},
		{true, secretwiretest.FaultDupKey, malformed},
		{true, secretwiretest.FaultNull, malformed},
		{true, secretwiretest.FaultUnknownValue, secretwire.ErrFrameUnknownField.Error()},
		{true, secretwiretest.FaultTooLarge, secretwire.ErrFrameTooLarge.Error()},
		{true, secretwiretest.FaultNonUTF8, malformed},
		{true, secretwiretest.FaultVersionMismatch, secretwire.ErrFrameBadVersion.Error()},
		{false, secretwiretest.FaultBrokenJSON, malformed},
		{false, secretwiretest.FaultDupKey, malformed},
		{false, secretwiretest.FaultNull, malformed},
		{false, secretwiretest.FaultUnknownValue, secretwire.ErrFrameUnknownField.Error()},
		{false, secretwiretest.FaultTooLarge, secretwire.ErrFrameTooLarge.Error()},
		{false, secretwiretest.FaultNonUTF8, malformed},
		{false, secretwiretest.FaultVersionMismatch, secretwire.ErrFrameBadVersion.Error()},
		{false, secretwiretest.FaultPlaneIDChange, secretwire.ErrFramePlaneIDMismatch.Error()},
		{false, secretwiretest.FaultSeqGap, secretwire.ErrFrameSeqOrder.Error()},
		{false, secretwiretest.FaultSeqNonIncreasing, secretwire.ErrFrameSeqOrder.Error()},
		{false, secretwiretest.FaultTooManyEntries, secretwire.ErrFrameTooManyEntries.Error()},
	}
	for _, c := range cases {
		e.fake.ClearFaults()
		if c.hello {
			e.fake.SetHelloFault(c.f, s)
		} else {
			e.fake.SetBatchFault(c.f, s)
		}
		e.ch.clean()
		e.cycle()
		if got := e.lastReport(); got != c.want {
			t.Fatalf("hello=%v fault %d: report %q", c.hello, c.f, got)
		}
		if !bytes.Equal(before, e.bytes()) {
			t.Fatalf("fault %d wrote", c.f)
		}
	}
	e.fake.ClearFaults()
	e.cycle()
	if st, _ := e.state(opN(1)); st != secretref.StateActive {
		t.Fatal("no recovery")
	}

	// A rejected entry after the faults; then ZV.
	e.fake.SetBatch([]secretwire.Entry{secretwiretest.ObservedEntry(5, ref(opN(1)), 1)})
	e.cycle()
	assertNoSentinel(t, "journal", e.bytes(), s)
	assertNoSentinel(t, "reports", []byte(strings.Join(e.reportsCopy(), "\n")), s)
	for _, f := range e.fake.Received() {
		assertNoSentinel(t, "rhizome frame", f, s)
	}
	for _, r := range e.reportsCopy() {
		if !validReport(r) {
			t.Fatalf("report %q outside §3.6", r)
		}
	}
}

func validReport(r string) bool {
	switch r {
	case secretwire.ReportSocketDirUnsafe, secretwire.ReportDialFailed, secretwire.ReportTimeout,
		secretwire.ReportPeerRejected, secretwire.ReportRetiredPlane, secretwire.ReportAppendFailed,
		secretwire.ReportRegistryUnavailable, secretwire.ReportJournalPoisoned, secretwire.ReportStallCleared:
		return true
	}
	if strings.HasPrefix(r, "frame-rejected:") {
		return true
	}
	return strings.HasPrefix(r, secretwire.EntryRejectedPrefix) &&
		secretwire.ValidEntryReason(strings.TrimPrefix(r, secretwire.EntryRejectedPrefix))
}

// Fake P5: a sticky override never leaks into the cutoff batch or into P′,
// and a frozen plane refuses new facts (also from a hook on the cutoff poll).
func TestFakeEpochOverrideFRRHZ137(t *testing.T) {
	e := newEnv(t)
	P, P2 := planeN(1), planeN(2)
	e.hw(3)
	e.fake.SetBatch([]secretwire.Entry{secretwiretest.ObservedEntry(4, ref(opN(1)), 1)})
	e.fake.Freeze(P2)
	if e.fake.Append(secretwiretest.ObservedEntry(0, ref(opN(1)), 1)) != 0 {
		t.Fatal("frozen plane accepted a fact")
	}
	var hookSeq atomic.Int64
	hookSeq.Store(-1)
	e.fake.OnPoll(func(secretwiretest.RecordedPoll) {
		hookSeq.Store(int64(e.fake.Append(secretwiretest.ObservedEntry(0, ref(opN(1)), 1))))
	})
	before := e.bytes()
	e.cycle() // cutoff on P: entries:[] although an override is set
	e.installHooks()
	if hookSeq.Load() != 0 {
		t.Fatalf("Append during the cutoff poll returned %d", hookSeq.Load())
	}
	if !bytes.Equal(before, e.bytes()) || e.project().HighWater(P) != 3 {
		t.Fatal("cutoff batch was not empty")
	}
	if eps := e.fake.Epochs(); len(eps) != 1 || eps[0].Cutoff != 3 || len(eps[0].Quarantined) != 0 {
		t.Fatalf("epoch %+v", eps)
	}
	e.cycle() // P′: empty outbox, the old override is gone
	if p := e.lastPoll(); p.Ack != (secretwire.Ack{PlaneID: P2}) {
		t.Fatalf("P' poll %+v", p)
	}
	if !bytes.Equal(before, e.bytes()) || e.count(secretref.TypeEntryRejected) != 0 {
		t.Fatal("P' sent stale entries")
	}
	if seq := e.fake.Append(secretwiretest.ObservedEntry(0, ref(opN(1)), 1)); seq != 1 {
		t.Fatalf("P' outbox not empty: seq %d", seq)
	}
	e.cycle()
	if e.project().HighWater(P2) != 1 || !e.project().Retired(P) {
		t.Fatal("P' fact not recorded")
	}
}

// W11 ②: a failed notice send is reported after the rejection; the E7 stays
// durable and the next poll carries it.
func TestNoticeSendFailureFRRHZ137(t *testing.T) {
	e := newEnv(t)
	e.hw(10)
	for _, en := range secretwiretest.PartialRejectScenario(opN(1))[10:] {
		e.fake.Append(en)
	}
	e.fake.DropNotices(true) // the fake closes right after the batch
	e.ch.cfg.OnAppend = func() { e.fake.WaitHandled(int(e.dials.Load()), 5*time.Second) }
	e.cycle()
	r := e.reportsCopy()
	if len(r) != 2 || r[0] != secretwire.EntryRejectedPrefix+secretwire.EntryVersionMismatch || r[1] != secretwire.ReportDialFailed {
		t.Fatalf("reports %v", r)
	}
	if e.count(secretref.TypeEntryRejected) != 1 || len(e.fake.Notices()) != 0 {
		t.Fatal("E7 not durable or notice received")
	}
	e.fake.DropNotices(false)
	e.cycle()
	if p := e.lastPoll(); p.LastRejected == nil || p.LastRejected.Seq != 12 || p.Ack.PlaneEntrySeq != 11 {
		t.Fatalf("poll %+v", p)
	}
}

// The fake deep-copies what it is given and what it returns.
func TestFakeCopiesFRRHZ137(t *testing.T) {
	e := newEnv(t)
	op := opN(4)
	en := secretwiretest.ReconciledEntry(1, op, "delta")
	en.ScopeTags = []string{"a", "b"}
	e.fake.SetBatch([]secretwire.Entry{en})
	en.ScopeTags[0] = "zz"
	r, err := e.svc.RequestRegister(secretref.RegisterRequest{OpID: opN(5), Name: "eps",
		RefKind: secretwire.RefKindOther, ScopeTags: []string{"x", "y"}, Actor: actor})
	if err != nil || !r.Accepted {
		t.Fatal("register")
	}
	e.cycle()
	v, ok := e.project().Ref(ref(op))
	if !ok || !slices.Equal(v.ScopeTags, []string{"a", "b"}) {
		t.Fatalf("override aliased the caller's slice: %+v", v)
	}
	e.fake.ClearBatch()
	e.fake.SetBatch([]secretwire.Entry{secretwiretest.ObservedEntry(9, ref(op), 1)})
	e.cycle() // seq-gap: lastRejected and a notice ack to mutate
	e.fake.ClearBatch()
	e.cycle()
	ps, ns, fs := e.fake.Polls(), e.fake.Notices(), e.fake.Frames()
	p := ps[len(ps)-1].Poll
	p.LastRejected.Seq = 99
	for i := range p.Pending {
		if p.Pending[i].ScopeTags != nil {
			p.Pending[i].ScopeTags[0] = "zz"
		}
	}
	ns[0].Notice.Ack.PlaneEntrySeq = 99
	for i := range fs {
		if fs[i].Poll.LastRejected != nil {
			fs[i].Poll.LastRejected.Seq = 99
		}
	}
	q := e.fake.Polls()[len(ps)-1].Poll
	if q.LastRejected.Seq != 9 || e.fake.Notices()[0].Notice.Ack.PlaneEntrySeq != 1 {
		t.Fatal("records aliased")
	}
	for _, x := range q.Pending {
		if x.ScopeTags != nil && x.ScopeTags[0] != "x" {
			t.Fatal("pending scopeTags aliased")
		}
	}
	for _, f := range e.fake.Frames() {
		if f.Poll.LastRejected != nil && f.Poll.LastRejected.Seq == 99 {
			t.Fatal("frames aliased")
		}
	}
}
