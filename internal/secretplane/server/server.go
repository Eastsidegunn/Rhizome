// Package server is the plane side of the secretwire v1 channel (RHZ-107 G1,
// mission M5b, FR-RHZ-139, SCP §3.1–3.3, §3.7 P6): a Unix socket listener in
// a private directory that answers Rhizome's connection cycle with the plane
// state of a secretplane.Plane.
//
// The server never completes an operation. It records what Rhizome reports
// (acks, rejections, epoch progress through Plane.OnPoll) and hands the
// pending requests to the interactive front end, which alone collects the
// human's decision (split plan §4, D19). It carries no value: frames have no
// value field (W13) and notices are fixed strings (S5).
package server

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"rhizome/internal/peercred"
	"rhizome/internal/secretplane"
	"rhizome/internal/secretwire"
)

// Phase names of secretplane.Status.Phase during an epoch freeze (P5 ①②).
const (
	phaseFrozen = "frozen"
	phaseCutoff = "cutoff-recorded"
)

// IOTimeout bounds all I/O of one connection, from accept (mirrors §3.1
// step 3 on the Rhizome side).
const IOTimeout = 10 * time.Second

// MaxSocketPath is the longest accepted socket path in bytes (the macOS
// sun_path limit is 104 bytes including the terminating NUL).
const MaxSocketPath = 103

// Errors of Listen (fixed text, no path).
var (
	ErrSocketPath      = errors.New("socket-path-invalid")
	ErrSocketDirUnsafe = errors.New("socket-dir-unsafe")
	ErrSocketInUse     = errors.New("socket-in-use")
	ErrListen          = errors.New("listen-failed")
)

// Notices passed to Config.OnNotice. Fixed strings: no input byte, no
// number.
const (
	// NoticeHalted: an ack or rejection from Rhizome contradicted the plane
	// records (W11 plane-side checks); batches carry no entry until restart.
	NoticeHalted = "halted"
	// NoticePeerRejected: a connection failed the same-user peer check (W2).
	NoticePeerRejected = "peer-rejected"
	// NoticeRetiredPlane: Rhizome reports the announced planeId as retired.
	NoticeRetiredPlane = "retired-plane"
	// NoticePinRejected: Rhizome is pinned to another planeId (Q3).
	NoticePinRejected = "rhizome-peer-rejected"
	// NoticeJournalPoisoned: Rhizome's journal is poisoned (§1.8); nothing
	// was acknowledged.
	NoticeJournalPoisoned = "journal-poisoned"
	// NoticeUnsupportedVersion: no common protocol version.
	NoticeUnsupportedVersion = "unsupported-version"
	// NoticeMalformed: a frame from Rhizome failed the strict decoder.
	NoticeMalformed = "malformed"
	// NoticePhasePrefix + phase name: the epoch phase changed (P5).
	NoticePhasePrefix = "epoch-phase:"
)

// Config wires a Server.
type Config struct {
	// Plane is the open plane. Required.
	Plane *secretplane.Plane
	// SocketPath is the absolute socket path; its parent directory is
	// created (0700) if absent and must pass the W1 checks.
	SocketPath string
	// Verify is the W2 peer check seam; nil means peercred.Verify.
	Verify func(*net.UnixConn) (peercred.Verified, error)
	// IOTimeout overrides IOTimeout when positive (tests).
	IOTimeout time.Duration
	// OnNotice receives operator notices (consecutive duplicates dropped).
	OnNotice func(string)
	// OnFatal runs when a plane call returns secretplane.ErrFatal; the
	// process must exit (P3).
	OnFatal func()
	// OnQueue runs after a poll changed the request queue.
	OnQueue func()
}

// Item is one pending request from the latest poll, as the plane classifies
// it. Only PendingNew and PendingInProgress items are actionable.
type Item struct {
	Pending secretwire.Pending
	Status  secretplane.PendingStatus
}

// Actionable reports whether the front end may act on the item.
func (it Item) Actionable() bool {
	return it.Status == secretplane.PendingNew || it.Status == secretplane.PendingInProgress
}

