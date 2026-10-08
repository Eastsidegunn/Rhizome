package workspace

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"rhizome/internal/events"
)

type poisonPortFRRHZ144 struct {
	store    *events.Store
	failNext atomic.Bool
	poisoned atomic.Bool
}

func (p *poisonPortFRRHZ144) Append(expected uint64, event events.Event) error {
	if p.poisoned.Load() {
		return events.ErrPoisoned
	}
	if p.failNext.CompareAndSwap(true, false) {
		p.poisoned.Store(true)
		return events.ErrPoisoned
	}
	return p.store.Append(expected, event)
}

func (p *poisonPortFRRHZ144) List(aggregateType, aggregateID string) []events.Event {
	return p.store.List(aggregateType, aggregateID)
}

func (p *poisonPortFRRHZ144) All() []events.Event { return p.store.All() }
func (p *poisonPortFRRHZ144) Poisoned() bool      { return p.poisoned.Load() }

type countingBlobsFRRHZ144 struct{ puts atomic.Int32 }

func (b *countingBlobsFRRHZ144) Get(string) ([]byte, error) { return nil, fs.ErrNotExist }
func (b *countingBlobsFRRHZ144) Put([]byte) (string, error) {
	b.puts.Add(1)
	return "", nil
}

func assertPoisonResponseFRRHZ144(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Body.String(); got != events.ErrPoisoned.Error() {
		t.Fatalf("body = %q", got)
	}
}

// FR-RHZ-144: the blob request passes the preflight check, then its own
// source.Register append poisons the journal. The post-error check at the
// writer boundary must translate that failure to the fixed poison response.
func TestBlobUploadSelfPoisonFRRHZ144(t *testing.T) {
	base := &events.Store{}
	p := &poisonPortFRRHZ144{store: base}
	blobs := &countingBlobsFRRHZ144{}
	h := NewHTTP(p)
	h.Blobs = blobs
	p.failNext.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/v1/blob", bytes.NewReader([]byte("poison on register")))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)

	assertPoisonResponseFRRHZ144(t, rec)
	if !p.Poisoned() {
		t.Fatal("poison latch is not set after source.Register append failure")
	}
	if blobs.puts.Load() != 1 {
		t.Fatalf("blob Put calls = %d, want 1", blobs.puts.Load())
	}
	if base.Len() != 0 {
		t.Fatalf("journal len = %d, want 0", base.Len())
	}
}

// FR-RHZ-144: fixture064 includes task knowledge, so this request reaches its
// own trace Create append and poisons there rather than returning an empty,
// read-only context bundle.
func TestContextTraceSelfPoisonFRRHZ144(t *testing.T) {
	base := fixture064(t)
	p := &poisonPortFRRHZ144{store: base}
	h := NewHTTP(p)
	before := len(traceEvents064(base))
	p.failNext.Store(true)

	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/context?task=mission-x", nil))

	assertPoisonResponseFRRHZ144(t, rec)
	if !p.Poisoned() {
		t.Fatal("poison latch is not set after trace Create append failure")
	}
	if got := len(traceEvents064(base)); got != before {
		t.Fatalf("trace events = %d, want %d", got, before)
	}
}

// FR-RHZ-144: a fresh code-index request reaches its own main.advanced
// append, poisons the journal, and is converted by the post-error branch.
func TestCodeIndexSelfPoisonFRRHZ144(t *testing.T) {
	base, h, _, _ := fixture059(t)
	p := &poisonPortFRRHZ144{store: base}
	h.Store = p
	p.failNext.Store(true)

	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/codeindex", nil))

	assertPoisonResponseFRRHZ144(t, rec)
	if !p.Poisoned() {
		t.Fatal("poison latch is not set after main.advanced append failure")
	}
	if got := len(base.List("repo", "main")); got != 0 {
		t.Fatalf("main.advanced events = %d, want 0", got)
	}
}

// FR-RHZ-144: a non-secret mission writer poisons during dispatch. The relay
// post-check converts its ordinary Reason path to 503, and every specified
// write-capable surface then stops before its non-journal side effects while
// the pure workspace projection remains readable.
func TestHTTPJournalPoisonFailStopFRRHZ144(t *testing.T) {
	base := fixture064(t)
	p := &poisonPortFRRHZ144{store: base}
	p.failNext.Store(true)
	blobs := &countingBlobsFRRHZ144{}

	repo := t.TempDir()
	runGit059(t, repo, "init", "-q", "-b", "main")
	write059(t, repo, "go.mod", "module poisonfixture\n\ngo 1.23\n")
	write059(t, repo, "main.go", "package poisonfixture\n")
	commit059(t, repo, "init")
	indexOut := filepath.Join(t.TempDir(), "index")

	h := NewHTTP(p)
	h.Blobs = blobs
	h.IndexRepo, h.IndexOut = repo, indexOut
	handler := h.Handler()
	before := len(base.All())

	// mission.Service returns an append error that RelayIntentHooks would
	// normally expose as HTTP-200 RelayResult.Reason. Its post-check must win.
	intent := httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(`{"kind":"mission.create","name":"poison","prompt":"done","actor":"operator"}`))
	intentRec := httptest.NewRecorder()
	handler.ServeHTTP(intentRec, intent)
	assertPoisonResponseFRRHZ144(t, intentRec)
	if !p.Poisoned() || len(base.All()) != before {
		t.Fatalf("poisoned=%v events=%d want %d", p.Poisoned(), len(base.All()), before)
	}

	// A later intent is rejected by the pre-dispatch check with zero writes.
	later := httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(`{"kind":"mission.create","name":"later","prompt":"done"}`))
	laterRec := httptest.NewRecorder()
	handler.ServeHTTP(laterRec, later)
	assertPoisonResponseFRRHZ144(t, laterRec)

	blobReq := httptest.NewRequest(http.MethodPost, "/v1/blob", bytes.NewReader([]byte("must not persist")))
	blobReq.Header.Set("Content-Type", "text/plain")
	blobRec := httptest.NewRecorder()
	handler.ServeHTTP(blobRec, blobReq)
	assertPoisonResponseFRRHZ144(t, blobRec)
	if blobs.puts.Load() != 0 {
		t.Fatalf("blob Put calls = %d", blobs.puts.Load())
	}

	traces := len(traceEvents064(base))
	contextRec := httptest.NewRecorder()
	handler.ServeHTTP(contextRec, httptest.NewRequest(http.MethodGet, "/v1/context?task=mission-x", nil))
	assertPoisonResponseFRRHZ144(t, contextRec)
	if got := len(traceEvents064(base)); got != traces {
		t.Fatalf("trace events = %d, want %d", got, traces)
	}

	codeIndexRec := httptest.NewRecorder()
	handler.ServeHTTP(codeIndexRec, httptest.NewRequest(http.MethodGet, "/v1/codeindex", nil))
	assertPoisonResponseFRRHZ144(t, codeIndexRec)
	if _, err := os.Stat(indexOut); !os.IsNotExist(err) {
		t.Fatalf("code index cache side effect: %v", err)
	}

	workspaceRec := httptest.NewRecorder()
	handler.ServeHTTP(workspaceRec, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	if workspaceRec.Code != http.StatusOK {
		t.Fatalf("workspace status = %d, body = %q", workspaceRec.Code, workspaceRec.Body.String())
	}
	if len(base.All()) != before {
		t.Fatalf("memory store changed: %d -> %d", before, len(base.All()))
	}
}
