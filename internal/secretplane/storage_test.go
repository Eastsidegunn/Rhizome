package secretplane

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rhizome/internal/secretwire"
)

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestStateDirModesAndPlaneIDFRRHZ138(t *testing.T) {
	if !strings.Contains(Banner, "local 0600 file = no same-user protection") {
		t.Fatal("banner must state the same-user limit")
	}
	hs := newHarness(t)
	if m := modeOf(t, hs.dir); !m.IsDir() || m.Perm() != 0o700 {
		t.Fatalf("state dir mode %v", m)
	}
	if m := modeOf(t, filepath.Join(hs.dir, ledgerName)); m.Perm() != 0o600 {
		t.Fatalf("ledger mode %v", m)
	}
	p := hs.planeID()
	if !secretwire.ValidPlaneID(p) {
		t.Fatal("planeId grammar")
	}
	hs.seed([]byte("v-one"), 1)
	if m := modeOf(t, filepath.Join(hs.dir, valuesName)); m.Perm() != 0o600 || !m.IsRegular() {
		t.Fatalf("values mode %v", m)
	}
	// Q3: planeId is persisted and stable across restarts.
	hs.reopen()
	if hs.planeID() != p {
		t.Fatal("planeId changed across restart")
	}
}

func TestStartupRefusesModeViolationsFRRHZ138(t *testing.T) {
	setup := func(t *testing.T) *harness {
		hs := newHarness(t)
		hs.seed([]byte("v"), 2)
		// Create a quarantine file via a conflict.
		hs.mustPoll(2, &secretwire.Rejected{Seq: 1, Reason: secretwire.EntrySeqConflict})
		hs.pl.Close()
		hs.pl = nil
		return hs
	}
	cases := []struct {
		name string
		mut  func(t *testing.T, dir string)
		want error
	}{
		{"dir-0755", func(t *testing.T, d string) { os.Chmod(d, 0o755) }, ErrStateDirUnsafe},
		{"dir-0710", func(t *testing.T, d string) { os.Chmod(d, 0o710) }, ErrStateDirUnsafe},
		{"ledger-0644", func(t *testing.T, d string) { os.Chmod(filepath.Join(d, ledgerName), 0o644) }, ErrFileUnsafe},
		{"ledger-0400", func(t *testing.T, d string) { os.Chmod(filepath.Join(d, ledgerName), 0o400) }, ErrFileUnsafe},
		{"values-0640", func(t *testing.T, d string) { os.Chmod(filepath.Join(d, valuesName), 0o640) }, ErrFileUnsafe},
		{"quarantine-0604", func(t *testing.T, d string) { os.Chmod(filepath.Join(d, quarantineName), 0o604) }, ErrFileUnsafe},
		{"values-symlink", func(t *testing.T, d string) {
			v := filepath.Join(d, valuesName)
			alt := filepath.Join(filepath.Dir(d), "elsewhere")
			os.Rename(v, alt)
			os.Symlink(alt, v)
		}, ErrFileUnsafe},
		{"values-dir", func(t *testing.T, d string) {
			os.Remove(filepath.Join(d, valuesName))
			os.Mkdir(filepath.Join(d, valuesName), 0o700)
		}, ErrFileUnsafe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs := setup(t)
			c.mut(t, hs.dir)
			before := hs.file(ledgerName)
			pl, err := hs.open()
			if !errors.Is(err, c.want) || pl != nil {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if !bytes.Equal(before, hs.file(ledgerName)) {
				t.Fatal("refused startup must not write")
			}
		})
	}
	t.Run("dir-symlink", func(t *testing.T) {
		hs := setup(t)
		link := filepath.Join(filepath.Dir(hs.dir), "link")
		if err := os.Symlink(hs.dir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := open(link, Options{}, seams{}); !errors.Is(err, ErrStateDirUnsafe) {
			t.Fatalf("symlinked dir: %v", err)
		}
	})
	t.Run("relative", func(t *testing.T) {
		if _, err := Open("relative/state", Options{}); !errors.Is(err, ErrStateDirUnsafe) {
			t.Fatalf("relative: %v", err)
		}
	})
}

func TestSingleWriterLockFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	if _, err := Open(hs.dir, Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second open: %v", err)
	}
	hs.pl.Close()
	pl, err := Open(hs.dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pl.Close()
}

// P3 ①: first creation fsyncs the ledger file, then the directory.
func TestLedgerCreateDirSyncFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	tr := hs.h.trace
	c := hs.h.index(t, "ledger-created")
	a := hs.h.index(t, "ledger-appended:init")
	if !(c < a && tr[a-1] == "sync:ledger" && tr[a+1] == "sync:dir") {
		t.Fatalf("creation order: %v", tr)
	}
	// Crash right after creation (empty ledger) → restart writes init.
	hs2 := &harness{t: t, dir: filepath.Join(t.TempDir(), "s"), h: &hooker{}}
	hs2.h.crashOn("ledger-created", 1)
	func() {
		defer func() {
			if _, ok := recover().(crashSignal); !ok {
				t.Fatal("expected crash")
			}
		}()
		hs2.open()
	}()
	hs2.h.fn = nil
	hs2.reopen()
	if !secretwire.ValidPlaneID(hs2.planeID()) {
		t.Fatal("init after empty ledger")
	}
}

