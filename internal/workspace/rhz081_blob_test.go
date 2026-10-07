package workspace

// RHZ-081 FR-RHZ-112: POST /v1/blob byte upload over source.Service.Register.
// B1~B6 per the task brief. Fixtures (blobFixture, getBlob, journalSnapshot)
// are shared with blobhttp_test.go (RHZ-056).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"rhizome/internal/events"
)

func uploadBlob(t *testing.T, h http.Handler, body []byte, contentType string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/blob", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, out
}

func decodeUpload(t *testing.T, raw []byte) blobUploadResponse {
	t.Helper()
	var res blobUploadResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return res
}

func uploadFixture(t *testing.T) (*events.Store, http.Handler) {
	t.Helper()
	s, fsStore, _ := blobFixture(t)
	h := NewHTTP(s)
	h.Blobs = fsStore
	return s, h.Handler()
}

// B1: upload → 200, id = sha256 of body, GET round-trips bytes + media type.
func TestBlobUploadRoundTripFRRHZ112(t *testing.T) {
	s, h := uploadFixture(t)
	content := []byte("hello blob\x00\x01 tail")
	code, raw := uploadBlob(t, h, content, "text/plain; charset=utf-8")
	if code != http.StatusOK {
		t.Fatalf("status %d body %q", code, raw)
	}
	res := decodeUpload(t, raw)
	sum := sha256.Sum256(content)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if res.BlobID != want {
		t.Fatalf("blobId %q want %q", res.BlobID, want)
	}
	if res.MediaType != "text/plain; charset=utf-8" || res.Size != int64(len(content)) {
		t.Fatalf("mediaType %q size %d", res.MediaType, res.Size)
	}
	if s.Len() != 1 {
		t.Fatalf("journal len %d want 1 (one source.registered)", s.Len())
	}
	gcode, ct, body := getBlob(t, h, "/v1/blob/"+res.BlobID)
	if gcode != http.StatusOK || !bytes.Equal(body, content) || ct != "text/plain; charset=utf-8" {
		t.Fatalf("GET status %d ct %q body %q", gcode, ct, body)
	}
}

// B2: identical re-upload → same id, journal unchanged (zero new events).
func TestBlobUploadIdempotentFRRHZ112(t *testing.T) {
	s, h := uploadFixture(t)
	content := []byte("same bytes twice")
	code1, raw1 := uploadBlob(t, h, content, "application/octet-stream")
	if code1 != http.StatusOK {
		t.Fatalf("first status %d", code1)
	}
	snap := journalSnapshot(t, s)
	n := s.Len()
	code2, raw2 := uploadBlob(t, h, content, "application/octet-stream")
	if code2 != http.StatusOK {
		t.Fatalf("second status %d", code2)
	}
	if decodeUpload(t, raw1).BlobID != decodeUpload(t, raw2).BlobID {
		t.Fatalf("ids differ: %q vs %q", raw1, raw2)
	}
	if s.Len() != n || journalSnapshot(t, s) != snap {
		t.Fatalf("journal changed on re-upload: len %d -> %d", n, s.Len())
	}
}

// B3: empty body / missing Content-Type / malformed Content-Type → 400,
// journal unchanged.
func TestBlobUploadBadRequestsFRRHZ112(t *testing.T) {
	s, h := uploadFixture(t)
	snap := journalSnapshot(t, s)
	cases := []struct {
		name string
		body []byte
		ct   string
	}{
		{"empty body", nil, "text/plain"},
		{"missing content-type", []byte("x"), ""},
		{"malformed content-type", []byte("x"), "not a type"},
		{"no subtype", []byte("x"), "text"},
		{"bad token chars", []byte("x"), "text/pl ain"},
	}
	for _, c := range cases {
		code, raw := uploadBlob(t, h, c.body, c.ct)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status %d body %q, want 400", c.name, code, raw)
		}
		// Error bodies are fixed strings; the request body is never echoed.
		if c.body != nil && bytes.Equal(bytes.TrimSpace(raw), c.body) {
			t.Fatalf("%s: body echoed", c.name)
		}
	}
	if journalSnapshot(t, s) != snap {
		t.Fatal("journal changed on rejected uploads")
	}
}

