package trust_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"rhizome/internal/approval"
	"rhizome/internal/assembly"
	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/question"
	"rhizome/internal/surface"
	"rhizome/internal/trust"
	"rhizome/internal/workspace"
	"sort"
	"strings"
	"testing"
)

var (
	_ func(approval.Service, approval.RequestKey, approval.Decision, string, string, string, string, string, string, trust.Authority, ...*question.Verification) (approval.Ref, error)                              = approval.Service.RecordInput
	_ func(approval.Service, approval.RequestKey, approval.Decision, string, string, string, string, string, string, trust.Authority, approval.GateFields, ...*question.Verification) (approval.Ref, error)         = approval.Service.RecordInputWithGate
	_ func(approval.Service, string, approval.RequestKey, approval.Decision, string, string, string, string, string, string, trust.Authority, approval.GateFields, ...*question.Verification) (approval.Ref, error) = approval.Service.Supersede

	_ func(events.Port, workspace.Intent, string, trust.Authority) (workspace.RelayResult, error)                         = workspace.RelayIntent
	_ func(events.Port, workspace.Intent, string, trust.Authority, workspace.ExecInjector) (workspace.RelayResult, error) = workspace.RelayIntentWith
	_ func(events.Port, workspace.Intent, string, trust.Authority, workspace.RelayHooks) (workspace.RelayResult, error)   = workspace.RelayIntentHooks

	_ func(surface.Service, string, string, string, trust.Authority, string) (surface.State, error) = surface.Service.Instruct
	_ func(edge.Service, edge.Spec, trust.Authority) (edge.Edge, error)                             = edge.Service.Create
	_ func(edge.Service, string, edge.Spec, trust.Authority) (edge.Edge, error)                     = edge.Service.Rewire
	_ trust.Authority                                                                               = assembly.RunSpec{}.Authority
)

func TestH9WriterSignaturesFRRHZ153(t *testing.T) {
	runSpec := reflect.TypeOf(assembly.RunSpec{})
	want := map[string]reflect.Type{
		"ProcedureID": reflect.TypeOf(""), "GoalID": reflect.TypeOf(""), "RunID": reflect.TypeOf(""),
		"Params": reflect.TypeOf(map[string]string{}), "Actor": reflect.TypeOf(""),
		"Authority": reflect.TypeOf(trust.Authority{}), "Correlation": reflect.TypeOf(""),
	}
	if runSpec.NumField() != len(want) {
		t.Fatalf("assembly.RunSpec has %d fields, want %d", runSpec.NumField(), len(want))
	}
	for name, fieldType := range want {
		field, ok := runSpec.FieldByName(name)
		if !ok || field.Type != fieldType {
			t.Fatalf("assembly.RunSpec.%s = %v, present=%v", name, field.Type, ok)
		}
	}
}

func TestAuthorityFieldsUnexportedFRRHZ153(t *testing.T) {
	typeOf := reflect.TypeOf(trust.Authority{})
	if typeOf.NumField() == 0 {
		t.Fatal("Authority must remain an opaque struct, not an empty marker")
	}
	for i := 0; i < typeOf.NumField(); i++ {
		if typeOf.Field(i).PkgPath == "" {
			t.Fatalf("Authority field %q is exported", typeOf.Field(i).Name)
		}
	}
}

func TestNewWritesNeverSetLegacyVerifiedFRRHZ153(t *testing.T) {
	store := &events.Store{}
	key := approval.RequestKey{TraceID: "1123456789abcdef0123456789abcdef", SpanID: "1123456789abcdef", RequestID: "h9"}
	input, err := (approval.Service{Store: store}).RecordInput(key, approval.Allow, "", "resp", "digest", "operator", "", "", trust.Authority{})
	if err != nil {
		t.Fatal(err)
	}
	if input.ActorVerified || input.ActorRef != "unverified-local-operator:operator" {
		t.Fatalf("new approval asserted legacy verification: %+v", input)
	}
	missions := mission.Service{Store: store}
	if _, err := missions.CreateGoal("g1", "one", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := missions.CreateGoal("g2", "two", "done", ""); err != nil {
		t.Fatal(err)
	}
	declared, err := (edge.Service{Store: store}).Create(edge.Spec{ID: "e", From: edge.Endpoint{Type: "goal", ID: "g1"}, To: edge.Endpoint{Type: "goal", ID: "g2"}, Kind: edge.Contains, Actor: "operator", Correlation: "h9"}, trust.Authority{})
	if err != nil {
		t.Fatal(err)
	}
	if declared.Verified || declared.Actor != "unverified-local-operator:operator" {
		t.Fatalf("new edge asserted legacy verification: %+v", declared)
	}
}

func TestTrustExportedAPIAllowlistFRRHZ153(t *testing.T) {
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool {
		return filepath.Ext(info.Name()) == ".go" && !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := packages["trust"]
	if pkg == nil {
		t.Fatal("trust package not parsed")
	}
	set := map[string]bool{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			switch declaration := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch item := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(item.Name.Name) {
							set[item.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, name := range item.Names {
							if ast.IsExported(name.Name) {
								set[name.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				if ast.IsExported(declaration.Name.Name) {
					set[declaration.Name.Name] = true
				}
			}
		}
	}
	got := make([]string, 0, len(set))
	for name := range set {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"AddMessage", "AlgorithmECDSAP256", "AlgorithmEd25519", "Anchor", "AnchorFormat", "Anchored",
		"ApprovalInputRecordedType", "AssuranceKey", "AttestAggregateID", "AttestAggregateType", "AttestItem",
		"AttestManifestBytes", "AttestMessage", "AttestMessageTag", "AttestRecordedType", "Attestation", "Attestations",
		"Authority", "CheckAppend", "CheckReplay",
		"DecisionMessage", "DecisionMessageTag", "DecisionStatus", "EnsureGenesis", "ErrInvalidAnchor", "KeyID", "List", "LoadAnchor",
		"ManifestDigest", "NewAnchored", "NewAnchorless", "NewUnanchored", "ParseAnchor", "QuestionAnsweredType",
		"RecordKeyChange", "RevokeMessage", "Signature", "Summary", "TrustAddedType", "TrustAggregateID", "TrustAggregateType", "TrustGenesisType",
		"TrustKeyMessageTag", "TrustRevokedType", "TrustSummary", "Verifier", "VerifyDecision", "VerifySignature",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported trust API\n got: %v\nwant: %v", got, want)
	}
}