// Server serves one connection at a time; a concurrent connection gets
// error{busy}.
type Server struct {
	cfg     Config
	timeout time.Duration
	ln      *net.UnixListener
	lock    *os.File // exclusive lock on the socket directory's lock file

	connMu sync.Mutex // held while a connection is served
	wg     sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	active     map[*net.UnixConn]bool
	queue      []Item
	lastNotice string
	lastPhase  string
	served     int
}

// LockName is the lock file in the socket directory. A listener holds an
// exclusive lock on it for its lifetime, so probing, removing and binding the
// socket path is serialized between plane processes.
const LockName = ".plane.lock"

// Listen prepares the socket directory (W1-equivalent check: a real
// directory owned by this user with no group/other permission bits), takes
// the socket directory lock (held until Close; a second listener gets
// ErrSocketInUse), replaces a stale socket file, listens and sets the socket
// mode to 0600.
func Listen(cfg Config) (*Server, error) {
	if cfg.Plane == nil {
		return nil, ErrListen
	}
	p := cfg.SocketPath
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || len(p) > MaxSocketPath {
		return nil, ErrSocketPath
	}
	if err := prepareDir(filepath.Dir(p)); err != nil {
		return nil, err
	}
	lock, err := lockSocketDir(filepath.Join(filepath.Dir(p), LockName))
	if err != nil {
		return nil, err
	}
	srv, err := listenLocked(cfg, p)
	if err != nil {
		lock.Close()
		return nil, err
	}
	srv.lock = lock
	return srv, nil
}

func listenLocked(cfg Config, p string) (*Server, error) {
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, ErrSocketDirUnsafe
		}
		if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
			c.Close()
			return nil, ErrSocketInUse
		}
		if err := os.Remove(p); err != nil {
			return nil, ErrSocketDirUnsafe
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrSocketDirUnsafe
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		return nil, ErrListen
	}
	ln.SetUnlinkOnClose(true)
	if err := os.Chmod(p, 0o600); err != nil {
		ln.Close()
		return nil, ErrListen
	}
	t := cfg.IOTimeout
	if t <= 0 {
		t = IOTimeout
	}
	if cfg.Verify == nil {
		cfg.Verify = peercred.Verify
	}
	return &Server{cfg: cfg, timeout: t, ln: ln, active: map[*net.UnixConn]bool{}}, nil
}

// prepareDir creates dir (one level, 0700) when absent and applies W1.
func prepareDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		switch err := os.Mkdir(dir, 0o700); {
		case err == nil:
			if err := os.Chmod(dir, 0o700); err != nil {
				return ErrSocketDirUnsafe
			}
		case !errors.Is(err, os.ErrExist): // a concurrent creator is checked below
			return ErrSocketDirUnsafe
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil || !dirSafe(fi) {
		return ErrSocketDirUnsafe
	}
	return nil
}

func dirSafe(fi os.FileInfo) bool {
	m := fi.Mode()
	return m&os.ModeSymlink == 0 && m.IsDir() && m.Perm()&0o077 == 0 && ownedBySelf(fi)
}

// Serve accepts connections until Close. It always returns nil after Close.
func (s *Server) Serve() error {
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return ErrListen
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return nil
		}
		s.active[c] = true
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handle(c)
			c.Close()
			s.mu.Lock()
			delete(s.active, c)
			s.mu.Unlock()
		}()
	}
}

// Close stops listening (removing the socket file), closes active
// connections and waits for their handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for c := range s.active {
		c.Close()
	}
	s.mu.Unlock()
	err := s.ln.Close() // unlinks the socket
	s.wg.Wait()
	s.lock.Close() // releases the socket directory lock
	if err != nil {
		return ErrListen
	}
	return nil
}

// Queue returns the requests of the latest poll that are not done and not
// invalid, in Rhizome's order.
func (s *Server) Queue() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Item, len(s.queue))
	for i, it := range s.queue {
		out[i] = Item{Pending: clonePending(it.Pending), Status: it.Status}
	}
	return out
}

