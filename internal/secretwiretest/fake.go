// Package secretwiretest provides FakeSecretPlane (RHZ-107 G1, SCP §3.5,
// FR-RHZ-137): an in-process secret plane that speaks the plane side of the
// secretwire v1 channel (§3.3) over a Unix socket in a private temporary
// directory, for tests of Rhizome's channel loop.
//
// The fake never holds a secret value: its entries are metadata only. Test
// sentinels appear only in the deliberately broken fault frames it sends.
// It imports the standard library, secretwire and peercred only (§7 R8).
package secretwiretest

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"rhizome/internal/peercred"
	"rhizome/internal/secretwire"
)

// Stage is a point of the plane side of one connection (§3.5 staged faults).
type Stage int

const (
	StageNone        Stage = iota
	StageBeforeHello       // after accept, before reading Rhizome's hello
	StageAfterHello        // after sending the plane hello, before reading the poll
	StageAfterPoll         // after reading the poll, before sending the batch
	StageMidBatch          // after sending the first half of the batch bytes
)

// Action is what the fake does at a faulted stage.
type Action int

const (
	ActionHold  Action = iota + 1 // keep the connection open, send nothing
	ActionClose                   // close the connection
)

// FrameFault replaces the frame the fake would send (§3.5 frame faults).
type FrameFault int

const (
	FaultNone             FrameFault = iota
	FaultBrokenJSON                  // truncated JSON object
	FaultDupKey                      // duplicate key; the sentinel is the first value
	FaultNull                        // null value
	FaultUnknownValue                // extra key "value" carrying the sentinel
	FaultTooLarge                    // line longer than MaxFrameBytes
	FaultNonUTF8                     // raw invalid UTF-8 byte next to the sentinel
	FaultVersionMismatch             // hello: version 2; batch: v 2
	FaultPlaneIDChange               // batch: planeId differs from the hello
	FaultSeqGap                      // batch: entries 1, 3 (not contiguous)
	FaultSeqNonIncreasing            // batch: entries 2, 2
	FaultTooManyEntries              // batch: MaxEntries+1 entries
)

// Violation strings recorded by the fake's checks (fixed; no input bytes).
const (
	ViolationDecode       = "decode"         // a frame from Rhizome failed DecodeFromRhizome
	ViolationPeer         = "peer"           // W2 check of Rhizome failed
	ViolationAckPlane     = "ack-plane"      // ack.planeId is not the hello planeId
	ViolationAckRegressed = "ack-regressed"  // ack below the recorded ack (P2)
	ViolationAckAbove     = "ack-above-sent" // ack above the largest seq ever offered
	ViolationR7bNotice    = "r7b-notice"     // entry-rejected relation broken
	ViolationR7bPoll      = "r7b-poll"       // lastRejected relation broken
	ViolationNoticeAck    = "notice-ack"     // notice ack below the poll ack
)

// RecordedPoll is one poll the fake received.
type RecordedPoll struct {
	PlaneID string // the planeId of the connection's plane hello
	Poll    secretwire.Poll
}

// RecordedNotice is one entry-rejected notice the fake received.
type RecordedNotice struct {
	PlaneID string
	Notice  secretwire.ErrorFrame
}

// Epoch is the fake's record of one P5 replacement.
type Epoch struct {
	Old, New     string
	Cutoff       uint64               // C = ack.planeEntrySeq of the first post-freeze poll
	LastRejected *secretwire.Rejected // lastRejected of that poll
	Quarantined  []secretwire.Entry   // entries with seq > C, plus a seq-conflict entry R <= C
}

