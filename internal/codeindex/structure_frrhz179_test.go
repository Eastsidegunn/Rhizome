package codeindex

// RHZ-102b FR-RHZ-179: JS/TS workspace·Rust crate 구조 (설계 rev5 §3.1).
// fixture는 전부 t.TempDir()에 합성한다(체크인 fixture 0). 실레포 모양은
// 출처 주석으로만 둔다: Gunnflow(pnpm apps/*·packages/*·testing/*),
// mmx = humanfriend(루트 crate mmx + wasm/ crate mmx-wasm, `mmx = { path = ".." }`).

import (
	"strconv"
	"strings"
	"testing"
)

func repoGraph(t *testing.T, files map[string]string) StructGraph {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	for name, content := range files {
		writeFile(t, dir, name, content)
	}
	commitAll(t, dir, "init")
	g, err := graphAt(t, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// kindGraph renders the nodes and edges whose ids start with prefix.
func kindGraph(g StructGraph, prefix string) (string, string) {
	var nodes, edges []string
	for _, n := range g.Nodes {
		if strings.HasPrefix(n.ID, prefix) {
			nodes = append(nodes, n.ID+"|"+n.Name)
		}
	}
	for _, e := range g.Edges {
		if strings.HasPrefix(e.From, prefix) {
			edges = append(edges, e.From+">"+e.To+":"+e.Kind+"{"+e.DepType+"}")
		}
	}
	return strings.Join(nodes, "\n"), strings.Join(edges, "\n")
}

func reasonList(st KindStatus) string {
	var rs []string
	for _, r := range st.Reasons {
		rs = append(rs, r.File+":"+strconv.Itoa(r.Line)+":"+r.Construct)
	}
	return strings.Join(rs, ",")
}

// unsupported: 그 kind의 노드·엣지 0(부분 그래프 금지), fallback은 남음, reasons 일치.
func wantUnsupported(t *testing.T, g StructGraph, k, prefix, reasons string) {
	t.Helper()
	st := kind(g, k)
	if st.Status != "unsupported" || reasonList(st) != reasons {
		t.Fatalf("%s kind: %s %s, want %s", k, st.Status, reasonList(st), reasons)
	}
	if nodes, edges := kindGraph(g, prefix); nodes != "" || edges != "" {
		t.Fatalf("partial graph:\n%s\n%s", nodes, edges)
	}
}

func TestStructureJSPnpmWorkspaceFRRHZ179(t *testing.T) {
	g := repoGraph(t, map[string]string{
		"package.json":        `{"name": "root", "private": true, "devDependencies": {"typescript": "^5"}}`,
		"pnpm-workspace.yaml": "# workspace\npackages:\n  - apps/*\n  - 'packages/*'   # quoted\n\n  - \"tools/cli\"\n",
		"apps/web/package.json": `{
  "name": "@x/web",
  "dependencies": {"@x/contract": "workspace:*", "solid-js": "^1.9"},
  "devDependencies": {"@x/cli": "link:../../tools/cli"}
}`,
		"apps/notes/README.md": "not a package\n",
		"packages/contract/package.json": `{"name": "@x/contract", "peerDependencies": {"@x/core": "workspace:^"},
  "optionalDependencies": {"@x/core": "file:../core"}}`,
		"packages/core/package.json":         `{"name": "@x/core", "dependencies": {"@x/core": "workspace:*"}}`,
		"tools/cli/package.json":             `{"name": "@x/cli", "dependencies": {"left-pad": "1.0.0", "vendored": "file:../vendored.tgz"}}`,
		"tools/other/package.json":           `{"name": "@x/other"}`,
		"apps/web/node_modules/package.json": `not json`,
	})
	if st := kind(g, "js"); st.Status != "complete" || len(st.Reasons) != 0 || st.Grammar != "js/v1" {
		t.Fatalf("js kind: %+v", st)
	}
	nodes, edges := kindGraph(g, "js-package:")
	wantNodes := "js-package:.|root\njs-package:apps/web|@x/web\njs-package:packages/contract|@x/contract\njs-package:packages/core|@x/core\njs-package:tools/cli|@x/cli"
	if nodes != wantNodes {
		t.Fatalf("nodes:\n%s", nodes)
	}
	// 자기 의존은 엣지가 아니다; 외부 의존(semver·밖의 tgz)도 아니다.
	wantEdges := strings.Join([]string{
		"js-package:apps/web>js-package:packages/contract:dependsOn{dependencies}",
		"js-package:apps/web>js-package:tools/cli:dependsOn{devDependencies}",
		"js-package:packages/contract>js-package:packages/core:dependsOn{optionalDependencies}",
		"js-package:packages/contract>js-package:packages/core:dependsOn{peerDependencies}",
	}, "\n")
	if edges != wantEdges {
		t.Fatalf("edges:\n%s", edges)
	}
}

func TestStructureJSNpmWorkspacesAndRootOnlyFRRHZ179(t *testing.T) {
	g := repoGraph(t, map[string]string{
		"package.json":        `{"name": "r", "workspaces": ["libs/*", "app"], "dependencies": {"a": "workspace:*"}}`,
		"libs/a/package.json": `{"name": "a"}`,
		"app/package.json":    `{"name": "app", "dependencies": {"a": "file:../libs/a"}}`,
	})
	nodes, edges := kindGraph(g, "js-package:")
	if kind(g, "js").Status != "complete" || nodes != "js-package:.|r\njs-package:app|app\njs-package:libs/a|a" ||
		edges != "js-package:.>js-package:libs/a:dependsOn{dependencies}\njs-package:app>js-package:libs/a:dependsOn{dependencies}" {
		t.Fatalf("npm workspaces:\n%s\n%s\n%+v", nodes, edges, kind(g, "js"))
	}

	// Gunnflow upstream-port 모양: peer 범위 + 같은 패키지의 dev workspace:* 링크.
	g = repoGraph(t, map[string]string{
		"package.json":           `{"workspaces": ["libs/*"]}`,
		"libs/a/package.json":    `{"name": "a"}`,
		"libs/port/package.json": `{"name": "port", "peerDependencies": {"a": "^0.3.1 || ^0.4.0"}, "devDependencies": {"a": "workspace:*"}}`,
	})
	nodes, edges = kindGraph(g, "js-package:")
	if kind(g, "js").Status != "complete" ||
		edges != "js-package:libs/port>js-package:libs/a:dependsOn{devDependencies}\njs-package:libs/port>js-package:libs/a:dependsOn{peerDependencies}" {
		t.Fatalf("publishable peer:\n%s\n%+v", edges, kind(g, "js"))
	}

	// 선언 없음 → 루트 package.json 1개만. 하위 package.json은 보지 않는다.
	g = repoGraph(t, map[string]string{
		"package.json":     `{"name": "solo", "dependencies": {"sub": "^1"}}`,
		"sub/package.json": `{"name": "sub"}`,
	})
	nodes, edges = kindGraph(g, "js-package:")
	if kind(g, "js").Status != "complete" || nodes != "js-package:.|solo" || edges != "" {
		t.Fatalf("root only:\n%s\n%s", nodes, edges)
	}
}

func TestStructureJSUnsupportedFRRHZ179(t *testing.T) {
	member := map[string]string{"libs/a/package.json": `{"name": "a"}`}
	for _, c := range []struct {
		name    string
		files   map[string]string
		reasons string
	}{
		// 설계 §9: workspace 패키지 이름에 걸린 일반 semver 의존은 모호 → unsupported.
		{"semver internal dep", map[string]string{
			"package.json":        "{\n  \"workspaces\": [\"libs/*\", \"app\"]\n}\n",
			"app/package.json":    "{\n  \"name\": \"app\",\n  \"dependencies\": {\n    \"a\": \"^1.0.0\"\n  }\n}\n",
			"libs/a/package.json": `{"name": "a"}`,
		}, "app/package.json:4:ambiguous-internal-spec"},
		{"peer range without explicit link", map[string]string{
			"package.json":     `{"workspaces": ["libs/*", "app"]}`,
			"app/package.json": `{"name": "app", "peerDependencies": {"a": "^1"}, "devDependencies": {"a": "^1"}}`,
		}, "app/package.json:1:ambiguous-internal-spec,app/package.json:1:ambiguous-internal-spec"},
		{"file: to another dir", map[string]string{
			"package.json":     `{"workspaces": ["libs/*", "app"]}`,
			"app/package.json": `{"name": "app", "dependencies": {"a": "file:../elsewhere"}}`,
		}, "app/package.json:1:ambiguous-internal-spec"},
		{"workspace: unknown name", map[string]string{
			"package.json": `{"workspaces": ["libs/*"], "dependencies": {"zz": "workspace:*"}}`,
		}, "package.json:1:ambiguous-internal-spec"},
		{"negation", map[string]string{"package.json": `{"workspaces": ["libs/*", "!libs/b"]}`}, "package.json:1:workspace-negation"},
		{"globstar", map[string]string{"pnpm-workspace.yaml": "packages:\n  - 'libs/**'\n"}, "pnpm-workspace.yaml:2:workspace-globstar"},
		{"inner glob", map[string]string{"package.json": `{"workspaces": ["li*/a"]}`}, "package.json:1:workspace-glob"},
		{"object workspaces", map[string]string{"package.json": "{\n\"workspaces\": {\"packages\": [\"libs/*\"]}}"}, "package.json:2:workspaces-object"},
		{"flow yaml", map[string]string{"pnpm-workspace.yaml": "packages: [libs/*]\n"}, "pnpm-workspace.yaml:0:pnpm-packages-missing,pnpm-workspace.yaml:1:yaml-flow"},
		{"anchor", map[string]string{"pnpm-workspace.yaml": "packages: &p\n  - libs/*\n"}, "pnpm-workspace.yaml:0:pnpm-packages-missing,pnpm-workspace.yaml:1:yaml-anchor,pnpm-workspace.yaml:2:pnpm-workspace-line"},
		{"alias item", map[string]string{"pnpm-workspace.yaml": "packages:\n  - *p\n"}, "pnpm-workspace.yaml:2:yaml-alias"},
		{"other yaml key", map[string]string{"pnpm-workspace.yaml": "packages:\n  - libs/*\ncatalog:\n  x: 1\n"}, "pnpm-workspace.yaml:3:pnpm-workspace-line,pnpm-workspace.yaml:4:pnpm-workspace-line"},
		{"declared twice", map[string]string{"package.json": `{"workspaces": ["libs/*"]}`, "pnpm-workspace.yaml": "packages:\n  - libs/*\n"}, "pnpm-workspace.yaml:0:workspaces-declared-twice"},
		{"missing literal member", map[string]string{"package.json": `{"workspaces": ["libs/a", "nope"]}`}, "package.json:1:workspace-member-missing"},
		{"duplicate name", map[string]string{"package.json": `{"name": "a", "workspaces": ["libs/*"]}`}, "libs/a/package.json:1:package-name-duplicate:package.json"},
		{"duplicate key", map[string]string{"package.json": "{\"workspaces\": [\"libs/*\"],\n\"dependencies\": {\"x\": \"1\", \"x\": \"2\"}}"}, "package.json:2:duplicate-key:x"},
		{"invalid json", map[string]string{"package.json": `{"workspaces": ["libs/*"]}`, "libs/b/package.json": `{"name": `}, "libs/b/package.json:0:json-invalid"},
		{"dep table form", map[string]string{"package.json": `{"workspaces": ["libs/*"], "dependencies": ["a"]}`}, "package.json:1:dep-table-form"},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range member {
				files[k] = v
			}
			for k, v := range c.files {
				files[k] = v
			}
			g := repoGraph(t, files)
			wantUnsupported(t, g, "js", "js-package:", c.reasons)
			if len(g.Nodes) == 0 {
				t.Fatal("fallback nodes missing")
			}
		})
	}
}

