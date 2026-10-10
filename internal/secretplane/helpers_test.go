package secretplane

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/secretwire"
)

// crashSignal is the panic value of an injected crash.
type crashSignal struct{}

// hooker is a re-armable hook; trace records every point passed.
type hooker struct {
	fn    func(point string) error
	trace []string
}

func (h *hooker) call(point string) error {
	h.trace = append(h.trace, point)
	if h.fn != nil {
		return h.fn(point)
	}
	return nil
}

// crashOn panics at the nth occurrence of point.
func (h *hooker) crashOn(point string, nth int) {
	n := 0
	h.fn = func(p string) error {
		if p == point {
			n++
			if n == nth {
				h.fn = nil
				panic(crashSignal{})
			}
		}
		return nil
	}
}

func (h *hooker) index(t *testing.T, point string) int {
	t.Helper()
	for i, p := range h.trace {
		if p == point {
			return i
		}
	}
	t.Fatalf("trace lacks %s: %v", point, h.trace)
	return -1
}

var fixedNow = time.Date(2026, 10, 10, 9, 30, 15, 987654321, time.FixedZone("x", 9*3600))

type harness struct {
	t   *testing.T
	dir string
	h   *hooker
	pl  *Plane
	// errs collects every error string seen (ZV scan).
	errs []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hs := &harness{t: t, dir: filepath.Join(t.TempDir(), "plane-state"), h: &hooker{}}
	hs.reopen()
	return hs
}

func (hs *harness) open() (*Plane, error) {
	pl, err := open(hs.dir, Options{Now: func() time.Time { return fixedNow }}, seams{hook: hs.h.call})
	hs.note(err)
	return pl, err
}

func (hs *harness) reopen() {
	hs.t.Helper()
	if hs.pl != nil {
		hs.pl.closeFiles()
	}
	pl, err := hs.open()
	if err != nil {
		hs.t.Fatalf("open: %v", err)
	}
	hs.pl = pl
}

func (hs *harness) note(err error) {
	if err != nil {
		hs.errs = append(hs.errs, err.Error())
	}
}

// crash runs f, which must hit the armed crash; the plane is then dropped
// without any further write, as a killed process would be.
func (hs *harness) crash(f func()) {
	hs.t.Helper()
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(crashSignal); !ok {
					panic(r)
				}
				return
			}
			hs.t.Fatal("expected injected crash")
		}()
		f()
	}()
	hs.h.fn = nil
	hs.pl.closeFiles()
	hs.pl = nil
}

func (hs *harness) file(name string) []byte {
	b, err := os.ReadFile(filepath.Join(hs.dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		hs.t.Fatal(err)
	}
	return b
}

func (hs *harness) planeID() string {
	hs.t.Helper()
	p, err := hs.pl.HelloPlaneID()
	if err != nil {
		hs.t.Fatal(err)
	}
	return p
}

func (hs *harness) status() Status {
	hs.t.Helper()
	s, err := hs.pl.Status()
	if err != nil {
		hs.t.Fatal(err)
	}
	return s
}

func (hs *harness) poll(ack uint64, lr *secretwire.Rejected) error {
	p := hs.planeID()
	err := hs.pl.OnPoll(p, secretwire.Ack{PlaneID: p, PlaneEntrySeq: ack, JournalSeq: ack * 3}, lr)
	hs.note(err)
	return err
}

func (hs *harness) mustPoll(ack uint64, lr *secretwire.Rejected) {
	hs.t.Helper()
	if err := hs.poll(ack, lr); err != nil {
		hs.t.Fatalf("poll: %v", err)
	}
}

func (hs *harness) batch() []secretwire.Entry {
	hs.t.Helper()
	b, err := hs.pl.NextBatch(hs.planeID(), 64)
	if err != nil {
		hs.t.Fatalf("batch: %v", err)
	}
	return b
}

func (hs *harness) apply(p secretwire.Pending, d Decision) Result {
	hs.t.Helper()
	r, err := hs.pl.Apply(p, d)
	if err != nil {
		hs.t.Fatalf("apply: %v", err)
	}
	return r
}

// observeN appends n observed facts for ref.
func (hs *harness) observeN(ref string, n int) {
	hs.t.Helper()
	for range n {
		if _, err := hs.pl.Observe(ref); err != nil {
			hs.t.Fatalf("observe: %v", err)
		}
	}
}

// seed registers one ref with value v and fills the outbox to n entries.
func (hs *harness) seed(v []byte, n int) string {
	hs.t.Helper()
	p := regPending(newOpIDT(hs.t))
	hs.apply(p, Decision{Value: v})
	hs.observeN(p.Ref, n-1)
	return p.Ref
}

func (hs *harness) quarantineKeys() []string {
	hs.t.Helper()
	items, err := hs.pl.QuarantineList()
	if err != nil {
		hs.t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if it.Seq != 0 {
			out = append(out, it.Reason+":"+itoa(it.Seq))
		} else {
			out = append(out, it.Reason+":"+it.OpID)
		}
	}
	return out
}

func itoa(n uint64) string { return string(appendUint(nil, n)) }

func appendUint(b []byte, n uint64) []byte {
	if n >= 10 {
		b = appendUint(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

func newOpIDT(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "op-" + hex.EncodeToString(b)
}

func regPending(opID string) secretwire.Pending {
	return secretwire.Pending{OpID: opID, Op: secretwire.OpRegister, Ref: secretwire.DeriveRef(opID),
		Name: "svc-key", RefKind: secretwire.RefKindProviderKey, ScopeTags: []string{"alpha", "beta"}, JournalSeq: 4}
}

func rotPending(opID, ref string) secretwire.Pending {
	return secretwire.Pending{OpID: opID, Op: secretwire.OpRotate, Ref: ref, Name: "svc-key",
		RefKind: secretwire.RefKindProviderKey, JournalSeq: 9}
}

func revPending(opID, ref string) secretwire.Pending {
	return secretwire.Pending{OpID: opID, Op: secretwire.OpRevoke, Ref: ref, Name: "svc-key",
		RefKind: secretwire.RefKindProviderKey, ReasonCode: secretwire.ReasonExposed, JournalSeq: 11}
}

// sentinel is the ZV sentinel "rhzsntl-" + 32 hex (crypto/rand).
func sentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

// zvForms are the §8.1 forms of s.
func zvForms(s string) []string {
	sum := sha256.Sum256([]byte(s))
	return []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
}

func assertNoForms(t *testing.T, what string, data []byte, s string) {
	t.Helper()
	for i, f := range zvForms(s) {
		if n := bytes.Count(data, []byte(f)); n != 0 {
			t.Fatalf("ZV: %s contains sentinel form %d (%d times)", what, i, n)
		}
	}
}

// allEntries flattens every outbox entry ever appended (all planeIds).
func (hs *harness) allEntries() []OutboxEntry {
	hs.pl.mu.Lock()
	defer hs.pl.mu.Unlock()
	var out []OutboxEntry
	for _, p := range hs.pl.st.order {
		for _, e := range hs.pl.st.outbox[p] {
			out = append(out, OutboxEntry{PlaneID: p, Entry: e})
		}
	}
	return out
}

func (hs *harness) outcomes(opID string) []secretwire.Entry {
	var out []secretwire.Entry
	for _, e := range hs.allEntries() {
		if e.Entry.OpID == opID && e.Entry.Fact == secretwire.FactOutcome {
			out = append(out, e.Entry)
		}
	}
	return out
}

func joinErrs(errs []string) []byte { return []byte(strings.Join(errs, "\n")) }