// P3 ②–④ ordering, including the values temp-file protocol.
func TestOperationDurabilityOrderFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.h.trace = nil
	hs.apply(regPending(newOpIDT(t)), Decision{Value: []byte("val")})
	idx := func(p string) int { return hs.h.index(t, p) }
	order := []string{"ledger-appended:begin", "before-effect", "values-tmp-created", "sync:values-tmp",
		"values-before-rename", "values-renamed", "after-effect", "before-terminal", "ledger-appended:end"}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) >= idx(order[i]) {
			t.Fatalf("%s must precede %s: %v", order[i-1], order[i], hs.h.trace)
		}
	}
	if hs.h.trace[idx("values-renamed")+1] != "sync:dir" {
		t.Fatalf("dir fsync must follow rename: %v", hs.h.trace)
	}
	// Terminal and outbox are one line: exactly one end line carrying the entry.
	lines := bytes.Split(bytes.TrimSpace(hs.file(ledgerName)), []byte("\n"))
	last := lines[len(lines)-1]
	if !bytes.Contains(last, []byte(`"t":"end"`)) || !bytes.Contains(last, []byte(`"entry":`)) {
		t.Fatal("terminal line must carry its outbox entry")
	}
}

func TestFsyncErrorIsFatalFRRHZ138(t *testing.T) {
	for _, nth := range []int{1, 2, 3, 4} { // begin ledger, values tmp, dir, end ledger
		t.Run(itoa(uint64(nth)), func(t *testing.T) {
			hs := newHarness(t)
			hs.pl.Close()
			calls, fatals := 0, 0
			failAt := -1
			pl, err := open(hs.dir, Options{OnFatal: func() { fatals++ }}, seams{syncFn: func(f *os.File) error {
				calls++
				if calls == failAt {
					return errors.New("injected")
				}
				return f.Sync()
			}})
			if err != nil {
				t.Fatal(err)
			}
			failAt = calls + nth
			p := regPending(newOpIDT(t))
			_, err = pl.Apply(p, Decision{Value: []byte("v")})
			if !errors.Is(err, ErrFatal) || fatals != 1 {
				t.Fatalf("got %v fatals %d", err, fatals)
			}
			n := calls
			if _, err := pl.Observe(p.Ref); !errors.Is(err, ErrFatal) {
				t.Fatal("dead plane must refuse")
			}
			if _, err := pl.Apply(p, Decision{Value: []byte("v")}); !errors.Is(err, ErrFatal) || calls != n {
				t.Fatal("no retry after fatal")
			}
			pl.closeFiles()
			hs.reopen()
			got := hs.outcomes(p.OpID)
			want := secretwire.ResultOK
			if nth == 1 || nth == 2 {
				want = secretwire.ResultFailed
			}
			if len(got) != 1 || got[0].Result != want {
				t.Fatalf("recovery: %+v want %s", got, want)
			}
		})
	}
}

