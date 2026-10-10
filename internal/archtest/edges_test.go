package archtest

// RHZ-107 G1 FR-RHZ-136: SCP §7 R8 — crafted CheckEdges cases (a), a
// tree-level mutation test (b) and the committed real-tree gate (c).

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const ri = "rhizome/internal/"

// (a) every §7 violation is reported and every allowed edge passes.
func TestCheckEdgesCraftedFRRHZ136(t *testing.T) {
	cases := []struct {
		from, to string
		bad      bool
	}{
		// §7 (a) violations.
		{"internal/secretwire", ri + "events", true},
		{"internal/secretwire", "golang.org/x/sys/unix", true},
		{"internal/secretwiretest", ri + "journal", true},
		{"internal/secretplane", ri + "journal", true},
		{"internal/secretplane/sub", ri + "events", true},
		{"cmd/rhizome-secret-plane", ri + "secretref", true},
		{"cmd/rhizome", ri + "secretplane", true},
		{"internal/workspace", ri + "secretplane", true},
		// Further violations: non-stdlib dotless paths and the module root.
		{"internal/secretwire", "corp/pkg", true},
		{"internal/secretwire", "rhizome", true},
		{"internal/secretwire", ri + "peercred", true},
		{"internal/workspace", ri + "secretplane/sub", true},
		{"cmd/rhizome-secret-plane/sub", ri + "journal", true},
		// Allowed edges.
		{"internal/secretwire", "encoding/json", false},
		{"internal/secretwire", "crypto/sha256", false},
		{"internal/secretwiretest", ri + "secretwire", false},
		{"internal/secretwiretest", ri + "peercred", false},
		{"internal/secretwiretest", "net", false},
		{"internal/secretplane", ri + "peercred", false},
		{"internal/secretplane", ri + "secretwire", false},
		{"internal/secretplane", ri + "termio", false},
		{"internal/secretplane", ri + "secretplane/sub", false},
		{"internal/secretplane/sub", ri + "secretplane", false},
		{"internal/secretplane/sub", "os", false},
		{"cmd/rhizome-secret-plane", ri + "secretplane", false},
		{"cmd/rhizome-secret-plane", ri + "secretplane/tty", false},
		{"cmd/rhizome-secret-plane", ri + "secretwire", false},
		{"cmd/rhizome-secret-plane", ri + "peercred", false},
		{"cmd/rhizome-secret-plane", ri + "termio", false},
		{"cmd/rhizome-secret-plane/sub", ri + "secretwire", false},
		// Packages outside the table keep only the legacy rules.
		{"internal/workspace", ri + "events", false},
		{"internal/termio", "golang.org/x/term", false},
		{"cmd/rhizome", ri + "journal", false},
		{"internal/secretref", ri + "secretwire", false},
	}
	for _, c := range cases {
		got := CheckEdges(map[string][]string{c.from: {c.to}})
		want := []string{}
		if c.bad {
			want = []string{"edge:" + c.from + "->" + c.to}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s -> %s: got %v want %v", c.from, c.to, got, want)
		}
	}
	if isStdlib("corp/pkg") || isStdlib("rhizome/internal/events") || isStdlib("golang.org/x/sys") || !isStdlib("net/http") {
		t.Fatal("stdlib judgment wrong")
	}
}

