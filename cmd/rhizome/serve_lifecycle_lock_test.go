package main

// FR-RHZ-107: journal lock via flock (released by the kernel when the holder
// dies) + serve shuts down on SIGINT/SIGTERM, draining in-flight work before
// the journal is closed and the lock released.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"rhizome/internal/journal"
)

// T1: a lock file left behind by a dead process (content present, nobody holds
// the flock) is acquired without manual removal.
func TestStaleLockDeadHolderFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	lock := jp + ".lock"
	stale := "2147483000 2026-01-01T00:00:00Z"
	if e := os.WriteFile(lock, []byte(stale), 0600); e != nil {
		t.Fatal(e)
	}
	var eout bytes.Buffer
	unlock, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatalf("stale lock not recovered: %v (%s)", e, eout.String())
	}
	b, _ := os.ReadFile(lock)
	if f := strings.Fields(string(b)); len(f) != 2 || f[0] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock content not ours: %q", b)
	}
	unlock()
	if _, e := os.Stat(lock); !os.IsNotExist(e) {
		t.Fatal("lock remains after unlock")
	}
}

// T2: a lock held by a live holder is refused with the existing message, even
// from the same process (flock is per open file description).
func TestLiveLockRefusedFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	lock := jp + ".lock"
	var eout bytes.Buffer
	unlock, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	content, _ := os.ReadFile(lock)
	eout.Reset()
	if u2, e2 := acquireJournalLock(jp, &eout); e2 == nil {
		u2()
		t.Fatal("live lock was taken over")
	}
	if !strings.Contains(eout.String(), "journal lock held by a running writer") || !strings.Contains(eout.String(), string(content)) || !strings.Contains(eout.String(), "stop that process first") || strings.Contains(eout.String(), "remove") {
		t.Fatalf("unexpected message: %q", eout.String())
	}
	if b, _ := os.ReadFile(lock); string(b) != string(content) {
		t.Fatalf("live lock modified: %q", b)
	}
	// Still refused for a holder that was never ours.
	unlock()
	unlock2, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatal("relock after release failed:", e)
	}
	unlock2()
}

// T3: starters racing on a stale lock — exactly one wins, every time.
func TestStaleLockRaceSingleWinnerFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	lock := jp + ".lock"
	for i := 0; i < 200; i++ {
		if e := os.WriteFile(lock, []byte("2147483000 2026-01-01T00:00:00Z"), 0600); e != nil {
			t.Fatal(e)
		}
		var wins int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		unlocks := make(chan func(), 2)
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if u, e := acquireJournalLock(jp, io.Discard); e == nil {
					atomic.AddInt32(&wins, 1)
					unlocks <- u
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("iteration %d: %d winners", i, wins)
		}
		(<-unlocks)()
	}
}

func waitLock(t *testing.T, lock string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, e := os.Stat(lock)
		if (e == nil) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lock %s existence != %v", lock, want)
}

// startServe runs serveCtx on an ephemeral port; the lock's existence marks it as started.
func startServe(t *testing.T, ctx context.Context, jp string, eout *bytes.Buffer) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- serveCtx(ctx, []string{"-journal", jp, "-addr", "127.0.0.1:0"}, io.Discard, eout) }()
	waitLock(t, jp+".lock", true)
	return done
}

func waitDone(t *testing.T, done <-chan int, eout *bytes.Buffer, within time.Duration) time.Duration {
	t.Helper()
	t0 := time.Now()
	select {
	case c := <-done:
		if c != 0 {
			t.Fatalf("serve exit %d: %s", c, eout.String())
		}
	case <-time.After(within):
		t.Fatalf("serve did not return within %s", within)
	}
	return time.Since(t0)
}

// T4a: context cancel → serveCtx returns 0 and the lock is gone.
func TestServeCancelReleasesLockFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var eout bytes.Buffer
	done := startServe(t, ctx, jp, &eout)
	cancel()
	waitDone(t, done, &eout, 10*time.Second)
	waitLock(t, jp+".lock", false)
}

// T4b: the real signal path — SIGINT to ourselves while serve runs (the signal
// context is registered before the lock is taken, so the lock's existence
// proves the handler is installed).
func TestServeSignalReleasesLockFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	var o, eout bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- serve([]string{"-journal", jp, "-addr", "127.0.0.1:0"}, &o, &eout) }()
	waitLock(t, jp+".lock", true)
	if e := syscall.Kill(os.Getpid(), syscall.SIGINT); e != nil {
		t.Fatal(e)
	}
	waitDone(t, done, &eout, 10*time.Second)
	waitLock(t, jp+".lock", false)
	unlock, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatalf("relock after signal failed: %v (%s)", e, eout.String())
	}
	unlock()
}

