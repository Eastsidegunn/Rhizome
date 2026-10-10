// Package secretchannel is Rhizome's side of the secret channel (RHZ-107 G1,
// SCP §3.1–§3.6, FR-RHZ-137): a loop that periodically dials the secret
// plane's Unix socket, runs one hello/poll/batch cycle per connection and
// hands the batch to the single secret writer (secretref.Service).
//
// Acks are never computed here: they come from the writer, which derives
// them from durable events (W8). Reports go to OnError and are only the
// fixed §3.6 strings.
//
// W8 invariant (ack = durable high water at send time). The poll ack is
// computed by Service.PollState and the notice ack by Service.ProcessBatch,
// each inside the writer's critical section; the frame is then encoded and
// sent after the lock is released. That is still the durable hw(P) at send
// time because only channel-side writer methods (ProcessBatch, Record*)
// append E4–E7, the channel is their only caller, and its cycles are
// serialized (cycleMu): nothing can append a fact or rejection for P between
// the computation and the send. Intent writes (E1–E3) do not move hw(P). A
// composition root must therefore give one Channel per Service and never call
// the channel-side Service methods elsewhere.
package secretchannel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/peercred"
	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
)

// Interval bounds (§3.4).
const (
	DefaultInterval = 2 * time.Second
	MinInterval     = 200 * time.Millisecond
	// DialTimeout and IOTimeout are §3.1 steps 2 and 3.
	DialTimeout = 2 * time.Second
	IOTimeout   = 10 * time.Second
)

// ErrConfig is New's only error (fixed text, no input echo).
var ErrConfig = errors.New("secret channel: invalid config")

// Config wires one channel. SocketPath and Service are required.
type Config struct {
	// SocketPath is the plane's absolute socket path (W1).
	SocketPath string
	// Interval between cycle starts; 0 means DefaultInterval, otherwise at
	// least MinInterval.
	Interval time.Duration
	// ExpectedPlaneID pins the plane (approval Q3). Empty accepts any
	// planeId (R7a); otherwise a plane hello with another planeId gets
	// error{code:"peer-rejected"}, the connection closes and nothing is
	// written.
	ExpectedPlaneID string
	// Service is the single secret writer shared with the intent relay. This
	// Channel must be the only caller of its channel-side methods
	// (ProcessBatch, Record*): the W8 invariant in the package comment
	// depends on it.
	Service *secretref.Service
	// Latch is the journal poison latch (§1.8); nil means never poisoned.
	Latch events.PoisonLatch
	// Verify is the W2 seam; nil means peercred.Verify.
	Verify func(*net.UnixConn) (peercred.Verified, error)
	// OnAppend is called after a batch appended at least one event: a fact
	// (E4–E6) or a rejection record (E7) (W12).
	OnAppend func()
	// OnError receives §3.6 report strings, consecutive duplicates dropped.
	OnError func(string)
}

// Channel is the loop. One cycle runs at a time.
type Channel struct {
	cfg      Config
	interval time.Duration

	// Test seams (in-package only).
	dial        func(ctx context.Context, path string, timeout time.Duration) (*net.UnixConn, error)
	lstat       func(string) (os.FileInfo, error)
	uid         int
	dialTimeout time.Duration
	ioTimeout   time.Duration

	cycleMu sync.Mutex // one cycle at a time

	repMu   sync.Mutex
	last    string // last reported message (dedupe)
	stalled bool

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New validates cfg and returns the channel.
func New(cfg Config) (*Channel, error) {
	if cfg.Service == nil || !filepath.IsAbs(cfg.SocketPath) || filepath.Clean(cfg.SocketPath) != cfg.SocketPath {
		return nil, ErrConfig
	}
	if cfg.ExpectedPlaneID != "" && !secretwire.ValidPlaneID(cfg.ExpectedPlaneID) {
		return nil, ErrConfig
	}
	iv := cfg.Interval
	if iv == 0 {
		iv = DefaultInterval
	}
	if iv < MinInterval {
		return nil, ErrConfig
	}
	if cfg.Verify == nil {
		cfg.Verify = peercred.Verify
	}
	return &Channel{cfg: cfg, interval: iv, dial: dialUnix, lstat: os.Lstat, uid: os.Getuid(),
		dialTimeout: DialTimeout, ioTimeout: IOTimeout}, nil
}

func dialUnix(ctx context.Context, path string, timeout time.Duration) (*net.UnixConn, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, errors.New("not a unix conn")
	}
	return uc, nil
}