// Crash injection at every operation point → deterministic recovery, one
// outcome per opId, no re-prompt, idempotent re-run.
func TestCrashInjectionOperationFRRHZ138(t *testing.T) {
	points := []struct {
		point  string
		effect bool
	}{
		{"ledger-mid-line:begin", false},
		{"ledger-appended:begin", false},
		{"before-effect", false},
		{"values-tmp-created", false},
		{"values-before-rename", false},
		{"values-renamed", true},
		{"after-effect", true},
		{"before-terminal", true},
		{"ledger-mid-line:end", true},
	}
	ops := []string{"register", "rotate", "revoke", "direct"}
	for _, kind := range ops {
		for _, pt := range points {
			t.Run(kind+"/"+pt.point, func(t *testing.T) {
				hs := newHarness(t)
				ref := hs.seed([]byte("first"), 1)
				opID := newOpIDT(t)
				var p secretwire.Pending
				var call func()
				switch kind {
				case "register":
					p = regPending(opID)
					call = func() { hs.pl.Apply(p, Decision{Value: []byte("second")}) }
				case "rotate":
					p = rotPending(opID, ref)
					call = func() { hs.pl.Apply(p, Decision{Value: []byte("second")}) }
				case "revoke":
					p = revPending(opID, ref)
					call = func() { hs.pl.Apply(p, Decision{}) }
				case "direct":
					call = func() { hs.pl.RegisterDirect("direct-key", secretwire.RefKindOther, nil, []byte("second")) }
				}
				hs.h.crashOn(pt.point, 1)
				hs.crash(call)
				hs.reopen()
				if ents, _ := os.ReadDir(hs.dir); len(ents) > 0 {
					for _, e := range ents {
						if strings.HasPrefix(e.Name(), valuesTmpPrefix) {
							t.Fatal("temp values file left after recovery")
						}
					}
				}
				before := hs.file(ledgerName)
				if kind == "direct" {
					var recon []secretwire.Entry
					for _, e := range hs.allEntries() {
						if e.Entry.Fact == secretwire.FactReconciled {
							recon = append(recon, e.Entry)
						}
					}
					hs.pl.mu.Lock()
					var dop *opRec
					for _, op := range hs.pl.st.ops {
						if op.Origin == originPlaneDirect {
							dop = op
						}
					}
					hs.pl.mu.Unlock()
					if pt.point == "ledger-mid-line:begin" {
						if dop != nil || len(recon) != 0 {
							t.Fatal("torn begin must vanish")
						}
						return
					}
					if dop == nil || !dop.Done {
						t.Fatal("direct op must be terminal")
					}
					if pt.effect != (len(recon) == 1) || (pt.effect && dop.Result != secretwire.ResultOK) ||
						(!pt.effect && dop.Failure != secretwire.FailureCrashBeforeEffect) {
						t.Fatalf("direct recovery: %+v %+v", dop, recon)
					}
					return
				}
				got := hs.outcomes(opID)
				if pt.point == "ledger-mid-line:begin" {
					// Torn begin: never started; still new.
					if len(got) != 0 {
						t.Fatal("torn begin must not produce an outcome")
					}
					if st, _ := hs.pl.Classify(p); st != PendingNew {
						t.Fatalf("status %v", st)
					}
					return
				}
				if len(got) != 1 {
					t.Fatalf("outcomes %d", len(got))
				}
				want := secretwire.ResultFailed
				if pt.effect {
					want = secretwire.ResultOK
				}
				if got[0].Result != want || (want == secretwire.ResultFailed && got[0].FailureCode != secretwire.FailureCrashBeforeEffect) {
					t.Fatalf("result %+v", got[0])
				}
				wantV := map[string][2]uint64{"register": {0, 1}, "rotate": {1, 2}, "revoke": {1, 1}}[kind]
				v := wantV[0]
				if pt.effect {
					v = wantV[1]
				}
				if got[0].Version != v {
					t.Fatalf("R4 version %d want %d", got[0].Version, v)
				}
				// Idempotent re-run by opId: no prompt, no write, same outcome.
				d := Decision{Value: []byte("third")}
				if kind == "revoke" {
					d = Decision{}
				}
				r, err := hs.pl.Apply(p, d)
				if err != nil || !r.Duplicate || r.Entry == nil || !entryEqual(*r.Entry, got[0]) {
					t.Fatalf("re-run: %+v %v", r, err)
				}
				if !bytes.Equal(before, hs.file(ledgerName)) {
					t.Fatal("re-run wrote")
				}
			})
		}
	}
}

func TestIdempotentByOpIDFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	p := regPending(newOpIDT(t))
	if err := hs.pl.Begin(p); err != nil {
		t.Fatal(err)
	}
	before := hs.file(ledgerName)
	if err := hs.pl.Begin(p); err != nil || !bytes.Equal(before, hs.file(ledgerName)) {
		t.Fatal("second Begin must be a no-op")
	}
	r1, err := hs.pl.Commit(p.OpID, Decision{Value: []byte("v")})
	if err != nil || r1.Duplicate {
		t.Fatal(err)
	}
	before = hs.file(ledgerName)
	vbefore := hs.file(valuesName)
	for range 3 {
		r, err := hs.pl.Apply(p, Decision{Value: []byte("other")})
		if err != nil || !r.Duplicate || !entryEqual(*r.Entry, *r1.Entry) {
			t.Fatal("dup")
		}
	}
	if !bytes.Equal(before, hs.file(ledgerName)) || !bytes.Equal(vbefore, hs.file(valuesName)) {
		t.Fatal("dedupe must write nothing")
	}
	if st, _ := hs.pl.Classify(p); st != PendingDone {
		t.Fatal("status")
	}
	if len(hs.outcomes(p.OpID)) != 1 {
		t.Fatal("P1: one outcome per opId")
	}
	// Same opId, different canonical content → invalid (each field varied).
	vary := map[string]func(*secretwire.Pending){
		"op":         func(q *secretwire.Pending) { q.Op = secretwire.OpRotate },
		"ref":        func(q *secretwire.Pending) { q.Ref = secretwire.DeriveRef(newOpIDT(t)) },
		"name":       func(q *secretwire.Pending) { q.Name = "other-key" },
		"refKind":    func(q *secretwire.Pending) { q.RefKind = secretwire.RefKindOther },
		"scopeTags":  func(q *secretwire.Pending) { q.ScopeTags = []string{"alpha"} },
		"noTags":     func(q *secretwire.Pending) { q.ScopeTags = nil },
		"reasonCode": func(q *secretwire.Pending) { q.ReasonCode = secretwire.ReasonUnused },
	}
	for name, f := range vary {
		q := p
		q.ScopeTags = cloneTags(p.ScopeTags)
		f(&q)
		if st, _ := hs.pl.Classify(q); st != PendingInvalid {
			t.Fatalf("register %s mismatch must be invalid", name)
		}
	}
	rot := rotPending(newOpIDT(t), p.Ref)
	hs.apply(rot, Decision{Value: []byte("r")})
	q := rot
	q.Ref = secretwire.DeriveRef(newOpIDT(t))
	if st, _ := hs.pl.Classify(q); st != PendingInvalid {
		t.Fatal("rotate ref mismatch")
	}
	rev := revPending(newOpIDT(t), p.Ref)
	hs.apply(rev, Decision{Reject: true})
	for _, f := range []func(*secretwire.Pending){
		func(q *secretwire.Pending) { q.ReasonCode = secretwire.ReasonOther },
		func(q *secretwire.Pending) { q.Ref = secretwire.DeriveRef(newOpIDT(t)) },
	} {
		q := rev
		f(&q)
		if st, _ := hs.pl.Classify(q); st != PendingInvalid {
			t.Fatal("revoke content mismatch")
		}
	}
	if st, _ := hs.pl.Classify(rev); st != PendingDone {
		t.Fatal("identical revoke is a duplicate")
	}
}

