package codeindex

// RHZ-102a (FR-RHZ-178): the Phase 1a structure graph (design rev5 §3.1),
// slice 1 — Go multi-module and the top-level fallback. "complete" means every
// line of the kind's finite grammar was recognised; anything else turns the
// kind unsupported, drops all of its nodes (no partial graphs) and lists
// reasons. RHZ-102b (FR-RHZ-179) adds JS/TS workspaces (structure_jsts.go) and
// Rust crates (structure_rust.go). The output is unstable (internal): its
// shape is SCP material for Phase 1b (B6).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	StructStability = "unstable-internal"
	structIndexer   = "codeindex-structure/1a-2"
	structLayer     = "structure"
	buildTagPolicy  = "union"
)

// structKinds fixes the kind order of the envelope and the input digest.
var structKinds = []struct{ kind, grammar string }{
	{"go", "go/v1"}, {"js", "js/v1"}, {"rust", "rust/v1"}, {"fallback", "fallback/v1"},
}

type StructNode struct {
	ID   string `json:"id"`   // <kind>:<path>, repo local
	Kind string `json:"kind"` // go-module | go-package | js-package | rust-crate | dir
	Path string `json:"path"`
	// Name is the module path (go-module), import path (go-package) or
	// package/crate name (js-package, rust-crate).
	Name string `json:"name,omitempty"`
	// Parent is the owning go-module node of a go-package.
	Parent string `json:"parent,omitempty"`
}

type StructEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"` // imports | dependsOn
	// DepType is the dependency table of a dependsOn edge.
	DepType string `json:"depType,omitempty"`
}

// Reason locates one construct outside the grammar; Line 0 means the whole
// file.
type Reason struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Construct string `json:"construct"`
}

type Constraint struct {
	File string `json:"file"`
	Expr string `json:"expr"`
}

// BuildTags records the build-tag policy: every file is read whatever its
// constraint (union), and the constrained files are listed.
type BuildTags struct {
	Policy      string       `json:"policy"`
	Constrained []Constraint `json:"constrained"`
}

type KindStatus struct {
	Kind      string     `json:"kind"`
	Status    string     `json:"status"` // complete | unsupported | absent
	Grammar   string     `json:"grammar"`
	Reasons   []Reason   `json:"reasons"`
	BuildTags *BuildTags `json:"buildTags,omitempty"`
}

// StructGraph carries no repo id, path or time: the same commit and input
// tuple always yield the same bytes (§4.1).
type StructGraph struct {
	Stability    string       `json:"stability"`
	Indexer      string       `json:"indexer"`
	ObjectFormat string       `json:"objectFormat"`
	Commit       string       `json:"commit"`
	InputDigest  string       `json:"inputDigest"`
	Kinds        []KindStatus `json:"kinds"`
	Nodes        []StructNode `json:"nodes"`
	Edges        []StructEdge `json:"edges"`
}

