package main

// RHZ-058/059 (FR-RHZ-088·089): index — 읽기 전용 캐시 생성·검증 도구. journal
// 무접촉(열지도, lock도 안 잡음 — serve 가동 중 실행 가능). 권위 있는
// main.advanced emit은 serve의 GET /v1/codeindex 경로만 한다(RHZ-059).
// graph — 최신 main.advanced의 캐시 조회(없으면 결정론 재생성).

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"rhizome/internal/codeindex"
	"rhizome/internal/journal"
)

func indexCmd(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("index", flag.ContinueOnError)
	f.SetOutput(errOut)
	repo := f.String("repo", "", "")
	outDir := f.String("out", "", "")
	if f.Parse(args) != nil {
		return 2
	}
	if *repo == "" || *outDir == "" {
		fmt.Fprintln(errOut, "repo and out are required")
		return 2
	}
	sha, err := codeindex.ResolveMain(*repo)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	g, err := codeindex.Build(*repo, sha)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	b := codeindex.Marshal(g)
	cachePath := filepath.Join(*outDir, sha, "graph.json")
	if existing, err := os.ReadFile(cachePath); err == nil {
		// 검증 모드: 기존 캐시가 재빌드와 다르면 결정론 위반 —
		// 조용히 덮지 않고 오류로 보고한다.
		if !bytes.Equal(existing, b) {
			fmt.Fprintf(errOut, "cache mismatch at %s: existing bytes differ from deterministic rebuild\n", cachePath)
			return 1
		}
		fmt.Fprintln(out, sha)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0700); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if err := os.WriteFile(cachePath, b, 0600); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintln(out, sha)
	return 0
}

func graphCmd(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("graph", flag.ContinueOnError)
	f.SetOutput(errOut)
	jp := f.String("journal", "", "")
	repo := f.String("repo", "", "")
	outDir := f.String("out", "", "")
	if f.Parse(args) != nil {
		return 2
	}
	if *jp == "" || *repo == "" || *outDir == "" {
		fmt.Fprintln(errOut, "journal, repo and out are required")
		return 2
	}
	unlock, err := acquireJournalLock(*jp, errOut)
	if err != nil {
		return 1
	}
	defer unlock()
	j, err := journal.Open(*jp)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer j.Close()
	sha, _, err := codeindex.LatestMainAdvanced(j.List("repo", "main"))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if sha == "" {
		fmt.Fprintln(errOut, "no index recorded")
		return 1
	}
	cachePath := filepath.Join(*outDir, sha, "graph.json")
	b, err := os.ReadFile(cachePath)
	if err != nil {
		// The cache is a derived, ephemeral artifact: a miss regenerates
		// deterministically from git@sha — never an error path.
		g, err := codeindex.Build(*repo, sha)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		b = codeindex.Marshal(g)
		if err := os.MkdirAll(filepath.Dir(cachePath), 0700); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		if err := os.WriteFile(cachePath, b, 0600); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
	}
	_, err = out.Write(b)
	if err != nil {
		return 1
	}
	return 0
}