// G12: rotate ok at version 1,000,000 would emit 1,000,001; it is refused
// before any effect with failed/invalid-state at the current version, and
// replay rejects any out-of-grammar version.
func TestVersionUpperBoundFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	ref := hs.seed([]byte("v"), 1)
	hs.pl.mu.Lock()
	hs.pl.st.refs[ref].Version = secretwire.MaxVersion // in-memory only (a million rotations is too slow)
	hs.pl.mu.Unlock()
	vbefore := hs.file(valuesName)
	hs.h.trace = nil
	r := hs.apply(rotPending(newOpIDT(t), ref), Decision{Value: []byte("w")})
	if r.Entry.Result != secretwire.ResultFailed || r.Entry.FailureCode != secretwire.FailureInvalidState ||
		r.Entry.Version != secretwire.MaxVersion {
		t.Fatalf("got %+v", r.Entry)
	}
	for _, x := range hs.h.trace {
		if strings.HasPrefix(x, "values") || x == "before-effect" {
			t.Fatal("no effect at the version bound")
		}
	}
	if !bytes.Equal(vbefore, hs.file(valuesName)) {
		t.Fatal("values changed")
	}
	// Rejection and revoke at the bound are fine (no increment).
	if r := hs.apply(rotPending(newOpIDT(t), ref), Decision{Reject: true}); r.Entry.Version != secretwire.MaxVersion {
		t.Fatal("reject keeps version")
	}
	// Replay: an end line whose rotate-ok entry exceeds G12 is refused.
	st := newState()
	p := "pl-" + strings.Repeat("a", 32)
	reg := regPending(newOpIDT(t))
	must := func(r *rec) {
		if err := st.apply(r); err != nil {
			t.Fatalf("setup %s", r.T)
		}
	}
	must(&rec{T: recInit, PlaneID: p})
	must(beginRec(reg, p, false))
	must(&rec{T: recEnd, OpID: reg.OpID, Result: "ok", Entry: st.terminalEntry(st.ops[reg.OpID], "ok", "")})
	st.refs[reg.Ref].Version = secretwire.MaxVersion
	rot := rotPending(newOpIDT(t), reg.Ref)
	must(beginRec(rot, p, false))
	e := st.terminalEntry(st.ops[rot.OpID], "ok", "")
	if e.Version != secretwire.MaxVersion+1 || st.apply(&rec{T: recEnd, OpID: rot.OpID, Result: "ok", Entry: e}) == nil {
		t.Fatal("replay must refuse a version beyond G12")
	}
	bad := secretwire.Entry{Seq: 2, Fact: secretwire.FactObserved, Ref: reg.Ref, Version: secretwire.MaxVersion + 1,
		PlaneObservedAt: "2026-10-10T00:00:00Z"}
	st.refs[reg.Ref].Version = secretwire.MaxVersion + 1
	if st.apply(&rec{T: recFact, Entry: &bad}) == nil {
		t.Fatal("replay must refuse an observed version beyond G12")
	}
}

