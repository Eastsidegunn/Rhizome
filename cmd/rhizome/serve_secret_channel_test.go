package main

// RHZ-107 G1 S4 (FR-RHZ-137): serve flags and wiring of the secret channel.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/secretchannel"
	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
	"rhizome/internal/secretwiretest"
	"rhizome/internal/workspace"
)

func TestServeSecretPlaneFlagsFRRHZ137(t *testing.T) {
	s := secretSentinel(t)
	for _, args := range [][]string{
		{"-secret-plane-interval=" + s},
		{"-secret-plane-interval=199ms"},
		{"-secret-plane-interval=-1s"},
		{"-secret-plane-socket=rel/" + s},
		{"-secret-plane-socket=/tmp/../" + s},
		{"-secret-plane-id=" + s},
		{"-secret-plane-id=pl-" + s},
	} {
		var errOut, out bytes.Buffer
		if code := serveCtx(context.Background(), args, &out, &errOut); code != 2 {
			t.Fatalf("exit %d", code)
		}
		if got := errOut.String(); got != errSecretPlaneFlag+"\n" || out.Len() != 0 {
			t.Fatalf("stderr %q", got)
		}
		assertNoSecretSentinel(t, "serve stderr", errOut.Bytes(), s)
	}
	if c, err := secretChannelFromFlags("", "2s", ""); c != nil || err != nil {
		t.Fatal("empty socket must turn the channel off")
	}
	c, err := secretChannelFromFlags("/x/p.sock", "200ms", fmt.Sprintf("pl-%032x", 3))
	if err != nil || c.Interval != secretchannel.MinInterval || c.SocketPath != "/x/p.sock" || c.ExpectedPlaneID != fmt.Sprintf("pl-%032x", 3) {
		t.Fatalf("config %+v %v", c, err)
	}
}

// serveArgs runs serveCtx with extra flags and returns its URL.
func serveArgs(t *testing.T, ctx context.Context, jp string, extra ...string) (string, <-chan int, *bytes.Buffer) {
	t.Helper()
	addrCh := make(chan string, 1)
	var eout bytes.Buffer
	done := make(chan int, 1)
	go func() {
		args := append([]string{"-journal", jp, "-addr", freeAddr(t, addrCh)}, extra...)
		done <- serveCtx(ctx, args, io.Discard, &eout)
	}()
	addr := <-addrCh
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

func postSecretIntent(t *testing.T, url, body string) string {
	t.Helper()
	res, err := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

type secretsView struct {
	Revision uint64 `json:"revision"`
	Body     struct {
		Secrets []struct {
			Ref, State string
			Version    int64
		} `json:"secrets"`
	} `json:"body"`
}

func getSecrets(t *testing.T, url string) secretsView {
	t.Helper()
	res, err := http.Get(url + "/v1/secrets")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var v secretsView
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil || res.StatusCode != 200 {
		t.Fatalf("/v1/secrets %d %v", res.StatusCode, err)
	}
	return v
}

// No socket: the channel is off, requests are accepted and stay pending.
func TestServeSecretChannelOffFRRHZ137(t *testing.T) {
	old := secretChannelInspect
	called := false
	secretChannelInspect = func(secretchannel.Config) { called = true }
	t.Cleanup(func() { secretChannelInspect = old })
	jp := filepath.Join(t.TempDir(), "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, done, eout := serveArgs(t, ctx, jp)
	op := fmt.Sprintf("op-%032x", 1)
	if r := postSecretIntent(t, url, `{"kind":"secret.register.request","actor":"op","opId":"`+op+`","name":"alpha","refKind":"other"}`); !strings.Contains(r, `"Accepted":true`) {
		t.Fatalf("intent %q", r)
	}
	time.Sleep(300 * time.Millisecond)
	v := getSecrets(t, url)
	if len(v.Body.Secrets) != 1 || v.Body.Secrets[0].State != secretref.StatePendingRegistration {
		t.Fatalf("secrets %+v", v)
	}
	cancel()
	waitDone(t, done, eout, 10*time.Second)
	if called || strings.Contains(eout.String(), "secret channel") {
		t.Fatal("channel started without a socket")
	}
}

// sseEvents streams the data revision of each SSE event; raw keeps every
// byte read from the stream (ZV evidence).
func sseEvents(t *testing.T, url string, raw *lockedBuf) (<-chan uint64, func()) {
	t.Helper()
	res, err := http.Get(url + "/v1/workspace/stream")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan uint64, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			line := sc.Text()
			raw.write(line + "\n")
			if strings.HasPrefix(line, "data: ") {
				var env struct {
					Revision uint64 `json:"revision"`
				}
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env)
				ch <- env.Revision
			}
		}
	}()
	return ch, func() { res.Body.Close() }
}

func nextRevision(t *testing.T, ch <-chan uint64, above uint64) uint64 {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				t.Fatal("stream closed")
			}
			if r > above {
				return r
			}
		case <-deadline:
			t.Fatal("no SSE push")
		}
	}
}

