package archtest

// RHZ-107 G1 FR-RHZ-136 (SCP §7): R8 exact import allowlists over full paths
// for the secret packages, plus the scanner that sees nested packages and
// cmd/. The legacy R1–R7 path keeps its input shape via reduceLegacy.

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	module      = "rhizome"
	internalPfx = module + "/internal/"
	planeImport = internalPfx + "secretplane"
	planeDir    = "internal/secretplane"
	planeCmdDir = "cmd/rhizome-secret-plane"
)

// edgeRule is one row of the §7 table: besides the standard library, a
// package may import exactly the listed paths, or anything under a listed
// prefix (the path itself included).
type edgeRule struct {
	exact    []string
	subtrees []string
}

// edgeRules is keyed by package; under says whether the rule also covers the
// package's nested packages ("및 그 하위 전부" in §7).
var edgeRules = []struct {
	pkg   string
	under bool
	rule  edgeRule
}{
	{"internal/secretwire", false, edgeRule{}},
	{"internal/secretwiretest", false, edgeRule{exact: []string{internalPfx + "secretwire", internalPfx + "peercred"}}},
	{planeDir, true, edgeRule{
		exact:    []string{internalPfx + "peercred", internalPfx + "secretwire", internalPfx + "termio"},
		subtrees: []string{planeImport},
	}},
	{planeCmdDir, true, edgeRule{
		exact:    []string{internalPfx + "secretwire", internalPfx + "peercred", internalPfx + "termio"},
		subtrees: []string{planeImport},
	}},
}

func within(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

// isStdlib is the §7 in-process standard-library judgment: no dot in the
// first element, not the module itself, and a directory under GOROOT/src.
// The os.Stat is its only filesystem access (no go/build, no go list).
func isStdlib(path string) bool {
	if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") || within(path, module) {
		return false
	}
	fi, err := os.Stat(filepath.Join(runtime.GOROOT(), "src", path))
	return err == nil && fi.IsDir()
}

// CheckEdges evaluates R8 over tree (module-relative package path → full
// import paths) and returns "edge:<from>-><to>" violations, sorted.
func CheckEdges(tree map[string][]string) []string {
	seen := map[string]bool{}
	for pkg, imports := range tree {
		inPlane := within(pkg, planeDir) || within(pkg, planeCmdDir)
		var rule *edgeRule
		for i, r := range edgeRules {
			if pkg == r.pkg || (r.under && within(pkg, r.pkg)) {
				rule = &edgeRules[i].rule
			}
		}
		for _, imp := range imports {
			bad := false
			if within(imp, planeImport) && !inPlane {
				bad = true // reverse ban
			}
			if rule != nil && !allowed(*rule, imp) {
				bad = true
			}
			if bad {
				seen[fmt.Sprintf("edge:%s->%s", pkg, imp)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func allowed(r edgeRule, imp string) bool {
	for _, e := range r.exact {
		if imp == e {
			return true
		}
	}
	for _, s := range r.subtrees {
		if within(imp, s) {
			return true
		}
	}
	return isStdlib(imp)
}

// scanTree walks root/internal and root/cmd recursively: every directory with
// a non-test .go file is a package keyed by its module-relative path, mapped
// to all of its import paths (stdlib and external included), sorted and
// deduplicated. Like the go tool it skips testdata and "."/"_" directories.
// parser.ImportsOnly, no external execution.
func scanTree(root string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if path != filepath.Join(root, top) && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			if out[pkg] == nil {
				out[pkg] = []string{}
			}
			for _, imp := range parsed.Imports {
				// Unquote, not Trim: raw `...` and escaped literals are valid
				// import paths and must not slip past CheckEdges.
				ip, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				out[pkg] = append(out[pkg], ip)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for pkg, imports := range out {
		sort.Strings(imports)
		dedup := imports[:0]
		for i, imp := range imports {
			if i == 0 || imp != imports[i-1] {
				dedup = append(dedup, imp)
			}
		}
		out[pkg] = dedup
	}
	return out, nil
}

// reduceLegacy derives the legacy Check input from a scanTree result (§7
// item 2): drop internal/secretplane/**, cmd/** and edges into
// rhizome/internal/secretplane/**, then map each top-level internal package to
// the sorted first elements of its internal imports. Nested packages inherit
// their top-level package's layer by merging into it.
func reduceLegacy(tree map[string][]string) map[string][]string {
	sets := map[string]map[string]bool{}
	for pkg, imports := range tree {
		rest, ok := strings.CutPrefix(pkg, "internal/")
		if !ok || within(pkg, planeDir) {
			continue
		}
		top := strings.SplitN(rest, "/", 2)[0]
		if sets[top] == nil {
			sets[top] = map[string]bool{}
		}
		for _, imp := range imports {
			name, ok := strings.CutPrefix(imp, internalPfx)
			if !ok || within(imp, planeImport) {
				continue
			}
			sets[top][strings.SplitN(name, "/", 2)[0]] = true
		}
	}
	out := map[string][]string{}
	for top, set := range sets {
		names := []string{}
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		out[top] = names
	}
	return out
}
