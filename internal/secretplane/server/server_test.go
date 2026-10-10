package server

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/peercred"
	"rhizome/internal/secretplane"
	"rhizome/internal/secretwire"
)

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

func newOpID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "op-" + hex.EncodeToString(b)
}

// shortDir is a private directory with a short path (socket path limit).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type rig struct {
	t       *testing.T
	dir     string
	sock    string
	pl      *secretplane.Plane
	srv     *Server
	done    chan struct{}
	mu      sync.Mutex
	notices []string
}

func newRig(t *testing.T, verify func(*net.UnixConn) (peercred.Verified, error)) *rig {
	t.Helper()
	base := shortDir(t)
	r := &rig{t: t, dir: filepath.Join(base, "st"), sock: filepath.Join(base, "run", "p.sock")}
	pl, err := secretplane.Open(r.dir, secretplane.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.pl = pl
	srv, err := Listen(Config{Plane: pl, SocketPath: r.sock, Verify: verify, IOTimeout: 3 * time.Second,
		OnNotice: func(n string) { r.mu.Lock(); r.notices = append(r.notices, n); r.mu.Unlock() },
		OnFatal:  func() { t.Error("fatal") }})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = srv
	r.done = make(chan struct{})
	go func() { srv.Serve(); close(r.done) }()
	t.Cleanup(func() {
		srv.Close()
		<-r.done
		pl.Close()
	})
	return r
}

func (r *rig) noticeSeen(n string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.notices {
		if x == n {
			return true
		}
	}
	return false
}

func (r *rig) dial() (*net.UnixConn, *bufio.Reader) {
	r.t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.sock, Net: "unix"})
	if err != nil {
		r.t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
}

func readPlane(t *testing.T, br *bufio.Reader, want string, ver uint64) secretwire.Frame {
	t.Helper()
	raw, err := secretwire.ReadFrame(br)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fr, err := secretwire.DecodeFromPlane(raw, want, ver)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return fr
}

func mustWrite(t *testing.T, c net.Conn, b []byte, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

// cycle is a minimal Rhizome client: hello, poll, batch, optional notice.
func (r *rig) cycle(pending []secretwire.Pending, ack func(pid string) secretwire.Ack, lr *secretwire.Rejected,
	notice *secretwire.ErrorFrame) (string, secretwire.Batch) {
	r.t.Helper()
	c, br := r.dial()
	defer c.Close()
	b, err := secretwire.EncodeRhizomeHello()
	mustWrite(r.t, c, b, err)
	h := readPlane(r.t, br, secretwire.TypeHello, 0)
	if h.Type != secretwire.TypeHello {
		r.t.Fatalf("plane sent %s", h.Error.Code)
	}
	pid, ver := h.PlaneHello.PlaneID, h.PlaneHello.Version
	b, err = secretwire.EncodePoll(secretwire.Poll{V: ver, Pending: pending, Ack: ack(pid), LastRejected: lr})
	mustWrite(r.t, c, b, err)
	bf := readPlane(r.t, br, secretwire.TypeBatch, ver)
	if bf.Type != secretwire.TypeBatch || secretwire.CheckBatchPlane(bf.Batch, pid) != nil {
		r.t.Fatal("bad batch")
	}
	if notice != nil {
		b, err = secretwire.EncodeError(*notice)
		mustWrite(r.t, c, b, err)
	}
	c.CloseWrite()
	// wait for the server to finish the connection
	for {
		if _, err := br.ReadByte(); err != nil {
			break
		}
	}
	return pid, bf.Batch
}

func zeroAck(pid string) secretwire.Ack { return secretwire.Ack{PlaneID: pid} }

func ackAt(n uint64) func(string) secretwire.Ack {
	return func(pid string) secretwire.Ack {
		return secretwire.Ack{PlaneID: pid, PlaneEntrySeq: n, JournalSeq: n + 10}
	}
}

func registerPending(t *testing.T) secretwire.Pending {
	op := newOpID(t)
	return secretwire.Pending{OpID: op, Op: secretwire.OpRegister, Ref: secretwire.DeriveRef(op), Name: "svc",
		RefKind: secretwire.RefKindProviderKey, ScopeTags: []string{"a", "b"}, JournalSeq: 3}
}

func TestServerSocketModesFRRHZ139(t *testing.T) {
	r := newRig(t, nil)
	fi, err := os.Lstat(r.sock)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", fi.Mode(), err)
	}
	di, err := os.Lstat(filepath.Dir(r.sock))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatal("socket dir mode")
	}
	// a second listener on the same live socket is refused
	if _, err := Listen(Config{Plane: r.pl, SocketPath: r.sock}); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("got %v", err)
	}
}