// Served is the number of connections that completed the poll step.
func (s *Server) Served() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

func clonePending(p secretwire.Pending) secretwire.Pending {
	if p.ScopeTags != nil {
		p.ScopeTags = append([]string(nil), p.ScopeTags...)
	}
	return p
}

func (s *Server) notice(n string) {
	s.mu.Lock()
	dup := n == s.lastNotice
	s.lastNotice = n
	s.mu.Unlock()
	if !dup && s.cfg.OnNotice != nil {
		s.cfg.OnNotice(n)
	}
}

// fatal reports whether err is secretplane.ErrFatal, running OnFatal.
func (s *Server) fatal(err error) bool {
	if !errors.Is(err, secretplane.ErrFatal) {
		return false
	}
	if s.cfg.OnFatal != nil {
		s.cfg.OnFatal()
	}
	return true
}

func (s *Server) notePhase() {
	st, err := s.cfg.Plane.Status()
	if err != nil {
		s.fatal(err)
		return
	}
	s.mu.Lock()
	changed := s.lastPhase != "" && s.lastPhase != st.Phase
	s.lastPhase = st.Phase
	s.mu.Unlock()
	if changed {
		s.notice(NoticePhasePrefix + st.Phase)
	}
}

func writeError(c *net.UnixConn, code string) {
	if b, err := secretwire.EncodeError(secretwire.ErrorFrame{Code: code}); err == nil {
		c.Write(b)
	}
}

// handle runs the plane side of one cycle (§3.1 from the plane's view).
func (s *Server) handle(c *net.UnixConn) {
	if err := c.SetDeadline(time.Now().Add(s.timeout)); err != nil {
		return
	}
	if v, err := s.cfg.Verify(c); err != nil || !v.SameUser() { // P6 / W2, every connection
		writeError(c, secretwire.CodePeerRejected)
		s.notice(NoticePeerRejected)
		return
	}
	if !s.connMu.TryLock() {
		writeError(c, secretwire.CodeBusy)
		return
	}
	defer s.connMu.Unlock()
	s.notePhase()
	r := bufio.NewReader(c)

	// Rhizome hello (or an ack-less journal-poisoned error, §1.8).
	raw, err := secretwire.ReadFrame(r)
	if err != nil {
		return
	}
	fr, err := secretwire.DecodeFromRhizome(raw, secretwire.TypeHello, 0)
	if err != nil {
		writeError(c, secretwire.CodeMalformed)
		s.notice(NoticeMalformed)
		return
	}
	if fr.Type == secretwire.TypeError {
		if fr.Error.Code == secretwire.CodeJournalPoisoned {
			s.notice(NoticeJournalPoisoned)
		}
		return // not an ack: nothing is cleaned (P2)
	}
	ver, ok := secretwire.Negotiate(fr.RhizomeHello.Versions)
	if !ok {
		writeError(c, secretwire.CodeUnsupportedVersion)
		s.notice(NoticeUnsupportedVersion)
		return
	}
	pid, err := s.cfg.Plane.HelloPlaneID()
	if err != nil {
		s.fatal(err)
		return
	}
	hb, err := secretwire.EncodePlaneHello(secretwire.PlaneHello{Version: ver, PlaneID: pid})
	if err != nil {
		return
	}
	if _, err := c.Write(hb); err != nil {
		return
	}

	// poll (or retired-plane / peer-rejected / journal-poisoned).
	raw, err = secretwire.ReadFrame(r)
	if err != nil {
		return
	}
	fr, err = secretwire.DecodeFromRhizome(raw, secretwire.TypePoll, ver)
	if err != nil {
		writeError(c, secretwire.CodeMalformed)
		s.notice(NoticeMalformed)
		return
	}
	if fr.Type == secretwire.TypeError {
		s.onError(pid, fr.Error)
		return
	}
	poll := fr.Poll
	// The phase at poll entry decides whether this poll may feed the queue:
	// a poll answered while frozen (P5 ①–④, including the cutoff poll that
	// splits to P′) belongs to the old epoch and never makes a request
	// actionable; only polls of the current planeId outside the freeze do.
	entry, err := s.cfg.Plane.Status()
	if err != nil {
		s.fatal(err)
		return
	}
	perr := s.cfg.Plane.OnPoll(pid, poll.Ack, poll.LastRejected)
	switch {
	case perr == nil:
	case s.fatal(perr):
		return
	case errors.Is(perr, secretplane.ErrHalted):
		s.notice(NoticeHalted)
	case errors.Is(perr, secretplane.ErrEpochPhase), errors.Is(perr, secretplane.ErrStalePlane):
	default:
		return
	}
	s.mu.Lock()
	s.served++
	s.mu.Unlock()
	cur, err := s.cfg.Plane.HelloPlaneID()
	if err != nil {
		s.fatal(err)
		return
	}
	feeds := cur == pid && entry.Phase != phaseFrozen && entry.Phase != phaseCutoff
	if err := s.updateQueue(poll.Pending, feeds); err != nil {
		return
	}
	s.notePhase()

	entries, err := s.cfg.Plane.NextBatch(pid, secretwire.MaxEntries)
	switch {
	case err == nil:
	case s.fatal(err):
		return
	case errors.Is(err, secretplane.ErrHalted), errors.Is(err, secretplane.ErrStalePlane):
		// P5 ① after a split on this poll, or a halted plane: no entry.
		entries = []secretwire.Entry{}
	default:
		return
	}
	bb, err := secretwire.EncodeBatch(secretwire.Batch{V: ver, PlaneID: pid, Entries: entries})
	if err != nil {
		return
	}
	if _, err := c.Write(bb); err != nil {
		return
	}

	// At most one optional frame (the entry-rejected notice, W11 ②), then
	// close.
	raw, err = secretwire.ReadFrame(r)
	if err != nil {
		return
	}
	fr, err = secretwire.DecodeFromRhizome(raw, secretwire.TypeError, ver)
	if err != nil || fr.Type != secretwire.TypeError {
		return
	}
	s.onError(pid, fr.Error)
}

