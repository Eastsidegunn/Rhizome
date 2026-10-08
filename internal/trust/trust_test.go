package trust

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testAnchorJSON = `{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`

func writeAnchorFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestAnchorMetadataSeamUIDFRRHZ146(t *testing.T) {
	valid := anchorFileInfo{regular: true, uid: 1000, mode: 0o600, size: int64(len(testAnchorJSON))}
	if err := validateAnchorFileInfo(valid, 1000); err != nil {
		t.Fatal(err)
	}
	valid.uid = 1001
	if err := validateAnchorFileInfo(valid, 1000); err == nil || !strings.Contains(err.Error(), "wrong owner") {
		t.Fatalf("uid mismatch = %v", err)
	}
}

func TestAuthorityInternalConstructorFRRHZ153(t *testing.T) {
	if newAuthority() == (Authority{}) {
		t.Fatal("internal authority constructor returned the unverified zero value")
	}
}

func TestAnchorRealDescriptorRulesFRRHZ146(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("descriptor ownership and fifo rules are unix-only")
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "anchor.json")
	writeAnchorFile(t, valid, testAnchorJSON, 0o600)
	if _, err := LoadAnchor(valid); err != nil {
		t.Fatalf("valid anchor: %v", err)
	}

	large := filepath.Join(dir, "large.json")
	writeAnchorFile(t, large, strings.Repeat("x", 4097), 0o600)
	if _, err := LoadAnchor(large); err == nil {
		t.Fatal("oversized anchor accepted")
	}

	if _, err := LoadAnchor(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory error = %v, want not a regular file", err)
	}
	fifo := filepath.Join(dir, "anchor.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoadAnchor(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("fifo error = %v, want not a regular file", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fifo without writer blocked")
	}

	link := filepath.Join(dir, "anchor-link.json")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAnchor(link); err != nil {
		t.Fatalf("symlink target descriptor was not validated: %v", err)
	}
}

func TestAnchorRefusesGroupWriteOnlyFRRHZ146(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("descriptor permission rules are unix-only")
	}
	path := filepath.Join(t.TempDir(), "group-writable.json")
	writeAnchorFile(t, path, testAnchorJSON, 0o620)
	if err := os.Chmod(path, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAnchor(path); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("group-write-only error = %v, want insecure permissions", err)
	}
}

func TestAnchorRefusesOtherWriteOnlyFRRHZ146(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("descriptor permission rules are unix-only")
	}
	path := filepath.Join(t.TempDir(), "other-writable.json")
	writeAnchorFile(t, path, testAnchorJSON, 0o602)
	if err := os.Chmod(path, 0o602); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAnchor(path); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("other-write-only error = %v, want insecure permissions", err)
	}
}

func TestAnchorExactKeysAndConstructorsFRRHZ146(t *testing.T) {
	bad := []string{
		`{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key"}`,
		`{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=","extra":1}`,
		`{"Format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`,
	}
	for _, raw := range bad {
		if _, err := ParseAnchor([]byte(raw)); err == nil {
			t.Fatalf("invalid anchor accepted: %s", raw)
		}
	}
	anchor, err := ParseAnchor([]byte(testAnchorJSON))
	if err != nil {
		t.Fatal(err)
	}
	if got := NewAnchored(anchor); got.mode != modeAnchored || got.statusCap != "verified" {
		t.Fatalf("anchored = %+v", got)
	}
	if got := NewAnchorless(); got.mode != modeAnchorless || got.statusCap != "" {
		t.Fatalf("anchorless = %+v", got)
	}
	if got := NewUnanchored(); got.mode != modeUnanchored || got.statusCap != "chain-valid" {
		t.Fatalf("unanchored = %+v", got)
	}
}
