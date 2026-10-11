// Package terminal is the terminal bridge (RHZ-140, FR-RHZ-184): a registry
// session (`claude attach <sessionId>`) or a bare login shell runs on a
// pseudo-terminal and its bytes travel over HTTP unchanged. It is a dumb
// pipe — Rhizome never interprets, filters or injects terminal bytes, and
// never writes them to the journal or a log: the journal sees only
// terminal.opened and terminal.closed on the terminal/<terminalId> stream.
package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"rhizome/internal/events"
	"rhizome/internal/session"
)

const (
	AggregateType = "terminal"
	OpenedType    = "terminal.opened"
	ClosedType    = "terminal.closed"

	KindSession = "session"
	KindShell   = "shell"

	// RingSize bounds the recent-output snapshot a new stream receives.
	RingSize = 256 << 10
	// DefaultIdleTimeout reaps a terminal nobody has streamed for this long.
	DefaultIdleTimeout = 30 * time.Minute

	readChunk = 32 << 10
	subBuffer = 256
	hupGrace  = 2 * time.Second
	termGrace = 3 * time.Second
	killGrace = 2 * time.Second
	// writeTimeout bounds one input write into a full pty input queue.
	writeTimeout = 5 * time.Second
	// drainGrace bounds how long the output reader may lag the process
	// exit (a background grandchild can keep the slave open).
	drainGrace = time.Second
)

var (
	ErrNotFound         = errors.New("terminal not found")
	ErrSessionNotOpen   = errors.New("session not open")
	ErrSessionsDisabled = errors.New("session targets unavailable: no launcher claudePath")
	ErrClosed           = errors.New("terminal bridge closed")
)

// ErrNotAttachable: a claude interactive session runs in another terminal;
// claude attach reaches background sessions only (RHZ-142).
var ErrNotAttachable = errors.New("session runs in another terminal: not attachable")

// Target is what a terminal runs: exactly one of Session or Shell.
type Target struct {
	Session string // registry session id → claude attach
	Shell   string // absolute working directory → $SHELL -l
}

type openedPayload struct {
	TerminalID string `json:"terminalId"`
	Kind       string `json:"kind"`
	SessionID  string `json:"sessionId,omitempty"`
}

type closedPayload struct {
	TerminalID string `json:"terminalId"`
	ExitCode   int    `json:"exitCode"`
}

// Host owns the live terminals of one serve process. The zero value is not
// usable; set Store and call New.
type Host struct {
	Store events.Port
	// ClaudePath is the launcher ledger's claudePath; "" disables session
	// targets (ErrSessionsDisabled, zero writes).
	ClaudePath  string
	IdleTimeout time.Duration
	// Status is the last observed claude status of a session (RHZ-142,
	// session.Observer.Status); nil = none.
	Status func(sessionID string) string
	// OnError reports journal failures with fixed text (never terminal bytes).
	OnError func(string)
	// argv overrides the command line of a target (tests only).
	argv func(Target, session.Session) ([]string, error)

	mu       sync.Mutex
	terms    map[string]*Terminal
	shutdown bool
}

func New(store events.Port, claudePath string) *Host {
	return &Host{Store: store, ClaudePath: claudePath, IdleTimeout: DefaultIdleTimeout, terms: map[string]*Terminal{}}
}

// Terminal is one pseudo-terminal and its child process group.
type Terminal struct {
	ID        string
	Kind      string
	SessionID string
	Cwd       string
	OpenedAt  time.Time

	host    *Host
	cmd     *exec.Cmd
	master  *os.File
	writeMu sync.Mutex

	mu       sync.Mutex
	cols     uint16
	rows     uint16
	ring     []byte
	subs     map[chan []byte]bool
	idle     *time.Timer
	reaped   bool // the child is waited for: its pid may be reused, never signal it
	exited   bool
	exitCode int
	done     chan struct{}
}

