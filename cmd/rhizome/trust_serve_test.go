package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"rhizome/internal/journal"
	"rhizome/internal/trust"
	"strings"
	"testing"
	"time"
)

const serveAnchorJSON148 = `{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`

func writeServeAnchor148(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTrustAnchorServeExitClassificationFRRHZ148(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.json")
	writeServeAnchor148(t, invalid, `{}`)
	journalPath := filepath.Join(dir, "invalid.ndjson")
	var stderr bytes.Buffer
	if code := serveCtx(context.Background(), []string{"-journal", journalPath, "-trust-anchor", invalid, "-addr", "bad-address"}, io.Discard, &stderr); code != 2 || stderr.String() != "invalid trust anchor\nformat\n" {
		t.Fatalf("invalid anchor exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(journalPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("invalid anchor reached lock: %v", err)
	}
	stderr.Reset()
	if code := serveCtx(context.Background(), []string{"-journal", journalPath, "-trust-anchor", "", "-addr", "bad-address"}, io.Discard, &stderr); code != 2 || stderr.String() != "invalid trust anchor\nformat\n" {
		t.Fatalf("empty anchor exit=%d stderr=%q", code, stderr.String())
	}

	valid := filepath.Join(dir, "valid.json")
	writeServeAnchor148(t, valid, serveAnchorJSON148)
	journalPath = filepath.Join(dir, "valid.ndjson")
	stderr.Reset()
	if code := serveCtx(context.Background(), []string{"-journal", journalPath, "-trust-anchor", valid, "-addr", "bad-address"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("pre-listen exit=%d stderr=%q", code, stderr.String())
	}
	otherPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	der, _ := x509.MarshalPKIXPublicKey(otherPrivate.Public())
	otherRaw, _ := json.Marshal(trust.Anchor{Format: trust.AnchorFormat, Principal: "H", Algorithm: trust.AlgorithmEd25519, Assurance: trust.AssuranceKey, PublicKey: base64.StdEncoding.EncodeToString(der)})
	other := filepath.Join(dir, "other.json")
	writeServeAnchor148(t, other, string(otherRaw))
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := serveCtx(ctx, []string{"-journal", journalPath, "-trust-anchor", other, "-addr", "127.0.0.1:0"}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "trust anchor does not match genesis") || strings.Contains(stderr.String(), "listen tcp") {
		t.Fatalf("mismatched anchor exit=%d stderr=%q", code, stderr.String())
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("mismatched anchor changed journal bytes")
	}
}

func TestTrustAnchorFailureReasonCategoriesFRRHZ148(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want string
	}{
		{err: "invalid trust anchor: wrong owner", want: "owner"},
		{err: "invalid trust anchor: owner unavailable", want: "owner"},
		{err: "invalid trust anchor: insecure permissions", want: "permissions"},
		{err: "invalid trust anchor: file too large", want: "size"},
		{err: "invalid trust anchor: invalid fields", want: "format"},
		{err: "invalid trust anchor: not a regular file", want: "not-regular"},
	} {
		if got := trustAnchorFailureReason(errors.New(tc.err)); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.err, got, tc.want)
		}
	}
}

func TestGenesisRestartViaServeIdempotentFRRHZ148(t *testing.T) {
	dir := t.TempDir()
	anchorPath := filepath.Join(dir, "anchor.json")
	journalPath := filepath.Join(dir, "journal.ndjson")
	writeServeAnchor148(t, anchorPath, serveAnchorJSON148)
	for i := 0; i < 2; i++ {
		var stderr bytes.Buffer
		if code := serveCtx(context.Background(), []string{"-journal", journalPath, "-trust-anchor", anchorPath, "-addr", "bad-address"}, io.Discard, &stderr); code != 1 {
			t.Fatalf("boot %d exit=%d stderr=%q", i, code, stderr.String())
		}
	}
	anchor, _ := trust.ParseAnchor([]byte(serveAnchorJSON148))
	j, err := journal.OpenGuarded(journalPath, trust.NewAnchored(anchor))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got := j.List(trust.TrustAggregateType, trust.TrustAggregateID); len(got) != 1 {
		t.Fatalf("genesis count = %d", len(got))
	}
}

func TestServeDoubleStartSingleGenesisFRRHZ148(t *testing.T) {
	dir := t.TempDir()
	anchorPath := filepath.Join(dir, "anchor.json")
	journalPath := filepath.Join(dir, "journal.ndjson")
	writeServeAnchor148(t, anchorPath, serveAnchorJSON148)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan int, 1)
	var firstErr bytes.Buffer
	go func() {
		firstDone <- serveCtx(ctx, []string{"-journal", journalPath, "-trust-anchor", anchorPath, "-addr", "127.0.0.1:0"}, io.Discard, &firstErr)
	}()
	waitLock(t, journalPath+".lock", true)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(journalPath); err == nil && info.Size() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var secondErr bytes.Buffer
	if code := serveCtx(context.Background(), []string{"-journal", journalPath, "-trust-anchor", anchorPath, "-addr", "127.0.0.1:0"}, io.Discard, &secondErr); code != 1 {
		t.Fatalf("second serve exit=%d stderr=%q", code, secondErr.String())
	}
	cancel()
	waitDone(t, firstDone, &firstErr, 10*time.Second)
	anchor, _ := trust.ParseAnchor([]byte(serveAnchorJSON148))
	j, err := journal.OpenGuarded(journalPath, trust.NewAnchored(anchor))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got := len(j.List(trust.TrustAggregateType, trust.TrustAggregateID)); got != 1 {
		t.Fatalf("genesis count = %d", got)
	}
}

func TestNoGenesisWritePathFRRHZ148(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	fset := token.NewFileSet()
	var callSites []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if rel != "cmd" && rel != "internal" && !strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) && !strings.HasPrefix(rel, "internal"+string(filepath.Separator)) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident:
				name = fun.Name
			}
			if name == "EnsureGenesis" {
				position := fset.Position(call.Pos())
				rel, _ := filepath.Rel(root, position.Filename)
				callSites = append(callSites, rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callSites) != 1 || filepath.ToSlash(callSites[0]) != "cmd/rhizome/main.go" {
		t.Fatalf("production EnsureGenesis call sites = %v", callSites)
	}
}
