//go:build darwin || linux

package terminal

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/session"
)

func shArgv(script string) func(Target, session.Session) ([]string, error) {
	return func(Target, session.Session) ([]string, error) { return []string{"/bin/sh", "-c", script}, nil }
}

func newHost(t *testing.T, store events.Port, claude string) (*Host, *httptest.Server) {
	t.Helper()
	h := New(store, claude)
	srv := httptest.NewServer(h.Handler(true))
	t.Cleanup(func() { srv.Close(); h.Shutdown() })
	return h, srv
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(res.Body)
	return res.StatusCode, b.String()
}

func openShell(t *testing.T, srv *httptest.Server, cols, rows int) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"target": map[string]any{"shell": map[string]string{"cwd": t.TempDir()}}, "cols": cols, "rows": rows})
	code, out := do(t, http.MethodPost, srv.URL+"/v1/terminals", string(body))
	if code != http.StatusCreated {
		t.Fatalf("open: %d %s", code, out)
	}
	var r struct{ TerminalID string }
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.TerminalID == "" {
		t.Fatalf("open response %q", out)
	}
	return r.TerminalID
}

func input(t *testing.T, srv *httptest.Server, id, text string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"data": base64.StdEncoding.EncodeToString([]byte(text))})
	if code, out := do(t, http.MethodPost, srv.URL+"/v1/terminals/"+id+"/input", string(body)); code != http.StatusNoContent {
		t.Fatalf("input: %d %s", code, out)
	}
}

// stream is one SSE subscriber: decoded bytes of snapshot and output events
// accumulate in text; the exit code lands in exit.
type stream struct {
	mu       sync.Mutex
	text     bytes.Buffer
	snapshot []byte
	kinds    []string
	exit     *int
	done     chan struct{}
	res      *http.Response
}

func subscribe(t *testing.T, srv *httptest.Server, id string) *stream {
	t.Helper()
	res, err := http.Get(srv.URL + "/v1/terminals/" + id + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", res.StatusCode)
	}
	s := &stream{done: make(chan struct{}), res: res}
	t.Cleanup(func() { res.Body.Close() })
	go func() {
		defer close(s.done)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		kind := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var d struct {
					Data     string `json:"data"`
					ExitCode *int   `json:"exitCode"`
				}
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d)
				b, _ := base64.StdEncoding.DecodeString(d.Data)
				s.mu.Lock()
				s.kinds = append(s.kinds, kind)
				if kind == "snapshot" {
					s.snapshot = b
				}
				s.text.Write(b)
				if kind == "exit" {
					s.exit = d.ExitCode
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *stream) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.text.String() }

func (s *stream) waitFor(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q; got %q", substr, s.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *stream) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exit == nil {
		t.Fatalf("stream ended without exit event; kinds %v", s.kinds)
	}
	return *s.exit
}

func sentinel(t *testing.T) string {
	b := make([]byte, 8)
	rand.Read(b)
	return "SENTINEL" + hex.EncodeToString(b)
}

func terminalEvents(store *events.Store) []events.Event {
	var out []events.Event
	for _, e := range store.All() {
		if e.AggregateType == AggregateType {
			out = append(out, e)
		}
	}
	return out
}

// TestRoundTripFRRHZ184: a real pty runs sh 'echo hi; read x; echo got:$x';
// typed input arrives unchanged, output streams back, the exit event closes
// the stream and the journal holds only opened/closed without any byte.
func TestRoundTripFRRHZ184(t *testing.T) {
	store := &events.Store{}
	h, srv := newHost(t, store, "")
	h.argv = shArgv("echo hi; read x; echo got:$x")
	id := openShell(t, srv, 80, 24)
	s := subscribe(t, srv, id)
	s.waitFor(t, "hi")
	sent := sentinel(t)
	input(t, srv, id, sent+"\n")
	s.waitFor(t, "got:"+sent)
	if code := s.waitExit(t); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	evs := terminalEvents(store)
	if len(evs) != 2 || evs[0].Type != OpenedType || evs[1].Type != ClosedType {
		t.Fatalf("terminal events %+v", evs)
	}
	var op openedPayload
	json.Unmarshal(evs[0].Payload, &op)
	if op.TerminalID != id || op.Kind != KindShell || op.SessionID != "" {
		t.Fatalf("opened %s", evs[0].Payload)
	}
	var cp closedPayload
	json.Unmarshal(evs[1].Payload, &cp)
	if cp.TerminalID != id || cp.ExitCode != 0 {
		t.Fatalf("closed %s", evs[1].Payload)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(sent))
	for _, e := range store.All() {
		if bytes.Contains(e.Payload, []byte(sent)) || bytes.Contains(e.Payload, []byte(b64)) || bytes.Contains(e.Payload, []byte("got:")) {
			t.Fatalf("journal event %s carries terminal bytes", e.Type)
		}
	}
	if len(h.List()) != 0 {
		t.Fatal("exited terminal still listed")
	}
	if code, _ := do(t, http.MethodGet, srv.URL+"/v1/terminals/"+id+"/stream", ""); code != http.StatusNotFound {
		t.Fatalf("stream of exited terminal: %d", code)
	}
}

