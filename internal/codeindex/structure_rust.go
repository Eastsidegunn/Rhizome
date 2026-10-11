package codeindex

// RHZ-102b (FR-RHZ-179): the Rust and Rust [workspace] rows of design rev5
// §3.1. Every Cargo.toml outside target/ with `[package] name = "…"` is a
// crate. Edges are dependsOn{depType} from the three dependency tables, taken
// only from one-line inline tables with a path to an in-repo crate. Inside a
// dependency table every line must be blank, a comment, `name = "ver"` or a
// one-line inline table; one unrecognised line turns the kind unsupported. A
// dependency naming an in-repo crate without a path is ambiguous-internal-spec.
// [workspace] members/exclude accept the one-line array and the one-string-
// per-line array; the crates found must equal the declared membership
// (workspace-mismatch otherwise).

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

var cargoDepTables = []string{"dependencies", "dev-dependencies", "build-dependencies"}

func rustInput(p string) bool {
	if path.Base(p) != "Cargo.toml" {
		return false
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "target" {
			return false
		}
	}
	return true
}

type cargoDep struct {
	table, key, pkg, path string
	hasPath               bool
	line                  int
}

type wsItem struct {
	s    string
	line int
}

type cargoWorkspace struct {
	line             int
	membersLine      int
	members, exclude []wsItem
	keys             map[string]bool
}

type cargoManifest struct {
	file, dir, name string
	nameLine        int
	deps            []cargoDep
	ws              *cargoWorkspace
}

var (
	tomlHeader      = regexp.MustCompile(`^\[(\[?)[ \t]*([^\[\]]+?)[ \t]*\](\]?)[ \t]*(?:#.*)?$`)
	tomlKey         = regexp.MustCompile(`^([A-Za-z0-9_-]+)[ \t]*(\.|=)[ \t]*`)
	cargoName       = regexp.MustCompile(`^name[ \t]*=[ \t]*"([^"\\]+)"[ \t]*(?:#.*)?$`)
	cargoVersionDep = regexp.MustCompile(`^([A-Za-z0-9_-]+)[ \t]*=[ \t]*"[^"\\]*"[ \t]*(?:#.*)?$`)
	cargoInlineDep  = regexp.MustCompile(`^([A-Za-z0-9_-]+)[ \t]*=[ \t]*\{(.*)$`)
	wsArrayOpen     = regexp.MustCompile(`^(members|exclude|default-members)[ \t]*=[ \t]*\[[ \t]*(?:#.*)?$`)
	wsArrayItem     = regexp.MustCompile(`^"([^"\\]*)"[ \t]*,?[ \t]*(?:#.*)?$`)
	wsArrayClose    = regexp.MustCompile(`^\][ \t]*(?:#.*)?$`)
	wsOneLine       = regexp.MustCompile(`^(members|exclude|default-members)[ \t]*=[ \t]*(\[.*)$`)
	wsResolver      = regexp.MustCompile(`^resolver[ \t]*=[ \t]*"[^"\\]*"[ \t]*(?:#.*)?$`)
)

func isCargoDepTable(name string) bool {
	for _, t := range cargoDepTables {
		if name == t {
			return true
		}
	}
	return false
}

func blankOrComment(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.HasPrefix(s, "#")
}

