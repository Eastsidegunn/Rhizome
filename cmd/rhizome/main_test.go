package main

import (
	"bytes"
	"os"
	"path/filepath"
	"rhizome/internal/memory"
	"strings"
	"testing"
)

func TestIngestFRRHZ054(t *testing.T) {
	d := t.TempDir()
	n := filepath.Join(d, "n.md")
	body := []byte("# note\nhello")
	if e := os.WriteFile(n, body, 0600); e != nil {
		t.Fatal(e)
	}
	j := filepath.Join(d, "j")
	b := filepath.Join(d, "b")
	var out, er bytes.Buffer
	if c := run([]string{"ingest", "-journal", j, "-blobs", b, "-file", n}, &out, &er); c != 0 {
		t.Fatal(c, er.String())
	}
	x, e := openTestJournal(j)
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	ms, e := (memory.Service{Store: x}).Search(memory.Reference, "", "")
	if e != nil || len(ms) != 1 || ms[0].Content != string(body) {
		t.Fatal(ms, e)
	}
	for _, ev := range x.All() {
		if ev.AggregateType == "source" && strings.Contains(string(ev.Payload), string(body)) {
			t.Fatal("body in journal")
		}
	}
	if _, e := os.Stat(filepath.Join(b, strings.TrimPrefix(ms[0].SourceID, "sha256:"))); e != nil {
		t.Fatal(e)
	}
	x.Close()
	x, e = openTestJournal(j)
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	ms, e = (memory.Service{Store: x}).Search(memory.Reference, "", "")
	if e != nil || len(ms) != 1 {
		t.Fatal(ms, e)
	}
}
func TestReingestAndInvalidFRRHZ054(t *testing.T) {
	d := t.TempDir()
	n := filepath.Join(d, "n")
	if e := os.WriteFile(n, []byte("same"), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"ingest", "-journal", filepath.Join(d, "j"), "-blobs", filepath.Join(d, "b"), "-file", n}
	var o, eout bytes.Buffer
	if run(args, &o, &eout) != 0 {
		t.Fatal(eout.String())
	}
	before, _ := os.ReadFile(args[2])
	if run(args, &o, &eout) == 0 {
		t.Fatal("duplicate accepted")
	}
	after, _ := os.ReadFile(args[2])
	if string(before) != string(after) {
		t.Fatal("journal changed")
	}
	empty := filepath.Join(d, "empty")
	os.WriteFile(empty, nil, 0600)
	if run([]string{"ingest", "-journal", filepath.Join(d, "new"), "-blobs", filepath.Join(d, "bb"), "-file", empty}, &o, &eout) == 0 {
		t.Fatal("empty accepted")
	}
}
func TestMemoriesDeterministic(t *testing.T) {
	d := t.TempDir()
	n := filepath.Join(d, "n")
	os.WriteFile(n, []byte("hello"), 0600)
	args := []string{"ingest", "-journal", filepath.Join(d, "j"), "-blobs", filepath.Join(d, "b"), "-file", n}
	var o, e bytes.Buffer
	if run(args, &o, &e) != 0 {
		t.Fatal(e.String())
	}
	var a, c bytes.Buffer
	run([]string{"memories", "-journal", args[2]}, &a, &e)
	run([]string{"memories", "-journal", args[2]}, &c, &e)
	if a.String() != c.String() {
		t.Fatal("nondeterministic output")
	}
}

// RHZ-042 잠금 테스트 (FR-RHZ-073). 잠금 경계의 역할 예외를 고정한다.
func TestJournalLockLifecycleFRRHZ073(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	var eout bytes.Buffer
	unlock, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatal(e)
	}
	if _, e2 := acquireJournalLock(jp, &eout); e2 == nil {
		t.Fatal("second lock acquired")
	}
	if !strings.Contains(eout.String(), "stop that process first") || !strings.Contains(eout.String(), jp+".lock") {
		t.Fatalf("no stale guidance: %q", eout.String())
	}
	unlock()
	unlock2, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatal("relock after release failed:", e)
	}
	unlock2()
}

func TestIngestBlockedWhileLockedFRRHZ073(t *testing.T) {
	d := t.TempDir()
	jp := filepath.Join(d, "j.ndjson")
	n := filepath.Join(d, "n.md")
	if e := os.WriteFile(n, []byte("note"), 0600); e != nil {
		t.Fatal(e)
	}
	var o, eout bytes.Buffer
	unlock, e := acquireJournalLock(jp, &eout)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	if c := run([]string{"ingest", "-journal", jp, "-blobs", filepath.Join(d, "b"), "-file", n}, &o, &eout); c == 0 {
		t.Fatal("ingest succeeded under lock")
	}
	if _, err := os.Stat(jp); err == nil {
		t.Fatal("journal created despite lock")
	}
	unlock()
	if c := run([]string{"ingest", "-journal", jp, "-blobs", filepath.Join(d, "b"), "-file", n}, &o, &eout); c != 0 {
		t.Fatal("ingest failed after unlock:", eout.String())
	}
}
