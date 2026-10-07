package workspace

// RHZ-056 FR-RHZ-086: GET /v1/blob/{blobID} read-only content-addressed
// serving. Test plan RHZ-056 (T1~T10):
// integrity failure = 500, non-GET = 404 fall-through.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/blob"
	"rhizome/internal/events"
	"rhizome/internal/source"
)

func blobFixture(t *testing.T) (*events.Store, blob.FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		t.Fatal(err)
	}
	return &events.Store{}, blob.FileStore{Dir: blobs}, dir
}

func putAndRegister(t *testing.T, s *events.Store, fsStore blob.FileStore, content []byte, mediaType string) string {
	t.Helper()
	id, err := fsStore.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "" {
		if _, err := (source.Service{Store: s}).Register(content, mediaType, "note://"+id[7:19]); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func getBlob(t *testing.T, h http.Handler, path string) (int, string, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, rec.Result().Header.Get("Content-Type"), body
}

func journalSnapshot(t *testing.T, s *events.Store) string {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dirListing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// T1 (I1 양성 + I3): 정상 blob + source 등록 → 200, 바이트 완전 일치, source의
// media_type이 Content-Type.
func TestBlobServeRoundTripFRRHZ086I1I3(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	content := []byte("# hello\n\x00\x01binary-ish tail")
	id := putAndRegister(t, s, fsStore, content, "text/markdown")
	h := NewHTTP(s)
	h.Blobs = fsStore
	code, ct, body := getBlob(t, h.Handler(), "/v1/blob/"+id)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !reflect.DeepEqual(body, content) {
		t.Fatalf("body mismatch: got %q want %q", body, content)
	}
	if ct != "text/markdown" {
		t.Fatalf("content-type %q", ct)
	}
}

// T2 (I1): store 디렉터리의 파일을 FileStore 우회로 변조(해시 불일치) → 500,
// 변조 바이트 미노출.
func TestBlobServeCorruptFileFailStopFRRHZ086I1(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	id := putAndRegister(t, s, fsStore, []byte("original content"), "text/plain")
	tampered := []byte("TAMPERED-BYTES")
	if err := os.WriteFile(filepath.Join(fsStore.Dir, strings.TrimPrefix(id, "sha256:")), tampered, 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHTTP(s)
	h.Blobs = fsStore
	code, _, body := getBlob(t, h.Handler(), "/v1/blob/"+id)
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	if strings.Contains(string(body), string(tampered)) {
		t.Fatal("corrupt bytes leaked")
	}
}

// FakeMismatchStore는 요청 id와 해시가 다른 바이트를 돌려주는 인메모리 대역
// (T3 전용 Fake — 실제 영속 서비스 아님).
type FakeMismatchStore struct{ bytes []byte }

func (f FakeMismatchStore) Get(string) ([]byte, error) { return append([]byte(nil), f.bytes...), nil }

// T3 (I1): 핸들러 층의 재검증을 store 구현과 독립적으로 핀한다. FileStore의
// 자체 해시 검사에 기대면 이 테스트가 FAIL한다(재검증 제거 변이 probe 보증).
func TestBlobServeHandlerReverifiesFRRHZ086I1(t *testing.T) {
	s := &events.Store{}
	wrong := []byte("WRONG-BYTES-FOR-THIS-ID")
	h := NewHTTP(s)
	h.Blobs = FakeMismatchStore{bytes: wrong}
	id := "sha256:" + strings.Repeat("ab", 32)
	code, _, body := getBlob(t, h.Handler(), "/v1/blob/"+id)
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	if strings.Contains(string(body), string(wrong)) {
		t.Fatal("mismatched bytes leaked")
	}
}

// T4 (I3): Content-Type은 source에서만 — PNG 매직 바이트로 시작해도 등록된
// media_type(text/markdown)으로 서빙. sniffing으로 바꾸면 image/png가 나와
// FAIL(변이 probe 보증).
func TestBlobServeNoSniffingFRRHZ086I3(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	pngish := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("not really a png")...)
	idA := putAndRegister(t, s, fsStore, pngish, "text/markdown")
	idB := putAndRegister(t, s, fsStore, []byte("plain text pretending"), "image/png")
	h := NewHTTP(s)
	h.Blobs = fsStore
	if _, ct, _ := getBlob(t, h.Handler(), "/v1/blob/"+idA); ct != "text/markdown" {
		t.Fatalf("A content-type %q, want text/markdown (sniffing?)", ct)
	}
	if _, ct, _ := getBlob(t, h.Handler(), "/v1/blob/"+idB); ct != "image/png" {
		t.Fatalf("B content-type %q, want image/png", ct)
	}
}

// T5 (I3 폴백): (a) source 미등록 blob, (b) media_type이 빈 source 이벤트(저널
// 직접 append로 재현) → 둘 다 200 + application/octet-stream + 정확한 바이트.
func TestBlobServeFallbackOctetStreamFRRHZ086I3(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	h := NewHTTP(s)
	h.Blobs = fsStore
	// (a) blob만 있고 source 없음.
	contentA := []byte("unregistered bytes")
	idA := putAndRegister(t, s, fsStore, contentA, "")
	code, ct, body := getBlob(t, h.Handler(), "/v1/blob/"+idA)
	if code != http.StatusOK || ct != "application/octet-stream" || !reflect.DeepEqual(body, contentA) {
		t.Fatalf("(a) code=%d ct=%q", code, ct)
	}
	// (b) media_type이 빈 source.registered(서비스 검증 우회 — 레거시 재현).
	contentB := []byte("empty media type bytes")
	idB := putAndRegister(t, s, fsStore, contentB, "")
	hash := strings.TrimPrefix(idB, "sha256:")
	raw, _ := json.Marshal(map[string]any{"blob_id": idB, "content_hash": hash, "media_type": "", "size_bytes": len(contentB), "source_uri": "note://x"})
	if err := s.Append(0, events.Event{AggregateType: "source", AggregateID: idB, Revision: 1, Type: "source.registered", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	code, ct, body = getBlob(t, h.Handler(), "/v1/blob/"+idB)
	if code != http.StatusOK || ct != "application/octet-stream" || !reflect.DeepEqual(body, contentB) {
		t.Fatalf("(b) code=%d ct=%q", code, ct)
	}
}

// T6 (I4·I6): 형식은 유효하나 미존재 → 404, 에러 바디에 내부 경로 미누설.
func TestBlobServeNotFoundFRRHZ086I4(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	h := NewHTTP(s)
	h.Blobs = fsStore
	missing := "sha256:" + strings.Repeat("0", 64)
	code, _, body := getBlob(t, h.Handler(), "/v1/blob/"+missing)
	if code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", code)
	}
	if strings.Contains(string(body), fsStore.Dir) {
		t.Fatal("error body leaks store path")
	}
}

// FakeRecordingStore는 Get 호출을 기록하고 not-found를 돌려주는 인메모리 대역
// (T7 전용 Fake).
type FakeRecordingStore struct{ calls *[]string }

func (f FakeRecordingStore) Get(id string) ([]byte, error) {
	*f.calls = append(*f.calls, id)
	return nil, fs.ErrNotExist
}

// T7 (I5): 형식 위반·경로 주입 blobID는 400/404이고 store의 Get에 도달하지
// 않는다. 보강: 실제 FileStore 구성에서 디렉터리 밖 센티널이 절대 안 나온다.
func TestBlobServePathInjectionBlockedFRRHZ086I5(t *testing.T) {
	s := &events.Store{}
	calls := []string{}
	h := NewHTTP(s)
	h.Blobs = FakeRecordingStore{calls: &calls}
	bad := []string{
		"/v1/blob/sha256:../../../etc/passwd",
		"/v1/blob/sha256:..%2f..%2fsecret",
		"/v1/blob/sha256:" + strings.Repeat("a", 63),
		"/v1/blob/sha256:" + strings.Repeat("a", 65),
		"/v1/blob/sha256:" + strings.Repeat("g", 64),
		"/v1/blob/sha256:" + strings.Repeat("A", 64),
		"/v1/blob/" + strings.Repeat("a", 64),
		"/v1/blob/md5:" + strings.Repeat("a", 64),
		"/v1/blob/",
		"/v1/blob/sha256:" + strings.Repeat("a", 64) + "/extra",
	}
	for _, path := range bad {
		code, _, _ := getBlob(t, h.Handler(), path)
		if code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 400/404", path, code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("store reached with invalid ids: %v", calls)
	}
	// 센티널: blobs 디렉터리의 부모에 비밀 파일을 두고 탈출 시도 응답에 그
	// 내용이 없음을 확인.
	s2, fsStore, parent := blobFixture(t)
	sentinel := []byte("SENTINEL-SECRET-CONTENT")
	if err := os.WriteFile(filepath.Join(parent, "secret"), sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	h2 := NewHTTP(s2)
	h2.Blobs = fsStore
	for _, path := range []string{"/v1/blob/sha256:../secret", "/v1/blob/..%2fsecret", "/v1/blob/sha256:..%2fsecret"} {
		_, _, body := getBlob(t, h2.Handler(), path)
		if strings.Contains(string(body), string(sentinel)) {
			t.Fatalf("%s: sentinel escaped the blob directory", path)
		}
	}
}

// T8 (I2): 성공·404·형식위반 GET 전후로 저널(이벤트 수·내용·sequence)과 blob
// 디렉터리가 완전 불변 — 이 경로는 이벤트 0 emit.
func TestBlobServeReadOnlyJournalUnchangedFRRHZ086I2(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	id := putAndRegister(t, s, fsStore, []byte("read only bytes"), "text/plain")
	h := NewHTTP(s)
	h.Blobs = fsStore
	before := journalSnapshot(t, s)
	filesBefore := dirListing(t, fsStore.Dir)
	paths := []string{
		"/v1/blob/" + id,
		"/v1/blob/sha256:" + strings.Repeat("0", 64),
		"/v1/blob/sha256:not-hex",
	}
	for _, p := range paths {
		getBlob(t, h.Handler(), p)
	}
	if after := journalSnapshot(t, s); after != before {
		t.Fatal("journal changed by GET /v1/blob")
	}
	if filesAfter := dirListing(t, fsStore.Dir); !reflect.DeepEqual(filesAfter, filesBefore) {
		t.Fatal("blob directory changed by GET /v1/blob")
	}
}

// T9 (I2 보강): GET 외 메서드는 404 fall-through(수동 라우팅
// 관례), 저널·store 불변.
func TestBlobServeNonGETRejectedFRRHZ086I2(t *testing.T) {
	s, fsStore, _ := blobFixture(t)
	id := putAndRegister(t, s, fsStore, []byte("method test bytes"), "text/plain")
	h := NewHTTP(s)
	h.Blobs = fsStore
	before := journalSnapshot(t, s)
	filesBefore := dirListing(t, fsStore.Dir)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/blob/"+id, strings.NewReader("attempted write"))
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", method, rec.Code)
		}
	}
	if journalSnapshot(t, s) != before {
		t.Fatal("journal changed by non-GET method")
	}
	if filesAfter := dirListing(t, fsStore.Dir); !reflect.DeepEqual(filesAfter, filesBefore) {
		t.Fatal("blob directory changed by non-GET method")
	}
}

// T10a (§2 배선): Blobs 미주입(nil) = 라우트 비활성 — panic 없이 404
// (ExecEvents nil 관례와 동일).
func TestBlobServeNilStoreDisabledFRRHZ086(t *testing.T) {
	h := NewHTTP(&events.Store{})
	code, _, _ := getBlob(t, h.Handler(), "/v1/blob/sha256:"+strings.Repeat("a", 64))
	if code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", code)
	}
}

// 보조: 해시 유틸 자가 검증 — 테스트 픽스처의 id 생성이 서버 검증과 같은
// 정의를 쓰는지 고정한다(픽스처 오류로 인한 위양성 방지).
func TestBlobFixtureHashAgreesFRRHZ086(t *testing.T) {
	content := []byte("hash agreement")
	sum := sha256.Sum256(content)
	want := "sha256:" + hex.EncodeToString(sum[:])
	_, fsStore, _ := blobFixture(t)
	got, err := fsStore.Put(content)
	if err != nil || got != want {
		t.Fatal(fmt.Errorf("put id %q want %q err %v", got, want, err))
	}
}
