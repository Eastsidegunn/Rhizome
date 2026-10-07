package workspace

// RHZ-059 FR-RHZ-089: GET /v1/codeindex — 조회 시점 생성, serve writer 경계
// emit. 테스트 계획 RHZ-059 test plan S1~S8.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"rhizome/internal/events"
)

func runGit059(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write059(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func commit059(t *testing.T, dir, msg string) {
	t.Helper()
	runGit059(t, dir, "add", "-A")
	runGit059(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

func fixture059(t *testing.T) (s *events.Store, h *HTTPServer, repo, outDir string) {
	t.Helper()
	repo = t.TempDir()
	runGit059(t, repo, "init", "-q", "-b", "main")
	write059(t, repo, "go.mod", "module m\n\ngo 1.23\n")
	write059(t, repo, "a/a.go", "package a\n\nimport _ \"m/b\"\n")
	write059(t, repo, "b/b.go", "package b\n")
	commit059(t, repo, "init")
	s = &events.Store{}
	h = NewHTTP(s)
	outDir = filepath.Join(t.TempDir(), "index")
	h.IndexRepo, h.IndexOut = repo, outDir
	return s, h, repo, outDir
}

func getCodeIndex(t *testing.T, h *HTTPServer) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/codeindex", nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	b, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, b
}

func journal059(t *testing.T, s *events.Store) string {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// S1: 미배선 = 비활성 — 404, journal 불변.
func TestCodeIndexDisabledWithoutWiringFRRHZ089(t *testing.T) {
	s := &events.Store{}
	h := NewHTTP(s)
	before := journal059(t, s)
	req := httptest.NewRequest(http.MethodGet, "/v1/codeindex", nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if journal059(t, s) != before {
		t.Fatal("journal changed")
	}
}

// S2: 첫 조회 = 색인 + serve writer emit 정확 1건. aggregate_type 전수 검사라
// 그래프(node/edge) 이벤트 적재 변이는 FAIL. (수용 1·4)
func TestCodeIndexFirstQueryEmitsOneFactFRRHZ089(t *testing.T) {
	s, h, repo, outDir := fixture059(t)
	n := len(s.All())
	code, body := getCodeIndex(t, h)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	sha := runGit059(t, repo, "rev-parse", "refs/heads/main")
	if !strings.Contains(string(body), `"buildSha": "`+sha+`"`) {
		t.Fatalf("buildSha missing: %s", body)
	}
	if _, err := os.Stat(filepath.Join(outDir, sha, "graph.json")); err != nil {
		t.Fatal(err)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("journal grew %d, want exactly 1", len(all)-n)
	}
	e := all[len(all)-1]
	if e.AggregateType != "repo" || e.AggregateID != "main" || e.Type != "main.advanced" || e.Revision != 1 {
		t.Fatalf("event %+v", e)
	}
	var p struct{ Sha, At string }
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.Sha != sha || p.At == "" {
		t.Fatalf("payload %s err=%v", e.Payload, err)
	}
	for _, e := range all[n:] {
		if e.AggregateType != "repo" {
			t.Fatalf("graph-ish event leaked: %s/%s", e.AggregateType, e.Type)
		}
	}
}

// S3: 같은 sha 재조회 = 이벤트 0, 응답 바이트 동일. (수용 2)
func TestCodeIndexSameShaNoNewEventsFRRHZ089(t *testing.T) {
	s, h, _, _ := fixture059(t)
	code, b1 := getCodeIndex(t, h)
	if code != http.StatusOK {
		t.Fatal("first query failed")
	}
	before := journal059(t, s)
	for i := 0; i < 2; i++ {
		code, b := getCodeIndex(t, h)
		if code != http.StatusOK || !bytes.Equal(b, b1) {
			t.Fatalf("requery %d: code=%d bytes-equal=%v", i, code, bytes.Equal(b, b1))
		}
	}
	if journal059(t, s) != before {
		t.Fatal("requery appended events")
	}
}

// S4: serve 가동 중 main 전진 → 새 sha 색인 + 2건째 정확 1건. 캐시 양쪽 보존.
// (수용 1)
func TestCodeIndexMainAdvanceWhileServingFRRHZ089(t *testing.T) {
	s, h, repo, outDir := fixture059(t)
	if code, _ := getCodeIndex(t, h); code != http.StatusOK {
		t.Fatal("first query failed")
	}
	oldSha := runGit059(t, repo, "rev-parse", "refs/heads/main")
	write059(t, repo, "c/c.go", "package c\n")
	commit059(t, repo, "advance")
	newSha := runGit059(t, repo, "rev-parse", "refs/heads/main")
	n := len(s.All())
	code, body := getCodeIndex(t, h)
	if code != http.StatusOK || !strings.Contains(string(body), `"buildSha": "`+newSha+`"`) {
		t.Fatalf("code=%d body=%s", code, body)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("advance appended %d, want 1", len(all)-n)
	}
	last := all[len(all)-1]
	var p struct{ Sha string }
	if last.Revision != 2 || json.Unmarshal(last.Payload, &p) != nil || p.Sha != newSha {
		t.Fatalf("second fact %+v payload=%s", last, last.Payload)
	}
	for _, sha := range []string{oldSha, newSha} {
		if _, err := os.Stat(filepath.Join(outDir, sha, "graph.json")); err != nil {
			t.Fatalf("cache for %s: %v", sha, err)
		}
	}
}

// S5: 캐시 삭제 후 재조회 = 바이트 동일 + 이벤트 0 (재생성은 재emit이 아님).
// (수용 2·3)
func TestCodeIndexCacheLossRegeneratesSameBytesFRRHZ089(t *testing.T) {
	s, h, _, outDir := fixture059(t)
	code, b1 := getCodeIndex(t, h)
	if code != http.StatusOK {
		t.Fatal("first query failed")
	}
	before := journal059(t, s)
	if err := os.RemoveAll(outDir); err != nil {
		t.Fatal(err)
	}
	code, b2 := getCodeIndex(t, h)
	if code != http.StatusOK || !bytes.Equal(b1, b2) {
		t.Fatal("regenerated bytes differ")
	}
	if journal059(t, s) != before {
		t.Fatal("regeneration re-emitted")
	}
}

// S6: fresh sha에 동시 조회 8개 → emit 정확 1건 + **8개 응답 바이트 전부
// 동일**(torn 캐시 읽기 검출 — 원자적 tmp+rename 쓰기가 없으면 콜드 캐시
// 경합에서 잘린 JSON이 응답될 수 있다). 뮤텍스 1차·revision 가드 2차,
// -race가 경합을 함께 핀.
func TestCodeIndexConcurrentQueriesEmitOnceFRRHZ089(t *testing.T) {
	s, h, _, _ := fixture059(t)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	n := len(s.All())
	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		code int
		body []byte
		err  error
	}
	results := make([]result, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Get(srv.URL + "/v1/codeindex")
			if err != nil {
				results[i] = result{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			results[i] = result{code: resp.StatusCode, body: body, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, r := range results {
		if r.err != nil || r.code != http.StatusOK {
			t.Fatalf("request %d: code=%d err=%v", i, r.code, r.err)
		}
		if !bytes.Equal(r.body, results[0].body) {
			t.Fatalf("request %d returned different bytes (torn cache read?): %d vs %d bytes", i, len(r.body), len(results[0].body))
		}
	}
	if got := len(s.All()) - n; got != 1 {
		t.Fatalf("concurrent queries emitted %d facts, want 1", got)
	}
}

// S7: main.advanced가 있는 store에서 기존 표면 회귀 없음 — Snapshot은 repo
// aggregate를 무시한다.
func TestCodeIndexExistingSurfacesUnaffectedFRRHZ089(t *testing.T) {
	_, h, _, _ := fixture059(t)
	if code, _ := getCodeIndex(t, h); code != http.StatusOK {
		t.Fatal("index query failed")
	}
	for _, path := range []string{"/v1/workspace", "/v1/knowledge"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
}

// S8: 오류 경로 fail-stop — git 아닌 repo는 5xx·journal 불변, 손상 커밋은
// 5xx·emit 0·캐시 미생성 (serve-side 부분 산출물 0).
func TestCodeIndexFailStopFRRHZ089(t *testing.T) {
	// (a) git 저장소가 아님.
	s := &events.Store{}
	h := NewHTTP(s)
	h.IndexRepo, h.IndexOut = t.TempDir(), filepath.Join(t.TempDir(), "index")
	before := journal059(t, s)
	code, _ := getCodeIndex(t, h)
	if code != http.StatusInternalServerError {
		t.Fatalf("(a) status %d", code)
	}
	if journal059(t, s) != before {
		t.Fatal("(a) journal changed")
	}
	// (b) main에 손상 .go 커밋.
	s2, h2, repo, outDir := fixture059(t)
	write059(t, repo, "a/broken.go", "package a\n\nfunc {\n")
	commit059(t, repo, "break")
	sha := runGit059(t, repo, "rev-parse", "refs/heads/main")
	before2 := journal059(t, s2)
	code, _ = getCodeIndex(t, h2)
	if code != http.StatusInternalServerError {
		t.Fatalf("(b) status %d", code)
	}
	if journal059(t, s2) != before2 {
		t.Fatal("(b) broken tree emitted")
	}
	if _, err := os.Stat(filepath.Join(outDir, sha)); !os.IsNotExist(err) {
		t.Fatal("(b) partial cache written")
	}
}