// B4: cap+1 bytes → 413, journal unchanged; exactly cap bytes is accepted.
func TestBlobUploadOversizeFRRHZ112(t *testing.T) {
	s, h := uploadFixture(t)
	snap := journalSnapshot(t, s)
	// The cap is a stated limit (16MiB): pin the literal so a silent raise
	// is caught.
	if MaxBlobUploadBytes != 16<<20 {
		t.Fatalf("MaxBlobUploadBytes = %d, the stated cap is 16MiB", MaxBlobUploadBytes)
	}
	big := bytes.Repeat([]byte{0x7a}, MaxBlobUploadBytes+1)
	code, raw := uploadBlob(t, h, big, "application/octet-stream")
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d body %q, want 413", code, raw)
	}
	if journalSnapshot(t, s) != snap {
		t.Fatal("journal changed on oversize upload")
	}
	// Chunked (unknown Content-Length) oversize must also be caught by the
	// reader, not only by the declared-length check.
	req := httptest.NewRequest(http.MethodPost, "/v1/blob", bytes.NewReader(big))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversize status %d, want 413", rec.Code)
	}
	if journalSnapshot(t, s) != snap {
		t.Fatal("journal changed on chunked oversize upload")
	}
	atCap := big[:MaxBlobUploadBytes]
	code, raw = uploadBlob(t, h, atCap, "application/octet-stream")
	if code != http.StatusOK {
		t.Fatalf("at-cap status %d body %q, want 200", code, raw)
	}
	if decodeUpload(t, raw).Size != MaxBlobUploadBytes {
		t.Fatalf("at-cap size %d", decodeUpload(t, raw).Size)
	}
}

// B5: different bytes → different id.
func TestBlobUploadDistinctBytesDistinctIDFRRHZ112(t *testing.T) {
	_, h := uploadFixture(t)
	_, raw1 := uploadBlob(t, h, []byte("alpha"), "text/plain")
	_, raw2 := uploadBlob(t, h, []byte("beta"), "text/plain")
	a, b := decodeUpload(t, raw1), decodeUpload(t, raw2)
	if a.BlobID == b.BlobID || a.BlobID == "" {
		t.Fatalf("ids not distinct: %q %q", a.BlobID, b.BlobID)
	}
}

// B6: determinism — response bytes and status identical across two
// identical uploads (no 200/201 split, no timestamps).
func TestBlobUploadDeterministicResponseFRRHZ112(t *testing.T) {
	_, h := uploadFixture(t)
	content := []byte("deterministic payload")
	code1, raw1 := uploadBlob(t, h, content, "text/markdown")
	code2, raw2 := uploadBlob(t, h, content, "text/markdown")
	if code1 != http.StatusOK || code2 != code1 {
		t.Fatalf("status %d / %d", code1, code2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("response bytes differ:\n%q\n%q", raw1, raw2)
	}
}

// Route guard: read-only BlobGetter (no Put) → POST disabled (404); nil → 404.
func TestBlobUploadDisabledWithoutPutterFRRHZ112(t *testing.T) {
	s := &events.Store{}
	h := NewHTTP(s)
	if code, _ := uploadBlob(t, h.Handler(), []byte("x"), "text/plain"); code != http.StatusNotFound {
		t.Fatalf("nil Blobs: status %d want 404", code)
	}
	h.Blobs = readOnlyBlobs{}
	if code, _ := uploadBlob(t, h.Handler(), []byte("x"), "text/plain"); code != http.StatusNotFound {
		t.Fatalf("read-only Blobs: status %d want 404", code)
	}
	if s.Len() != 0 {
		t.Fatalf("journal len %d want 0", s.Len())
	}
}

type readOnlyBlobs struct{}

func (readOnlyBlobs) Get(string) ([]byte, error) { return nil, nil }

// B7: 32 concurrent identical uploads → all 200, same id, journal grows by
// exactly one source.registered (uploadMu + Get fallback). Run under -race.
func TestBlobUploadConcurrentIdenticalFRRHZ112(t *testing.T) {
	s, h := uploadFixture(t)
	content := []byte("concurrent identical payload")
	const n = 32
	codes := make([]int, n)
	ids := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/blob", bytes.NewReader(content))
			req.Header.Set("Content-Type", "application/octet-stream")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
			var res blobUploadResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &res)
			ids[i] = res.BlobID
		}(i)
	}
	close(start)
	wg.Wait()
	sum := sha256.Sum256(content)
	want := "sha256:" + hex.EncodeToString(sum[:])
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK || ids[i] != want {
			t.Fatalf("upload %d: status %d id %q", i, codes[i], ids[i])
		}
	}
	if s.Len() != 1 {
		t.Fatalf("journal len %d want 1", s.Len())
	}
}
