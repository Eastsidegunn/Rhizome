package codeindex

// RHZ-102b (FR-RHZ-179): the JS/TS row of design rev5 §3.1. Packages are the
// root package.json plus the workspace members declared either by the root
// package.json "workspaces" (an array of strings) or by the block list
// "packages:" of pnpm-workspace.yaml; patterns are a literal directory or a
// literal directory with a final "/*". Edges are dependsOn{depType} over the
// four dependency tables. A dependency that names a workspace package must be
// linked explicitly (workspace:<any>, or file:/link: to that package's
// directory); anything else depends on package-manager settings and is
// reported as ambiguous-internal-spec instead of being silently dropped. The
// one exception is a peerDependencies range on a workspace package that the
// same package also links explicitly in another table.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
)

var jsDepTables = []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"}

func jsInput(p string) bool {
	if p == "pnpm-workspace.yaml" {
		return true
	}
	if path.Base(p) != "package.json" {
		return false
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "node_modules" {
			return false
		}
	}
	return true
}

// jsonMember is one member of a JSON object with the file lines of its key
// and value.
type jsonMember struct {
	key   string
	line  int
	vline int
	voff  int
	raw   json.RawMessage
}

var errNotObject = errors.New("not an object")

func lineAt(doc []byte, off int) int {
	return 1 + bytes.Count(doc[:off], []byte("\n"))
}

