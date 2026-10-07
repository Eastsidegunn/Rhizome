package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"rhizome/internal/source"
)

// BlobGetter is the minimal seam for GET /v1/blob (RHZ-056, FR-RHZ-086).
// blob.FileStore satisfies it. The GET path itself never writes; POST
// /v1/blob is served only when the injected value also implements
// BlobPutter (RHZ-081, FR-RHZ-112).
type BlobGetter interface {
	Get(id string) ([]byte, error)
}

// blobIDPattern is invariant I5: only an exact content address may reach the
// store, so no caller-controlled segment can name a path.
var blobIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// serveBlob serves content-addressed bytes read-only (FR-RHZ-086). It appends
// no event and touches no writer boundary (I2). Error bodies are fixed
// strings: nothing but the outcome leaves the handler (I6).
func (h *HTTPServer) serveBlob(w http.ResponseWriter, r *http.Request, id string) {
	if h.Blobs == nil {
		// 미배선 = 비활성, ExecEvents nil 관례와 동일.
		http.NotFound(w, r)
		return
	}
	if !blobIDPattern.MatchString(id) {
		http.Error(w, "invalid blob id", http.StatusBadRequest)
		return
	}
	b, err := h.Blobs.Get(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		// Store-detected corruption included: a hash mismatch is "exists but
		// damaged", a server fault served loudly as 500, never as bytes (I1).
		http.Error(w, "blob unavailable", http.StatusInternalServerError)
		return
	}
	// I1: the handler re-verifies the digest itself, independent of whatever
	// store implementation is injected behind BlobGetter.
	sum := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(sum[:]) != id {
		http.Error(w, "blob integrity check failed", http.StatusInternalServerError)
		return
	}
	// I3: Content-Type comes from the registered source only — never sniffed
	// from the bytes. Unknown or unreadable source falls back to octet-stream.
	ct := "application/octet-stream"
	if ref, e := (source.Service{Store: h.Store}).Get(id); e == nil && strings.TrimSpace(ref.MediaType) != "" {
		ct = ref.MediaType
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}

// ---- RHZ-081 (FR-RHZ-112): POST /v1/blob — byte upload ----

// BlobPutter is the write seam for POST /v1/blob (RHZ-081, FR-RHZ-112). It is
// discovered by type assertion on the injected Blobs value: blob.FileStore
// satisfies both Get and Put, so the composition root needs no new wiring.
// A read-only BlobGetter (no Put) leaves the upload route disabled (404),
// and serveBlob (GET) still never touches Put.
type BlobPutter interface {
	Put(content []byte) (string, error)
}

// BlobUploadSourceURI is the fixed source_uri recorded for every upload that
// enters through POST /v1/blob. It names the surface, not the caller: the
// handler accepts no caller-supplied URI.
const BlobUploadSourceURI = "intent://blob"

// MaxBlobUploadBytes caps a single upload body at 16 MiB. This is a NEW rule
// introduced by RHZ-081: neither internal/source nor internal/blob enforces a
// size limit, so the surface enforces one (http.MaxBytesReader) and answers
// 413 above it. Exactly MaxBlobUploadBytes is accepted; cap+1 is rejected.
const MaxBlobUploadBytes = 16 << 20

// mediaTypePattern validates FORMAT only (RFC 7231 type "/" subtype as
// token/token); there is no allowlist. Parameters are stripped before the
// check by mime.ParseMediaType, which also lowercases type/subtype.
var mediaTypePattern = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+/[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)

// blobUploadResponse is the fixed wire shape of a successful upload. Field
// order is the struct order, so the bytes are deterministic for a given
// (id, mediaType, size).
type blobUploadResponse struct {
	BlobID    string `json:"blobId"`
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
}

// serveBlobUpload stores raw body bytes content-addressed and registers the
// source reference through source.Service — the existing single writer for
// source events, the same path note.create uses (FR-RHZ-112). It is a byte
// store entry, not an event write of its own: the only journal effect is the
// one source.registered event that Register appends on first sight of a
// digest. Re-upload of identical bytes returns the same id and appends
// nothing (Register short-circuits on an existing stream).
//
// No authentication: serve binds 127.0.0.1 (deployment assumption inherited
// from the rest of this handler); anything reachable here is already local.
//
// The body is never logged, echoed, or written anywhere except the blob
// store; error bodies are fixed strings (I6).
func (h *HTTPServer) serveBlobUpload(w http.ResponseWriter, r *http.Request) {
	putter, ok := h.Blobs.(BlobPutter)
	if h.Blobs == nil || !ok {
		// 미배선 또는 읽기 전용 = 비활성, GET 관례와 동일.
		http.NotFound(w, r)
		return
	}
	rawCT := strings.TrimSpace(r.Header.Get("Content-Type"))
	if rawCT == "" {
		http.Error(w, "content-type is required", http.StatusBadRequest)
		return
	}
	mt, _, err := mime.ParseMediaType(rawCT)
	if err != nil || !mediaTypePattern.MatchString(mt) {
		http.Error(w, "invalid content-type", http.StatusBadRequest)
		return
	}
	// Declared length over the cap is refused before any byte is read.
	if r.ContentLength > MaxBlobUploadBytes {
		http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBlobUploadBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	// uploadMu serializes [Put → Register] so concurrent identical uploads
	// cannot race Register's List-then-Append(0) into ErrRevisionConflict
	// (precedent: indexMu/contextMu). Validation and body read stay outside
	// the lock.
	h.uploadMu.Lock()
	defer h.uploadMu.Unlock()
	id, err := putter.Put(body)
	if err != nil {
		http.Error(w, "blob store failed", http.StatusInternalServerError)
		return
	}
	// Writer boundary: source.Service owns the source stream. The RAW
	// Content-Type header (trimmed, parameters included) is what gets
	// registered; validation above only parses, it does not normalize. The
	// media type that sticks is the one registered on first sight; a re-upload
	// with a different Content-Type does not rewrite history (append-only)
	// and the response reflects the registered reference.
	svc := source.Service{Store: h.Store}
	ref, err := svc.Register(body, rawCT, BlobUploadSourceURI)
	if err != nil {
		// Belt and braces: if a concurrent writer registered the same digest
		// between Register's List and Append, the stream now exists and the
		// outcome is identical to a re-upload.
		if existing, e := svc.Get(id); e == nil && existing.BlobID == id {
			ref, err = existing, nil
		} else {
			http.Error(w, "source registration failed", http.StatusInternalServerError)
			return
		}
	}
	if ref.BlobID != id {
		http.Error(w, "blob integrity check failed", http.StatusInternalServerError)
		return
	}
	out, err := json.Marshal(blobUploadResponse{BlobID: ref.BlobID, MediaType: ref.MediaType, Size: ref.SizeBytes})
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// 200 always: the resource is content-addressed, so "created" vs
	// "already there" is not an observable distinction and must not leak
	// through the status code (determinism, B6).
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}