// End to end with the fake plane: HTTP register → plane outcome → active(1)
// on /v1/secrets and an SSE push from the channel (W12), also for an
// E7-only batch; the channel and the HTTP relay share one writer; serve
// stderr carries only fixed reports (ZV).
func TestServeSecretChannelEndToEndFRRHZ137(t *testing.T) {
	s := secretSentinel(t)
	P := fmt.Sprintf("pl-%032x", 1)
	fake, err := secretwiretest.Start(P)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	var mu sync.Mutex
	var httpSvc, chanSvc []*secretref.Service
	oldA, oldC := assembleServeInspect, secretChannelInspect
	assembleServeInspect = func(h *workspace.HTTPServer) { mu.Lock(); httpSvc = append(httpSvc, h.Secrets); mu.Unlock() }
	secretChannelInspect = func(c secretchannel.Config) {
		mu.Lock()
		chanSvc = append(chanSvc, c.Service)
		mu.Unlock()
		if c.Latch == nil || c.OnAppend == nil || c.OnError == nil || c.ExpectedPlaneID != P {
			t.Error("channel config incomplete")
		}
	}
	t.Cleanup(func() { assembleServeInspect, secretChannelInspect = oldA, oldC })

	jp := filepath.Join(t.TempDir(), "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, done, eout := serveArgs(t, ctx, jp, "-secret-plane-socket", fake.SocketPath(),
		"-secret-plane-interval", "200ms", "-secret-plane-id", P)

	op := fmt.Sprintf("op-%032x", 1)
	if r := postSecretIntent(t, url, `{"kind":"secret.register.request","actor":"op","opId":"`+op+`","name":"alpha","refKind":"other"}`); !strings.Contains(r, `"Accepted":true`) {
		t.Fatalf("intent %q", r)
	}
	var sseRaw lockedBuf
	events, stop := sseEvents(t, url, &sseRaw)
	defer stop()
	rev := nextRevision(t, events, 0) // snapshot
	fake.Append(secretwiretest.OutcomeEntry(0, op, secretwire.DeriveRef(op), 1, secretwire.ResultOK))
	rev = nextRevision(t, events, rev) // pushed by the channel after the E4
	v := getSecrets(t, url)
	if len(v.Body.Secrets) != 1 || v.Body.Secrets[0].State != secretref.StateActive || v.Body.Secrets[0].Version != 1 {
		t.Fatalf("secrets %+v", v)
	}

	// E7-only batch: an outcome for an unknown op is rejected and recorded.
	fake.Append(secretwiretest.OutcomeEntry(0, fmt.Sprintf("op-%032x", 9), secretwire.DeriveRef(fmt.Sprintf("op-%032x", 9)), 1, secretwire.ResultOK))
	nextRevision(t, events, rev)

	// A sentinel-carrying fault frame: refused with a fixed report.
	fake.SetBatchFault(secretwiretest.FaultUnknownValue, s)
	if !fake.WaitHandled(fake.Connections()+2, 10*time.Second) {
		t.Fatal("no faulted cycle")
	}
	for _, path := range []string{"/v1/secrets", "/v1/workspace"} {
		res, err := http.Get(url + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		assertNoSecretSentinel(t, path, b, s)
	}
	stop()
	assertNoSecretSentinel(t, "workspace SSE", []byte(sseRaw.String()), s)
	cancel()
	waitDone(t, done, eout, 10*time.Second)

	mu.Lock()
	if len(httpSvc) != 1 || len(chanSvc) != 1 || httpSvc[0] == nil || httpSvc[0] != chanSvc[0] {
		t.Fatalf("writer not shared: http %v channel %v", httpSvc, chanSvc)
	}
	mu.Unlock()
	errText := eout.String()
	if !strings.Contains(errText, "secret channel: entry-rejected:unknown-op\n") ||
		!strings.Contains(errText, "secret channel: frame-rejected:unknown-field\n") {
		t.Fatalf("stderr %q", errText)
	}
	for _, line := range strings.Split(strings.TrimSpace(errText), "\n") {
		if strings.HasPrefix(line, "secret channel: ") && !strings.HasPrefix(line, "secret channel: entry-rejected:") &&
			!strings.HasPrefix(line, "secret channel: frame-rejected:") && line != "secret channel: stall-cleared" {
			t.Fatalf("unexpected report %q", line)
		}
	}
	assertNoSecretSentinel(t, "serve stderr", []byte(errText), s)
	jb, _ := os.ReadFile(jp)
	assertNoSecretSentinel(t, "journal", jb, s)
	for _, f := range fake.Received() {
		assertNoSecretSentinel(t, "rhizome frame", f, s)
	}
}

func secretSentinel(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(raw)
}

// assertNoSecretSentinel is the §8.1 ZV check over data.
func assertNoSecretSentinel(t *testing.T, where string, data []byte, s string) {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	for _, f := range []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]} {
		if strings.Contains(string(data), f) {
			t.Fatalf("%s contains a sentinel form", where)
		}
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) write(s string) { l.mu.Lock(); l.b.WriteString(s); l.mu.Unlock() }

func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A failed bind returns before any background loop starts: the channel never
// dials and the journal is untouched.
func TestServeSecretChannelBindFailureFRRHZ137(t *testing.T) {
	fake, err := secretwiretest.Start(fmt.Sprintf("pl-%032x", 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	fake.Append(secretwiretest.ReconciledEntry(0, fmt.Sprintf("op-%032x", 1), "alpha"))
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	jp := filepath.Join(t.TempDir(), "j.ndjson")
	if err := os.WriteFile(jp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	code := serveCtx(context.Background(), []string{"-journal", jp, "-addr", busy.Addr().String(),
		"-secret-plane-socket", fake.SocketPath(), "-secret-plane-interval", "200ms"}, io.Discard, &errOut)
	if code == 0 {
		t.Fatal("serve succeeded on an occupied address")
	}
	time.Sleep(400 * time.Millisecond)
	if n := fake.Connections(); n != 0 {
		t.Fatalf("channel dialled %d times", n)
	}
	if b, err := os.ReadFile(jp); err != nil || len(b) != 0 {
		t.Fatalf("journal changed: %d bytes %v", len(b), err)
	}
}