// serveWithURL starts serveCtx on a port the test picked (so it knows the URL)
// and, via the test hook, optionally delays /v1/intent.
func serveWithURL(t *testing.T, ctx context.Context, jp string, delay time.Duration, arrived chan<- struct{}) (string, <-chan int, *bytes.Buffer) {
	t.Helper()
	addrCh := make(chan string, 1)
	serveHandlerWrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/intent" && delay > 0 {
				if arrived != nil {
					arrived <- struct{}{}
				}
				time.Sleep(delay)
			}
			next.ServeHTTP(w, r)
		})
	}
	t.Cleanup(func() { serveHandlerWrap = nil })
	var eout bytes.Buffer
	done := make(chan int, 1)
	// Bind the port ourselves so the test knows the URL: pick a free port.
	go func() {
		done <- serveCtx(ctx, []string{"-journal", jp, "-addr", freeAddr(t, addrCh)}, io.Discard, &eout)
	}()
	addr := <-addrCh
	waitLock(t, jp+".lock", true)
	// Wait until the listener answers.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, e := http.Get("http://" + addr + "/v1/workspace"); e == nil {
			res.Body.Close()
			return "http://" + addr, done, &eout
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server not reachable")
	return "", nil, nil
}

func freeAddr(t *testing.T, out chan<- string) string {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := ln.Addr().String()
	ln.Close()
	out <- addr
	return addr
}

// T5: a slow intent in flight when ctx is cancelled completes and its event is
// in the journal after serveCtx returns (shutdown drains before close/unlock).
func TestServeShutdownDrainsInFlightFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	arrived := make(chan struct{}, 1)
	url, done, eout := serveWithURL(t, ctx, jp, 300*time.Millisecond, arrived)
	type result struct {
		status int
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		res, e := http.Post(url+"/v1/intent", "application/json", strings.NewReader(`{"kind":"mission.create","name":"drain","prompt":"p","actor":"op"}`))
		if e != nil {
			resCh <- result{0, e}
			return
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		resCh <- result{res.StatusCode, nil}
	}()
	<-arrived // the request is inside the handler's 300ms delay
	cancel()
	waitDone(t, done, eout, 10*time.Second)
	// Observable: the mission event is in the journal after serveCtx returned.
	j, e := journal.Open(jp)
	if e != nil {
		t.Fatal(e)
	}
	defer j.Close()
	found := false
	for _, ev := range j.All() {
		if ev.AggregateType == "mission" {
			found = true
		}
	}
	if !found {
		t.Fatal("in-flight intent was not written before shutdown completed")
	}
	r := <-resCh
	if r.err != nil || r.status != 200 {
		t.Fatalf("in-flight request: status %d err %v", r.status, r.err)
	}
}

// T6: an open SSE client does not delay shutdown (request contexts end with ctx).
func TestServeShutdownWithOpenSSEFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, done, eout := serveWithURL(t, ctx, jp, 0, nil)
	res, e := http.Get(url + "/v1/workspace/stream")
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	buf := make([]byte, 16)
	if _, e := res.Body.Read(buf); e != nil { // first snapshot event has arrived
		t.Fatal(e)
	}
	cancel()
	if took := waitDone(t, done, eout, 10*time.Second); took > time.Second {
		t.Fatalf("shutdown with open SSE took %s", took)
	}
	waitLock(t, jp+".lock", false)
}

// T7: churn — 8 goroutines acquire/unlock in a loop for ~1s; the holder count
// never exceeds 1. Fails if the post-flock SameFile re-check is removed (an
// unlinked inode can be locked by a straggler while a newcomer locks the new file).
func TestLockChurnSingleHolderFRRHZ107(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	var holders, overlaps, acquired int32
	deadline := time.Now().Add(1 * time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				u, e := acquireJournalLock(jp, io.Discard)
				if e != nil {
					continue
				}
				if atomic.AddInt32(&holders, 1) > 1 {
					atomic.AddInt32(&overlaps, 1)
				}
				atomic.AddInt32(&acquired, 1)
				// Hold briefly so a concurrent (wrongful) holder is observed; a
				// zero-length hold would let two holders miss each other.
				time.Sleep(50 * time.Microsecond)
				if atomic.LoadInt32(&holders) > 1 {
					atomic.AddInt32(&overlaps, 1)
				}
				atomic.AddInt32(&holders, -1)
				u()
			}
		}()
	}
	wg.Wait()
	if overlaps != 0 {
		t.Fatalf("%d overlapping holders in %d acquisitions", overlaps, acquired)
	}
	t.Logf("%d acquisitions, 0 overlaps", acquired)
}
