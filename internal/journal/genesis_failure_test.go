package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"rhizome/internal/events"
	"rhizome/internal/trust"
	"testing"
	"time"
)

const anchorJSON148 = `{"format":"rhizome-trust-anchor-v1","principal":"H","algorithm":"ed25519","assurance":"key","publicKey":"MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`

type captureTrustGuard148 struct {
	inner *trust.Verifier
	ids   []string
}

func (g *captureTrustGuard148) capture(next events.Event) {
	if next.Type != trust.TrustGenesisType {
		return
	}
	var payload struct{ JournalID string }
	if json.Unmarshal(next.Payload, &payload) == nil {
		g.ids = append(g.ids, payload.JournalID)
	}
}

func (g *captureTrustGuard148) CheckAppend(prefix events.View, next events.Event, now time.Time) error {
	g.capture(next)
	return g.inner.CheckAppend(prefix, next, now)
}

func (g *captureTrustGuard148) CheckReplay(prefix events.View, next events.Event) error {
	g.capture(next)
	return g.inner.CheckReplay(prefix, next)
}

type partialFile148 struct {
	journalFile
	n int
}

func (f *partialFile148) Write(p []byte) (int, error) {
	if f.n > len(p) {
		f.n = len(p)
	}
	n, _ := f.journalFile.Write(p[:f.n])
	return n, errors.New("partial write")
}

func TestGenesisFailureCasesFRRHZ148(t *testing.T) {
	anchor, err := trust.ParseAnchor([]byte(anchorJSON148))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("full line then sync failure is adopted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.ndjson")
		guard := &captureTrustGuard148{inner: trust.NewAnchored(anchor)}
		j, err := OpenGuarded(path, guard)
		if err != nil {
			t.Fatal(err)
		}
		j.file = &faultFile{journalFile: j.file, syncErr: errors.New("sync")}
		if err := trust.EnsureGenesis(j, anchor); !errors.Is(err, events.ErrPoisoned) {
			t.Fatalf("sync failure = %v", err)
		}
		attempted := guard.ids[0]
		_ = j.Close()
		reopened, err := OpenGuarded(path, trust.NewAnchored(anchor))
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		summary, err := trust.NewAnchored(anchor).TrustSummary(reopened)
		if err != nil || summary == nil || summary.JournalID != attempted {
			t.Fatalf("adopted summary=%+v err=%v attempted=%s", summary, err, attempted)
		}
	})

	t.Run("partial line makes next open fail", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.ndjson")
		j, err := OpenGuarded(path, trust.NewAnchored(anchor))
		if err != nil {
			t.Fatal(err)
		}
		j.file = &partialFile148{journalFile: j.file, n: 17}
		if err := trust.EnsureGenesis(j, anchor); !errors.Is(err, events.ErrPoisoned) {
			t.Fatalf("partial failure = %v", err)
		}
		_ = j.Close()
		if _, err := OpenGuarded(path, trust.NewAnchored(anchor)); err == nil {
			t.Fatal("partial genesis reopened")
		}
	})

	t.Run("zero byte failure retries with a new id", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.ndjson")
		firstGuard := &captureTrustGuard148{inner: trust.NewAnchored(anchor)}
		j, err := OpenGuarded(path, firstGuard)
		if err != nil {
			t.Fatal(err)
		}
		j.file = &faultFile{journalFile: j.file, writeErr: errors.New("zero")}
		if err := trust.EnsureGenesis(j, anchor); !errors.Is(err, events.ErrPoisoned) {
			t.Fatalf("zero-byte failure = %v", err)
		}
		failedID := firstGuard.ids[0]
		_ = j.Close()
		secondGuard := &captureTrustGuard148{inner: trust.NewAnchored(anchor)}
		reopened, err := OpenGuarded(path, secondGuard)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if err := trust.EnsureGenesis(reopened, anchor); err != nil {
			t.Fatal(err)
		}
		if len(secondGuard.ids) != 1 || secondGuard.ids[0] == failedID {
			t.Fatalf("failed=%s retry=%v", failedID, secondGuard.ids)
		}
	})

	t.Run("exit before listen leaves one idempotent genesis", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.ndjson")
		j, err := OpenGuarded(path, trust.NewAnchored(anchor))
		if err != nil {
			t.Fatal(err)
		}
		if err := trust.EnsureGenesis(j, anchor); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		_ = j.Close()
		reopened, err := OpenGuarded(path, trust.NewAnchored(anchor))
		if err != nil {
			t.Fatal(err)
		}
		if err := trust.EnsureGenesis(reopened, anchor); err != nil {
			t.Fatal(err)
		}
		_ = reopened.Close()
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatal("restart appended a second genesis")
		}
	})
}