// mmx(humanfriend) 모양: [workspace] 없음, 루트 crate + wasm/ crate, path 엣지 1개,
// target/ 아래 Cargo.toml은 제외, [package] 안 여러 줄 배열의 "/src/**"는 무시.
func TestStructureRustMmxShapeFRRHZ179(t *testing.T) {
	g := repoGraph(t, map[string]string{
		"Cargo.toml": `[package]
name = "mmx"
version = "0.4.3"
description = "x"
include = [
    "/Cargo.toml", "/src/**",
    "/skills/mmx/**",
]

[dependencies]
mermaid-rs-renderer = "=0.3.1"
clap = { version = "4", features = ["derive"] }
serde_json = "1"

[profile.release]
lto = true

# The profile that 'dist' will build with
[profile.dist]
inherits = "release"
`,
		"wasm/Cargo.toml": `[package]
name = "mmx-wasm"
publish = false

[lib]
crate-type = ["cdylib"]

[dependencies]
mmx = { path = ".." }
serde_json = "1"
`,
		"target/package/mmx-0.4.3/Cargo.toml": "[package]\nname = \"mmx\"\n",
	})
	nodes, edges := kindGraph(g, "rust-crate:")
	if st := kind(g, "rust"); st.Status != "complete" || len(st.Reasons) != 0 || st.Grammar != "rust/v1" {
		t.Fatalf("rust kind: %+v", st)
	}
	if nodes != "rust-crate:.|mmx\nrust-crate:wasm|mmx-wasm" || edges != "rust-crate:wasm>rust-crate:.:dependsOn{dependencies}" {
		t.Fatalf("mmx shape:\n%s\n%s", nodes, edges)
	}
}

