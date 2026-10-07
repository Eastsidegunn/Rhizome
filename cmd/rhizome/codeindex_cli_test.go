package main

// RHZ-059 FR-RHZ-089: CLI index 읽기전용·검증 모드, serve -index-repo/-index-out
// 배선. 테스트 계획 C1~C3 (serve 쪽 S1~S8은 internal/workspace/codeindex_query_test.go).

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rhizome/internal/events"
)

// C1: CLI index는 journal 무접촉 — 파일·lock 미생성, 기존 journal 바이트 불변.
// CLI 쓰기 잔존 변이가 여기서 FAIL. (수용 5)
func TestIndexCLIReadOnlyFRRHZ089(t *testing.T) {
	repo, jp, outDir := fixture058(t)
	// 기존 journal이 있는 케이스: 내용이 무엇이든 CLI는 건드리면 안 된다.
	if err := os.WriteFile(jp, []byte("{\"sequence\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	code, sha := runIndex(t, repo, outDir)
	if code != 0 || sha == "" {
		t.Fatalf("exit %d", code)
	}
	after, err := os.ReadFile(jp)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("CLI index touched the journal")
	}
	entries, err := os.ReadDir(filepath.Dir(jp))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("lock file created: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, sha, "graph.json")); err != nil {
		t.Fatal(err)
	}
}

// C2: 검증 모드 — 변조된 캐시는 오류로 보고하고 조용히 덮지 않는다; 일치하면
// exit 0에 바이트 불변.
func TestIndexCLIVerifiesExistingCacheFRRHZ089(t *testing.T) {
	repo, _, outDir := fixture058(t)
	code, sha := runIndex(t, repo, outDir)
	if code != 0 {
		t.Fatal("index failed")
	}
	cache := filepath.Join(outDir, sha, "graph.json")
	// 일치 재실행: exit 0, 불변.
	b1, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := runIndex(t, repo, outDir); code != 0 {
		t.Fatal("clean rerun failed")
	}
	// 변조 후 재실행: 비0 + 변조 바이트가 그대로 남음(덮지 않음).
	tampered := []byte("{\"tampered\":true}\n")
	if err := os.WriteFile(cache, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	if code := run([]string{"index", "-repo", repo, "-out", outDir}, io.Discard, &errOut); code == 0 {
		t.Fatal("tampered cache accepted")
	}
	if !strings.Contains(errOut.String(), "mismatch") {
		t.Fatalf("mismatch not reported: %s", errOut.String())
	}
	after, err := os.ReadFile(cache)
	if err != nil || !bytes.Equal(after, tampered) {
		t.Fatal("tampered cache silently overwritten")
	}
	_ = b1
}

// C3: serve 배선 — 둘 다 설정 시 /v1/codeindex 활성, 하나만 설정 시 exit 2
// (D4 관례), 미설정 시 404로 기존 동작 불변.
func TestServeCodeIndexWiringFRRHZ089(t *testing.T) {
	repo, jp, outDir := fixture058(t)
	// ① 둘 다: 조립 레벨에서 라우트 활성.
	handler, loop := assembleServe(&events.Store{}, nil, repo, outDir, nil, io.Discard)
	if loop != nil {
		t.Fatal("loop constructed without janus configuration")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/codeindex")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
	// ② 하나만: serve가 조립 전에 설정 오류로 거부(usage 2) — lock 미생성.
	if code := run([]string{"serve", "-journal", jp, "-index-repo", repo}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("partial config exited %d, want 2", code)
	}
	if _, err := os.Stat(jp + ".lock"); !os.IsNotExist(err) {
		t.Fatal("partial-config serve acquired the journal lock")
	}
	// ③ 미설정: 기존과 동일, /v1/codeindex는 404.
	disabled, _ := assembleServe(&events.Store{}, nil, "", "", nil, io.Discard)
	srv2 := httptest.NewServer(disabled)
	defer srv2.Close()
	resp2, err := http.Get(srv2.URL + "/v1/codeindex")
	if err != nil || resp2.StatusCode != http.StatusNotFound {
		t.Fatal(resp2, err)
	}
	resp2.Body.Close()
}
