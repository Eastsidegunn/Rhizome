package codeindex

// RHZ-102a FR-RHZ-178: git 고정 계층 + Go 다중 모듈·fallback 구조 그래프.
// fixture는 전부 t.TempDir()에 합성한다(체크인 fixture 0).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func gitOut(t *testing.T, dir string, args ...string) string {
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

// multiModuleRepo: 루트 모듈 m(a, b), 중첩 모듈 "m/sub"(sub/x, sub/y),
// 경로가 다른 중첩 모듈 example.com/tools(tools/t), vendor/·testdata/의
// go.mod(제외), build tag 파일 두 개, 숨은 최상위 디렉터리.
func multiModuleRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "go.mod", "module m // root\n\ngo 1.23\n")
	writeFile(t, dir, "a/a.go", "package a\n\nimport (\n\t_ \"fmt\"\n\t_ \"m/b\"\n\t_ \"m/sub/x\"\n\t_ \"m/tools/t\"\n\t_ \"m/zzz\"\n)\n")
	writeFile(t, dir, "a/a_linux.go", "//go:build linux\n\npackage a\n\nimport _ \"m/sub/y\"\n")
	writeFile(t, dir, "a/a_other.go", "//go:build !linux\n\npackage a\n\nimport _ \"example.com/tools/t\"\n")
	writeFile(t, dir, "a/a_test.go", "package a\n\nimport _ \"m/sub/x\"\n")
	writeFile(t, dir, "b/b.go", "package b\n")
	writeFile(t, dir, "sub/go.mod", "module \"m/sub\"\n\ngo 1.23\n")
	writeFile(t, dir, "sub/x/x.go", "package x\n\nimport _ \"m/b\"\n")
	writeFile(t, dir, "sub/y/y.go", "package y\n\nimport _ \"m/sub/x\"\n")
	writeFile(t, dir, "tools/go.mod", "module example.com/tools\n")
	writeFile(t, dir, "tools/t/t.go", "package t\n")
	writeFile(t, dir, "vendor/go.mod", "module (\n")
	writeFile(t, dir, "vendor/v/v.go", "package v\n\nimport _ \"m/a\"\n")
	writeFile(t, dir, "a/testdata/go.mod", "garbage\n")
	writeFile(t, dir, ".github/w.yml", "x\n")
	writeFile(t, dir, "README.md", "x\n")
	commitAll(t, dir, "init")
	return dir
}

func graphAt(t *testing.T, dir, sha string) (StructGraph, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	return GraphAt(ctx, dir, "refs/heads/main", sha)
}