// helper/는 members에 없지만 멤버의 path 의존이라 cargo가 자동 멤버로 삼는다.
func TestStructureRustWorkspaceFRRHZ179(t *testing.T) {
	g := repoGraph(t, map[string]string{
		"Cargo.toml": `[workspace]
resolver = "2"
members = [
    "crates/*", # all
    "tools/gen",
]
exclude = ["crates/skip"]
default-members = ["tools/gen"]

[workspace.package]
edition = "2021"
`,
		"crates/core/Cargo.toml": "[package]\nname = \"core\"\n",
		"crates/api/Cargo.toml": `[package]
name = "api"

[dependencies]
core = { path = "../core", version = "0.1" } # internal
renamed = { package = "gen", path = "../../tools/gen", optional = true }

[dev-dependencies]
core = { path = "../core" }

[build-dependencies]
helper = { path = "../../helper" }
`,
		"crates/skip/Cargo.toml": "[package]\nname = \"skip\"\n",
		"tools/gen/Cargo.toml":   "[package]\nname = \"gen\"\n",
		"helper/Cargo.toml":      "[package]\nname = \"helper\"\n",
	})
	if st := kind(g, "rust"); st.Status != "complete" {
		t.Fatalf("rust kind: %+v", st)
	}
	nodes, edges := kindGraph(g, "rust-crate:")
	wantNodes := "rust-crate:crates/api|api\nrust-crate:crates/core|core\nrust-crate:crates/skip|skip\nrust-crate:helper|helper\nrust-crate:tools/gen|gen"
	wantEdges := strings.Join([]string{
		"rust-crate:crates/api>rust-crate:crates/core:dependsOn{dependencies}",
		"rust-crate:crates/api>rust-crate:crates/core:dependsOn{dev-dependencies}",
		"rust-crate:crates/api>rust-crate:helper:dependsOn{build-dependencies}",
		"rust-crate:crates/api>rust-crate:tools/gen:dependsOn{dependencies}",
	}, "\n")
	if nodes != wantNodes || edges != wantEdges {
		t.Fatalf("workspace:\n%s\n%s", nodes, edges)
	}
}

