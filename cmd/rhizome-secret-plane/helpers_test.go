package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/secretplane"
	"rhizome/internal/secretplane/server"
	"rhizome/internal/secretwire"
)

// sentinel is the §8.1 ZV value: "rhzsntl-" + 32 hex from crypto/rand.
func sentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

func zvForms(s string) []string {
	sum := sha256.Sum256([]byte(s))
	return []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
}

func assertNoSentinel(t *testing.T, where string, data []byte, ss ...string) {
	t.Helper()
	for _, s := range ss {
		for _, f := range zvForms(s) {
			if bytes.Contains(data, []byte(f)) {
				t.Fatalf("%s contains a sentinel form", where)
			}
		}
	}
}

// forbidden wording (split plan §4 item 5, D19(d)).
func assertWording(t *testing.T, where, text string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, w := range []string{"approved", "usable", "human-confirmed", "human confirmed", "confirmed by a human"} {
		if strings.Contains(low, w) {
			t.Fatalf("%s uses forbidden wording", where)
		}
	}
}

func newOpID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "op-" + hex.EncodeToString(b)
}

// shortDir is a private directory with a short path (socket path limit).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return b
}

// syncBuf is a goroutine-safe output capture.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// passwords feeds scripted non-echo reads; reading past the script fails.
type passwords struct {
	mu    sync.Mutex
	vals  [][]byte
	t     *testing.T
	given [][]byte // every slice handed to the session (zeroing checks)
}

func (p *passwords) push(vs ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, v := range vs {
		p.vals = append(p.vals, []byte(v))
	}
}

func (p *passwords) read() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.vals) == 0 {
		p.t.Error("unexpected password read")
		return nil, io.EOF
	}
	v := p.vals[0]
	p.vals = p.vals[1:]
	p.given = append(p.given, v)
	return v, nil
}

// unit is a session over a real plane with a scripted queue (no socket).
type unit struct {
	t       *testing.T
	dir     string
	pl      *secretplane.Plane
	pending []secretwire.Pending
	pw      *passwords
	out     syncBuf
	fatals  int
}

func newUnit(t *testing.T) *unit {
	t.Helper()
	u := &unit{t: t, dir: filepath.Join(t.TempDir(), "plane"), pw: &passwords{t: t}}
	pl, err := secretplane.Open(u.dir, secretplane.Options{Now: func() time.Time {
		return time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u.pl = pl
	t.Cleanup(func() { pl.Close() })
	return u
}

// queue mimics the server: the latest pending list minus done/invalid.
func (u *unit) queue() []server.Item {
	var out []server.Item
	for _, p := range u.pending {
		st, err := u.pl.Classify(p)
		if err != nil {
			u.t.Fatal(err)
		}
		if st == secretplane.PendingDone || st == secretplane.PendingInvalid {
			continue
		}
		out = append(out, server.Item{Pending: p, Status: st})
	}
	return out
}

// run runs the session loop over script until end of input.
func (u *unit) run(script ...string) string {
	u.t.Helper()
	before := len(u.out.String())
	in := strings.NewReader(strings.Join(script, "\n") + "\n")
	s := newSession(u.pl, u.queue, in, &u.out, u.pw.read, func() { u.fatals++ })
	s.loop()
	if len(u.pw.vals) != 0 {
		u.t.Fatal("scripted values left unread")
	}
	return u.out.String()[before:]
}

func (u *unit) file(name string) []byte { return readFile(u.t, filepath.Join(u.dir, name)) }

func regPending(t *testing.T, name string, tags ...string) secretwire.Pending {
	op := newOpID(t)
	return secretwire.Pending{OpID: op, Op: secretwire.OpRegister, Ref: secretwire.DeriveRef(op), Name: name,
		RefKind: secretwire.RefKindProviderKey, ScopeTags: tags, JournalSeq: 1}
}

func rotPending(t *testing.T, ref, name string) secretwire.Pending {
	return secretwire.Pending{OpID: newOpID(t), Op: secretwire.OpRotate, Ref: ref, Name: name,
		RefKind: secretwire.RefKindProviderKey, JournalSeq: 2}
}

func revPending(t *testing.T, ref, name, reason string) secretwire.Pending {
	return secretwire.Pending{OpID: newOpID(t), Op: secretwire.OpRevoke, Ref: ref, Name: name,
		RefKind: secretwire.RefKindProviderKey, ReasonCode: reason, JournalSeq: 3}
}