// InputDigest is sha256 over the canonical input tuple (§4.1); together with
// the commit it is the cache key of the next slice.
func InputDigest(objectFormat, commit string) string {
	grammars := map[string]string{}
	kinds := make([]string, 0, len(structKinds))
	for _, k := range structKinds {
		kinds = append(kinds, k.kind)
		grammars[k.kind] = k.grammar
	}
	b, _ := json.Marshal(struct {
		ObjectFormat   string            `json:"objectFormat"`
		Commit         string            `json:"commit"`
		IndexerVersion string            `json:"indexerVersion"`
		Kinds          []string          `json:"kinds"`
		Grammars       map[string]string `json:"grammars"`
		BuildTags      string            `json:"buildTags"`
		Layer          string            `json:"layer"`
	}{objectFormat, commit, structIndexer, kinds, grammars, buildTagPolicy, structLayer})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// GraphAt is the whole Phase 1a request: open and pin repo, resolve
// branch/sha, build the structure graph.
func GraphAt(ctx context.Context, repo, branch, sha string) (StructGraph, error) {
	r, err := OpenPinned(ctx, repo)
	if err != nil {
		return StructGraph{}, err
	}
	defer r.Close()
	commit, err := r.Resolve(ctx, branch, sha)
	if err != nil {
		return StructGraph{}, err
	}
	return BuildStructure(ctx, r, commit)
}

func goInput(p string) bool {
	if excluded(p) {
		return false
	}
	return path.Base(p) == "go.mod" || strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go")
}

// BuildStructure reads commit's tree and the files the grammars need.
func BuildStructure(ctx context.Context, r *PinnedRepo, commit string) (StructGraph, error) {
	tree, err := r.ListTree(ctx, commit)
	if err != nil {
		return StructGraph{}, err
	}
	var want []TreeEntry
	for _, e := range tree {
		if e.Regular() && (goInput(e.Path) || jsInput(e.Path) || rustInput(e.Path)) {
			want = append(want, e)
		}
	}
	blobs, err := r.ReadBlobs(ctx, want)
	if err != nil {
		return StructGraph{}, err
	}
	src := make(map[string][]byte, len(want))
	for i, e := range want {
		src[e.Path] = blobs[i]
	}
	return structureFrom(r.ObjectFormat, commit, tree, src)
}

func structureFrom(objectFormat, commit string, tree []TreeEntry, src map[string][]byte) (StructGraph, error) {
	g := StructGraph{
		Stability:    StructStability,
		Indexer:      structIndexer,
		ObjectFormat: objectFormat,
		Commit:       commit,
		InputDigest:  InputDigest(objectFormat, commit),
		Nodes:        []StructNode{},
		Edges:        []StructEdge{},
	}
	goStatus, nodes, edges, err := buildGo(commit, tree, src)
	if err != nil {
		return StructGraph{}, err
	}
	g.Nodes = append(g.Nodes, nodes...)
	g.Edges = append(g.Edges, edges...)
	jsStatus, nodes, edges := buildJS(src)
	g.Nodes = append(g.Nodes, nodes...)
	g.Edges = append(g.Edges, edges...)
	rustStatus, nodes, edges := buildRust(tree, src)
	g.Nodes = append(g.Nodes, nodes...)
	g.Edges = append(g.Edges, edges...)
	g.Nodes = append(g.Nodes, fallbackNodes(tree)...)
	for _, k := range structKinds {
		switch k.kind {
		case "go":
			g.Kinds = append(g.Kinds, goStatus)
		case "js":
			g.Kinds = append(g.Kinds, jsStatus)
		case "rust":
			g.Kinds = append(g.Kinds, rustStatus)
		case "fallback":
			g.Kinds = append(g.Kinds, KindStatus{Kind: k.kind, Status: "complete", Grammar: k.grammar, Reasons: []Reason{}})
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.DepType < b.DepType
	})
	return g, nil
}

// finish sorts a kind's reasons and settles its status: any reason makes it
// unsupported and drops its nodes and edges (no partial graphs).
func finish(st KindStatus, nodes []StructNode, edges []StructEdge) (KindStatus, []StructNode, []StructEdge) {
	sortReasons(st.Reasons)
	if len(st.Reasons) != 0 {
		st.Status = "unsupported"
		return st, nil, nil
	}
	st.Status = "complete"
	return st, nodes, edges
}

func sortReasons(rs []Reason) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Construct < b.Construct
	})
}

// wsPattern is one workspace member pattern: a literal directory or a literal
// directory followed by a final "/*" (glob), as declared at file:line.
type wsPattern struct {
	dir  string
	glob bool
	file string
	line int
}

