package archtest

// RHZ-055 FR-RHZ-085: 커널 경계 CI 강제 (P1~P4). 변이 probe(P5)는 모두 P4로
// 수렴하고, A7 예외 흡수(P6)는 assembly 쪽 의존 제거로 해소했다.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func check1(t *testing.T, imports map[string][]string) []string {
	t.Helper()
	return Check(imports, Layers, nil)
}

// P1: 규칙별 순수 단위 핀 — 위반 입력은 검출, 정상 입력은 0.
func TestRulesDetectViolationsFRRHZ085(t *testing.T) {
	cases := []struct {
		name    string
		imports map[string][]string
		want    []string
	}{
		// R1 양방향.
		{"R1 exec->knowledge", map[string][]string{"mission": {"knowledge"}}, []string{"mission->knowledge"}},
		{"R1 knowledge->exec", map[string][]string{"memory": {"decision"}}, []string{"memory->decision"}},
		{"R1 ok exec->exec", map[string][]string{"mission": {"domain"}}, []string{}},
		// R2.
		{"R2 substrate->exec", map[string][]string{"journal": {"mission"}}, []string{"journal->mission"}},
		{"R2 ok substrate->substrate", map[string][]string{"journal": {"events"}}, []string{}},
		// R3.
		{"R3 exec->surface", map[string][]string{"mission": {"workspace"}}, []string{"mission->workspace"}},
		{"R3 bridge->surface", map[string][]string{"trace": {"workspace"}}, []string{"trace->workspace"}},
		{"R3 substrate->surface", map[string][]string{"events": {"workspace"}}, []string{"events->workspace"}},
		// R6: assembly는 두 커널+bridge+substrate 가능, surface 불가.
		{"R6 assembly->surface", map[string][]string{"assembly": {"workspace"}}, []string{"assembly->workspace"}},
		{"R6 ok assembly imports", map[string][]string{"assembly": {"mission", "procedure", "events", "trace", "edge"}}, []string{}},
		// R7: assembly의 피-import는 surface만.
		{"R7 knowledge->assembly", map[string][]string{"procedure": {"assembly"}}, []string{"procedure->assembly"}},
		{"R7 exec->assembly", map[string][]string{"mission": {"assembly"}}, []string{"mission->assembly"}},
		{"R7 bridge->assembly", map[string][]string{"trace": {"assembly"}}, []string{"trace->assembly"}},
		{"R7 ok surface->assembly", map[string][]string{"workspace": {"assembly"}}, []string{}},
	}
	for _, c := range cases {
		if got := check1(t, c.imports); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// P2: R4 미분류 — 패키지·import 양쪽 모두.
func TestUnclassifiedPackagesFailFRRHZ085(t *testing.T) {
	if got := check1(t, map[string][]string{"zzz": {"events"}}); !reflect.DeepEqual(got, []string{"unclassified:zzz"}) {
		t.Fatalf("got %v", got)
	}
	if got := check1(t, map[string][]string{"mission": {"zzz"}}); !reflect.DeepEqual(got, []string{"unclassified:zzz"}) {
		t.Fatalf("got %v", got)
	}
}

// P3: R5 정확 일치 양방향 — 예외 흡수·초과 위반·유령(소멸) 예외.
func TestExceptionsExactMatchFRRHZ085(t *testing.T) {
	violating := map[string][]string{"coordinator": {"memory"}}
	if got := Check(violating, Layers, []string{"coordinator->memory"}); len(got) != 0 {
		t.Fatalf("exception not absorbed: %v", got)
	}
	if got := Check(violating, Layers, nil); !reflect.DeepEqual(got, []string{"coordinator->memory"}) {
		t.Fatalf("unlisted violation not reported: %v", got)
	}
	if got := Check(map[string][]string{}, Layers, []string{"ghost->ghost"}); !reflect.DeepEqual(got, []string{"stale exception: ghost->ghost"}) {
		t.Fatalf("stale exception not reported: %v", got)
	}
}

// scanInternal collects non-test imports of every internal/* package —
// parser.ImportsOnly, no external execution (§2).
func scanInternal(t *testing.T) map[string][]string {
	t.Helper()
	const prefix = "rhizome/internal/"
	out := map[string][]string{}
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkg := entry.Name()
		seen := map[string]bool{}
		files, err := os.ReadDir(filepath.Join("..", pkg))
		if err != nil {
			t.Fatal(err)
		}
		imports := []string{}
		for _, f := range files {
			// _test.go 미검사(§7-3): coordinator 테스트가 workspace를 import
			// 하는 현실에서 green이어야 한다 — 필터가 그 증명의 전제.
			if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", pkg, f.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if !strings.HasPrefix(path, prefix) {
					continue
				}
				name := strings.SplitN(strings.TrimPrefix(path, prefix), "/", 2)[0]
				if !seen[name] {
					seen[name] = true
					imports = append(imports, name)
				}
			}
		}
		sort.Strings(imports)
		out[pkg] = imports
	}
	return out
}

// P4: 실저장소 전체가 비준 경계를 지킨다 — 위반 정확히 0(coordinator 2건은
// allowed가 흡수, R5가 그 목록의 신선함까지 강제). 변이 probe 4종(금지
// import·예외 삭제·유령 예외·미분류 패키지)은 전부 이 테스트에서 FAIL한다.
func TestRepositoryObeysKernelBoundaryFRRHZ085(t *testing.T) {
	imports := scanInternal(t)
	// 스캐너 퇴행 방지: 조용히 0개를 읽으면 경계 검사는 공허하게 green이 된다.
	if len(imports) < 30 {
		t.Fatalf("scanner found only %d packages — scan broken?", len(imports))
	}
	if _, ok := imports["coordinator"]; !ok {
		t.Fatal("scanner missed coordinator")
	}
	if got := Check(imports, Layers, Allowed); len(got) != 0 {
		t.Fatalf("kernel boundary violations:\n%s", strings.Join(got, "\n"))
	}
}

func TestTrustImportsOnlyStdlibAndEventsFRRHZ146(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "trust", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var productionFiles int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		productionFiles++
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			importPath := strings.Trim(imported.Path.Value, `"`)
			if importPath == "rhizome/internal/events" {
				continue
			}
			first := strings.SplitN(importPath, "/", 2)[0]
			if strings.HasPrefix(importPath, "rhizome/") || strings.Contains(first, ".") {
				t.Errorf("%s imports %q; only stdlib and rhizome/internal/events are allowed", path, importPath)
			}
		}
	}
	if productionFiles == 0 {
		t.Fatal("no production Go files found in internal/trust")
	}
}

// FR-RHZ-154: request is an exec-kernel package; Allowed remains the exact
// pre-RHZ-118 exception list.
func TestRequestLayerPinnedFRRHZ154(t *testing.T) {
	if Layers["request"] != "exec" {
		t.Fatalf("request layer = %q, want exec", Layers["request"])
	}
	want := []string{"coordinator->memory", "coordinator->source"}
	if !reflect.DeepEqual(Allowed, want) {
		t.Fatalf("Allowed changed: got %v want %v", Allowed, want)
	}
}