// TestResizeFRRHZ184: the initial size and a resize both show in stty size;
// DELETE hangs up the process group and the exit event reports it.
func TestResizeFRRHZ184(t *testing.T) {
	store := &events.Store{}
	h, srv := newHost(t, store, "")
	h.argv = shArgv("stty size; read x; stty size; read y")
	id := openShell(t, srv, 80, 24)
	s := subscribe(t, srv, id)
	s.waitFor(t, "24 80")
	if code, out := do(t, http.MethodPost, srv.URL+"/v1/terminals/"+id+"/resize", `{"cols":100,"rows":40}`); code != http.StatusNoContent {
		t.Fatalf("resize: %d %s", code, out)
	}
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals/"+id+"/resize", `{"cols":0,"rows":40}`); code != http.StatusBadRequest {
		t.Fatalf("invalid resize: %d", code)
	}
	if l := h.List(); len(l) != 1 || l[0].Cols != 100 || l[0].Rows != 40 || l[0].Subscribers != 1 {
		t.Fatalf("list %+v", l)
	}
	input(t, srv, id, "\n")
	s.waitFor(t, "40 100")
	if code, out := do(t, http.MethodDelete, srv.URL+"/v1/terminals/"+id, ""); code != http.StatusOK || !strings.Contains(out, `"exited":true`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	if code := s.waitExit(t); code != 128+1 {
		t.Fatalf("exit code %d, want SIGHUP", code)
	}
	evs := terminalEvents(store)
	if len(evs) != 2 || evs[1].Type != ClosedType {
		t.Fatalf("terminal events %+v", evs)
	}
}

// TestCloseKillFRRHZ184: a child ignoring SIGHUP and SIGTERM is killed.
func TestCloseKillFRRHZ184(t *testing.T) {
	store := &events.Store{}
	h, srv := newHost(t, store, "")
	h.argv = shArgv(`trap "" HUP TERM; echo trapped; read x`)
	id := openShell(t, srv, 80, 24)
	s := subscribe(t, srv, id)
	s.waitFor(t, "trapped")
	if code, out := do(t, http.MethodDelete, srv.URL+"/v1/terminals/"+id, ""); code != http.StatusOK || !strings.Contains(out, `"exited":true`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	if code := s.waitExit(t); code != 128+9 {
		t.Fatalf("exit code %d, want SIGKILL", code)
	}
}

// TestSnapshotFRRHZ184: a later subscriber starts from the ring snapshot;
// several streams share one terminal and anyone may type.
func TestSnapshotFRRHZ184(t *testing.T) {
	store := &events.Store{}
	h, srv := newHost(t, store, "")
	h.argv = shArgv("echo early-output; read x; echo second:$x")
	id := openShell(t, srv, 80, 24)
	a := subscribe(t, srv, id)
	a.waitFor(t, "early-output")
	b := subscribe(t, srv, id)
	b.waitFor(t, "early-output")
	b.mu.Lock()
	snap := string(b.snapshot)
	first := b.kinds[0]
	b.mu.Unlock()
	if first != "snapshot" || !strings.Contains(snap, "early-output") {
		t.Fatalf("second stream snapshot %q (first event %s)", snap, first)
	}
	input(t, srv, id, "typed\n")
	a.waitFor(t, "second:typed")
	b.waitFor(t, "second:typed")
	a.waitExit(t)
	b.waitExit(t)
}

func TestRingBoundFRRHZ184(t *testing.T) {
	r := make([]byte, 0, RingSize)
	r = appendRing(r, bytes.Repeat([]byte("a"), RingSize-10))
	r = appendRing(r, []byte("0123456789ABCDEF"))
	if len(r) != RingSize || !bytes.HasSuffix(r, []byte("0123456789ABCDEF")) || r[0] != 'a' {
		t.Fatalf("ring len %d", len(r))
	}
	big := append(bytes.Repeat([]byte("x"), RingSize), 'z')
	r = appendRing(r, big)
	if len(r) != RingSize || r[len(r)-1] != 'z' || r[0] != 'x' {
		t.Fatalf("ring after big chunk len %d", len(r))
	}
}

// TestNonLoopback501FRRHZ184: a non-loopback bind disables every route with
// 501 and zero writes.
func TestNonLoopback501FRRHZ184(t *testing.T) {
	store := &events.Store{}
	h := New(store, "/bin/echo")
	srv := httptest.NewServer(h.Handler(false))
	defer srv.Close()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/terminals", ""},
		{http.MethodPost, "/v1/terminals", `{"target":{"shell":{"cwd":"/"}},"cols":80,"rows":24}`},
		{http.MethodGet, "/v1/terminals/term-x/stream", ""},
		{http.MethodDelete, "/v1/terminals/term-x", ""},
	} {
		if code, _ := do(t, c.method, srv.URL+c.path, c.body); code != http.StatusNotImplemented {
			t.Fatalf("%s %s: %d", c.method, c.path, code)
		}
	}
	if len(store.All()) != 0 || len(h.List()) != 0 {
		t.Fatal("disabled bridge wrote or opened something")
	}
	for addr, want := range map[string]bool{"127.0.0.1:8790": true, "[::1]:8790": true, "0.0.0.0:8790": false, ":8790": false, "192.168.0.2:8790": false, "localhost:8790": false} {
		if LoopbackAddr(addr) != want {
			t.Fatalf("LoopbackAddr(%q) != %v", addr, want)
		}
	}
}

