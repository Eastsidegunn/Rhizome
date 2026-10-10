package main

// End-to-end tests of FR-RHZ-139 (M5b ↔ M4, no Fake): the real Rhizome
// channel (internal/secretchannel) and secret writer (internal/secretref)
// over a real journal dial the real plane session. Test files are outside
// the §7 R8 edge rules (archtest ignores _test.go), so they may import the
// Rhizome side.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/peercred"
	"rhizome/internal/secretchannel"
	"rhizome/internal/secretplane/server"
	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
)

type allowGuard struct{}

func (allowGuard) CheckAppend(events.View, events.Event, time.Time) error { return nil }
func (allowGuard) CheckReplay(events.View, events.Event) error            { return nil }

// rhz is one Rhizome process: journal, single secret writer, channel.
type rhz struct {
	t       *testing.T
	path    string
	j       *journal.Journal
	svc     *secretref.Service
	ch      *secretchannel.Channel
	mu      sync.Mutex
	reports []string
}

func openRhz(t *testing.T, path, sock string) *rhz {
	t.Helper()
	j, err := journal.OpenGuarded(path, allowGuard{})
	if err != nil {
		t.Fatal(err)
	}
	r := &rhz{t: t, path: path, j: j, svc: secretref.New(j)}
	r.ch, err = secretchannel.New(secretchannel.Config{SocketPath: sock, Service: r.svc, Latch: j,
		OnError: func(s string) { r.mu.Lock(); r.reports = append(r.reports, s); r.mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rhz) close() { r.j.Close() }

func (r *rhz) cycle() { r.ch.Cycle(context.Background()) }

// poll runs channel cycles until one is served by the plane through its poll
// step, so exactly one poll/batch exchange is known to have happened. A cycle
// can legitimately get error{busy} while the plane's handler of the previous
// connection is still draining its optional post-batch frame; such a cycle
// writes nothing on either side and is retried. Bounded; the failure shows
// the last observed counts.
func (r *rhz) poll(p *proc) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		before := p.srv.Served()
		r.cycle()
		after := p.srv.Served()
		if after > before {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no cycle served by the plane (served %d before, %d after; reports %v)", before, after, r.reportList())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *rhz) reportList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reports...)
}

func (r *rhz) ref(ref string) secretref.RefView {
	r.t.Helper()
	p, _, err := r.svc.Snapshot()
	if err != nil {
		r.t.Fatal(err)
	}
	v, ok := p.Ref(ref)
	if !ok {
		r.t.Fatal("ref not in projection")
	}
	return v
}

func (r *rhz) refByName(name string) secretref.RefView {
	r.t.Helper()
	p, _, err := r.svc.Snapshot()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, v := range p.Refs() {
		if v.Name == name {
			return v
		}
	}
	r.t.Fatal("name not in projection")
	return secretref.RefView{}
}

func (r *rhz) expect(ref, state string, version int64) {
	r.t.Helper()
	if v := r.ref(ref); v.State != state || v.Version != version {
		r.t.Fatalf("ref state %s version %d, want %s %d", v.State, v.Version, state, version)
	}
}

func (r *rhz) register(name string, tags ...string) (string, string) {
	r.t.Helper()
	op := newOpID(r.t)
	res, err := r.svc.RequestRegister(secretref.RegisterRequest{OpID: op, Name: name, RefKind: secretwire.RefKindProviderKey,
		ScopeTags: tags, Actor: "tester"})
	if err != nil || !res.Accepted {
		r.t.Fatalf("register refused: %s", res.Reason)
	}
	return op, res.Ref
}

func (r *rhz) rotate(ref string) {
	r.t.Helper()
	res, err := r.svc.RequestRotate(secretref.RotateRequest{OpID: newOpID(r.t), Ref: ref, Actor: "tester"})
	if err != nil || !res.Accepted {
		r.t.Fatalf("rotate refused: %s", res.Reason)
	}
}

func (r *rhz) revoke(ref, reason string) {
	r.t.Helper()
	res, err := r.svc.RequestRevoke(secretref.RevokeRequest{OpID: newOpID(r.t), Ref: ref, ReasonCode: reason, Actor: "tester"})
	if err != nil || !res.Accepted {
		r.t.Fatalf("revoke refused: %s", res.Reason)
	}
}

// proc is one plane process: run() in a goroutine with a scripted terminal.
type proc struct {
	t       *testing.T
	in      *io.PipeWriter
	out     syncBuf
	errb    syncBuf
	pw      *passwords
	done    chan int
	srv     *server.Server
	prompts int
}

func startPlane(t *testing.T, dir, sock string, verify func(*net.UnixConn) (peercred.Verified, error)) *proc {
	t.Helper()
	inR, inW := io.Pipe()
	p := &proc{t: t, in: inW, pw: &passwords{t: t}, done: make(chan int, 1)}
	started := make(chan *server.Server, 1)
	e := &env{stdin: inR, stdout: &p.out, stderr: &p.errb, stdinFd: -1,
		isTerminal:   func(int) bool { return true },
		readPassword: func(int) ([]byte, error) { return p.pw.read() },
		configDir:    func() (string, error) { return "", fmt.Errorf("unused") },
		exit:         func(int) { t.Error("fatal exit") },
		verify:       verify,
		started:      func(s *server.Server) { started <- s }}
	go func() { p.done <- run([]string{"session", "-state-dir", dir, "-socket", sock}, e) }()
	select {
	case p.srv = <-started:
	case code := <-p.done:
		t.Fatalf("plane exited %d: %s", code, p.errb.String())
	case <-time.After(5 * time.Second):
		t.Fatal("plane did not start")
	}
	p.prompts = 1
	p.waitPrompts()
	return p
}

// do sends top-level commands (with their answers) and waits until the loop
// is back at the prompt after the given number of top-level commands.
func (p *proc) do(commands int, lines ...string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, strings.Join(lines, "\n")+"\n"); err != nil {
		p.t.Fatal(err)
	}
	p.prompts += commands
	p.waitPrompts()
}

func (p *proc) waitPrompts() {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(p.out.String(), "plane> ") < p.prompts {
		if time.Now().After(deadline) {
			p.t.Fatalf("timeout; output:\n%s", p.out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *proc) quit() {
	p.t.Helper()
	io.WriteString(p.in, "quit\n")
	select {
	case code := <-p.done:
		if code != 0 {
			p.t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		p.t.Fatal("plane did not quit")
	}
	p.in.Close()
}

// TestE2EPlaneSessionFRRHZ139 runs the M4 ↔ M5b loop: register → active(1),
// rotate → active(2), restart of both sides, revoke → revoked, reject →
// rejected, attested declare/rotate/revoke (record-only), epoch replacement
// with quarantine, and quarantine list. ZV over journal, terminal, ledger and
// quarantine; the values file holds the value.
func TestE2EPlaneSessionFRRHZ139(t *testing.T) {
	s1, s2, s3 := sentinel(t), sentinel(t), sentinel(t)
	base := shortDir(t)
	dir, sock := filepath.Join(base, "st"), filepath.Join(base, "run", "p.sock")
	jpath := filepath.Join(t.TempDir(), "journal.ndjson")
	var terminal []string

	pl := startPlane(t, dir, sock, nil)
	rz := openRhz(t, jpath, sock)

	// register → active(1)
	_, ref := rz.register("svc", "prod")
	rz.poll(pl)
	if q := pl.srv.Queue(); len(q) != 1 || q[0].Pending.Ref != ref {
		t.Fatalf("queue %+v", q)
	}
	pl.pw.push(s1, s1)
	pl.do(1, "next", "accept", "yes")
	rz.poll(pl)
	rz.expect(ref, secretref.StateActive, 1)

	// rotate → active(2)
	rz.rotate(ref)
	rz.poll(pl)
	pl.pw.push(s2, s2)
	pl.do(1, "next", "accept", "yes")
	rz.poll(pl)
	rz.expect(ref, secretref.StateActive, 2)

	// restart both sides: same planeId, nothing resent or lost
	planeBefore := rz.ref(ref)
	pl.quit()
	terminal = append(terminal, pl.out.String(), pl.errb.String())
	rz.close()
	pl = startPlane(t, dir, sock, nil)
	rz = openRhz(t, jpath, sock)
	rz.poll(pl)
	rz.expect(ref, secretref.StateActive, 2)
	if got := rz.ref(ref); got.LastOutcome == nil || *got.LastOutcome != *planeBefore.LastOutcome {
		t.Fatal("restart changed the outcome")
	}
	pl.do(1, "status")
	if !strings.Contains(pl.out.String(), " ack 2 outbox 2") {
		t.Fatalf("plane ack after restart:\n%s", pl.out.String())
	}

	// revoke → revoked
	rz.revoke(ref, secretwire.ReasonUnused)
	rz.poll(pl)
	pl.do(1, "next", "accept", "revoke")
	rz.poll(pl)
	rz.expect(ref, secretref.StateRevoked, 2)

	// reject → rejected outcome
	_, ref4 := rz.register("unwanted")
	rz.poll(pl)
	pl.do(1, "next", "reject", "yes")
	rz.poll(pl)
	if v := rz.ref(ref4); v.State != secretref.StateRegistrationFailed || v.LastOutcome == nil ||
		v.LastOutcome.Result != secretwire.ResultRejected {
		t.Fatalf("reject: %+v", v)
	}

	// attested (Q2): declare, rotate, revoke; values file untouched
	vals := readFile(t, filepath.Join(dir, "values.json"))
	pl.do(1, "attest", "broker", "provider-key", "", "attest")
	rz.poll(pl)
	att := rz.refByName("broker")
	if att.Origin != secretwire.OriginAttested || att.State != secretref.StateActive || att.Version != 1 {
		t.Fatalf("attested: %+v", att)
	}
	rz.rotate(att.Ref)
	rz.poll(pl)
	pl.do(1, "next", "accept", "rotated externally")
	rz.poll(pl)
	rz.expect(att.Ref, secretref.StateActive, 2)
	rz.revoke(att.Ref, secretwire.ReasonExposed)
	rz.poll(pl)
	pl.do(1, "next", "accept", "revoke")
	rz.poll(pl)
	rz.expect(att.Ref, secretref.StateRevoked, 2)
	if string(vals) != string(readFile(t, filepath.Join(dir, "values.json"))) {
		t.Fatal("attested operations touched the values file")
	}

	// epoch replacement: an unacked plane-direct registration is quarantined
	pid1, _ := planeID(t, pl)
	pl.pw.push(s3, s3)
	pl.do(1, "register", "late", "other", "", "yes")
	pl.do(1, "epoch-replace", "replace epoch")
	rz.poll(pl) // first poll after freeze: cutoff, quarantine, split
	rz.poll(pl) // first poll of P′: epoch-active
	pl.do(1, "status")
	pid2, phase := planeID(t, pl)
	if pid2 == pid1 || phase != "new-epoch-active" {
		t.Fatalf("epoch: phase %s", phase)
	}
	if !strings.Contains(pl.out.String(), "notice: epoch-phase:split-recorded") {
		t.Fatal("phase notice missing")
	}
	p, _, _ := rz.svc.Snapshot()
	for _, v := range p.Refs() {
		if v.Name == "late" {
			t.Fatal("quarantined registration reached Rhizome")
		}
	}

	// quarantine list: busy while the session runs, then value-free
	e := newCmdEnv(t, false, "")
	if code := run([]string{"quarantine", "list", "-state-dir", dir}, &e.env); code != 1 || e.errb.String() != "rhizome-secret-plane: busy\n" {
		t.Fatalf("busy: %d %q", code, e.errb.String())
	}
	pl.quit()
	terminal = append(terminal, pl.out.String(), pl.errb.String())
	e = newCmdEnv(t, false, "")
	if code := run([]string{"quarantine", "list", "-state-dir", dir}, &e.env); code != 0 {
		t.Fatalf("list: %d", code)
	}
	if !strings.HasPrefix(e.out.String(), pid1+" ") || !strings.Contains(e.out.String(), " reconciled ") ||
		strings.Count(e.out.String(), "\n") != 1 {
		t.Fatalf("list %q", e.out.String())
	}
	terminal = append(terminal, e.out.String(), e.errb.String())

	if reps := rz.reportList(); len(reps) != 0 {
		t.Fatalf("channel reports %v", reps)
	}
	rz.close()
	all := []byte(strings.Join(terminal, "\n"))
	assertNoSentinel(t, "terminal", all, s1, s2, s3)
	assertWording(t, "terminal", string(all))
	assertNoSentinel(t, "journal", readFile(t, jpath), s1, s2, s3)
	assertNoSentinel(t, "ledger", readFile(t, filepath.Join(dir, "ledger.ndjson")), s1, s2, s3)
	assertNoSentinel(t, "quarantine", readFile(t, filepath.Join(dir, "quarantine.ndjson")), s1, s2, s3)
	v := readFile(t, filepath.Join(dir, "values.json"))
	if !strings.Contains(string(v), base64.StdEncoding.EncodeToString([]byte(s3))) {
		t.Fatal("values file lacks the plane-direct value")
	}
	assertNoSentinel(t, "values (revoked)", v, s1, s2)
}

// planeID parses the last "plane <id> phase <phase>" status line.
func planeID(t *testing.T, p *proc) (string, string) {
	t.Helper()
	lines := strings.Split(p.out.String(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.Fields(strings.TrimPrefix(lines[i], "plane> "))
		if len(f) >= 4 && f[0] == "plane" && f[2] == "phase" {
			return f[1], f[3]
		}
	}
	t.Fatal("no status line")
	return "", ""
}

// TestE2EPeerRejectedFRRHZ139: a plane whose W2 check fails answers
// error{peer-rejected}; Rhizome records nothing and the plane notices it.
func TestE2EPeerRejectedFRRHZ139(t *testing.T) {
	base := shortDir(t)
	dir, sock := filepath.Join(base, "st"), filepath.Join(base, "run", "p.sock")
	var mu sync.Mutex
	calls := 0
	pl := startPlane(t, dir, sock, func(*net.UnixConn) (peercred.Verified, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return peercred.Verified{}, nil
	})
	rz := openRhz(t, filepath.Join(t.TempDir(), "journal.ndjson"), sock)
	_, ref := rz.register("svc")
	before := readFile(t, rz.path)
	rz.cycle()
	rz.cycle()
	if string(before) != string(readFile(t, rz.path)) {
		t.Fatal("journal changed")
	}
	rz.expect(ref, secretref.StatePendingRegistration, 0)
	if len(pl.srv.Queue()) != 0 {
		t.Fatal("rejected peer's pending reached the queue")
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("verifier calls %d", n)
	}
	pl.do(1, "status")
	if !strings.Contains(pl.out.String(), "notice: "+server.NoticePeerRejected) {
		t.Fatal("no peer-rejected notice")
	}
	pl.quit()
	rz.close()
}

// TestE2ECutoffPendingFRRHZ139: a Rhizome request pending across an epoch
// replacement is not actionable from the cutoff poll (old planeId, frozen);
// only the first poll of P′ queues it, and it then completes under P′. The
// socket path carries a sentinel and never appears in the plane's output.
func TestE2ECutoffPendingFRRHZ139(t *testing.T) {
	sockSentinel, s := sentinel(t), sentinel(t)
	base, err := os.MkdirTemp("/tmp", "sp")
	if err != nil {
		t.Skip("no short /tmp directory for a sentinel socket path")
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	sock := filepath.Join(base, sockSentinel, "p.sock")
	dir := filepath.Join(shortDir(t), "st")
	pl := startPlane(t, dir, sock, nil)
	rz := openRhz(t, filepath.Join(t.TempDir(), "journal.ndjson"), sock)

	_, ref := rz.register("svc")
	rz.poll(pl)
	if q := pl.srv.Queue(); len(q) != 1 || !q[0].Actionable() {
		t.Fatal("request not queued")
	}
	pl.do(1, "epoch-replace", "replace epoch")
	pid1, _ := planeID(t, pl)
	rz.poll(pl) // cutoff poll: old planeId, frozen at entry; splits to P′
	for _, it := range pl.srv.Queue() {
		if it.Actionable() {
			t.Fatal("cutoff poll made the request actionable")
		}
	}
	pl.do(2, "next", "status")
	if !strings.Contains(pl.out.String(), "no actionable request") {
		t.Fatal("request handled from the cutoff poll")
	}
	pid2, phase := planeID(t, pl)
	if pid2 == pid1 || phase != "split-recorded" {
		t.Fatalf("phase %s", phase)
	}
	rz.poll(pl) // first poll of P′ feeds the queue
	if q := pl.srv.Queue(); len(q) != 1 || !q[0].Actionable() {
		t.Fatal("P′ poll did not queue the request")
	}
	pl.pw.push(s, s)
	pl.do(1, "next", "accept", "yes")
	rz.poll(pl)
	rz.expect(ref, secretref.StateActive, 1)
	pl.quit()
	if reps := rz.reportList(); len(reps) != 0 {
		t.Fatalf("channel reports %v", reps)
	}
	rz.close()
	term := []byte(pl.out.String() + pl.errb.String())
	assertNoSentinel(t, "terminal", term, sockSentinel, s)
	assertNoSentinel(t, "ledger", readFile(t, filepath.Join(dir, "ledger.ndjson")), sockSentinel, s)
	if !strings.Contains(pl.out.String(), "socket ready") {
		t.Fatal("no socket ready line")
	}
}
