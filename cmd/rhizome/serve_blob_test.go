package main

// RHZ-056 FR-RHZ-086 T10: serve 배선 — -blobs 플래그가 읽기전용 blob 핸들로
// HTTPServer에 전달되는지, 미지정이면 라우트가 비활성(404)인지.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"rhizome/internal/blob"
	"rhizome/internal/events"
	"rhizome/internal/source"
)

// T10b-1: 플래그 → 핸들 변환. 빈 플래그 = nil(비활성), 지정 = 그 디렉터리의
// FileStore.
func TestBlobStoreFromFlagFRRHZ086(t *testing.T) {
	if got := blobStoreFromFlag(""); got != nil {
		t.Fatalf("empty flag must disable the route, got %#v", got)
	}
	if got := blobStoreFromFlag("  "); got != nil {
		t.Fatalf("blank flag must disable the route, got %#v", got)
	}
	fsStore, ok := blobStoreFromFlag("/some/dir").(blob.FileStore)
	if !ok || fsStore.Dir != "/some/dir" {
		t.Fatalf("flag not wired to FileStore: %#v ok=%v", fsStore, ok)
	}
}

// T10b-2 (행동 검증): assembleServe에 주입된 store로 /v1/blob이 실제 서빙되고
// (바이트·Content-Type), nil 주입이면 같은 요청이 404.
func TestAssembleServeBlobRouteFRRHZ086(t *testing.T) {
	dir := t.TempDir()
	fsStore := blob.FileStore{Dir: dir}
	content := []byte("wired bytes through serve assembly")
	id, err := fsStore.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	s := &events.Store{}
	if _, err := (source.Service{Store: s}).Register(content, "text/plain", "note://wired"); err != nil {
		t.Fatal(err)
	}
	handler, loop := assembleServe(s, fsStore, "", "", nil, io.Discard)
	if loop != nil {
		t.Fatal("loop constructed without janus configuration")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/blob/" + id)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !reflect.DeepEqual(body, content) {
		t.Fatalf("body mismatch: %q err=%v", body, err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("content-type %q", ct)
	}
	disabled, _ := assembleServe(s, nil, "", "", nil, io.Discard)
	srv2 := httptest.NewServer(disabled)
	defer srv2.Close()
	resp2, err := http.Get(srv2.URL + "/v1/blob/" + id)
	if err != nil || resp2.StatusCode != http.StatusNotFound {
		t.Fatal(resp2, err)
	}
	resp2.Body.Close()
}