func kind(g StructGraph, k string) KindStatus {
	for _, s := range g.Kinds {
		if s.Kind == k {
			return s
		}
	}
	return KindStatus{}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *CodeError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestStructureGoMultiModuleFRRHZ178(t *testing.T) {
	dir := multiModuleRepo(t)
	g, err := graphAt(t, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Stability != "unstable-internal" || g.ObjectFormat != "sha1" || g.Commit != gitOut(t, dir, "rev-parse", "main") || len(g.InputDigest) != 64 {
		t.Fatalf("envelope: %+v", g)
	}
	var order []string
	for _, k := range g.Kinds {
		order = append(order, k.Kind+"="+k.Status+"/"+k.Grammar)
	}
	if got := strings.Join(order, " "); got != "go=complete/go/v1 js=absent/js/v1 rust=absent/rust/v1 fallback=complete/fallback/v1" {
		t.Fatalf("kinds: %s", got)
	}
	bt := kind(g, "go").BuildTags
	if bt == nil || bt.Policy != "union" || len(bt.Constrained) != 2 ||
		bt.Constrained[0] != (Constraint{"a/a_linux.go", "//go:build linux"}) || bt.Constrained[1] != (Constraint{"a/a_other.go", "//go:build !linux"}) {
		t.Fatalf("buildTags: %+v", bt)
	}
	var nodes []string
	for _, n := range g.Nodes {
		nodes = append(nodes, n.ID+"|"+n.Name+"|"+n.Parent)
	}
	wantNodes := []string{
		"dir:a||", "dir:b||", "dir:sub||", "dir:tools||", "dir:vendor||",
		"go-module:.|m|", "go-module:sub|m/sub|", "go-module:tools|example.com/tools|",
		"go-package:a|m/a|go-module:.", "go-package:b|m/b|go-module:.",
		"go-package:sub/x|m/sub/x|go-module:sub", "go-package:sub/y|m/sub/y|go-module:sub",
		"go-package:tools/t|example.com/tools/t|go-module:tools",
	}
	if strings.Join(nodes, "\n") != strings.Join(wantNodes, "\n") {
		t.Fatalf("nodes:\n%s", strings.Join(nodes, "\n"))
	}
	var edges []string
	for _, e := range g.Edges {
		edges = append(edges, e.From+">"+e.To+":"+e.Kind)
	}
	// m/tools/t names no package (tools/t belongs to example.com/tools);
	// both build-tag variants count (union); a_test.go and vendor/ do not.
	wantEdges := []string{
		"go-package:a>go-package:b:imports",
		"go-package:a>go-package:sub/x:imports",
		"go-package:a>go-package:sub/y:imports",
		"go-package:a>go-package:tools/t:imports",
		"go-package:sub/x>go-package:b:imports",
		"go-package:sub/y>go-package:sub/x:imports",
	}
	if strings.Join(edges, "\n") != strings.Join(wantEdges, "\n") {
		t.Fatalf("edges:\n%s", strings.Join(edges, "\n"))
	}
}

// 바이트 결정론: 같은 레포 2회 + 다른 경로의 full clone 1회가 바이트 동일
// (경로·레포 id·시각이 바이트에 없음).
func TestStructureDeterministicBytesFRRHZ178(t *testing.T) {
	dir := multiModuleRepo(t)
	g1, err := graphAt(t, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := graphAt(t, dir, g1.Commit)
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitOut(t, dir, "clone", "-q", "--no-local", dir, clone)
	g3, err := graphAt(t, clone, g1.Commit)
	if err != nil {
		t.Fatal(err)
	}
	b1, b2, b3 := MarshalStruct(g1), MarshalStruct(g2), MarshalStruct(g3)
	if !bytes.Equal(b1, b2) || !bytes.Equal(b1, b3) {
		t.Fatalf("bytes differ:\n%s\n---\n%s\n---\n%s", b1, b2, b3)
	}
	if bytes.Contains(b1, []byte(dir)) {
		t.Fatal("repo path leaked into bytes")
	}
}

// 문법 밖 → unsupported: go 노드 0(부분 그래프 금지), fallback 노드는 남음.
func TestStructureGoUnsupportedFRRHZ178(t *testing.T) {
	for _, c := range []struct {
		name    string
		files   map[string]string
		reasons string
	}{
		{"module block", map[string]string{"go.mod": "module (\n\tm\n)\n", "a/a.go": "package a\n"}, "go.mod:1:module-form"},
		{"module repeated", map[string]string{"go.mod": "module m\nmodule n\n", "a/a.go": "package a\n"}, "go.mod:2:module-repeated"},
		{"module missing", map[string]string{"go.mod": "go 1.23\n", "a/a.go": "package a\n"}, "go.mod:0:module-missing"},
		{"outside module", map[string]string{"lib/go.mod": "module lib\n", "lib/l.go": "package lib\n", "cmd/c.go": "package main\n"}, "cmd/c.go:0:package-outside-module"},
		{"duplicate path", map[string]string{"x/go.mod": "module d\n", "y/go.mod": "module d\n", "x/x.go": "package x\n"}, "y/go.mod:0:module-duplicate-path:x/go.mod"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init", "-q", "-b", "main")
			for name, content := range c.files {
				writeFile(t, dir, name, content)
			}
			commitAll(t, dir, "init")
			g, err := graphAt(t, dir, "")
			if err != nil {
				t.Fatal(err)
			}
			st := kind(g, "go")
			var rs []string
			for _, r := range st.Reasons {
				rs = append(rs, r.File+":"+strconv.Itoa(r.Line)+":"+r.Construct)
			}
			if st.Status != "unsupported" || strings.Join(rs, ",") != c.reasons {
				t.Fatalf("go kind: %+v", st)
			}
			for _, n := range g.Nodes {
				if n.Kind != "dir" {
					t.Fatalf("partial graph: %+v", n)
				}
			}
			if len(g.Nodes) == 0 || len(g.Edges) != 0 {
				t.Fatalf("fallback: %+v", g)
			}
		})
	}
}

func TestStructureAbsentAndParseFailStopFRRHZ178(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "docs/x.md", "x\n")
	commitAll(t, dir, "init")
	g, err := graphAt(t, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if kind(g, "go").Status != "absent" || len(g.Nodes) != 1 || g.Nodes[0].ID != "dir:docs" {
		t.Fatalf("absent: %+v", g)
	}
	writeFile(t, dir, "go.mod", "module m\n")
	writeFile(t, dir, "a/a.go", "package a\n\nfunc {\n")
	commitAll(t, dir, "broken")
	if _, err := graphAt(t, dir, ""); err == nil || !strings.Contains(err.Error(), "parse a/a.go@") {
		t.Fatalf("want fail-stop parse error, got %v", err)
	}
}

// B7: shallow·grafts·replace·partial clone 레포 거부.
func TestStructureRejectsUnpinnableReposFRRHZ178(t *testing.T) {
	src := multiModuleRepo(t)
	writeFile(t, src, "b/b2.go", "package b\n")
	commitAll(t, src, "second")

	shallow := filepath.Join(t.TempDir(), "shallow")
	gitOut(t, src, "clone", "-q", "--depth", "1", "file://"+src, shallow)
	_, err := graphAt(t, shallow, "")
	wantCode(t, err, CodeUnsupportedRepo)

	grafts := filepath.Join(t.TempDir(), "grafts")
	gitOut(t, src, "clone", "-q", "--no-local", src, grafts)
	head := gitOut(t, grafts, "rev-parse", "HEAD")
	writeFile(t, grafts, ".git/info/grafts", head+"\n")
	_, err = graphAt(t, grafts, "")
	wantCode(t, err, CodeUnsupportedRepo)

	replace := filepath.Join(t.TempDir(), "replace")
	gitOut(t, src, "clone", "-q", "--no-local", src, replace)
	gitOut(t, replace, "replace", gitOut(t, replace, "rev-parse", "HEAD"), gitOut(t, replace, "rev-parse", "HEAD~1"))
	_, err = graphAt(t, replace, "")
	wantCode(t, err, CodeUnsupportedRepo)

	for _, kv := range [][2]string{{"extensions.partialClone", "origin"}, {"remote.origin.promisor", "true"}} {
		partial := filepath.Join(t.TempDir(), "partial")
		gitOut(t, src, "clone", "-q", "--no-local", src, partial)
		gitOut(t, partial, "config", kv[0], kv[1])
		_, err = graphAt(t, partial, "")
		wantCode(t, err, CodeUnsupportedRepo)
	}
}

// §4.2: full OID·commit 객체·main 도달 가능만. 병합된 사이드 커밋은 허용.
func TestStructureCommitPinningFRRHZ178(t *testing.T) {
	dir := multiModuleRepo(t)
	first := gitOut(t, dir, "rev-parse", "main")
	gitOut(t, dir, "checkout", "-q", "-b", "side")
	writeFile(t, dir, "side/s.go", "package s\n")
	commitAll(t, dir, "side")
	merged := gitOut(t, dir, "rev-parse", "side")
	gitOut(t, dir, "checkout", "-q", "main")
	gitOut(t, dir, "-c", "commit.gpgsign=false", "merge", "-q", "--no-ff", "-m", "merge", "side")
	gitOut(t, dir, "checkout", "-q", "-b", "unmerged")
	writeFile(t, dir, "u/u.go", "package u\n")
	commitAll(t, dir, "unmerged")
	unmerged := gitOut(t, dir, "rev-parse", "unmerged")
	gitOut(t, dir, "checkout", "-q", "main")

	for _, ok := range []string{first, merged} {
		g, err := graphAt(t, dir, ok)
		if err != nil || g.Commit != ok {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	_, err := graphAt(t, dir, unmerged)
	wantCode(t, err, CodeCommitUnavailable)
	_, err = graphAt(t, dir, first[:12])
	wantCode(t, err, CodeInvalidInput)
	_, err = graphAt(t, dir, strings.ToUpper(first))
	wantCode(t, err, CodeInvalidInput)
	_, err = graphAt(t, dir, gitOut(t, dir, "rev-parse", "main^{tree}"))
	wantCode(t, err, CodeCommitUnavailable)
	_, err = graphAt(t, dir, strings.Repeat("0", 40))
	wantCode(t, err, CodeCommitUnavailable)

	ctx := context.Background()
	_, err = GraphAt(ctx, dir, "refs/heads/nope", "")
	wantCode(t, err, CodeCommitUnavailable)
	_, err = GraphAt(ctx, dir, "main", "")
	wantCode(t, err, CodeInvalidInput)
	_, err = GraphAt(ctx, dir, "refs/heads/unmerged", unmerged)
	if err != nil {
		t.Fatalf("unmerged is reachable from its own branch: %v", err)
	}
}

// §4.1: 상속 env(GIT_DIR·GIT_INDEX_FILE·GIT_CONFIG_*)가 읽기에 새지 않는다;
// 기한 초과는 timeout.
func TestStructureScrubbedEnvAndDeadlineFRRHZ178(t *testing.T) {
	dir := multiModuleRepo(t)
	want, err := graphAt(t, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	decoy := multiModuleRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "extensions.partialClone")
	t.Setenv("GIT_CONFIG_VALUE_0", "origin")
	got, err := graphAt(t, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(MarshalStruct(want), MarshalStruct(got)) {
		t.Fatal("inherited git env changed the result")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = GraphAt(ctx, dir, "refs/heads/main", "")
	wantCode(t, err, CodeTimeout)
}