func TestServerSocketDirUnsafeFRRHZ139(t *testing.T) {
	base := shortDir(t)
	pl, err := secretplane.Open(filepath.Join(base, "st"), secretplane.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(open, 0o755)
	link := filepath.Join(base, "link")
	priv := filepath.Join(base, "priv")
	os.Mkdir(priv, 0o700)
	os.Symlink(priv, link)
	file := filepath.Join(priv, "f.sock")
	os.WriteFile(file, nil, 0o600)
	for _, p := range []string{filepath.Join(open, "p.sock"), filepath.Join(link, "p.sock"), file} {
		if _, err := Listen(Config{Plane: pl, SocketPath: p}); !errors.Is(err, ErrSocketDirUnsafe) {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := Listen(Config{Plane: pl, SocketPath: "rel/p.sock"}); !errors.Is(err, ErrSocketPath) {
		t.Fatal("relative path accepted")
	}
	if _, err := Listen(Config{Plane: pl, SocketPath: "/" + strings.Repeat("x", MaxSocketPath)}); !errors.Is(err, ErrSocketPath) {
		t.Fatal("long path accepted")
	}
	// a stale socket file (no listener) is replaced
	stale := filepath.Join(priv, "s.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	srv, err := Listen(Config{Plane: pl, SocketPath: stale})
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	srv.Close()
}

func TestServerConformanceFRRHZ139(t *testing.T) {
	s := sentinel(t)
	r := newRig(t, nil)
	p := registerPending(t)

	pid, b := r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil)
	if want, _ := r.pl.HelloPlaneID(); pid != want || len(b.Entries) != 0 {
		t.Fatalf("hello planeId or batch: %d entries", len(b.Entries))
	}
	q := r.srv.Queue()
	if len(q) != 1 || q[0].Pending.OpID != p.OpID || !q[0].Actionable() {
		t.Fatalf("queue %+v", q)
	}
	// the interactive front end completes it
	if _, err := r.pl.Apply(p, secretplane.Decision{Value: []byte(s)}); err != nil {
		t.Fatal(err)
	}
	// unacked entries are resent; the done request leaves the queue (P1)
	for i := 0; i < 2; i++ {
		_, b = r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil)
		if len(b.Entries) != 1 || b.Entries[0].Seq != 1 || b.Entries[0].Fact != secretwire.FactOutcome ||
			b.Entries[0].Result != secretwire.ResultOK || b.Entries[0].Version != 1 {
			t.Fatalf("batch %+v", b.Entries)
		}
	}
	if len(r.srv.Queue()) != 0 {
		t.Fatal("done request still queued")
	}
	_, b = r.cycle(nil, ackAt(1), nil, nil)
	if len(b.Entries) != 0 {
		t.Fatal("acked entry resent")
	}
	st, _ := r.pl.Status()
	if st.RecordedAck != 1 || st.Halted {
		t.Fatalf("status %+v", st)
	}

	// an ack above the outbox is a W11 violation: halt + notice, no entries
	_, b = r.cycle(nil, ackAt(9), nil, nil)
	if len(b.Entries) != 0 || !r.noticeSeen(NoticeHalted) {
		t.Fatal("halt not reported")
	}
	if st, _ := r.pl.Status(); !st.Halted {
		t.Fatal("not halted")
	}

	led, _ := os.ReadFile(filepath.Join(r.dir, "ledger.ndjson"))
	assertNoSentinel(t, "ledger", led, s)
}

func TestServerEntryRejectedNoticeFRRHZ139(t *testing.T) {
	r := newRig(t, nil)
	p := registerPending(t)
	r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil)
	if _, err := r.pl.Apply(p, secretplane.Decision{Reject: true}); err != nil {
		t.Fatal(err)
	}
	pid, _ := r.pl.HelloPlaneID()
	notice := &secretwire.ErrorFrame{Code: secretwire.CodeEntryRejected, Reason: secretwire.EntryUnknownOp,
		Ack: &secretwire.Ack{PlaneID: pid}, RejectedSeq: 1}
	_, b := r.cycle(nil, zeroAck, nil, notice)
	if len(b.Entries) != 1 {
		t.Fatal("entry not sent")
	}
	st, _ := r.pl.Status()
	if !st.Stalled || st.LastRejected == nil || st.LastRejected.Seq != 1 || st.LastRejected.Reason != secretwire.EntryUnknownOp {
		t.Fatalf("status %+v", st)
	}
	// stalled: the same entry keeps being offered with the same lastRejected
	_, b = r.cycle(nil, zeroAck, &secretwire.Rejected{Seq: 1, Reason: secretwire.EntryUnknownOp}, nil)
	if len(b.Entries) != 1 {
		t.Fatal("stalled plane stopped resending")
	}
}

func TestServerHandshakeErrorsFRRHZ139(t *testing.T) {
	s := sentinel(t)
	var vmu sync.Mutex
	verifies := 0
	r := newRig(t, func(c *net.UnixConn) (peercred.Verified, error) {
		vmu.Lock()
		verifies++
		vmu.Unlock()
		return peercred.Verify(c)
	})
	countVerifies := func() int { vmu.Lock(); defer vmu.Unlock(); return verifies }
	before, _ := os.ReadFile(filepath.Join(r.dir, "ledger.ndjson"))
	expectError := func(send []byte, code string) {
		t.Helper()
		c, br := r.dial()
		defer c.Close()
		c.Write(send)
		fr := readPlane(t, br, secretwire.TypeHello, 0)
		if fr.Type != secretwire.TypeError || fr.Error.Code != code {
			t.Fatalf("want %s got %+v", code, fr)
		}
	}
	expectError([]byte(`{"v":1,"type":"hello","role":"rhizome","versions":[2]}`+"\n"), secretwire.CodeUnsupportedVersion)
	expectError([]byte(`{"v":1,"type":"hello","role":"rhizome","versions":[1],"value":"`+s+`"}`+"\n"), secretwire.CodeMalformed)
	if !r.noticeSeen(NoticeUnsupportedVersion) || !r.noticeSeen(NoticeMalformed) {
		t.Fatal("notices")
	}

	// journal-poisoned instead of hello: not an ack, the plane closes
	c, br := r.dial()
	b, err := secretwire.EncodeError(secretwire.ErrorFrame{Code: secretwire.CodeJournalPoisoned})
	mustWrite(t, c, b, err)
	if _, err := secretwire.ReadFrame(br); err == nil {
		t.Fatal("plane answered a journal-poisoned frame")
	}
	c.Close()

	// busy: a second connection while one is served; the peer check runs
	// for both connections before busy is answered
	time.Sleep(50 * time.Millisecond)
	v0 := countVerifies()
	hold, _ := r.dial()
	time.Sleep(100 * time.Millisecond)
	c2, br2 := r.dial()
	fr := readPlane(t, br2, secretwire.TypeHello, 0)
	if fr.Type != secretwire.TypeError || fr.Error.Code != secretwire.CodeBusy {
		t.Fatalf("want busy, got %+v", fr)
	}
	c2.Close()
	hold.Close()
	if n := countVerifies() - v0; n != 2 {
		t.Fatalf("verifier calls %d, want 2", n)
	}

	after, _ := os.ReadFile(filepath.Join(r.dir, "ledger.ndjson"))
	if string(before) != string(after) {
		t.Fatal("handshake errors wrote to the ledger")
	}
	r.mu.Lock()
	all := strings.Join(r.notices, "\n")
	r.mu.Unlock()
	assertNoSentinel(t, "notices", []byte(all), s)
}

func TestServerPeerRejectedFRRHZ139(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	r := newRig(t, func(*net.UnixConn) (peercred.Verified, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return peercred.Verified{}, nil // zero Verified: SameUser is false
	})
	before, _ := os.ReadFile(filepath.Join(r.dir, "ledger.ndjson"))
	for i := 0; i < 2; i++ {
		c, br := r.dial()
		b, _ := secretwire.EncodeRhizomeHello()
		c.Write(b) // the plane may already have answered and closed
		fr := readPlane(t, br, secretwire.TypeHello, 0)
		if fr.Type != secretwire.TypeError || fr.Error.Code != secretwire.CodePeerRejected {
			t.Fatalf("got %+v", fr)
		}
		c.Close()
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 || !r.noticeSeen(NoticePeerRejected) {
		t.Fatalf("verifier calls %d", n)
	}
	after, _ := os.ReadFile(filepath.Join(r.dir, "ledger.ndjson"))
	if string(before) != string(after) {
		t.Fatal("rejected peer changed the ledger")
	}
}

// TestServerConcurrentListenFRRHZ139: listeners racing for one socket path
// are serialized by the socket directory lock: exactly one wins, the others
// get socket-in-use, and the winner's socket keeps working.
func TestServerConcurrentListenFRRHZ139(t *testing.T) {
	base := shortDir(t)
	sock := filepath.Join(base, "run", "p.sock")
	const n = 8
	planes := make([]*secretplane.Plane, n)
	for i := range planes {
		pl, err := secretplane.Open(filepath.Join(base, "st"+strconv.Itoa(i)), secretplane.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer pl.Close()
		planes[i] = pl
	}
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		srvs := make([]*Server, n)
		errs := make([]error, n)
		for i := range planes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				srvs[i], errs[i] = Listen(Config{Plane: planes[i], SocketPath: sock, IOTimeout: 2 * time.Second})
			}(i)
		}
		wg.Wait()
		var win *Server
		for i := range errs {
			switch {
			case errs[i] == nil:
				if win != nil {
					t.Fatal("two listeners won")
				}
				win = srvs[i]
			case !errors.Is(errs[i], ErrSocketInUse):
				t.Fatalf("loser got %v", errs[i])
			}
		}
		if win == nil {
			t.Fatal("no listener won")
		}
		done := make(chan struct{})
		go func() { win.Serve(); close(done) }()
		r := &rig{t: t, sock: sock}
		c, br := r.dial()
		b, _ := secretwire.EncodeRhizomeHello()
		c.Write(b)
		if fr := readPlane(t, br, secretwire.TypeHello, 0); fr.Type != secretwire.TypeHello {
			t.Fatal("winner socket not served")
		}
		c.Close()
		win.Close() // the next round replaces a released path
		<-done
	}
}