// parsePattern accepts only a clean relative literal path, optionally ending
// in "/*"; anything else is reported as the returned construct.
func parsePattern(s string) (wsPattern, string) {
	p := wsPattern{dir: s}
	if strings.HasPrefix(s, "!") {
		return p, "workspace-negation"
	}
	if strings.Contains(s, "**") {
		return p, "workspace-globstar"
	}
	if rest, ok := strings.CutSuffix(s, "/*"); ok {
		p.dir, p.glob = rest, true
	}
	if p.dir == "" || strings.ContainsAny(p.dir, `*?[]{}!\`) {
		return p, "workspace-glob"
	}
	if path.IsAbs(p.dir) || path.Clean(p.dir) != p.dir || p.dir == "." || p.dir == ".." || strings.HasPrefix(p.dir, "../") {
		return p, "workspace-path"
	}
	return p, ""
}

// match reports whether dir (repo relative) is named by the pattern, with the
// pattern rooted at base.
func (p wsPattern) match(base, dir string) bool {
	d := path.Join(base, p.dir)
	if p.glob {
		return path.Dir(dir) == d
	}
	return dir == d
}

// fallbackNodes: one dir node per top-level directory not starting with ".".
func fallbackNodes(tree []TreeEntry) []StructNode {
	seen := map[string]bool{}
	var nodes []StructNode
	for _, e := range tree {
		top, _, nested := strings.Cut(e.Path, "/")
		if !nested || strings.HasPrefix(top, ".") || seen[top] {
			continue
		}
		seen[top] = true
		nodes = append(nodes, StructNode{ID: "dir:" + top, Kind: "dir", Path: top})
	}
	return nodes
}

// moduleLine is the only accepted module directive: `module p` or
// `module "p"` (p in module-path characters), optionally followed by a line
// comment.
var moduleLine = regexp.MustCompile(`^module[ \t]+(?:"([A-Za-z0-9][A-Za-z0-9._~/+-]*)"|([A-Za-z0-9][A-Za-z0-9._~/+-]*))[ \t]*(?://.*)?$`)

func isModuleDirective(line string) bool {
	return line == "module" || strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "module\t") ||
		strings.HasPrefix(line, "module(") || strings.HasPrefix(line, "module\"")
}

// parseGoMod returns the module path, or reasons when the file is outside
// the grammar.
func parseGoMod(file string, b []byte) (string, []Reason) {
	mod, line := "", 0
	var reasons []Reason
	for i, raw := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(raw)
		if !isModuleDirective(l) {
			continue
		}
		if line != 0 {
			reasons = append(reasons, Reason{File: file, Line: i + 1, Construct: "module-repeated"})
			continue
		}
		line = i + 1
		m := moduleLine.FindStringSubmatch(l)
		if m == nil {
			reasons = append(reasons, Reason{File: file, Line: i + 1, Construct: "module-form"})
			continue
		}
		mod = m[1] + m[2]
	}
	if line == 0 {
		reasons = append(reasons, Reason{File: file, Line: 0, Construct: "module-missing"})
	}
	return mod, reasons
}

type goModule struct {
	dir, path string
}

func dirWithin(dir, root string) bool {
	return root == "." || dir == root || strings.HasPrefix(dir, root+"/")
}

// buildGo: every go.mod outside vendor/testdata is a module; a package
// belongs to its nearest enclosing module; imports resolve by the longest
// module path prefix and only count when the target package really belongs
// to that module. go.work is ignored. Build tags: union. A Go parse error is
// fail-stop, as in the legacy index.
func buildGo(commit string, tree []TreeEntry, src map[string][]byte) (KindStatus, []StructNode, []StructEdge, error) {
	st := KindStatus{Kind: "go", Grammar: "go/v1", Reasons: []Reason{}}
	var mods []goModule
	var goFiles []string
	for _, e := range tree {
		if _, ok := src[e.Path]; !ok || !goInput(e.Path) {
			continue
		}
		if path.Base(e.Path) == "go.mod" {
			mp, reasons := parseGoMod(e.Path, src[e.Path])
			st.Reasons = append(st.Reasons, reasons...)
			mods = append(mods, goModule{dir: path.Dir(e.Path), path: mp})
		} else {
			goFiles = append(goFiles, e.Path)
		}
	}
	if len(mods) == 0 && len(goFiles) == 0 {
		st.Status = "absent"
		return st, nil, nil, nil
	}
	byPath := map[string]string{}
	for _, m := range mods {
		if m.path == "" {
			continue
		}
		if other, dup := byPath[m.path]; dup {
			st.Reasons = append(st.Reasons, Reason{File: path.Join(m.dir, "go.mod"), Line: 0, Construct: "module-duplicate-path:" + path.Join(other, "go.mod")})
			continue
		}
		byPath[m.path] = m.dir
	}
	depth := func(dir string) int {
		if dir == "." {
			return 0
		}
		return strings.Count(dir, "/") + 1
	}
	owner := func(dir string) (goModule, bool) {
		best, found := goModule{}, false
		for _, m := range mods {
			if dirWithin(dir, m.dir) && (!found || depth(m.dir) > depth(best.dir)) {
				best, found = m, true
			}
		}
		return best, found
	}
	pkgMod := map[string]goModule{}
	imports := map[string]map[string]bool{}
	tags := &BuildTags{Policy: buildTagPolicy, Constrained: []Constraint{}}
	for _, file := range goFiles {
		dir := path.Dir(file)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, src[file], parser.ParseComments)
		if err != nil {
			return KindStatus{}, nil, nil, fmt.Errorf("parse %s@%s: %w", file, commit, err)
		}
		for _, cg := range parsed.Comments {
			if cg.Pos() >= parsed.Package {
				break
			}
			for _, c := range cg.List {
				if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
					tags.Constrained = append(tags.Constrained, Constraint{File: file, Expr: strings.TrimSpace(c.Text)})
				}
			}
		}
		m, ok := owner(dir)
		if !ok {
			st.Reasons = append(st.Reasons, Reason{File: file, Line: 0, Construct: "package-outside-module"})
			continue
		}
		pkgMod[dir] = m
		for _, imp := range parsed.Imports {
			v, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return KindStatus{}, nil, nil, fmt.Errorf("import path %s in %s@%s: %w", imp.Path.Value, file, commit, err)
			}
			if imports[dir] == nil {
				imports[dir] = map[string]bool{}
			}
			imports[dir][v] = true
		}
	}
	st.BuildTags = tags
	sortReasons(st.Reasons)
	if len(st.Reasons) != 0 {
		st.Status = "unsupported"
		return st, nil, nil, nil
	}
	st.Status = "complete"
	modID := func(m goModule) string { return "go-module:" + m.dir }
	pkgID := func(dir string) string { return "go-package:" + dir }
	var nodes []StructNode
	var edges []StructEdge
	for _, m := range mods {
		nodes = append(nodes, StructNode{ID: modID(m), Kind: "go-module", Path: m.dir, Name: m.path})
	}
	// resolve maps an import path to the in-repo package dir it names.
	resolve := func(v string) (string, bool) {
		best, found := goModule{}, false
		for _, m := range mods {
			if (v == m.path || strings.HasPrefix(v, m.path+"/")) && (!found || len(m.path) > len(best.path)) {
				best, found = m, true
			}
		}
		if !found {
			return "", false // stdlib and external modules are out of scope
		}
		target := path.Join(best.dir, strings.TrimPrefix(strings.TrimPrefix(v, best.path), "/"))
		if pm, ok := pkgMod[target]; !ok || pm.dir != best.dir {
			return "", false // no package of that module there: the edge set stays closed
		}
		return target, true
	}
	for dir, m := range pkgMod {
		name := m.path
		switch {
		case dir == m.dir:
		case m.dir == ".":
			name += "/" + dir
		default:
			name += "/" + strings.TrimPrefix(dir, m.dir+"/")
		}
		nodes = append(nodes, StructNode{ID: pkgID(dir), Kind: "go-package", Path: dir, Name: name, Parent: modID(m)})
		for v := range imports[dir] {
			if target, ok := resolve(v); ok && target != dir {
				edges = append(edges, StructEdge{From: pkgID(dir), To: pkgID(target), Kind: "imports"})
			}
		}
	}
	return st, nodes, edges, nil
}

// MarshalStruct renders the graph bytes: fixed field order, sorted slices,
// no clock, no environment.
func MarshalStruct(g StructGraph) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(g)
	return buf.Bytes()
}
