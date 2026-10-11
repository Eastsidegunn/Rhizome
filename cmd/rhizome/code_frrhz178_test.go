package main

// RHZ-102a FR-RHZ-178: rhizome code graph — 구조 그래프 CLI(불안정·내부,
// 저널·캐시 무접촉).

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"rhizome/internal/codeindex"
)

func TestCodeGraphCLIFRRHZ178(t *testing.T) {
	repo, _, _ := fixture058(t)
	sha := runGit058(t, repo, "rev-parse", "main")
	before, _ := os.ReadDir(repo)

	var out, errOut bytes.Buffer
	if code := run([]string{"code", "graph", "-repo", repo, "-sha", sha}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var g codeindex.StructGraph
	if err := json.Unmarshal(out.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g.Stability != "unstable-internal" || g.Commit != sha || len(g.Edges) != 1 || g.Edges[0] != (codeindex.StructEdge{From: "go-package:a", To: "go-package:b", Kind: "imports"}) {
		t.Fatalf("graph: %s", out.String())
	}
	var tip bytes.Buffer
	if code := run([]string{"code", "graph", "-repo", repo}, &tip, &errOut); code != 0 || !bytes.Equal(tip.Bytes(), out.Bytes()) {
		t.Fatalf("tip default: exit %d, %s", code, errOut.String())
	}
	if after, _ := os.ReadDir(repo); len(after) != len(before) {
		t.Fatal("code graph wrote into the repo")
	}

	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"code"}, 2, "usage: rhizome code graph"},
		{[]string{"code", "graph"}, 2, "usage: rhizome code graph"},
		{[]string{"code", "graph", "-repo", repo, "extra"}, 2, "usage"},
		{[]string{"code", "graph", "-repo", repo, "-sha", sha[:7]}, 2, "invalid_input"},
		{[]string{"code", "graph", "-repo", repo, "-branch", "main"}, 2, "invalid_input"},
		{[]string{"code", "graph", "-repo", repo, "-sha", strings.Repeat("0", 40)}, 1, "commit_unavailable"},
		{[]string{"code", "graph", "-repo", t.TempDir()}, 1, "git_error"},
	} {
		var o, e bytes.Buffer
		if code := run(c.args, &o, &e); code != c.code || o.Len() != 0 || !strings.Contains(e.String(), c.msg) {
			t.Fatalf("%v: exit=%d stdout=%q stderr=%q", c.args, code, o.String(), e.String())
		}
	}

	var help bytes.Buffer
	if run([]string{"help"}, &help, &errOut) != 0 || !strings.Contains(help.String(), "  code ") {
		t.Fatalf("usage lacks code: %s", help.String())
	}
}
