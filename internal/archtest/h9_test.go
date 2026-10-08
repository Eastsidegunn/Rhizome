package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestNoBoolVerifiedWriteAPIFRRHZ153(t *testing.T) {
	allowed := map[string]bool{
		"approval.Ref.ActorVerified":   true,
		"approval.input.ActorVerified": true,
		"edge.Edge.Verified":           true,
		"edge.payload.Verified":        true,
		"peercred.Verified.verified":   true,
	}
	found := map[string]bool{}
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkg := entry.Name()
		files, err := os.ReadDir(filepath.Join("..", pkg))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", pkg, file.Name()), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range parsed.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && ast.IsExported(fn.Name.Name) && fn.Type.Params != nil {
					for _, parameter := range fn.Type.Params.List {
						if !boolOrBoolPointer(parameter.Type) {
							continue
						}
						for _, name := range parameter.Names {
							if strings.Contains(strings.ToLower(name.Name), "verified") {
								found[pkg+"."+fn.Name.Name+"("+name.Name+")"] = true
							}
						}
					}
				}
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					typeSpec := spec.(*ast.TypeSpec)
					structure, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, field := range structure.Fields.List {
						if !boolOrBoolPointer(field.Type) {
							continue
						}
						for _, name := range field.Names {
							if strings.Contains(strings.ToLower(name.Name), "verified") {
								found[pkg+"."+typeSpec.Name.Name+"."+name.Name] = true
							}
						}
					}
				}
			}
		}
	}
	var unexpected, missing []string
	for name := range found {
		if !allowed[name] {
			unexpected = append(unexpected, name)
		}
	}
	for name := range allowed {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)
	if len(unexpected) != 0 || len(missing) != 0 {
		t.Fatalf("unexpected=%v missing=%v", unexpected, missing)
	}
}

func boolOrBoolPointer(expression ast.Expr) bool {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	ident, ok := expression.(*ast.Ident)
	return ok && ident.Name == "bool"
}