// tomlString reads a basic string without escapes at the start of s.
func tomlString(s string) (string, string, string) {
	switch {
	case strings.HasPrefix(s, "'"):
		return "", "", "literal-string"
	case !strings.HasPrefix(s, `"`):
		return "", "", "dep-line"
	}
	end := strings.IndexByte(s[1:], '"')
	if end < 0 {
		return "", "", "dep-line"
	}
	v := s[1 : 1+end]
	if strings.Contains(v, `\`) {
		return "", "", "escape-string"
	}
	return v, s[2+end:], ""
}

// tomlStrings reads a one-line array of basic strings at the start of s
// (trailing comma allowed).
func tomlStrings(s string) ([]string, string, string) {
	s = strings.TrimLeft(strings.TrimPrefix(s, "["), " \t")
	var items []string
	for {
		if rest, ok := strings.CutPrefix(s, "]"); ok {
			return items, rest, ""
		}
		v, rest, bad := tomlString(s)
		if bad != "" {
			return nil, "", bad
		}
		items = append(items, v)
		s = strings.TrimLeft(rest, " \t")
		if rest, ok := strings.CutPrefix(s, ","); ok {
			s = strings.TrimLeft(rest, " \t")
		} else if !strings.HasPrefix(s, "]") {
			return nil, "", "dep-line"
		}
	}
}

// parseInline reads the body of a one-line inline table after "{": bare
// keys, values that are basic strings, booleans or arrays of basic strings.
// It returns the string values and every key seen.
func parseInline(s string) (map[string]string, map[string]bool, string) {
	kv := map[string]string{}
	seen := map[string]bool{}
	s = strings.TrimLeft(s, " \t")
	if rest, ok := strings.CutPrefix(s, "}"); ok {
		if !blankOrComment(rest) {
			return nil, nil, "dep-line"
		}
		return kv, seen, ""
	}
	for {
		m := tomlKey.FindStringSubmatch(s)
		switch {
		case m == nil:
			if strings.HasPrefix(s, "'") {
				return nil, nil, "literal-string"
			}
			return nil, nil, "dep-line"
		case m[2] == ".":
			return nil, nil, "dotted-key"
		case m[1] == "workspace":
			return nil, nil, "workspace-inherit"
		case seen[m[1]]:
			return nil, nil, "dep-key-repeated"
		}
		seen[m[1]] = true
		s = s[len(m[0]):]
		var rest, bad string
		switch {
		case strings.HasPrefix(s, "["):
			_, rest, bad = tomlStrings(s)
		case strings.HasPrefix(s, "true") || strings.HasPrefix(s, "false"):
			rest = strings.TrimPrefix(strings.TrimPrefix(s, "true"), "false")
		default:
			var v string
			v, rest, bad = tomlString(s)
			kv[m[1]] = v
		}
		if bad != "" {
			return nil, nil, bad
		}
		s = strings.TrimLeft(rest, " \t")
		if rest, ok := strings.CutPrefix(s, ","); ok {
			s = strings.TrimLeft(rest, " \t")
			continue
		}
		if rest, ok := strings.CutPrefix(s, "}"); ok && blankOrComment(rest) {
			return kv, seen, ""
		}
		return nil, nil, "dep-line"
	}
}

// cargoDepLine recognises one non-blank, non-comment dependency table line.
func cargoDepLine(l string) (cargoDep, string) {
	if m := cargoVersionDep.FindStringSubmatch(l); m != nil {
		return cargoDep{key: m[1]}, ""
	}
	if m := cargoInlineDep.FindStringSubmatch(l); m != nil {
		kv, keys, bad := parseInline(m[2])
		if bad != "" {
			return cargoDep{}, bad
		}
		for _, k := range []string{"path", "package"} {
			if keys[k] && kv[k] == "" {
				return cargoDep{}, "dep-line"
			}
		}
		return cargoDep{key: m[1], pkg: kv["package"], path: kv["path"], hasPath: keys["path"]}, ""
	}
	switch k := tomlKey.FindStringSubmatch(l); {
	case k != nil && k[2] == "." && strings.HasPrefix(l[len(k[0]):], "workspace"):
		return cargoDep{}, "workspace-inherit"
	case k != nil && k[2] == ".":
		return cargoDep{}, "dotted-key"
	case strings.Contains(l, "'"):
		return cargoDep{}, "literal-string"
	case strings.Contains(l, `\`):
		return cargoDep{}, "escape-string"
	}
	return cargoDep{}, "dep-line"
}

// rootDotted are the root-level dotted keys that would define a table this
// grammar reads.
var rootDotted = []string{"package", "workspace", "target", "dependencies", "dev-dependencies", "build-dependencies"}

func (w *cargoWorkspace) set(key string, items []wsItem, line int) []Reason {
	var rs []Reason
	if w.keys[key] {
		rs = append(rs, Reason{Line: line, Construct: "workspace-key-repeated"})
	}
	w.keys[key] = true
	switch key {
	case "members":
		w.members, w.membersLine = items, line
	case "exclude":
		w.exclude = items
	}
	return rs
}

// parseCargo reads one Cargo.toml line by line. Tables other than [package],
// the dependency tables and [workspace] are skipped, tracking only multi-line
// strings so their contents are not taken for headers.
func parseCargo(file string, b []byte) (cargoManifest, []Reason) {
	m := cargoManifest{file: file, dir: path.Dir(file)}
	var rs []Reason
	add := func(line int, c string) { rs = append(rs, Reason{File: file, Line: line, Construct: c}) }
	table, ml, arr, arrLine := "", "", "", 0
	var arrItems []wsItem
	seenTables := map[string]bool{}
	depKeys := map[string]bool{}
	hasPackage := false
	for i, raw := range strings.Split(string(b), "\n") {
		n := i + 1
		l := strings.TrimSpace(raw)
		if ml != "" {
			if strings.Contains(l, ml) {
				ml = ""
			}
			continue
		}
		if arr != "" {
			switch {
			case blankOrComment(l):
			case wsArrayClose.MatchString(l):
				for _, r := range m.ws.set(arr, arrItems, arrLine) {
					add(r.Line, r.Construct)
				}
				arr = ""
			default:
				if mm := wsArrayItem.FindStringSubmatch(l); mm != nil {
					arrItems = append(arrItems, wsItem{mm[1], n})
				} else {
					add(n, "workspace-array-line")
				}
			}
			continue
		}
		if blankOrComment(l) {
			continue
		}
		if strings.HasPrefix(l, "[") {
			h := tomlHeader.FindStringSubmatch(l)
			if h == nil {
				add(n, "table-header")
				table = "[?]"
				continue
			}
			name, array := h[2], h[1] == "["
			head, _, dotted := strings.Cut(name, ".")
			switch {
			case head == "target":
				add(n, "target-table")
			case strings.ContainsAny(name, `"'`):
				add(n, "table-header-quoted")
			case name == "dev_dependencies" || name == "build_dependencies":
				add(n, "dep-table-alias")
			case dotted && isCargoDepTable(head):
				add(n, "dep-table-dotted")
			}
			table = name
			if array {
				table = "[[" + name + "]]"
			} else {
				if seenTables[name] {
					add(n, "table-repeated")
				}
				seenTables[name] = true
			}
			switch table {
			case "package":
				hasPackage = true
			case "workspace":
				m.ws = &cargoWorkspace{line: n, keys: map[string]bool{}}
			}
			continue
		}
		switch {
		case table == "package":
			if k := tomlKey.FindStringSubmatch(l); k != nil && k[1] == "name" {
				if mm := cargoName.FindStringSubmatch(l); mm != nil && m.nameLine == 0 {
					m.name, m.nameLine = mm[1], n
				} else {
					add(n, "package-name-form")
				}
				continue
			}
		case isCargoDepTable(table):
			dep, bad := cargoDepLine(l)
			if bad != "" {
				add(n, bad)
				continue
			}
			if depKeys[table+"\x00"+dep.key] {
				add(n, "dep-repeated")
			}
			depKeys[table+"\x00"+dep.key] = true
			dep.table, dep.line = table, n
			m.deps = append(m.deps, dep)
			continue
		case table == "workspace":
			if mm := wsArrayOpen.FindStringSubmatch(l); mm != nil {
				arr, arrLine, arrItems = mm[1], n, nil
				continue
			}
			if mm := wsOneLine.FindStringSubmatch(l); mm != nil {
				items, rest, bad := tomlStrings(mm[2])
				if bad != "" || !blankOrComment(rest) {
					add(n, "workspace-line")
					continue
				}
				var its []wsItem
				for _, s := range items {
					its = append(its, wsItem{s, n})
				}
				for _, r := range m.ws.set(mm[1], its, n) {
					add(r.Line, r.Construct)
				}
				continue
			}
			if !wsResolver.MatchString(l) {
				add(n, "workspace-line")
			}
			continue
		case table == "":
			if k := tomlKey.FindStringSubmatch(l); k != nil && k[2] == "." {
				for _, p := range rootDotted {
					if k[1] == p {
						add(n, "dotted-key")
					}
				}
			}
		}
		for _, d := range []string{`"""`, `'''`} {
			if strings.Count(l, d)%2 == 1 {
				ml = d
				break
			}
		}
	}
	if arr != "" {
		add(arrLine, "workspace-array-unclosed")
	}
	if hasPackage && m.nameLine == 0 {
		add(0, "package-name-missing")
	}
	if !hasPackage {
		m.name = ""
	}
	return m, rs
}