// FakeSecretPlane is the plane side of the channel. All methods are safe for
// concurrent use. Connections are served one at a time.
type FakeSecretPlane struct {
	dir, sock string
	ln        *net.UnixListener

	mu       sync.Mutex
	cond     *sync.Cond
	planeID  string
	helloID  string // non-empty: announce this planeId instead (stale / retired P)
	version  uint64
	outbox   []secretwire.Entry // current plane, ascending contiguous seq from 1
	maxSent  map[string]uint64  // largest seq ever offered per planeId
	acked    map[string]uint64  // recorded ack per planeId (P2)
	override []secretwire.Entry // sticky explicit batch
	hasOver  bool
	stage    Stage
	action   Action
	helloF   FrameFault
	batchF   FrameFault
	sentinel string
	drop     bool
	frozen   bool   // P5 ①–③: no new facts, batches are entries:[]
	splitTo  string // cutoff recorded on this connection's poll; P′ after its batch
	nextID   string
	epochs   []Epoch

	received   [][]byte
	frames     []secretwire.Frame
	polls      []RecordedPoll
	notices    []RecordedNotice
	violations []string
	conns      int
	handled    int
	onPoll     func(RecordedPoll)
	onNotice   func(RecordedNotice)
	active     net.Conn
	closed     bool
	done       chan struct{}
}

// NewPlaneID returns a fresh G10 planeId from crypto/rand.
func NewPlaneID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "pl-" + hex.EncodeToString(b)
}

// Start listens on a socket in a new 0700 temporary directory. The directory
// name is short so the socket path fits the platform limit.
func Start(planeID string) (*FakeSecretPlane, error) {
	if !secretwire.ValidPlaneID(planeID) {
		return nil, errors.New("secretwiretest: invalid planeId")
	}
	dir, err := os.MkdirTemp("", "sw")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	f := &FakeSecretPlane{dir: dir, sock: sock, ln: ln, planeID: planeID, version: 1,
		maxSent: map[string]uint64{}, acked: map[string]uint64{}, done: make(chan struct{})}
	f.cond = sync.NewCond(&f.mu)
	go f.serve()
	return f, nil
}

// Dir is the socket's parent directory (mode 0700).
func (f *FakeSecretPlane) Dir() string { return f.dir }

// SocketPath is the absolute socket path.
func (f *FakeSecretPlane) SocketPath() string { return f.sock }

// Close stops listening, drops any active connection and removes the
// directory.
func (f *FakeSecretPlane) Close() {
	f.mu.Lock()
	f.closed = true
	if f.active != nil {
		f.active.Close()
	}
	f.mu.Unlock()
	f.ln.Close()
	<-f.done
	os.Chmod(f.dir, 0o700)
	os.RemoveAll(f.dir)
	f.mu.Lock()
	f.cond.Broadcast()
	f.mu.Unlock()
}

// PlaneID is the fake's current planeId.
func (f *FakeSecretPlane) PlaneID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planeID
}

// AnnouncePlaneID makes the plane hello carry id instead of the current
// planeId (planeId change, stale or retired planeId). "" restores normal.
func (f *FakeSecretPlane) AnnouncePlaneID(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.helloID = id
}

// Append adds e to the current plane's outbox with the next seq and returns
// it. Entries stay until acked and are resent on every batch. A frozen plane
// creates no facts (P5 ①): Append then refuses and returns 0.
func (f *FakeSecretPlane) Append(e secretwire.Entry) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return 0
	}
	e = cloneEntry(e)
	e.Seq = uint64(len(f.outbox)) + 1
	f.outbox = append(f.outbox, e)
	return e.Seq
}

// Replace overwrites the outbox entry at e.Seq (a plane whose entry differs
// from what Rhizome recorded: seq-conflict). e.Seq must be in the outbox.
func (f *FakeSecretPlane) Replace(e secretwire.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.Seq >= 1 && e.Seq <= uint64(len(f.outbox)) {
		f.outbox[e.Seq-1] = cloneEntry(e)
	}
}

// SetBatch makes every following batch carry exactly entries (sticky until
// ClearBatch): redelivery, seq gaps, conflicts, new-seq outcomes.
func (f *FakeSecretPlane) SetBatch(entries []secretwire.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override = cloneEntries(entries)
	f.hasOver = true
}