// (b) a mini module tree proves nested packages and cmd/ are scanned.
func TestScanTreeMutationFRRHZ136(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                              "module rhizome\n",
		"internal/secretplane/sub/x.go":       "package sub\nimport (\n\t\"os\"\n\t\"rhizome/internal/journal\"\n)\n",
		"internal/secretplane/sub/x_test.go":  "package sub\nimport _ \"rhizome/internal/workspace\"\n",
		"internal/secretplane/p.go":           "package secretplane\nimport _ \"rhizome/internal/secretplane/sub\"\n",
		"cmd/rhizome-secret-plane/main.go":    "package main\nimport (\n\t_ \"rhizome/internal/events\"\n\t_ \"rhizome/internal/secretwire\"\n)\n",
		"cmd/rhizome/main.go":                 "package main\nimport _ \"rhizome/internal/secretplane\"\n",
		"internal/secretwire/w.go":            "package secretwire\nimport \"encoding/json\"\n",
		"internal/workspace/w.go":             "package workspace\nimport (\n\t_ \"corp/pkg\"\n\t_ \"rhizome/internal/events\"\n)\n",
		"internal/workspace/testdata/fix.go":  "package fix\nimport _ \"rhizome/internal/secretplane\"\n",
		"internal/events/e.go":                "package events\n",
		"cmd/rawimp/main.go":                  "package main\nimport _ `rhizome/internal/secretplane`\n",
		"internal/events/only_test/e_test.go": "package only\n",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := scanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"edge:cmd/rawimp->rhizome/internal/secretplane", // raw-literal import (Unquote)
		"edge:cmd/rhizome->rhizome/internal/secretplane",
		"edge:cmd/rhizome-secret-plane->rhizome/internal/events",
		"edge:internal/secretplane/sub->rhizome/internal/journal",
	}
	if got := CheckEdges(tree); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !reflect.DeepEqual(tree["internal/workspace"], []string{"corp/pkg", ri + "events"}) || isStdlib("corp/pkg") {
		t.Fatalf("corp/pkg must be scanned and judged non-stdlib: %v", tree["internal/workspace"])
	}
	if _, ok := tree["internal/events/only_test"]; ok {
		t.Fatal("a test-only directory became a package")
	}
	wantLegacy := map[string][]string{"events": {}, "secretwire": {}, "workspace": {"events"}}
	if got := reduceLegacy(tree); !reflect.DeepEqual(got, wantLegacy) {
		t.Fatalf("reduced map: got %v want %v", got, wantLegacy)
	}
}

// (c) the committed real-tree gate: R8 holds, and the map handed to the
// legacy Check equals the previous one-level scanner's output (after
// dropping secretplane and edges into it, as §7 item 2 does).
func TestRepoEdgesFRRHZ136(t *testing.T) {
	tree, err := scanTree(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"cmd/rhizome", "internal/coordinator", "internal/peercred"} {
		if _, ok := tree[pkg]; !ok {
			t.Fatalf("scanTree missed %s", pkg)
		}
	}
	if got := CheckEdges(tree); len(got) != 0 {
		t.Fatalf("R8 violations:\n%s", strings.Join(got, "\n"))
	}
	legacy := scanInternal(t)
	delete(legacy, "secretplane")
	for pkg, imports := range legacy {
		kept := []string{}
		for _, imp := range imports {
			if imp != "secretplane" {
				kept = append(kept, imp)
			}
		}
		legacy[pkg] = kept
	}
	if got := reduceLegacy(tree); !reflect.DeepEqual(got, legacy) {
		t.Fatalf("reduced map differs from the legacy scanner:\n got %v\nwant %v", got, legacy)
	}
}

// FR-RHZ-136: the §7 layer rows are pinned; secretplane stays out of Layers
// and Allowed is unchanged.
func TestSecretLayersPinnedFRRHZ136(t *testing.T) {
	want := map[string]string{"secretwire": "substrate", "secretwiretest": "substrate", "secretref": "exec", "secretchannel": "exec"}
	for pkg, layer := range want {
		if Layers[pkg] != layer {
			t.Fatalf("%s layer = %q, want %q", pkg, Layers[pkg], layer)
		}
	}
	if _, ok := Layers["secretplane"]; ok {
		t.Fatal("secretplane must not be in Layers")
	}
	if !reflect.DeepEqual(Allowed, []string{"coordinator->memory", "coordinator->source"}) {
		t.Fatalf("Allowed changed: %v", Allowed)
	}
}
