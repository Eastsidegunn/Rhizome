package secretplane

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/secretwire"
)

// TestListQuarantineReadOnlyFRRHZ139: `quarantine list` reads the summary
// without opening the plane: busy while a plane holds the lock, value-free,
// writes nothing (an unterminated last line is skipped, not truncated).
func TestListQuarantineReadOnlyFRRHZ139(t *testing.T) {
	s := sentinel(t)
	hs := newHarness(t)
	ref := hs.seed([]byte(s), 3)
	if err := hs.pl.Freeze(); err != nil {
		t.Fatal(err)
	}
	hs.mustPoll(1, nil) // C = 1: seq 2 and 3 are quarantined, then split

	if _, err := ListQuarantine(hs.dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	hs.pl.Close()
	hs.pl = nil

	qpath := filepath.Join(hs.dir, quarantineName)
	f, err := os.OpenFile(qpath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"k":"entry"`)
	f.Close()
	before := hs.file(quarantineName)
	led := hs.file(ledgerName)

	items, err := ListQuarantine(hs.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Seq != 2 || items[1].Seq != 3 || items[0].Ref != ref ||
		items[0].Fact != secretwire.FactObserved || items[0].Reason != QuarantineAfterCutoff {
		t.Fatalf("items %+v", items)
	}
	out := FormatQuarantineList(items)
	assertNoForms(t, "list", []byte(out), s)
	if !bytes.Equal(before, hs.file(quarantineName)) || !bytes.Equal(led, hs.file(ledgerName)) {
		t.Fatal("ListQuarantine wrote")
	}

	// unsafe directory mode and relative path are refused
	if _, err := ListQuarantine("rel"); !errors.Is(err, ErrStateDirUnsafe) {
		t.Fatal("relative dir accepted")
	}
	os.Chmod(hs.dir, 0o750)
	defer os.Chmod(hs.dir, 0o700)
	if _, err := ListQuarantine(hs.dir); !errors.Is(err, ErrStateDirUnsafe) {
		t.Fatal("0750 dir accepted")
	}
}

// TestRefsSummaryFRRHZ139: Refs lists value-free summaries with origin.
func TestRefsSummaryFRRHZ139(t *testing.T) {
	s := sentinel(t)
	hs := newHarness(t)
	ref := hs.seed([]byte(s), 1)
	res, err := hs.pl.DeclareAttested("broker", secretwire.RefKindProviderKey, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := hs.pl.Refs()
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs %v %v", refs, err)
	}
	for _, r := range refs {
		switch r.Ref {
		case ref:
			if r.Attested() || r.Version != 1 || r.Origin != originRequest {
				t.Fatalf("%+v", r)
			}
		case res.Ref:
			if !r.Attested() || r.Name != "broker" || len(r.ScopeTags) != 1 {
				t.Fatalf("%+v", r)
			}
		default:
			t.Fatal("unknown ref")
		}
	}
}