// onError passes an error frame to the plane.
func (s *Server) onError(pid string, f secretwire.ErrorFrame) {
	switch f.Code {
	case secretwire.CodeRetiredPlane:
		s.notice(NoticeRetiredPlane)
	case secretwire.CodePeerRejected:
		s.notice(NoticePinRejected)
	case secretwire.CodeJournalPoisoned:
		s.notice(NoticeJournalPoisoned)
	}
	err := s.cfg.Plane.OnErrorFrame(pid, f)
	switch {
	case err == nil:
		s.notePhase()
	case s.fatal(err):
	case errors.Is(err, secretplane.ErrHalted):
		s.notice(NoticeHalted)
	}
}

// updateQueue replaces the queue with the poll's pending requests that are
// neither done nor invalid (P1 dedupe through Classify). When the poll may
// not feed the queue (feeds false: answered while frozen or under a planeId
// that is no longer current) every request is listed as PendingFrozen, which
// is never actionable.
func (s *Server) updateQueue(pending []secretwire.Pending, feeds bool) error {
	q := make([]Item, 0, len(pending))
	for _, p := range pending {
		if !feeds {
			q = append(q, Item{Pending: clonePending(p), Status: secretplane.PendingFrozen})
			continue
		}
		st, err := s.cfg.Plane.Classify(p)
		if err != nil {
			s.fatal(err)
			return err
		}
		if st == secretplane.PendingDone || st == secretplane.PendingInvalid {
			continue
		}
		q = append(q, Item{Pending: clonePending(p), Status: st})
	}
	s.mu.Lock()
	changed := !sameQueue(s.queue, q)
	s.queue = q
	s.mu.Unlock()
	if changed && s.cfg.OnQueue != nil {
		s.cfg.OnQueue()
	}
	return nil
}

func sameQueue(a, b []Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Pending.OpID != b[i].Pending.OpID || a[i].Status != b[i].Status {
			return false
		}
	}
	return true
}