// TestSessionTargetFRRHZ184: open registry sessions are listed and attach
// runs claudePath attach <attachId> (RHZ-143: only a session with a short id
// attaches); closed/unknown sessions are 404 and a missing claudePath is
// 409, both with zero writes.
func TestSessionTargetFRRHZ184(t *testing.T) {
	store := &events.Store{}
	cwd := t.TempDir()
	if _, err := (session.Service{Store: store}).Open(session.Opened{SessionID: "sid-1", Name: "work", MissionID: "m-1", Mode: session.Interactive, Cwd: cwd, Kind: session.KindBackground, AttachID: "ab-1"}); err != nil {
		t.Fatal(err)
	}
	// a stand-in claude: prints its argv and waits like an attached client.
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\necho \"$@\"\nread x\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h, srv := newHost(t, store, claude)
	code, out := do(t, http.MethodGet, srv.URL+"/v1/terminals", "")
	var l listDTO
	if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil || !l.SessionTargets || len(l.Sessions) != 1 || l.Sessions[0].SessionID != "sid-1" || len(l.Terminals) != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	before := len(store.All())
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"nope"}},"cols":80,"rows":24}`); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"sid-1"},"shell":{"cwd":"/"}},"cols":80,"rows":24}`); code != http.StatusBadRequest {
		t.Fatalf("two targets: %d", code)
	}
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"shell":{"cwd":"relative"}},"cols":80,"rows":24}`); code != http.StatusBadRequest {
		t.Fatalf("relative cwd: %d", code)
	}
	h.ClaudePath = ""
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"sid-1"}},"cols":80,"rows":24}`); code != http.StatusConflict {
		t.Fatalf("no claudePath: %d", code)
	}
	if len(store.All()) != before {
		t.Fatal("rejected open wrote to the journal")
	}
	h.ClaudePath = claude
	code, out = do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"sid-1"}},"cols":80,"rows":24}`)
	if code != http.StatusCreated {
		t.Fatalf("open session: %d %s", code, out)
	}
	var r struct{ TerminalID string }
	json.Unmarshal([]byte(out), &r)
	s := subscribe(t, srv, r.TerminalID)
	s.waitFor(t, "attach ab-1")
	if l := h.List(); len(l) != 1 || l[0].Kind != KindSession || l[0].SessionID != "sid-1" || l[0].Cwd != cwd {
		t.Fatalf("list %+v", l)
	}
	if code, _ := do(t, http.MethodDelete, srv.URL+"/v1/terminals/"+r.TerminalID, ""); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if c := s.waitExit(t); c != 128+1 {
		t.Fatalf("exit %d", c)
	}
	evs := terminalEvents(store)
	var op openedPayload
	json.Unmarshal(evs[0].Payload, &op)
	if len(evs) != 2 || op.Kind != KindSession || op.SessionID != "sid-1" {
		t.Fatalf("events %+v", evs)
	}
	if ss, _ := (session.Service{Store: store}).Get("sid-1"); ss.State != session.Open {
		t.Fatal("closing the attach touched the session")
	}
}

// TestIdleReapFRRHZ184: a terminal nobody streams is closed after the idle
// timeout; a subscriber holds it open.
func TestIdleReapFRRHZ184(t *testing.T) {
	store := &events.Store{}
	h, srv := newHost(t, store, "")
	h.IdleTimeout = time.Second
	h.argv = shArgv("read x")
	held := openShell(t, srv, 80, 24)
	s := subscribe(t, srv, held)
	idle := openShell(t, srv, 80, 24)
	ti, err := h.get(idle)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ti.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("idle terminal not reaped")
	}
	if l := h.List(); len(l) != 1 || l[0].TerminalID != held {
		t.Fatalf("list %+v", l)
	}
	s.res.Body.Close()
	th, err := h.get(held)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-th.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("terminal not reaped after its last subscriber left")
	}
}