// jsonObject reads the object val, found at byte offset base of doc, member
// by member so that every key keeps its line and duplicates stay visible.
func jsonObject(doc []byte, base int, val []byte) ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(val))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	var ms []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		kend := base + int(dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		voff := base + int(dec.InputOffset()) - len(raw)
		ms = append(ms, jsonMember{key: key, line: lineAt(doc, kend), vline: lineAt(doc, voff), voff: voff, raw: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return ms, nil
}

type jsDep struct {
	table, name, spec string
	line              int
}

type jsPkg struct {
	dir, file, name string
	nameLine        int
	deps            []jsDep
}

var (
	pnpmPackages = regexp.MustCompile(`^packages:[ \t]*(?:#.*)?$`)
	pnpmItem     = regexp.MustCompile(`^ *- +(?:'([^']*)'|"([^"\\]*)"|([A-Za-z0-9_./][A-Za-z0-9_./@*!-]*))[ \t]*(?:#.*)?$`)
)

// pnpmConstruct names the reason for a pnpm-workspace.yaml line outside the
// grammar.
func pnpmConstruct(l string) string {
	t := strings.TrimSpace(l)
	v := strings.TrimSpace(strings.TrimPrefix(t, "-"))
	if !strings.HasPrefix(t, "-") {
		if _, after, ok := strings.Cut(t, ":"); ok {
			v = strings.TrimSpace(after)
		}
	}
	switch {
	case strings.HasPrefix(v, "&"):
		return "yaml-anchor"
	case strings.HasPrefix(v, "*"):
		return "yaml-alias"
	case strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{"):
		return "yaml-flow"
	}
	return "pnpm-workspace-line"
}

// pnpmPatterns reads pnpm-workspace.yaml: blank lines, comments, one
// "packages:" key and its block list items, nothing else.
func pnpmPatterns(b []byte) ([]string, []int, []Reason) {
	const file = "pnpm-workspace.yaml"
	var items []string
	var lines []int
	var reasons []Reason
	seen, in := false, false
	for i, raw := range strings.Split(string(b), "\n") {
		l := strings.TrimRight(raw, " \t\r")
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if pnpmPackages.MatchString(l) {
			if seen {
				reasons = append(reasons, Reason{File: file, Line: i + 1, Construct: "yaml-key-repeated"})
			}
			seen, in = true, true
			continue
		}
		if m := pnpmItem.FindStringSubmatch(l); m != nil && in {
			items = append(items, m[1]+m[2]+m[3])
			lines = append(lines, i+1)
			continue
		}
		in = false
		reasons = append(reasons, Reason{File: file, Line: i + 1, Construct: pnpmConstruct(l)})
	}
	if !seen {
		reasons = append(reasons, Reason{File: file, Line: 0, Construct: "pnpm-packages-missing"})
	}
	return items, lines, reasons
}

// jsWorkspaces reads the root "workspaces" member: an array of strings only.
func jsWorkspaces(doc []byte, m jsonMember) ([]string, []int, []Reason) {
	const file = "package.json"
	switch {
	case len(m.raw) > 0 && m.raw[0] == '{':
		return nil, nil, []Reason{{File: file, Line: m.vline, Construct: "workspaces-object"}}
	case len(m.raw) == 0 || m.raw[0] != '[':
		return nil, nil, []Reason{{File: file, Line: m.vline, Construct: "workspaces-form"}}
	}
	var items []string
	var lines []int
	dec := json.NewDecoder(bytes.NewReader(m.raw))
	if _, err := dec.Token(); err != nil {
		return nil, nil, []Reason{{File: file, Line: m.vline, Construct: "workspaces-form"}}
	}
	for dec.More() {
		tok, err := dec.Token()
		line := lineAt(doc, m.voff+int(dec.InputOffset()))
		s, ok := tok.(string)
		if err != nil || !ok {
			return nil, nil, []Reason{{File: file, Line: line, Construct: "workspaces-form"}}
		}
		items = append(items, s)
		lines = append(lines, line)
	}
	return items, lines, nil
}

// buildJS: see the file comment. Unparsable package.json files and any
// construct outside the grammar make the kind unsupported (not fail-stop).
func buildJS(src map[string][]byte) (KindStatus, []StructNode, []StructEdge) {
	st := KindStatus{Kind: "js", Grammar: "js/v1", Reasons: []Reason{}}
	rootDoc, hasRoot := src["package.json"]
	yamlDoc, hasPnpm := src["pnpm-workspace.yaml"]
	if !hasRoot && !hasPnpm {
		st.Status = "absent"
		return st, nil, nil
	}
	reason := func(file string, line int, c string) {
		st.Reasons = append(st.Reasons, Reason{File: file, Line: line, Construct: c})
	}
	parsed := map[string][]jsonMember{}
	invalid := map[string]bool{}
	object := func(file string) ([]jsonMember, bool) {
		if invalid[file] {
			return nil, false
		}
		if ms, ok := parsed[file]; ok {
			return ms, true
		}
		doc := src[file]
		ms, err := jsonObject(doc, 0, doc)
		if err != nil {
			reason(file, 0, "json-invalid")
			invalid[file] = true
			return nil, false
		}
		dupKeys(file, ms, reason)
		parsed[file] = ms
		return ms, true
	}

	var pats []wsPattern
	declared := false
	addPatterns := func(file string, items []string, lines []int) {
		for i, s := range items {
			p, bad := parsePattern(s)
			if bad != "" {
				reason(file, lines[i], bad)
				continue
			}
			p.file, p.line = file, lines[i]
			pats = append(pats, p)
		}
	}
	if hasRoot {
		ms, _ := object("package.json")
		for _, m := range ms {
			if m.key != "workspaces" {
				continue
			}
			declared = true
			items, lines, rs := jsWorkspaces(rootDoc, m)
			st.Reasons = append(st.Reasons, rs...)
			addPatterns("package.json", items, lines)
		}
	}
	if hasPnpm {
		if declared {
			reason("pnpm-workspace.yaml", 0, "workspaces-declared-twice")
		}
		declared = true
		items, lines, rs := pnpmPatterns(yamlDoc)
		st.Reasons = append(st.Reasons, rs...)
		addPatterns("pnpm-workspace.yaml", items, lines)
	}

	var manifests []string
	for f := range src {
		if path.Base(f) == "package.json" && f != "package.json" {
			manifests = append(manifests, f)
		}
	}
	sort.Strings(manifests)
	var dirs []string
	member := map[string]bool{}
	if hasRoot {
		dirs, member["."] = append(dirs, "."), true
	}
	for _, p := range pats {
		matched := false
		for _, f := range manifests {
			d := path.Dir(f)
			if !p.match(".", d) {
				continue
			}
			matched = true
			if !member[d] {
				dirs, member[d] = append(dirs, d), true
			}
		}
		if !p.glob && !matched {
			reason(p.file, p.line, "workspace-member-missing")
		}
	}
	sort.Strings(dirs)

	var pkgs []jsPkg
	for _, d := range dirs {
		file := path.Join(d, "package.json")
		ms, ok := object(file)
		if !ok {
			continue
		}
		pkg := jsPkg{dir: d, file: file}
		for _, m := range ms {
			switch {
			case m.key == "name":
				if json.Unmarshal(m.raw, &pkg.name) != nil || pkg.name == "" {
					reason(file, m.vline, "name-form")
				}
				pkg.nameLine = m.line
			case isDepTable(m.key):
				deps, err := jsonObject(src[file], m.voff, m.raw)
				if err != nil {
					reason(file, m.vline, "dep-table-form")
					continue
				}
				dupKeys(file, deps, reason)
				for _, dep := range deps {
					var spec string
					if json.Unmarshal(dep.raw, &spec) != nil {
						reason(file, dep.vline, "dep-spec-form")
						continue
					}
					pkg.deps = append(pkg.deps, jsDep{table: m.key, name: dep.key, spec: spec, line: dep.line})
				}
			}
		}
		pkgs = append(pkgs, pkg)
	}

	byName := map[string]jsPkg{}
	for _, p := range pkgs {
		if p.name == "" {
			continue
		}
		if other, dup := byName[p.name]; dup {
			reason(p.file, p.nameLine, "package-name-duplicate:"+other.file)
			continue
		}
		byName[p.name] = p
	}
	id := func(dir string) string { return "js-package:" + dir }
	var nodes []StructNode
	var edges []StructEdge
	seen := map[StructEdge]bool{}
	for _, p := range pkgs {
		nodes = append(nodes, StructNode{ID: id(p.dir), Kind: "js-package", Path: p.dir, Name: p.name})
		// A peer range is a constraint, not a link: it is unambiguous when the
		// same package links that workspace package explicitly elsewhere (the
		// publishable-peer shape: peer "^0.5" + dev "workspace:*").
		linked := map[string]bool{}
		for _, dep := range p.deps {
			if internal, named := byName[dep.name]; named && jsExplicit(dep.spec) {
				if target, ok := jsTarget(p.dir, dep, byName, member); ok && target == internal.dir {
					linked[dep.name] = true
				}
			}
		}
		for _, dep := range p.deps {
			target, ok := jsTarget(p.dir, dep, byName, member)
			if !ok && dep.table == "peerDependencies" && linked[dep.name] && !jsExplicit(dep.spec) {
				target, ok = byName[dep.name].dir, true
			}
			if !ok {
				reason(p.file, dep.line, "ambiguous-internal-spec")
				continue
			}
			if target == "" || target == p.dir {
				continue
			}
			e := StructEdge{From: id(p.dir), To: id(target), Kind: "dependsOn", DepType: dep.table}
			if !seen[e] {
				seen[e] = true
				edges = append(edges, e)
			}
		}
	}
	return finish(st, nodes, edges)
}

// jsTarget resolves a dependency to a workspace package directory: "" with ok
// for an external dependency, false when the link is ambiguous.
func jsTarget(from string, dep jsDep, byName map[string]jsPkg, member map[string]bool) (string, bool) {
	internal, named := byName[dep.name]
	if strings.HasPrefix(dep.spec, "workspace:") {
		return internal.dir, named
	}
	rel, local := strings.CutPrefix(dep.spec, "file:")
	if !local {
		rel, local = strings.CutPrefix(dep.spec, "link:")
	}
	if local {
		dir := path.Join(from, rel)
		if member[dir] {
			return dir, !named || internal.dir == dir
		}
	}
	return "", !named
}

func jsExplicit(spec string) bool {
	return strings.HasPrefix(spec, "workspace:") || strings.HasPrefix(spec, "file:") || strings.HasPrefix(spec, "link:")
}

func isDepTable(key string) bool {
	for _, t := range jsDepTables {
		if key == t {
			return true
		}
	}
	return false
}

func dupKeys(file string, ms []jsonMember, reason func(string, int, string)) {
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m.key] {
			reason(file, m.line, "duplicate-key:"+m.key)
		}
		seen[m.key] = true
	}
}