// ClearBatch returns to outbox batches.
func (f *FakeSecretPlane) ClearBatch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override, f.hasOver = nil, false
}

// SetStageFault holds or closes every following connection at stage
// (sticky; StageNone clears).
func (f *FakeSecretPlane) SetStageFault(stage Stage, action Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stage, f.action = stage, action
}

// SetHelloFault replaces the plane hello with a fault frame (sticky).
func (f *FakeSecretPlane) SetHelloFault(ff FrameFault, sentinel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.helloF, f.sentinel = ff, sentinel
}

// SetBatchFault replaces the batch with a fault frame (sticky).
func (f *FakeSecretPlane) SetBatchFault(ff FrameFault, sentinel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchF, f.sentinel = ff, sentinel
}

// ClearFaults removes stage and frame faults and notice dropping.
func (f *FakeSecretPlane) ClearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stage, f.action, f.helloF, f.batchF, f.sentinel, f.drop = StageNone, 0, FaultNone, FaultNone, "", false
}

// DropNotices makes the fake lose entry-rejected notices: it closes right
// after sending the batch and records nothing more (sticky).
func (f *FakeSecretPlane) DropNotices(drop bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drop = drop
}

// OnNotice registers a hook called (without the fake's lock) after an
// entry-rejected notice is recorded.
func (f *FakeSecretPlane) OnNotice(fn func(RecordedNotice)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onNotice = fn
}

// OnPoll registers a hook called (without the fake's lock) after a poll is
// recorded and before the batch is sent.
func (f *FakeSecretPlane) OnPoll(fn func(RecordedPoll)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onPoll = fn
}

// Freeze starts a P5 epoch replacement (§3.7 P5 ①–④, simplified): from now
// on batches are entries:[] (overrides included) and Append refuses; the
// first poll after this call records the cutoff C and lastRejected and
// quarantines outbox entries with seq > C and a seq-conflict entry R <= C;
// after that connection's batch the fake switches to next with an empty
// outbox and no batch override. No entry is renumbered.
func (f *FakeSecretPlane) Freeze(next string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen, f.nextID = true, next
}

// Epochs returns the recorded epoch replacements.
func (f *FakeSecretPlane) Epochs() []Epoch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Epoch, len(f.epochs))
	for i, e := range f.epochs {
		e.Quarantined = cloneEntries(e.Quarantined)
		if e.LastRejected != nil {
			lr := *e.LastRejected
			e.LastRejected = &lr
		}
		out[i] = e
	}
	return out
}

// Received returns every raw frame Rhizome sent, in order (ZV evidence).
func (f *FakeSecretPlane) Received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.received))
	for i, b := range f.received {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

// Frames returns every decoded frame Rhizome sent, in order.
func (f *FakeSecretPlane) Frames() []secretwire.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]secretwire.Frame, len(f.frames))
	for i, fr := range f.frames {
		out[i] = cloneFrame(fr)
	}
	return out
}

// Polls returns the received polls.
func (f *FakeSecretPlane) Polls() []RecordedPoll {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedPoll, len(f.polls))
	for i, p := range f.polls {
		out[i] = RecordedPoll{PlaneID: p.PlaneID, Poll: clonePoll(p.Poll)}
	}
	return out
}

// Notices returns the received entry-rejected notices.
func (f *FakeSecretPlane) Notices() []RecordedNotice {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedNotice, len(f.notices))
	for i, n := range f.notices {
		out[i] = RecordedNotice{PlaneID: n.PlaneID, Notice: cloneError(n.Notice)}
	}
	return out
}

// Violations returns the fixed strings of failed fake-side checks.
func (f *FakeSecretPlane) Violations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.violations...)
}

