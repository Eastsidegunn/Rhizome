package main

// RHZ-107 G1 (FR-RHZ-135): the serve composition root creates exactly one
// secret writer and hands that pointer to the HTTP relay and GET
// /v1/secrets (the secret channel will share the same pointer).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/secretref"
	"rhizome/internal/workspace"
)

func TestAssembleServeSharesSecretWriterFRRHZ135(t *testing.T) {
	var got []*secretref.Service
	old := assembleServeInspect
	assembleServeInspect = func(h *workspace.HTTPServer) { got = append(got, h.Secrets) }
	t.Cleanup(func() { assembleServeInspect = old })
	store := &events.Store{}
	svc := secretref.New(store)
	assembleServeShared(store, svc, nil, "", "", nil, nil, io.Discard, false)
	if len(got) != 1 || got[0] != svc {
		t.Fatalf("HTTP server got %v, want the given writer", got)
	}
	got = nil
	assembleServe(store, nil, "", "", nil, io.Discard, false)
	if len(got) != 1 || got[0] == nil {
		t.Fatal("assembleServe left the secret writer unwired")
	}
}

func TestServeWiresSecretIntentsFRRHZ135(t *testing.T) {
	var mu sync.Mutex
	var wired []*secretref.Service
	old := assembleServeInspect
	assembleServeInspect = func(h *workspace.HTTPServer) { mu.Lock(); wired = append(wired, h.Secrets); mu.Unlock() }
	t.Cleanup(func() { assembleServeInspect = old })
	jp := filepath.Join(t.TempDir(), "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, done, eout := serveWithURL(t, ctx, jp, 0, nil)
	op := fmt.Sprintf("op-%032x", 1)
	body := `{"kind":"secret.register.request","actor":"op","opId":"` + op + `","name":"alpha","refKind":"other"}`
	res, err := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.HasPrefix(string(b), `{"Accepted":true,"Reason":"","opId":"`+op+`","ref":"sref-`) {
		t.Fatalf("intent: %q", b)
	}
	res, err = http.Get(url + "/v1/secrets")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(string(b), `{"revision":1,"body":{"secrets":[{"ref":"sref-`) {
		t.Fatalf("/v1/secrets: %d %q", res.StatusCode, b)
	}
	cancel()
	waitDone(t, done, eout, 10*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(wired) != 1 || wired[0] == nil {
		t.Fatalf("serve wired %d HTTP servers / writer %v", len(wired), wired)
	}
}

// TestServeOutputZVFRRHZ135 is §8.1 target ②: rejected secret intents
// carrying a crypto/rand sentinel leave no sentinel form on serve's stdout
// or stderr.
func TestServeOutputZVFRRHZ135(t *testing.T) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	s := "rhzsntl-" + hex.EncodeToString(raw)
	jp := filepath.Join(t.TempDir(), "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- serveCtx(ctx, []string{"-journal", jp, "-addr", freeAddr(t, addrCh)}, &stdout, &stderr)
	}()
	url := "http://" + <-addrCh
	waitLock(t, jp+".lock", true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if res, err := http.Get(url + "/v1/workspace"); err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server not reachable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	op := fmt.Sprintf("op-%032x", 1)
	reg := `"kind":"secret.register.request","actor":"op","opId":"` + op + `","name":"n","refKind":"other"`
	esc := ""
	for _, c := range s {
		esc += fmt.Sprintf("\\u%04x", c)
	}
	for _, body := range []string{
		`{` + reg + `,"value":"` + s + `"}`,
		`{` + reg + `,"secretValue":"` + s + `"}`,
		`{"kind":"secret.register.request","actor":"op","opId":"` + op + `","name":"` + s + `","name":"n","refKind":"other"}`,
		`{"kind":"secret.rotate.request","actor":"op","opId":"` + op + `","ref":"` + s + `"}`,
		"{" + reg + `,"value":"` + s + "\xff\xfe" + `"}`,
		`{` + reg + `,"value":"` + esc + `"}`,
		`{"kind":"secret.op.outcome","value":"` + s + `"}`,
	} {
		res, err := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if strings.Contains(string(b), `"Accepted":true`) {
			t.Fatalf("accepted: %q", b)
		}
	}
	cancel()
	waitDone(t, done, &stderr, 10*time.Second)
	sum := sha256.Sum256([]byte(s))
	forms := []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
	for name, out := range map[string][]byte{"stdout": stdout.Bytes(), "stderr": stderr.Bytes()} {
		for _, f := range forms {
			if bytes.Contains(out, []byte(f)) {
				t.Fatalf("serve %s contains a sentinel form", name)
			}
		}
	}
}