// Run cycles every Interval until ctx is cancelled or Stop is called. It
// returns nil when stopped.
func (c *Channel) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.runMu.Lock()
	if c.cancel != nil {
		c.runMu.Unlock()
		cancel()
		return errors.New("secret channel: already running")
	}
	c.cancel, c.done = cancel, done
	c.runMu.Unlock()
	defer func() {
		cancel()
		c.runMu.Lock()
		c.cancel, c.done = nil, nil
		c.runMu.Unlock()
		close(done)
	}()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		c.Cycle(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Stop cancels a running Run and waits for it to return.
func (c *Channel) Stop() {
	c.runMu.Lock()
	cancel, done := c.cancel, c.done
	c.runMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (c *Channel) report(msg string) {
	c.repMu.Lock()
	dup := msg == c.last
	c.last = msg
	c.repMu.Unlock()
	if !dup && c.cfg.OnError != nil {
		c.cfg.OnError(msg)
	}
}

// clean marks a cycle that ended without a report, so the next identical
// failure is reported again.
func (c *Channel) clean() {
	c.repMu.Lock()
	c.last = ""
	c.repMu.Unlock()
}

// setStalled tracks W11 stall; the end of a stall is reported once.
func (c *Channel) setStalled(s bool) {
	c.repMu.Lock()
	was := c.stalled
	c.stalled = s
	c.repMu.Unlock()
	if was && !s {
		c.report(secretwire.ReportStallCleared)
	}
}

func (c *Channel) poisoned() bool { return c.cfg.Latch != nil && c.cfg.Latch.Poisoned() }

// socketDirSafe is W1: the parent directory is a real directory (not a
// symlink) owned by this process's UID with no group/other permission bits.
func (c *Channel) socketDirSafe() bool {
	fi, err := c.lstat(filepath.Dir(c.cfg.SocketPath))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	owner, ok := ownerUID(fi)
	return ok && owner == c.uid
}

// result of one cycle, for tests.
type result string

const resultOK result = ""

// Cycle runs one connection cycle (§3.1 steps 1–11). It blocks while
// another cycle is running.
func (c *Channel) Cycle(ctx context.Context) {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	msg := c.cycle(ctx)
	if ctx.Err() != nil {
		return // shutting down: nothing to report
	}
	if msg == resultOK {
		c.clean()
		return
	}
	c.report(string(msg))
}

func ioReport(err error) result {
	switch {
	case errors.Is(err, secretwire.ErrTimeout), errors.Is(err, os.ErrDeadlineExceeded):
		return secretwire.ReportTimeout
	case errors.Is(err, io.EOF):
		// The plane closed before a frame: a missing frame (W3).
		return result(secretwire.ErrFrameMalformed.Error())
	}
	var fe interface{ Timeout() bool }
	if errors.As(err, &fe) && fe.Timeout() {
		return secretwire.ReportTimeout
	}
	for _, f := range []error{secretwire.ErrFrameTooLarge, secretwire.ErrFrameMalformed, secretwire.ErrFrameUnknownField,
		secretwire.ErrFrameBadVersion, secretwire.ErrFrameBadType, secretwire.ErrFramePlaneIDMismatch,
		secretwire.ErrFrameSeqOrder, secretwire.ErrFrameTooManyEntries} {
		if errors.Is(err, f) {
			return result(f.Error())
		}
	}
	return secretwire.ReportDialFailed // the connection broke
}

func writerReport(err error) result {
	switch {
	case errors.Is(err, events.ErrPoisoned):
		return secretwire.ReportJournalPoisoned
	case errors.Is(err, secretref.ErrRetiredPlane):
		return secretwire.ReportRetiredPlane
	case errors.Is(err, secretref.ErrPeerRejected):
		return secretwire.ReportPeerRejected
	case errors.Is(err, secretref.ErrAppendFailed):
		return secretwire.ReportAppendFailed
	case errors.Is(err, secretwire.ErrFrameMalformed):
		return result(secretwire.ErrFrameMalformed.Error())
	}
	return secretwire.ReportRegistryUnavailable
}

func (c *Channel) cycle(ctx context.Context) result {
	poisonedAtStart := c.poisoned() // §1.8: checked before dial
	if !c.socketDirSafe() {         // step 1, W1
		return secretwire.ReportSocketDirUnsafe
	}
	conn, err := c.dial(ctx, c.cfg.SocketPath, c.dialTimeout) // step 2
	if err != nil {
		return secretwire.ReportDialFailed
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(c.ioTimeout)); err != nil { // step 3
		return secretwire.ReportDialFailed
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	v, err := c.cfg.Verify(conn) // step 4, W2
	if err != nil || !v.SameUser() {
		return secretwire.ReportPeerRejected
	}
	send := func(b []byte) result {
		if _, err := conn.Write(b); err != nil {
			return ioReport(err)
		}
		return resultOK
	}
	sendError := func(code string) result {
		b, err := secretwire.EncodeError(secretwire.ErrorFrame{Code: code})
		if err != nil {
			return secretwire.ReportRegistryUnavailable
		}
		return send(b)
	}

	// step 5: hello, or the ack-less journal-poisoned frame (§1.8).
	if poisonedAtStart || c.poisoned() {
		if r := sendError(secretwire.CodeJournalPoisoned); r != resultOK {
			return r
		}
		return secretwire.ReportJournalPoisoned
	}
	hello, err := secretwire.EncodeRhizomeHello()
	if err != nil {
		return secretwire.ReportRegistryUnavailable
	}
	if r := send(hello); r != resultOK {
		return r
	}

	// step 6: plane hello (W5 negotiation is DecodeFromPlane's version check).
	r := bufio.NewReader(conn)
	raw, err := secretwire.ReadFrame(r)
	if err != nil {
		return ioReport(err)
	}
	fr, err := secretwire.DecodeFromPlane(raw, secretwire.TypeHello, 0)
	if err != nil {
		return ioReport(err)
	}
	if fr.Type == secretwire.TypeError {
		return resultOK // plane error codes are neither recorded nor reported (S4)
	}
	ver, planeID := fr.PlaneHello.Version, fr.PlaneHello.PlaneID

	if c.poisoned() {
		return secretwire.ReportJournalPoisoned
	}
	if c.cfg.ExpectedPlaneID != "" && planeID != c.cfg.ExpectedPlaneID { // Q3 pin
		if r := sendError(secretwire.CodePeerRejected); r != resultOK {
			return r
		}
		return secretwire.ReportPeerRejected
	}

	// step 7: poll, computed by the writer right before sending (W7, W8).
	ps, err := c.cfg.Service.PollState(planeID)
	if err != nil {
		return writerReport(err)
	}
	if ps.Retired { // R7a, W8
		if c.poisoned() {
			return secretwire.ReportJournalPoisoned
		}
		if r := sendError(secretwire.CodeRetiredPlane); r != resultOK {
			return r
		}
		return secretwire.ReportRetiredPlane
	}
	c.setStalled(ps.LastRejected != nil)
	poll, err := secretwire.EncodePoll(secretwire.Poll{V: ver, Pending: ps.Pending, Ack: ps.Ack, LastRejected: ps.LastRejected})
	if err != nil {
		return secretwire.ReportRegistryUnavailable
	}
	if c.poisoned() { // the poll was prepared after poison: drop it
		return secretwire.ReportJournalPoisoned
	}
	if r := send(poll); r != resultOK {
		return r
	}

	// step 8: the whole batch.
	raw, err = secretwire.ReadFrame(r)
	if err != nil {
		return ioReport(err)
	}
	fr, err = secretwire.DecodeFromPlane(raw, secretwire.TypeBatch, ver)
	if err != nil {
		return ioReport(err)
	}
	if fr.Type == secretwire.TypeError {
		return resultOK
	}
	if err := secretwire.CheckBatchPlane(fr.Batch, planeID); err != nil { // W9
		return ioReport(err)
	}

	// step 9: W10 under the writer.
	res, err := c.cfg.Service.ProcessBatch(v, planeID, fr.Batch.Entries)
	if (res.Appended > 0 || res.RejectionRecorded) && c.cfg.OnAppend != nil {
		c.cfg.OnAppend()
	}
	if err != nil {
		return writerReport(err) // includes ErrPoisoned: close, send nothing
	}
	if res.Rejection == nil {
		return resultOK
	}
	c.setStalled(true)
	rep := result(secretwire.EntryRejectedPrefix + res.Rejection.Reason)

	// step 10: the entry-rejected notice with the post-append ack (W11 ②).
	if c.poisoned() {
		return rep
	}
	ack := res.Ack
	notice, err := secretwire.EncodeError(secretwire.ErrorFrame{Code: secretwire.CodeEntryRejected,
		Reason: res.Rejection.Reason, Ack: &ack, RejectedSeq: res.Rejection.Seq})
	if err != nil {
		return rep
	}
	if r := send(notice); r != resultOK {
		// The E7 stays durable and the next poll's lastRejected carries it;
		// report the rejection and then the failed send.
		c.report(string(rep))
		return r
	}
	return rep
}
