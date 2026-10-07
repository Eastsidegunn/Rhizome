package main

// RHZ-058 FR-RHZ-088 — RHZ-059(FR-RHZ-089)에서 개정: CLI index는 읽기 전용이
// 되어 journal 단언이 반전됐다(쓰기 존재→부재, 약화 아님). CLI의 journal emit
// 커버리지("전진→1건"·"같은 sha→0")는 serve 쪽 rhz059 테스트(S2~S6)로 완전
// 이전. 유지: 캐시 바이트 결정론·브랜치 미색인·fail-stop·usage·graph 조회.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/codeindex"
	"rhizome/internal/events"
	"rhizome/internal/journal"
)

func runGit058(t *testing.T, dir string, args ...string) string {
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

func write058(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func commit058(t *testing.T, dir, msg string) {
	t.Helper()
	runGit058(t, dir, "add", "-A")
	runGit058(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

// repo(모듈 m: a→b) + 작업 디렉터리의 journal 경로·out 디렉터리.
func fixture058(t *testing.T) (repo, jp, outDir string) {
	t.Helper()
	repo = t.TempDir()
	runGit058(t, repo, "init", "-q", "-b", "main")
	write058(t, repo, "go.mod", "module m\n\ngo 1.23\n")
	write058(t, repo, "a/a.go", "package a\n\nimport _ \"m/b\"\n")
	write058(t, repo, "b/b.go", "package b\n")
	commit058(t, repo, "init")
	work := t.TempDir()
	return repo, filepath.Join(work, "journal.ndjson"), filepath.Join(work, "index")
}

func runIndex(t *testing.T, repo, outDir string) (code int, stdout string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run([]string{"index", "-repo", repo, "-out", outDir}, &out, &errOut)
	return code, strings.TrimSpace(out.String())
}

// 테스트 전용 writer로 main.advanced를 심는다(serve emit의 대역이 아니라,
// graph CLI가 읽을 기록을 만드는 픽스처 — serve 경로 테스트는 workspace 쪽).
func seedMainAdvanced(t *testing.T, jp, sha string) {
	t.Helper()
	j, err := journal.Open(jp)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	raw, err := json.Marshal(codeindex.MainAdvancedPayload{Sha: sha, At: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	_, prevRev, err := codeindex.LatestMainAdvanced(j.List("repo", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(prevRev, events.Event{AggregateType: "repo", AggregateID: "main", Revision: prevRev + 1, Type: "main.advanced", Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

// T2: 캐시 삭제 후 재생성 바이트 동일 — B1(최초)==B2(전체 삭제 후)==B3(무삭제
// 재실행). 정렬 제거·시각 삽입 변이가 여기서 FAIL.
func TestIndexCacheRegenerationByteIdenticalFRRHZ088(t *testing.T) {
	repo, _, outDir := fixture058(t)
	if code, _ := runIndex(t, repo, outDir); code != 0 {
		t.Fatal("index failed")
	}
	sha := runGit058(t, repo, "rev-parse", "refs/heads/main")
	cache := filepath.Join(outDir, sha, "graph.json")
	b1, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(outDir); err != nil {
		t.Fatal(err)
	}
	if code, _ := runIndex(t, repo, outDir); code != 0 {
		t.Fatal("reindex failed")
	}
	b2, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := runIndex(t, repo, outDir); code != 0 {
		t.Fatal("third run failed")
	}
	b3, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) || !bytes.Equal(b2, b3) {
		t.Fatal("regenerated cache bytes differ")
	}
}

// T5: 브랜치 미색인 — HEAD를 feature에 둔 채 실행해도 refs/heads/main만
// 색인된다(ref 고정, 브랜치 색인 경로 자체가 없음).
func TestIndexIgnoresBranchesFRRHZ088(t *testing.T) {
	repo, _, outDir := fixture058(t)
	mainSha := runGit058(t, repo, "rev-parse", "refs/heads/main")
	runGit058(t, repo, "checkout", "-q", "-b", "feature")
	write058(t, repo, "c/c.go", "package c\n")
	commit058(t, repo, "branch work")
	featureSha := runGit058(t, repo, "rev-parse", "HEAD")
	code, sha := runIndex(t, repo, outDir)
	if code != 0 || sha != mainSha {
		t.Fatalf("indexed %q (HEAD=feature %q), want main %q", sha, featureSha, mainSha)
	}
	if _, err := os.Stat(filepath.Join(outDir, featureSha)); !os.IsNotExist(err) {
		t.Fatal("branch sha cache created")
	}
	b, err := os.ReadFile(filepath.Join(outDir, mainSha, "graph.json"))
	if err != nil || strings.Contains(string(b), `"c"`) {
		t.Fatalf("branch package leaked into main graph: %v", err)
	}
}

// T6: graph 조회 — buildSha 노출·낡음 판별·캐시 소실 시 결정론 재생성.
// main.advanced 기록은 테스트 전용 writer로 심는다(RHZ-059: CLI index는 더는
// journal에 쓰지 않으므로).
func TestGraphQueryBuildShaAndStalenessFRRHZ088(t *testing.T) {
	repo, jp, outDir := fixture058(t)
	if code, _ := runIndex(t, repo, outDir); code != 0 {
		t.Fatal("index failed")
	}
	oldSha := runGit058(t, repo, "rev-parse", "refs/heads/main")
	seedMainAdvanced(t, jp, oldSha)
	write058(t, repo, "c/c.go", "package c\n")
	commit058(t, repo, "advance without reindex")
	journalBefore, err := os.ReadFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"graph", "-journal", jp, "-repo", repo, "-out", outDir}, &out, io.Discard); code != 0 {
		t.Fatal("graph failed")
	}
	if !strings.Contains(out.String(), `"buildSha": "`+oldSha+`"`) {
		t.Fatalf("buildSha missing: %s", out.String())
	}
	if cur := runGit058(t, repo, "rev-parse", "refs/heads/main"); cur == oldSha {
		t.Fatal("fixture did not advance")
	}
	journalAfter, err := os.ReadFile(jp)
	if err != nil || !bytes.Equal(journalBefore, journalAfter) {
		t.Fatal("query wrote to journal")
	}
	if err := os.RemoveAll(outDir); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	if code := run([]string{"graph", "-journal", jp, "-repo", repo, "-out", outDir}, &out2, io.Discard); code != 0 {
		t.Fatal("graph regeneration failed")
	}
	if !bytes.Equal(out.Bytes(), out2.Bytes()) {
		t.Fatal("regenerated query bytes differ")
	}
}

// T8①: 손상 .go 커밋 → index fail-stop: 캐시 미생성, 작업 디렉터리에 journal·
// lock 등 어떤 파일도 생기지 않음(읽기 전용 + 부분 산출물 0).
func TestIndexFailStopNoPartialArtifactsFRRHZ088(t *testing.T) {
	repo, jp, outDir := fixture058(t)
	write058(t, repo, "a/broken.go", "package a\n\nfunc {\n")
	commit058(t, repo, "break")
	sha := runGit058(t, repo, "rev-parse", "refs/heads/main")
	code, _ := runIndex(t, repo, outDir)
	if code == 0 {
		t.Fatal("broken tree indexed")
	}
	if _, err := os.Stat(filepath.Join(outDir, sha)); !os.IsNotExist(err) {
		t.Fatal("partial cache written")
	}
	entries, err := os.ReadDir(filepath.Dir(jp))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("work dir not empty after failed read-only index: %v", entries)
	}
}

// T9: 플래그 누락·폐기 플래그 → usage(2), 부작용 0. -journal은 RHZ-059에서
// 제거됐으므로 전달 자체가 파싱 오류다(죽은 표면 금지).
func TestIndexGraphUsageFRRHZ088(t *testing.T) {
	for _, args := range [][]string{
		{"index"},
		{"index", "-repo", "r"},
		{"index", "-journal", "j", "-repo", "r", "-out", "o"},
		{"graph"},
		{"graph", "-out", "o"},
	} {
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Fatalf("%v exited %d, want 2", args, code)
		}
	}
}