// TestServerOneOptionalFrameFRRHZ139: after the batch the plane processes at
// most one frame and closes.
func TestServerOneOptionalFrameFRRHZ139(t *testing.T) {
	r := newRig(t, nil)
	p := registerPending(t)
	r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil)
	if _, err := r.pl.Apply(p, secretplane.Decision{Reject: true}); err != nil {
		t.Fatal(err)
	}
	pid, _ := r.pl.HelloPlaneID()
	c, br := r.dial()
	defer c.Close()
	b, _ := secretwire.EncodeRhizomeHello()
	c.Write(b)
	readPlane(t, br, secretwire.TypeHello, 0)
	b, _ = secretwire.EncodePoll(secretwire.Poll{V: 1, Ack: zeroAck(pid)})
	c.Write(b)
	readPlane(t, br, secretwire.TypeBatch, 1)
	first, _ := secretwire.EncodeError(secretwire.ErrorFrame{Code: secretwire.CodeEntryRejected,
		Reason: secretwire.EntryUnknownOp, Ack: &secretwire.Ack{PlaneID: pid}, RejectedSeq: 1})
	second, _ := secretwire.EncodeError(secretwire.ErrorFrame{Code: secretwire.CodeEntryRejected,
		Reason: secretwire.EntryRefMismatch, Ack: &secretwire.Ack{PlaneID: pid}, RejectedSeq: 1})
	c.Write(append(first, second...))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("plane wrote after the batch")
	}
	st, _ := r.pl.Status()
	if st.LastRejected == nil || st.LastRejected.Reason != secretwire.EntryUnknownOp {
		t.Fatalf("status %+v", st)
	}
}

