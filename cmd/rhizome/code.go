package main

// RHZ-102a (FR-RHZ-178): code graph — the Phase 1a structure graph of one
// local repository at an exact commit (design rev5 §5). Read-only: no
// journal, no events, no cache yet (slice 1a-3). The output is unstable
// (internal) — "stability":"unstable-internal" — until the B6 SCP bundle.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"rhizome/internal/codeindex"
)

const codeUsage = `usage: rhizome code graph -repo <path> [-branch refs/heads/main] [-sha <oid>]

  graph  structure graph (Go modules·packages, top-level dirs) of <path> at
         -sha (full OID reachable from -branch; default: the branch tip).
         Output is unstable (internal), not a contract.
`

func codeCmd(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "graph" {
		fmt.Fprint(errOut, codeUsage)
		return 2
	}
	f := flag.NewFlagSet("code graph", flag.ContinueOnError)
	f.SetOutput(errOut)
	repo := f.String("repo", "", "")
	branch := f.String("branch", "refs/heads/main", "")
	sha := f.String("sha", "", "")
	if f.Parse(args[1:]) != nil {
		return 2
	}
	if *repo == "" || f.NArg() != 0 {
		fmt.Fprint(errOut, codeUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), codeindex.GitTimeout)
	defer cancel()
	g, err := codeindex.GraphAt(ctx, *repo, *branch, *sha)
	if err != nil {
		fmt.Fprintln(errOut, err)
		var ce *codeindex.CodeError
		if errors.As(err, &ce) && ce.Code == codeindex.CodeInvalidInput {
			return 2
		}
		return 1
	}
	if _, err := out.Write(codeindex.MarshalStruct(g)); err != nil {
		return 1
	}
	return 0
}
