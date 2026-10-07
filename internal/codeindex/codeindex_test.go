package codeindex

// RHZ-058 FR-RHZ-088: f(git@sha) 결정론 그래프. 테스트 계획
// RHZ-058 test plan T1·T2(단위)·T7·T8② + vendor/testdata 핀.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

// fixtureRepo: 모듈 m — a(fmt·m/b·존재하지 않는 m/zzz import), b,
// b/x_test.go(m/a import — 제외 대상), vendor/·testdata/ 안의 .go(제외 대상).
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "go.mod", "module m\n\ngo 1.23\n")
	writeFile(t, dir, "a/a.go", "package a\n\nimport (\n\t_ \"fmt\"\n\t_ \"m/b\"\n\t_ \"m/zzz\"\n)\n")
	writeFile(t, dir, "b/b.go", "package b\n")
	writeFile(t, dir, "b/x_test.go", "package b\n\nimport _ \"m/a\"\n")
	writeFile(t, dir, "vendor/v/v.go", "package v\n\nimport _ \"m/a\"\n")
	writeFile(t, dir, "a/testdata/td.go", "package td\n\nimport _ \"m/a\"\n")
	commitAll(t, dir, "init")
	return dir
}

func mainSha(t *testing.T, dir string) string {
	t.Helper()
	sha, err := ResolveMain(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// T1: 노드·엣지·포인터 2종 + 제외 핀 — vendor/·testdata/·_test.go·stdlib·
// 미존재 타깃(m/zzz)이 전부 결과에 없음.
func TestBuildGraphShapeFRRHZ088(t *testing.T) {
	dir := fixtureRepo(t)
	sha := mainSha(t, dir)
	g, err := Build(dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	if g.BuildSha != sha {
		t.Fatalf("buildSha %q", g.BuildSha)
	}
	if len(g.Nodes) != 2 || g.Nodes[0].ID != "a" || g.Nodes[1].ID != "b" {
		t.Fatalf("nodes %+v (vendor/testdata leak?)", g.Nodes)
	}
	for _, n := range g.Nodes {
		if n.Path != n.ID || n.PathAtSha != n.ID+"@"+sha {
			t.Fatalf("pointer pair broken: %+v", n)
		}
	}
	// a→b 하나뿐: fmt(stdlib)·m/zzz(미존재 타깃)·b→a(_test.go 유래)는 없음.
	if len(g.Edges) != 1 || g.Edges[0] != (Edge{From: "a", To: "b", Kind: "imports"}) {
		t.Fatalf("edges %+v", g.Edges)
	}
}

// T2(단위): 같은 sha → 같은 바이트. CLI 캐시 삭제 왕복은 cmd 테스트가 커버.
func TestBuildDeterministicBytesFRRHZ088(t *testing.T) {
	dir := fixtureRepo(t)
	sha := mainSha(t, dir)
	g1, err := Build(dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := Build(dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Marshal(g1), Marshal(g2)) {
		t.Fatal("same sha produced different bytes")
	}
	if strings.Contains(string(Marshal(g1)), "\"at\"") {
		t.Fatal("graph bytes carry a time field")
	}
}

// T7: f(git@sha) 순수성 — 워킹트리를 오염시켜도(import 제거 저장 + 새 패키지
// 디렉터리) 결과 바이트가 완전 동일. git 객체만 읽는다는 사실의 행동 핀.
func TestBuildIgnoresWorkingTreeFRRHZ088(t *testing.T) {
	dir := fixtureRepo(t)
	sha := mainSha(t, dir)
	g1, err := Build(dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a/a.go", "package a\n") // 커밋 안 함: import 전부 제거
	writeFile(t, dir, "d/d.go", "package d\n\nimport _ \"m/a\"\n")
	g2, err := Build(dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Marshal(g1), Marshal(g2)) {
		t.Fatal("working tree pollution changed the build")
	}
	for _, n := range g2.Nodes {
		if n.ID == "d" {
			t.Fatal("uncommitted package indexed")
		}
	}
}

// T8①(단위): 손상 .go가 커밋된 sha는 fail-stop — 부분 그래프 금지.
func TestBuildFailStopOnParseErrorFRRHZ088(t *testing.T) {
	dir := fixtureRepo(t)
	writeFile(t, dir, "a/broken.go", "package a\n\nfunc {\n")
	commitAll(t, dir, "break")
	if _, err := Build(dir, mainSha(t, dir)); err == nil {
		t.Fatal("broken file indexed without error")
	}
}
