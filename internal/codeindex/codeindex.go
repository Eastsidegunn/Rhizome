// Package codeindex builds the main-anchored Go package graph (RHZ-058,
// FR-RHZ-088). Build is f(git@sha): it reads git objects only — never the
// working tree — and Marshal is deterministic (everything sorted, no time
// fields), so the same sha always yields the same bytes.
package codeindex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os/exec"
	"path"
	"sort"
	"strings"

	"rhizome/internal/events"
)

// MainAdvancedPayload is the one journal fact this index leaves behind
// (RHZ-058/059): the graph itself is never event-sourced. At lives here only —
// graph bytes carry no time.
type MainAdvancedPayload struct {
	Sha string `json:"sha"`
	At  string `json:"at"`
}

// LatestMainAdvanced reads the newest main.advanced fact from a repo/main
// stream; ("", 0) means none recorded. Shared by the serve emitter (RHZ-059)
// and the read-only graph CLI.
func LatestMainAdvanced(evs []events.Event) (string, uint64, error) {
	if len(evs) == 0 {
		return "", 0, nil
	}
	last := evs[len(evs)-1]
	var p MainAdvancedPayload
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		return "", 0, fmt.Errorf("decode main.advanced: %w", err)
	}
	return p.Sha, last.Revision, nil
}

type Node struct {
	ID string `json:"id"`
	// Path is the navigation pointer: the package's path at main, refreshed on
	// every regeneration. PathAtSha is the audit pointer, fixed to the build.
	Path      string `json:"path"`
	PathAtSha string `json:"pathAtSha"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type Graph struct {
	BuildSha string `json:"buildSha"`
	Nodes    []Node `json:"nodes"`
	Edges    []Edge `json:"edges"`
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Governance: external git reads never take optional locks.
	cmd.Env = append(cmd.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// ResolveMain resolves refs/heads/main — the only ref this indexer accepts.
// There is deliberately no flag for another ref: branch commits are
// structurally unindexable (FR-RHZ-088).
func ResolveMain(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "refs/heads/main")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// excluded drops vendor/ and testdata/ trees (go convention).
func excluded(file string) bool {
	for _, seg := range strings.Split(path.Dir(file), "/") {
		if seg == "vendor" || seg == "testdata" {
			return true
		}
	}
	return false
}

// Build derives the package graph from git@sha. Any unparsable .go file is a
// hard error (fail-stop): a partial graph would silently break determinism.
func Build(dir, sha string) (Graph, error) {
	modRaw, err := git(dir, "show", sha+":go.mod")
	if err != nil {
		return Graph{}, fmt.Errorf("go.mod at %s: %w", sha, err)
	}
	module := ""
	for _, line := range strings.Split(string(modRaw), "\n") {
		if strings.HasPrefix(line, "module ") {
			module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			break
		}
	}
	if module == "" {
		return Graph{}, fmt.Errorf("module path missing in go.mod at %s", sha)
	}
	lsRaw, err := git(dir, "ls-tree", "-r", "--name-only", sha)
	if err != nil {
		return Graph{}, err
	}
	pkgs := map[string]bool{}
	imports := map[string]map[string]bool{}
	for _, file := range strings.Split(strings.TrimSpace(string(lsRaw)), "\n") {
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") || excluded(file) {
			continue
		}
		d := path.Dir(file)
		pkgs[d] = true
		src, err := git(dir, "show", sha+":"+file)
		if err != nil {
			return Graph{}, err
		}
		// Full parse (not ImportsOnly): ImportsOnly stops before the body, so
		// a syntactically broken file would index silently — fail-stop needs
		// the whole file checked.
		parsed, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
		if err != nil {
			return Graph{}, fmt.Errorf("parse %s@%s: %w", file, sha, err)
		}
		for _, imp := range parsed.Imports {
			v := strings.Trim(imp.Path.Value, `"`)
			var target string
			switch {
			case v == module:
				target = "."
			case strings.HasPrefix(v, module+"/"):
				target = strings.TrimPrefix(v, module+"/")
			default:
				continue // stdlib and external modules are out of scope
			}
			if imports[d] == nil {
				imports[d] = map[string]bool{}
			}
			imports[d][target] = true
		}
	}
	g := Graph{BuildSha: sha, Nodes: []Node{}, Edges: []Edge{}}
	dirs := make([]string, 0, len(pkgs))
	for d := range pkgs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		g.Nodes = append(g.Nodes, Node{ID: d, Path: d, PathAtSha: d + "@" + sha})
	}
	for _, d := range dirs {
		targets := make([]string, 0, len(imports[d]))
		for target := range imports[d] {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			// An import of a path that is no package in this tree carries no
			// node to point at; the edge set stays closed over the nodes.
			if target == d || !pkgs[target] {
				continue
			}
			g.Edges = append(g.Edges, Edge{From: d, To: target, Kind: "imports"})
		}
	}
	return g, nil
}

// Marshal renders the cache bytes. Determinism: field order is fixed by the
// structs, both slices arrive sorted from Build, and nothing here reads the
// clock or the environment.
func Marshal(g Graph) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Encoding these structs cannot fail; Encode appends the final newline.
	_ = enc.Encode(g)
	return buf.Bytes()
}