func TestStructureRustUnsupportedFRRHZ179(t *testing.T) {
	lib := "[package]\nname = \"lib\"\n"
	app := func(deps string) string { return "[package]\nname = \"app\"\n\n[dependencies]\n" + deps }
	for _, c := range []struct {
		name    string
		files   map[string]string
		reasons string
	}{
		{"multi-line inline table", map[string]string{"app/Cargo.toml": app("x = {\n  version = \"1\" }\n")}, "app/Cargo.toml:5:dep-line,app/Cargo.toml:6:dep-line"},
		{"workspace inherit", map[string]string{"app/Cargo.toml": app("x = { workspace = true }\ny.workspace = true\n")}, "app/Cargo.toml:5:workspace-inherit,app/Cargo.toml:6:workspace-inherit"},
		{"dotted key", map[string]string{"app/Cargo.toml": app("x.version = \"1\"\n")}, "app/Cargo.toml:5:dotted-key"},
		{"literal string", map[string]string{"app/Cargo.toml": app("x = '1'\ny = { path = '../lib' }\n")}, "app/Cargo.toml:5:literal-string,app/Cargo.toml:6:literal-string"},
		{"escape string", map[string]string{"app/Cargo.toml": app("x = \"\\u0031\"\n")}, "app/Cargo.toml:5:escape-string"},
		{"target table", map[string]string{"app/Cargo.toml": app("") + "\n[target.'cfg(unix)'.dependencies]\nlib = { path = \"../lib\" }\n"}, "app/Cargo.toml:6:target-table"},
		{"dotted dep table", map[string]string{"app/Cargo.toml": app("") + "[dependencies.lib]\npath = \"../lib\"\n"}, "app/Cargo.toml:5:dep-table-dotted"},
		{"internal without path", map[string]string{"app/Cargo.toml": app("lib = \"0.1\"\n")}, "app/Cargo.toml:5:ambiguous-internal-spec"},
		{"renamed internal without path", map[string]string{"app/Cargo.toml": app("l = { package = \"lib\", version = \"0.1\" }\n")}, "app/Cargo.toml:5:ambiguous-internal-spec"},
		{"path to non crate", map[string]string{"app/Cargo.toml": app("x = { path = \"../docs\" }\n")}, "app/Cargo.toml:5:path-dep-unresolved"},
		// 설계 §9: 허용하지 않는 members 형식 → unsupported.
		{"members literal string", map[string]string{"Cargo.toml": "[workspace]\nmembers = ['lib']\n"}, "Cargo.toml:2:workspace-line"},
		{"members two strings per line", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\n  \"lib\", \"app\",\n]\n"}, "Cargo.toml:3:workspace-array-line"},
		{"members nested", map[string]string{"Cargo.toml": "[workspace]\nmembers = [[\"lib\"]]\n"}, "Cargo.toml:2:workspace-line"},
		{"members unclosed", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\n  \"lib\",\n"}, "Cargo.toml:2:workspace-array-unclosed"},
		{"members inner glob", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"l*b\"]\n"}, "Cargo.toml:2:workspace-glob"},
		{"unknown workspace key", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"lib\"]\nfoo = 1\n"}, "Cargo.toml:3:workspace-line"},
		{"mismatch: crate not a member", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"lib\"]\n", "app/Cargo.toml": app("")}, "Cargo.toml:2:workspace-mismatch:app"},
		{"mismatch: member not a crate", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"lib\", \"gone\"]\n"}, "Cargo.toml:2:workspace-mismatch:gone"},
		{"duplicate crate name", map[string]string{"app/Cargo.toml": "[package]\nname = \"lib\"\n"}, "lib/Cargo.toml:2:crate-name-duplicate:app/Cargo.toml"},
		{"package name form", map[string]string{"app/Cargo.toml": "[package]\nname = 'app'\n"}, "app/Cargo.toml:0:package-name-missing,app/Cargo.toml:2:package-name-form"},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"lib/Cargo.toml": lib}
			for k, v := range c.files {
				files[k] = v
			}
			g := repoGraph(t, files)
			wantUnsupported(t, g, "rust", "rust-crate:", c.reasons)
		})
	}
}

// 종류별 독립: Rust가 unsupported여도 JS·Go는 complete로 남는다.
func TestStructureKindsIndependentFRRHZ179(t *testing.T) {
	files := map[string]string{
		"go.mod":           "module m\n",
		"a/a.go":           "package a\n",
		"package.json":     `{"name": "r"}`,
		"crate/Cargo.toml": "[package]\nname = \"c\"\n\n[dependencies]\nx = {\n",
		"crate/src/lib.rs": "",
		"web/index.html":   "",
	}
	g := repoGraph(t, files)
	var order []string
	for _, k := range g.Kinds {
		order = append(order, k.Kind+"="+k.Status)
	}
	if got := strings.Join(order, " "); got != "go=complete js=complete rust=unsupported fallback=complete" {
		t.Fatalf("kinds: %s", got)
	}
	if nodes, _ := kindGraph(g, "js-package:"); nodes != "js-package:.|r" {
		t.Fatalf("js nodes: %s", nodes)
	}
	if reasonList(kind(g, "rust")) != "crate/Cargo.toml:5:dep-line" {
		t.Fatalf("rust reasons: %s", reasonList(kind(g, "rust")))
	}
}