func TestVersionsAndFailuresR4FRRHZ138(t *testing.T) {
	hs := newHarness(t)
	reg := regPending(newOpIDT(t))
	if r := hs.apply(reg, Decision{Value: []byte("a")}); r.Entry.Version != 1 || r.Entry.Result != "ok" {
		t.Fatal("register ok v1")
	}
	ref := reg.Ref
	check := func(r Result, result string, v uint64) {
		t.Helper()
		if r.Entry.Result != result || r.Entry.Version != v || r.Entry.Fact != secretwire.FactOutcome || r.Entry.Ref != ref {
			t.Fatalf("got %+v want %s v%d", r.Entry, result, v)
		}
	}
	check(hs.apply(rotPending(newOpIDT(t), ref), Decision{Value: []byte("b")}), "ok", 2)
	check(hs.apply(rotPending(newOpIDT(t), ref), Decision{Reject: true}), "rejected", 2)
	check(hs.apply(revPending(newOpIDT(t), ref), Decision{Reject: true}), "rejected", 2)
	// Backend error: write failure removes the temp file → failed/backend-error.
	hs.h.fn = func(p string) error {
		if p == "values-write" {
			return errors.New("injected")
		}
		return nil
	}
	r := hs.apply(rotPending(newOpIDT(t), ref), Decision{Value: []byte("c")})
	check(r, "failed", 2)
	if r.Entry.FailureCode != secretwire.FailureBackendError {
		t.Fatal("backend-error")
	}
	r = hs.apply(regPending(newOpIDT(t)), Decision{Value: []byte("d")})
	if r.Entry.Result != "failed" || r.Entry.Version != 0 {
		t.Fatal("register failed v0")
	}
	hs.h.fn = nil
	ents, _ := os.ReadDir(hs.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), valuesTmpPrefix) {
			t.Fatal("temp left")
		}
	}
	check(hs.apply(revPending(newOpIDT(t), ref), Decision{}), "ok", 2)
	reg2 := regPending(newOpIDT(t))
	if r := hs.apply(reg2, Decision{Reject: true}); r.Entry.Result != "rejected" || r.Entry.Version != 0 {
		t.Fatal("register rejected v0")
	}
	// Revoked ref: no further operations.
	if st, _ := hs.pl.Classify(rotPending(newOpIDT(t), ref)); st != PendingInvalid {
		t.Fatal("revoked ref")
	}
	// Decision shape checks write nothing.
	p := rotPending(newOpIDT(t), hs.seed([]byte("x"), 1))
	if err := hs.pl.Begin(p); err != nil {
		t.Fatal(err)
	}
	before := hs.file(ledgerName)
	if _, err := hs.pl.Commit(p.OpID, Decision{}); !errors.Is(err, ErrInvalidDecision) {
		t.Fatal("rotate needs a value")
	}
	if _, err := hs.pl.Commit(p.OpID, Decision{Reject: true, Value: []byte("x")}); !errors.Is(err, ErrInvalidDecision) {
		t.Fatal("reject carries no value")
	}
	if !bytes.Equal(before, hs.file(ledgerName)) {
		t.Fatal("invalid decision wrote")
	}
}