// Info is the listing view of a live terminal.
type Info struct {
	TerminalID  string `json:"terminalId"`
	Kind        string `json:"kind"`
	SessionID   string `json:"sessionId,omitempty"`
	Cwd         string `json:"cwd"`
	Cols        uint16 `json:"cols"`
	Rows        uint16 `json:"rows"`
	Subscribers int    `json:"subscribers"`
	OpenedAt    int64  `json:"openedAt"`
}

// OpenSessions replays the registry and returns its open sessions by id.
func OpenSessions(store events.Port) ([]session.Session, error) {
	ids := map[string]bool{}
	for _, e := range store.All() {
		if e.AggregateType == session.AggregateType {
			ids[e.AggregateID] = true
		}
	}
	out := []session.Session{}
	for id := range ids {
		s, err := session.Replay(store.List(session.AggregateType, id))
		if err != nil {
			return nil, err
		}
		if s.State == session.Open {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// List returns the live terminals by id.
func (h *Host) List() []Info {
	h.mu.Lock()
	ts := make([]*Terminal, 0, len(h.terms))
	for _, t := range h.terms {
		ts = append(ts, t)
	}
	h.mu.Unlock()
	out := make([]Info, 0, len(ts))
	for _, t := range ts {
		t.mu.Lock()
		out = append(out, Info{TerminalID: t.ID, Kind: t.Kind, SessionID: t.SessionID, Cwd: t.Cwd, Cols: t.cols, Rows: t.rows, Subscribers: len(t.subs), OpenedAt: t.OpenedAt.UnixMilli()})
		t.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TerminalID < out[j].TerminalID })
	return out
}

func (h *Host) get(id string) (*Terminal, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.terms[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t, nil
}

func loginShell() string {
	if s := os.Getenv("SHELL"); filepath.IsAbs(s) {
		return s
	}
	return "/bin/zsh"
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// resolve validates a target and returns its kind, session id, argv and cwd.
func (h *Host) resolve(t Target) (kind, sid string, argv []string, dir string, err error) {
	switch {
	case (t.Session == "") == (t.Shell == ""):
		return "", "", nil, "", errors.New("target needs exactly one of session or shell")
	case t.Shell != "":
		if !filepath.IsAbs(t.Shell) || filepath.Clean(t.Shell) != t.Shell || !isDir(t.Shell) {
			return "", "", nil, "", errors.New("shell cwd must be an absolute, clean, existing directory")
		}
		argv = []string{loginShell(), "-l"}
		if h.argv != nil {
			if argv, err = h.argv(t, session.Session{}); err != nil {
				return "", "", nil, "", err
			}
		}
		return KindShell, "", argv, t.Shell, nil
	}
	if h.ClaudePath == "" {
		return "", "", nil, "", ErrSessionsDisabled
	}
	s, err := session.Replay(h.Store.List(session.AggregateType, t.Session))
	if err != nil || s.State != session.Open {
		return "", "", nil, "", ErrSessionNotOpen
	}
	if s.Kind == session.KindInteractive {
		return "", "", nil, "", ErrNotAttachable
	}
	dir = s.Cwd
	if !isDir(dir) {
		if dir, err = os.UserHomeDir(); err != nil {
			return "", "", nil, "", err
		}
	}
	ref := s.SessionID
	if s.AttachID != "" {
		ref = s.AttachID // RHZ-142: the short id `claude agents` reports
	}
	argv = []string{h.ClaudePath, "attach", ref}
	if h.argv != nil {
		if argv, err = h.argv(t, s); err != nil {
			return "", "", nil, "", err
		}
	}
	return KindSession, s.SessionID, argv, dir, nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "term-" + hex.EncodeToString(b)
}

// control runs fn on f's descriptor without switching it to blocking mode
// (File.Fd would, and a blocked Read could then never be interrupted).
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func setSize(f *os.File, cols, rows uint16) error {
	return control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}

func validSize(cols, rows int) bool { return cols >= 1 && cols <= 1000 && rows >= 1 && rows <= 1000 }

// Open starts a target on a new pseudo-terminal and records terminal.opened.
func (h *Host) Open(t Target, cols, rows int) (*Terminal, error) {
	if !validSize(cols, rows) {
		return nil, errors.New("cols and rows must be 1..1000")
	}
	kind, sid, argv, dir, err := h.resolve(t)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.shutdown {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	h.mu.Unlock()
	master, slave, err := openPTY()
	if err != nil {
		return nil, err
	}
	if err := setSize(master, uint16(cols), uint16(rows)); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	slave.Close()
	if err != nil {
		master.Close()
		return nil, err
	}
	term := &Terminal{ID: newID(), Kind: kind, SessionID: sid, Cwd: dir, OpenedAt: time.Now(), host: h, cmd: cmd, master: master,
		cols: uint16(cols), rows: uint16(rows), ring: make([]byte, 0, RingSize), subs: map[chan []byte]bool{}, done: make(chan struct{})}
	b, _ := json.Marshal(openedPayload{TerminalID: term.ID, Kind: kind, SessionID: sid})
	if err := h.Store.Append(0, events.Event{AggregateType: AggregateType, AggregateID: term.ID, Revision: 1, Type: OpenedType, Payload: b}); err != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		master.Close()
		return nil, err
	}
	h.mu.Lock()
	h.terms[term.ID] = term
	late := h.shutdown
	h.mu.Unlock()
	if late {
		defer term.Close()
	}
	term.mu.Lock()
	term.armIdle()
	term.mu.Unlock()
	readerDone := make(chan struct{})
	go term.read(readerDone)
	go term.wait(readerDone)
	return term, nil
}

func (t *Terminal) read(done chan struct{}) {
	defer close(done)
	buf := make([]byte, readChunk)
	for {
		n, err := t.master.Read(buf)
		if n > 0 {
			t.publish(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// publish appends a chunk to the ring and fans it out. A subscriber whose
// buffer is full is dropped (its stream ends; a reconnect gets the snapshot).
func (t *Terminal) publish(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exited {
		return
	}
	t.ring = appendRing(t.ring, p)
	chunk := append([]byte(nil), p...)
	dropped := false
	for ch := range t.subs {
		select {
		case ch <- chunk:
		default:
			delete(t.subs, ch)
			close(ch)
			dropped = true
		}
	}
	if dropped {
		t.armIdle()
	}
}

func appendRing(r, p []byte) []byte {
	if len(p) >= RingSize {
		return append(r[:0], p[len(p)-RingSize:]...)
	}
	if over := len(r) + len(p) - RingSize; over > 0 {
		r = r[:copy(r, r[over:])]
	}
	return append(r, p...)
}

// wait reaps the child, records terminal.closed and unlists the terminal
// before any stream sees exit, so a client acting on exit sees it gone.
func (t *Terminal) wait(readerDone chan struct{}) {
	t.cmd.Wait()
	t.mu.Lock()
	t.reaped = true
	t.mu.Unlock()
	select {
	case <-readerDone:
	case <-time.After(drainGrace):
	}
	t.master.Close()
	code := -1
	if ws, ok := t.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		switch {
		case ws.Exited():
			code = ws.ExitStatus()
		case ws.Signaled():
			code = 128 + int(ws.Signal())
		}
	}
	h := t.host
	b, _ := json.Marshal(closedPayload{TerminalID: t.ID, ExitCode: code})
	if err := h.Store.Append(1, events.Event{AggregateType: AggregateType, AggregateID: t.ID, Revision: 2, Type: ClosedType, Payload: b}); err != nil && h.OnError != nil {
		h.OnError("terminal " + t.ID + ": closed event not recorded")
	}
	h.mu.Lock()
	delete(h.terms, t.ID)
	h.mu.Unlock()
	t.mu.Lock()
	t.exited, t.exitCode = true, code
	if t.idle != nil {
		t.idle.Stop()
	}
	for ch := range t.subs {
		close(ch)
	}
	t.subs = map[chan []byte]bool{}
	t.mu.Unlock()
	close(t.done)
}

// armIdle (t.mu held) schedules the idle reap while nobody streams.
func (t *Terminal) armIdle() {
	if t.exited || len(t.subs) > 0 {
		return
	}
	if t.idle != nil {
		t.idle.Stop()
	}
	d := t.host.IdleTimeout
	if d <= 0 {
		d = DefaultIdleTimeout
	}
	var tm *time.Timer
	tm = time.AfterFunc(d, func() {
		t.mu.Lock()
		// a superseded timer (re-armed after this one fired) does nothing
		idle := t.idle == tm && !t.exited && len(t.subs) == 0
		t.mu.Unlock()
		if idle {
			t.Close()
		}
	})
	t.idle = tm
}

// Subscribe atomically returns the ring snapshot and a channel of later
// chunks. The channel closes when the terminal exits or the subscriber
// falls behind; exited reports an already finished terminal.
func (t *Terminal) Subscribe() (snapshot []byte, ch chan []byte, exited bool, code int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	snapshot = append([]byte(nil), t.ring...)
	if t.exited {
		return snapshot, nil, true, t.exitCode
	}
	ch = make(chan []byte, subBuffer)
	t.subs[ch] = true
	if t.idle != nil {
		t.idle.Stop()
	}
	return snapshot, ch, false, 0
}

// Unsubscribe removes a stream; the last one leaving arms the idle reap.
func (t *Terminal) Unsubscribe(ch chan []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.subs[ch] {
		delete(t.subs, ch)
		close(ch)
	}
	t.armIdle()
}

// Exit reports the exit code once the terminal has finished.
func (t *Terminal) Exit() (code int, exited bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitCode, t.exited
}

// Done closes when the terminal has finished and terminal.closed is recorded.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Input writes bytes to the pseudo-terminal unchanged.
func (t *Terminal) Input(p []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	// a child that stops reading must not wedge every later writer
	t.master.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := t.master.Write(p)
	return err
}

// Resize sets the window size; the kernel signals the foreground group.
func (t *Terminal) Resize(cols, rows int) error {
	if !validSize(cols, rows) {
		return errors.New("cols and rows must be 1..1000")
	}
	if err := setSize(t.master, uint16(cols), uint16(rows)); err != nil {
		return err
	}
	t.mu.Lock()
	t.cols, t.rows = uint16(cols), uint16(rows)
	t.mu.Unlock()
	return nil
}

// Close hangs up the process group (SIGHUP, then SIGTERM after a grace, and
// SIGKILL as a last resort) and waits for the exit. For a session target only
// the attach client ends; the Claude session keeps running in its own daemon.
func (t *Terminal) Close() {
	pid := t.cmd.Process.Pid
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{{syscall.SIGHUP, hupGrace}, {syscall.SIGTERM, termGrace}, {syscall.SIGKILL, killGrace}} {
		t.mu.Lock()
		if !t.reaped {
			syscall.Kill(-pid, step.sig)
		}
		t.mu.Unlock()
		select {
		case <-t.done:
			return
		case <-time.After(step.grace):
		}
	}
}

// Shutdown closes every terminal (serve exit: children never outlive serve),
// including one an in-flight Open registers meanwhile.
func (h *Host) Shutdown() {
	for round := 0; round < 3; round++ {
		h.mu.Lock()
		h.shutdown = true
		ts := make([]*Terminal, 0, len(h.terms))
		for _, t := range h.terms {
			ts = append(ts, t)
		}
		h.mu.Unlock()
		if len(ts) == 0 {
			return
		}
		var wg sync.WaitGroup
		for _, t := range ts {
			wg.Add(1)
			go func(t *Terminal) { defer wg.Done(); t.Close() }(t)
		}
		wg.Wait()
	}
}