// Connections is the number of accepted connections.
func (f *FakeSecretPlane) Connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// WaitHandled waits until n connections have been fully handled (the fake
// finished reading and recording) or the timeout passes.
func (f *FakeSecretPlane) WaitHandled(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	t := time.AfterFunc(timeout, func() { f.mu.Lock(); f.cond.Broadcast(); f.mu.Unlock() })
	defer t.Stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.handled < n && !f.closed {
		if time.Now().After(deadline) {
			return false
		}
		f.cond.Wait()
	}
	return f.handled >= n
}

func (f *FakeSecretPlane) serve() {
	defer close(f.done)
	for {
		c, err := f.ln.AcceptUnix()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			c.Close()
			return
		}
		f.conns++
		f.active = c
		f.mu.Unlock()
		f.handle(c)
		c.Close()
		f.mu.Lock()
		f.active = nil
		f.handled++
		f.cond.Broadcast()
		f.mu.Unlock()
	}
}

func (f *FakeSecretPlane) violate(s string) {
	f.mu.Lock()
	f.violations = append(f.violations, s)
	f.mu.Unlock()
}

// fault reports whether the connection must stop at stage, holding or
// closing it as configured.
func (f *FakeSecretPlane) fault(c net.Conn, stage Stage) bool {
	f.mu.Lock()
	st, act := f.stage, f.action
	f.mu.Unlock()
	if st != stage || stage == StageNone {
		return false
	}
	if act == ActionHold {
		io.Copy(io.Discard, c) // until Rhizome closes at its deadline
	}
	return true
}

// read reads and records one frame from Rhizome.
func (f *FakeSecretPlane) read(r *bufio.Reader, want string, v uint64) (secretwire.Frame, bool) {
	raw, err := secretwire.ReadFrame(r)
	if err != nil {
		return secretwire.Frame{}, false
	}
	fr, derr := secretwire.DecodeFromRhizome(raw, want, v)
	f.mu.Lock()
	f.received = append(f.received, append([]byte(nil), raw...))
	if derr == nil {
		f.frames = append(f.frames, cloneFrame(fr))
	} else {
		f.violations = append(f.violations, ViolationDecode)
	}
	f.mu.Unlock()
	return fr, derr == nil
}

func (f *FakeSecretPlane) handle(c *net.UnixConn) {
	if v, err := peercred.Verify(c); err != nil || !v.SameUser() { // P6 / W2
		f.violate(ViolationPeer)
		return
	}
	if f.fault(c, StageBeforeHello) {
		return
	}
	r := bufio.NewReader(c)
	hello, ok := f.read(r, secretwire.TypeHello, 0)
	if !ok || hello.Type != secretwire.TypeHello {
		return // journal-poisoned or a bad frame: close, clean nothing (P2)
	}
	ver, ok := secretwire.Negotiate(hello.RhizomeHello.Versions)
	f.mu.Lock()
	planeID := f.planeID
	if f.helloID != "" {
		planeID = f.helloID
	}
	helloF, sentinel := f.helloF, f.sentinel
	f.mu.Unlock()
	if !ok {
		b, _ := secretwire.EncodeError(secretwire.ErrorFrame{Code: secretwire.CodeUnsupportedVersion})
		c.Write(b)
		return
	}
	var hb []byte
	if helloF != FaultNone {
		hb = FaultFrame(helloF, secretwire.TypeHello, planeID, sentinel)
	} else {
		hb, _ = secretwire.EncodePlaneHello(secretwire.PlaneHello{Version: ver, PlaneID: planeID})
	}
	if _, err := c.Write(hb); err != nil || helloF != FaultNone {
		f.drain(r, ver)
		return
	}
	if f.fault(c, StageAfterHello) {
		return
	}
	poll, ok := f.read(r, secretwire.TypePoll, ver)
	if !ok || poll.Type != secretwire.TypePoll {
		f.drain(r, ver)
		return // retired-plane, peer-rejected or a bad frame
	}
	rp := RecordedPoll{PlaneID: planeID, Poll: clonePoll(poll.Poll)}
	f.recordPoll(rp)
	f.mu.Lock()
	hook := f.onPoll
	f.mu.Unlock()
	if hook != nil {
		hook(RecordedPoll{PlaneID: rp.PlaneID, Poll: clonePoll(rp.Poll)})
	}
	if f.fault(c, StageAfterPoll) {
		return
	}
	bb := f.batchBytes(planeID, poll.Poll.Ack.PlaneEntrySeq, ver)
	f.mu.Lock()
	if f.splitTo != "" { // P5 ④: P′ starts with an empty outbox, nothing renumbered
		f.planeID, f.outbox, f.override, f.hasOver = f.splitTo, nil, nil, false
		f.frozen, f.splitTo = false, ""
	}
	mid, drop := f.stage == StageMidBatch, f.drop
	f.mu.Unlock()
	if mid {
		c.Write(bb[:len(bb)/2])
		f.fault(c, StageMidBatch)
		return
	}
	if _, err := c.Write(bb); err != nil || drop {
		return
	}
	f.drain(r, ver)
}