// Q2: attested refs are record-only; rotate → ok version+1, revoke → ok;
// the values file is never touched.
func TestAttestedRecordOnlyFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	r, err := hs.pl.DeclareAttested("broker-key", secretwire.RefKindProviderKey, []string{"prod"})
	if err != nil {
		t.Fatal(err)
	}
	e := r.Entry
	if e.Fact != secretwire.FactReconciled || e.Origin != secretwire.OriginAttested || e.Version != 1 ||
		e.Ref != secretwire.DeriveRef(e.OpID) || !secretwire.ValidOpID(e.OpID) {
		t.Fatalf("attested entry %+v", e)
	}
	if _, err := os.Stat(filepath.Join(hs.dir, valuesName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("attested declaration must not create values")
	}
	rot := secretwire.Pending{OpID: newOpIDT(t), Op: secretwire.OpRotate, Ref: e.Ref, Name: "broker-key",
		RefKind: secretwire.RefKindProviderKey, JournalSeq: 3}
	if err := hs.pl.Begin(rot); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.pl.Commit(rot.OpID, Decision{Value: []byte("v")}); !errors.Is(err, ErrInvalidDecision) {
		t.Fatal("attested rotate takes no value")
	}
	hs.h.trace = nil
	res, err := hs.pl.Commit(rot.OpID, Decision{})
	if err != nil || res.Entry.Result != "ok" || res.Entry.Version != 2 {
		t.Fatalf("attested rotate %+v %v", res, err)
	}
	for _, p := range hs.h.trace {
		if strings.HasPrefix(p, "values") {
			t.Fatal("attested rotate touched values")
		}
	}
	rev := secretwire.Pending{OpID: newOpIDT(t), Op: secretwire.OpRevoke, Ref: e.Ref, Name: "broker-key",
		RefKind: secretwire.RefKindProviderKey, ReasonCode: secretwire.ReasonRotatedOut, JournalSeq: 5}
	res = hs.apply(rev, Decision{})
	if res.Entry.Result != "ok" || res.Entry.Version != 2 {
		t.Fatal("attested revoke ok at current version")
	}
	if _, err := os.Stat(filepath.Join(hs.dir, valuesName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("values touched")
	}
	if _, err := hs.pl.Observe(e.Ref); !errors.Is(err, ErrUnknownRef) {
		t.Fatal("attested refs are not observed")
	}
	// Crash mid attested op → failed/crash-before-effect (no value evidence).
	r2, _ := hs.pl.DeclareAttested("broker-two", secretwire.RefKindOther, nil)
	rv := secretwire.Pending{OpID: newOpIDT(t), Op: secretwire.OpRevoke, Ref: r2.Ref, Name: "broker-two",
		RefKind: secretwire.RefKindOther, ReasonCode: secretwire.ReasonUnused, JournalSeq: 7}
	hs.h.crashOn("before-terminal", 1)
	hs.crash(func() { hs.pl.Apply(rv, Decision{}) })
	hs.reopen()
	got := hs.outcomes(rv.OpID)
	if len(got) != 1 || got[0].Result != "failed" || got[0].FailureCode != secretwire.FailureCrashBeforeEffect {
		t.Fatalf("attested recovery %+v", got)
	}
}

func TestRegisterDirectAndObserveFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	r, err := hs.pl.RegisterDirect("direct-key", secretwire.RefKindGatewayCredential, []string{"a"}, []byte("dv"))
	if err != nil {
		t.Fatal(err)
	}
	e := r.Entry
	if e.Fact != secretwire.FactReconciled || e.Origin != secretwire.OriginPlaneDirect || e.Ref != secretwire.DeriveRef(e.OpID) ||
		e.Version != 1 || e.Name != "direct-key" || len(e.ScopeTags) != 1 {
		t.Fatalf("direct %+v", e)
	}
	if _, err := hs.pl.RegisterDirect("Bad Name", secretwire.RefKindOther, nil, []byte("x")); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("invalid name")
	}
	o, err := hs.pl.Observe(e.Ref)
	if err != nil || !o.Present || o.Version != 1 || o.PlaneObservedAt != "2026-10-10T00:30:15Z" || o.Seq != 2 {
		t.Fatalf("observe %+v %v", o, err)
	}
	if !secretwire.ValidObservedAt(o.PlaneObservedAt) {
		t.Fatal("G13")
	}
	// A request register, then revoke → present false, version unchanged.
	reg := regPending(newOpIDT(t))
	hs.apply(reg, Decision{Value: []byte("rv")})
	hs.apply(revPending(newOpIDT(t), reg.Ref), Decision{})
	o, _ = hs.pl.Observe(reg.Ref)
	if o.Present || o.Version != 1 {
		t.Fatalf("observe revoked %+v", o)
	}
	if _, err := hs.pl.Observe("sref-000000000000000000000000"); !errors.Is(err, ErrUnknownRef) {
		t.Fatal("unknown ref")
	}
	// Manually added values entries are listed as reconciliation candidates.
	refs, err := hs.pl.UnreconciledValueRefs()
	if err != nil || len(refs) != 0 {
		t.Fatal("no candidates yet")
	}
}