func buildRust(tree []TreeEntry, src map[string][]byte) (KindStatus, []StructNode, []StructEdge) {
	st := KindStatus{Kind: "rust", Grammar: "rust/v1", Reasons: []Reason{}}
	var files []string
	for _, e := range tree {
		if _, ok := src[e.Path]; ok && rustInput(e.Path) {
			files = append(files, e.Path)
		}
	}
	if len(files) == 0 {
		st.Status = "absent"
		return st, nil, nil
	}
	sort.Strings(files)
	reason := func(file string, line int, c string) {
		st.Reasons = append(st.Reasons, Reason{File: file, Line: line, Construct: c})
	}
	var mans []cargoManifest
	crates := map[string]cargoManifest{}
	byName := map[string]string{}
	norm := func(s string) string { return strings.ReplaceAll(s, "_", "-") }
	malformed := map[string]bool{}
	for _, f := range files {
		m, rs := parseCargo(f, src[f])
		st.Reasons = append(st.Reasons, rs...)
		malformed[f] = len(rs) != 0
		mans = append(mans, m)
		if m.name == "" {
			continue
		}
		crates[m.dir] = m
		if other, dup := byName[norm(m.name)]; dup {
			reason(f, m.nameLine, "crate-name-duplicate:"+crates[other].file)
			continue
		}
		byName[norm(m.name)] = m.dir
	}
	var dirs []string
	for d := range crates {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	id := func(dir string) string { return "rust-crate:" + dir }
	var nodes []StructNode
	var edges []StructEdge
	seen := map[StructEdge]bool{}
	pathDeps := map[string][]string{}
	for _, d := range dirs {
		m := crates[d]
		nodes = append(nodes, StructNode{ID: id(d), Kind: "rust-crate", Path: d, Name: m.name})
		for _, dep := range m.deps {
			name := dep.key
			if dep.pkg != "" {
				name = dep.pkg
			}
			internal, named := byName[norm(name)]
			if !dep.hasPath {
				if named {
					reason(m.file, dep.line, "ambiguous-internal-spec")
				}
				continue
			}
			target := path.Join(d, dep.path)
			if target == ".." || strings.HasPrefix(target, "../") || path.IsAbs(dep.path) {
				if named {
					reason(m.file, dep.line, "ambiguous-internal-spec")
				}
				continue
			}
			if _, ok := crates[target]; !ok {
				reason(m.file, dep.line, "path-dep-unresolved")
				continue
			}
			if named && internal != target {
				reason(m.file, dep.line, "ambiguous-internal-spec")
				continue
			}
			if target == d {
				continue
			}
			pathDeps[d] = append(pathDeps[d], target)
			e := StructEdge{From: id(d), To: id(target), Kind: "dependsOn", DepType: dep.table}
			if !seen[e] {
				seen[e] = true
				edges = append(edges, e)
			}
		}
	}

	var roots []cargoManifest
	for _, m := range mans {
		if m.ws != nil {
			roots = append(roots, m)
		}
	}
	// A declaration outside the grammar is already a reason; comparing its
	// partial membership would only add noise.
	owner := func(dir string) string {
		best := ""
		found := false
		for _, r := range roots {
			if dirWithin(dir, r.dir) && (!found || len(r.dir) > len(best) || best == ".") {
				best, found = r.dir, true
			}
		}
		return best
	}
	for _, r := range roots {
		if malformed[r.file] {
			continue
		}
		for _, c := range workspaceMismatch(r, crates, dirs, pathDeps, owner, reason) {
			line := r.ws.membersLine
			if line == 0 {
				line = r.ws.line
			}
			reason(r.file, line, "workspace-mismatch:"+c)
		}
	}
	return finish(st, nodes, edges)
}

// workspaceMismatch returns the crate directories on which the declared
// membership of workspace root r (members − exclude, plus the root package
// and, as cargo does, the path dependencies of members inside the workspace)
// and the crates found under r disagree.
func workspaceMismatch(r cargoManifest, crates map[string]cargoManifest, dirs []string, pathDeps map[string][]string,
	owner func(string) string, reason func(string, int, string)) []string {
	valid := true
	patterns := func(items []wsItem) []wsPattern {
		var ps []wsPattern
		for _, it := range items {
			p, bad := parsePattern(it.s)
			if bad != "" {
				reason(r.file, it.line, bad)
				valid = false
				continue
			}
			p.line = it.line
			ps = append(ps, p)
		}
		return ps
	}
	members, exclude := patterns(r.ws.members), patterns(r.ws.exclude)
	if !valid {
		return nil
	}
	excluded := func(dir string) bool {
		for _, p := range exclude {
			base := path.Join(r.dir, p.dir)
			if dirWithin(dir, base) && (!p.glob || dir != base) {
				return true
			}
		}
		return false
	}
	want := map[string]bool{}
	var missing []string
	for _, p := range members {
		matched := false
		for _, d := range dirs {
			if p.match(r.dir, d) {
				matched = true
				if !excluded(d) {
					want[d] = true
				}
			}
		}
		if !p.glob && !matched {
			missing = append(missing, path.Join(r.dir, p.dir))
		}
	}
	if _, ok := crates[r.dir]; ok {
		want[r.dir] = true
	}
	for queue := sortedKeys(want); len(queue) > 0; queue = queue[1:] {
		for _, t := range pathDeps[queue[0]] {
			if !want[t] && dirWithin(t, r.dir) && !excluded(t) {
				want[t] = true
				queue = append(queue, t)
			}
		}
	}
	diff := missing
	for _, d := range dirs {
		found := owner(d) == r.dir && !excluded(d)
		if found != want[d] {
			diff = append(diff, d)
		}
	}
	return diff
}

func sortedKeys(m map[string]bool) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