// TestServerCutoffPollNotActionableFRRHZ139: the poll answered while frozen
// (the cutoff poll that splits to P′) never makes its pending requests
// actionable; the first poll of P′ does.
func TestServerCutoffPollNotActionableFRRHZ139(t *testing.T) {
	r := newRig(t, nil)
	p := registerPending(t)
	r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil)
	if q := r.srv.Queue(); len(q) != 1 || !q[0].Actionable() {
		t.Fatal("not queued")
	}
	if err := r.pl.Freeze(); err != nil {
		t.Fatal(err)
	}
	old, _ := r.pl.HelloPlaneID()
	pid, _ := r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil) // cutoff + split
	if now, _ := r.pl.HelloPlaneID(); pid != old || now == old {
		t.Fatal("no split on the cutoff poll")
	}
	for _, it := range r.srv.Queue() {
		if it.Actionable() {
			t.Fatal("cutoff poll fed the actionable queue")
		}
	}
	if _, err := r.pl.Classify(p); err != nil {
		t.Fatal(err)
	}
	r.cycle([]secretwire.Pending{p}, zeroAck, nil, nil) // first poll of P′
	if q := r.srv.Queue(); len(q) != 1 || !q[0].Actionable() {
		t.Fatalf("P′ poll did not feed the queue: %+v", q)
	}
}