func TestTornTailCorruptionAndTempsFRRHZ138(t *testing.T) {
	t.Run("ledger-torn-tail", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 3)
		hs.pl.Close()
		path := filepath.Join(hs.dir, ledgerName)
		good := hs.file(ledgerName)
		f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		f.Write([]byte(`{"t":"fact","entry":{"seq":4`))
		f.Close()
		hs.h.trace = nil
		hs.reopen()
		if !bytes.Equal(good, hs.file(ledgerName)) {
			t.Fatal("torn tail must be truncated")
		}
		if hs.h.index(t, "sync:ledger") < 0 {
			t.Fatal("truncate must be fsynced")
		}
		if hs.status().OutboxMax != 3 {
			t.Fatal("state after truncate")
		}
	})
	t.Run("ledger-mid-corruption", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 3)
		hs.pl.Close()
		data := hs.file(ledgerName)
		lines := bytes.SplitAfter(data, []byte("\n"))
		lines[1] = []byte("{garbage}\n")
		os.WriteFile(filepath.Join(hs.dir, ledgerName), bytes.Join(lines, nil), 0o600)
		before := hs.file(ledgerName)
		if _, err := hs.open(); !errors.Is(err, ErrLedgerCorrupt) {
			t.Fatalf("got %v", err)
		}
		if !bytes.Equal(before, hs.file(ledgerName)) {
			t.Fatal("refused start wrote")
		}
	})
	t.Run("ledger-semantic-corruption", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 2)
		hs.pl.Close()
		data := hs.file(ledgerName)
		// Duplicate the last fact line: seq contiguity breaks.
		lines := bytes.SplitAfter(bytes.TrimSuffix(data, nil), []byte("\n"))
		last := lines[len(lines)-2]
		os.WriteFile(filepath.Join(hs.dir, ledgerName), append(data, last...), 0o600)
		if _, err := hs.open(); !errors.Is(err, ErrLedgerCorrupt) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("values-corrupt", func(t *testing.T) {
		hs := newHarness(t)
		hs.seed([]byte("v"), 1)
		ref := hs.seed([]byte("w"), 1)
		p := rotPending(newOpIDT(t), ref)
		hs.pl.Begin(p) // in progress → recovery must read values
		hs.pl.closeFiles()
		os.WriteFile(filepath.Join(hs.dir, valuesName), []byte("{nope"), 0o600)
		if _, err := hs.open(); !errors.Is(err, ErrValuesCorrupt) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("temp-cleanup", func(t *testing.T) {
		hs := newHarness(t)
		hs.pl.Close()
		tmp := filepath.Join(hs.dir, valuesTmpPrefix+"deadbeef")
		os.WriteFile(tmp, []byte("partial"), 0o600)
		hs.reopen()
		if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("temp not removed")
		}
	})
}

func TestUnreconciledValueRefsFRRHZ138(t *testing.T) {
	hs := newHarness(t)
	hs.seed([]byte("v"), 1)
	hs.pl.valuesMu.Lock()
	vals, _ := hs.pl.readValues()
	orphan := secretwire.DeriveRef(newOpIDT(t))
	vals = append(vals, valueRec{Ref: orphan, Version: 1, OpID: newOpIDT(t), Value: []byte("manual")})
	if err := hs.pl.writeValues(vals); err != nil {
		t.Fatal(err)
	}
	hs.pl.valuesMu.Unlock()
	refs, err := hs.pl.UnreconciledValueRefs()
	if err != nil || len(refs) != 1 || refs[0] != orphan {
		t.Fatalf("candidates %v %v", refs, err)
	}
	if hs.status().OutboxMax != 1 {
		t.Fatal("candidates are not recorded automatically")
	}
}