// drain reads Rhizome's remaining frames (entry-rejected notice, error
// frames) until it closes.
func (f *FakeSecretPlane) drain(r *bufio.Reader, ver uint64) {
	for {
		fr, ok := f.read(r, secretwire.TypeError, ver)
		if !ok {
			return
		}
		if fr.Type == secretwire.TypeError && fr.Error.Code == secretwire.CodeEntryRejected {
			f.recordNotice(fr.Error)
			f.mu.Lock()
			hook := f.onNotice
			planeID := ""
			if fr.Error.Ack != nil {
				planeID = fr.Error.Ack.PlaneID
			}
			f.mu.Unlock()
			if hook != nil {
				hook(RecordedNotice{PlaneID: planeID, Notice: cloneError(fr.Error)})
			}
		}
	}
}

func relation(seq uint64, reason string, ack uint64) bool {
	switch reason {
	case secretwire.EntrySeqConflict:
		return seq <= ack
	case secretwire.EntrySeqGap:
		return seq > ack+1
	}
	return seq == ack+1
}

// recordPoll records rp, checks W8/R7b relations, and runs the P5 cutoff
// step on the first poll after Freeze; the switch to P′ happens after this
// connection's (empty) batch.
func (f *FakeSecretPlane) recordPoll(rp RecordedPoll) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls = append(f.polls, rp)
	ack := rp.Poll.Ack
	f.checkAck(rp.PlaneID, ack)
	if lr := rp.Poll.LastRejected; lr != nil && !relation(lr.Seq, lr.Reason, ack.PlaneEntrySeq) {
		f.violations = append(f.violations, ViolationR7bPoll)
	}
	if !f.frozen || f.splitTo != "" || rp.PlaneID != f.planeID || f.helloID != "" {
		return
	}
	ep := Epoch{Old: f.planeID, New: f.nextID, Cutoff: ack.PlaneEntrySeq}
	if lr := rp.Poll.LastRejected; lr != nil {
		c := *lr
		ep.LastRejected = &c
	}
	for _, e := range f.outbox {
		conflict := ep.LastRejected != nil && ep.LastRejected.Reason == secretwire.EntrySeqConflict &&
			ep.LastRejected.Seq == e.Seq && e.Seq <= ep.Cutoff
		if e.Seq > ep.Cutoff || conflict {
			ep.Quarantined = append(ep.Quarantined, cloneEntry(e))
		}
	}
	f.epochs = append(f.epochs, ep)
	f.splitTo = ep.New
}

func (f *FakeSecretPlane) checkAck(planeID string, ack secretwire.Ack) {
	if ack.PlaneID != planeID {
		f.violations = append(f.violations, ViolationAckPlane)
		return
	}
	if ack.PlaneEntrySeq < f.acked[planeID] {
		f.violations = append(f.violations, ViolationAckRegressed)
	}
	if ack.PlaneEntrySeq > f.maxSent[planeID] {
		f.violations = append(f.violations, ViolationAckAbove)
	}
	if ack.PlaneEntrySeq > f.acked[planeID] {
		f.acked[planeID] = ack.PlaneEntrySeq
	}
}

func (f *FakeSecretPlane) recordNotice(e secretwire.ErrorFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	planeID := ""
	if e.Ack != nil {
		planeID = e.Ack.PlaneID
		var pollAck uint64
		if n := len(f.polls); n > 0 && f.polls[n-1].PlaneID == planeID {
			pollAck = f.polls[n-1].Poll.Ack.PlaneEntrySeq
		}
		if e.Ack.PlaneEntrySeq < pollAck {
			f.violations = append(f.violations, ViolationNoticeAck)
		}
		f.checkAck(planeID, *e.Ack)
		if !relation(e.RejectedSeq, e.Reason, e.Ack.PlaneEntrySeq) {
			f.violations = append(f.violations, ViolationR7bNotice)
		}
	}
	f.notices = append(f.notices, RecordedNotice{PlaneID: planeID, Notice: cloneError(e)})
}

// batchBytes builds the batch for a poll with the given ack.
func (f *FakeSecretPlane) batchBytes(planeID string, ack, ver uint64) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.batchF != FaultNone && !f.frozen {
		return FaultFrame(f.batchF, secretwire.TypeBatch, planeID, f.sentinel)
	}
	var entries []secretwire.Entry
	switch {
	case f.frozen:
		// P5 ①–③ (including the cutoff poll's own batch): entries:[].
	case f.hasOver:
		entries = cloneEntries(f.override)
	case planeID != f.planeID:
		// a stale announced planeId: send nothing.
	default:
		for _, e := range f.outbox {
			if e.Seq > ack && len(entries) < secretwire.MaxEntries {
				entries = append(entries, e)
			}
		}
	}
	for _, e := range entries {
		if e.Seq > f.maxSent[planeID] {
			f.maxSent[planeID] = e.Seq
		}
	}
	b, err := secretwire.EncodeBatch(secretwire.Batch{V: ver, PlaneID: planeID, Entries: entries})
	if err != nil {
		// An override the encoder refuses (non-contiguous): send it raw.
		b = rawBatch(ver, planeID, entries)
	}
	return b
}

func cloneEntry(e secretwire.Entry) secretwire.Entry {
	if len(e.ScopeTags) == 0 {
		e.ScopeTags = nil
	} else {
		e.ScopeTags = append([]string(nil), e.ScopeTags...)
	}
	return e
}

func cloneEntries(es []secretwire.Entry) []secretwire.Entry {
	if es == nil {
		return nil
	}
	out := make([]secretwire.Entry, len(es))
	for i, e := range es {
		out[i] = cloneEntry(e)
	}
	return out
}

func clonePoll(p secretwire.Poll) secretwire.Poll {
	if p.Pending != nil {
		pend := make([]secretwire.Pending, len(p.Pending))
		for i, x := range p.Pending {
			if x.ScopeTags != nil {
				x.ScopeTags = append([]string(nil), x.ScopeTags...)
			}
			pend[i] = x
		}
		p.Pending = pend
	}
	if p.LastRejected != nil {
		lr := *p.LastRejected
		p.LastRejected = &lr
	}
	return p
}

func cloneError(e secretwire.ErrorFrame) secretwire.ErrorFrame {
	if e.Ack != nil {
		a := *e.Ack
		e.Ack = &a
	}
	return e
}

func cloneFrame(f secretwire.Frame) secretwire.Frame {
	if f.RhizomeHello.Versions != nil {
		f.RhizomeHello.Versions = append([]uint64(nil), f.RhizomeHello.Versions...)
	}
	f.Poll = clonePoll(f.Poll)
	f.Batch.Entries = cloneEntries(f.Batch.Entries)
	f.Error = cloneError(f.Error)
	return f
}
