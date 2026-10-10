package secretplane

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/secretwire"
)

// FR-RHZ-174.

func execFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(safeDir(t), "prog")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProfilesAddRemoveFRRHZ174: profile validation, ref checks (attested,
// revoked, unknown and value-less refs are refused), uniqueness, removal,
// and a value-free 0600 profiles.json.
func TestProfilesAddRemoveFRRHZ174(t *testing.T) {
	s := sentinel(t)
	hs := newHarness(t)
	prog := execFile(t)
	stored := hs.seed([]byte(s), 1)
	att, err := hs.pl.DeclareAttested("broker", secretwire.RefKindProviderKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	revOp := newOpIDT(t)
	rp := regPending(revOp)
	rp.Name = "gone"
	hs.apply(rp, Decision{Value: []byte(s)})
	rv := revPending(newOpIDT(t), rp.Ref)
	rv.Name = "gone"
	hs.apply(rv, Decision{})

	ok := Profile{Name: "hf", Program: prog, Args: []string{"download"}, Env: map[string]string{"HF_TOKEN": stored}}
	cases := []struct {
		p    Profile
		want error
	}{
		{Profile{Name: "Bad", Program: prog, Env: ok.Env}, ErrInvalidInput},
		{Profile{Name: "x", Program: "rel/prog", Env: ok.Env}, ErrInvalidInput},
		{Profile{Name: "x", Program: prog + "/../prog", Env: ok.Env}, ErrInvalidInput},
		{Profile{Name: "x", Program: filepath.Join(t.TempDir(), "none"), Env: ok.Env}, ErrInvalidInput},
		{Profile{Name: "x", Program: prog}, ErrInvalidInput},
		{Profile{Name: "x", Program: prog, Env: map[string]string{"PATH": stored}}, ErrInvalidInput},
		{Profile{Name: "x", Program: prog, Env: map[string]string{"hf_token": stored}}, ErrInvalidInput},
		{Profile{Name: "x", Program: prog, Env: map[string]string{"A": "sref-000000000000000000000000"}}, ErrUnknownRef},
		{Profile{Name: "x", Program: prog, Env: map[string]string{"A": att.Ref}}, ErrUnknownRef},
		{Profile{Name: "x", Program: prog, Env: map[string]string{"A": rp.Ref}}, ErrUnknownRef},
	}
	for i, c := range cases {
		if err := hs.pl.AddProfile(c.p); !errors.Is(err, c.want) {
			t.Fatalf("case %d: got %v", i, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(hs.dir, profilesName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused profiles wrote")
	}
	if err := hs.pl.AddProfile(ok); err != nil {
		t.Fatal(err)
	}
	if err := hs.pl.AddProfile(ok); !errors.Is(err, ErrProfileExists) {
		t.Fatal("duplicate accepted")
	}
	fi, _ := os.Lstat(filepath.Join(hs.dir, profilesName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatal("profiles.json mode")
	}
	ps, err := hs.pl.Profiles()
	if err != nil || len(ps) != 1 || ps[0].Env["HF_TOKEN"] != stored || ps[0].Args[0] != "download" {
		t.Fatalf("profiles %+v %v", ps, err)
	}
	assertNoForms(t, "profiles.json", hs.file(profilesName), s)

	// reopen: profiles.json passes the startup mode check
	hs.reopen()
	if err := hs.pl.RemoveProfile("none"); !errors.Is(err, ErrUnknownProfile) {
		t.Fatal("unknown remove")
	}
	if err := hs.pl.RemoveProfile("hf"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := hs.pl.Profiles(); len(ps) != 0 {
		t.Fatal("not removed")
	}
	hs.pl.Close()
	os.Chmod(filepath.Join(hs.dir, profilesName), 0o644)
	if _, err := hs.open(); !errors.Is(err, ErrFileUnsafe) {
		t.Fatal("unsafe profiles.json accepted at startup")
	}
	hs.pl = nil
}

// TestResolveRunFRRHZ174: ResolveRun reads without the lock (a plane may
// be open), returns exactly the stored values, and refuses unknown profiles,
// missing values and unsafe files with no value returned.
func TestResolveRunFRRHZ174(t *testing.T) {
	s1, s2 := sentinel(t), sentinel(t)
	hs := newHarness(t)
	prog := execFile(t)
	r1 := hs.seed([]byte(s1), 1)
	p2 := regPending(newOpIDT(t))
	p2.Name = "second"
	hs.apply(p2, Decision{Value: []byte(s2)})
	if err := hs.pl.AddProfile(Profile{Name: "both", Program: prog, AllowExtraArgs: true,
		Env: map[string]string{"B_TOKEN": p2.Ref, "A_TOKEN": r1}}); err != nil {
		t.Fatal(err)
	}
	if err := hs.pl.AddProfile(Profile{Name: "two", Program: prog, Env: map[string]string{"T": p2.Ref}}); err != nil {
		t.Fatal(err)
	}
	led := hs.file(ledgerName)

	spec, err := ResolveRun(hs.dir, "both") // plane still open: no lock taken
	if err != nil {
		t.Fatal(err)
	}
	if spec.Program != prog || !spec.AllowExtraArgs || len(spec.Env) != 2 || spec.Env[0].Name != "A_TOKEN" ||
		string(spec.Env[0].Value) != s1 || string(spec.Env[1].Value) != s2 {
		t.Fatal("resolved spec")
	}
	vals := []*[]byte{&spec.Env[0].Value, &spec.Env[1].Value}
	spec.Wipe()
	for _, v := range vals {
		if !bytes.Equal(*v, make([]byte, len(*v))) {
			t.Fatal("Wipe left a value")
		}
	}
	if _, err := ResolveRun(hs.dir, "nope"); !errors.Is(err, ErrUnknownProfile) {
		t.Fatal("unknown profile")
	}
	rv := revPending(newOpIDT(t), p2.Ref)
	rv.Name = "second"
	hs.apply(rv, Decision{}) // the value is removed
	if _, err := ResolveRun(hs.dir, "two"); !errors.Is(err, ErrValueMissing) {
		t.Fatalf("value-missing: %v", err)
	}
	if !bytes.Equal(led, hs.file(ledgerName)[:len(led)]) {
		t.Fatal("ResolveRun changed the ledger")
	}

	pf := filepath.Join(hs.dir, profilesName)
	os.Chmod(pf, 0o644)
	if _, err := ResolveRun(hs.dir, "both"); !errors.Is(err, ErrFileUnsafe) {
		t.Fatal("0644 profiles accepted")
	}
	os.Chmod(pf, 0o600)
	os.Rename(pf, pf+".real")
	os.Symlink(pf+".real", pf)
	if _, err := ResolveRun(hs.dir, "both"); !errors.Is(err, ErrFileUnsafe) {
		t.Fatal("symlinked profiles accepted")
	}
	os.Remove(pf)
	os.Rename(pf+".real", pf)
	os.Chmod(hs.dir, 0o750)
	if _, err := ResolveRun(hs.dir, "both"); !errors.Is(err, ErrStateDirUnsafe) {
		t.Fatal("0750 dir accepted")
	}
	os.Chmod(hs.dir, 0o700)
	if _, err := ResolveRun("rel", "both"); !errors.Is(err, ErrStateDirUnsafe) {
		t.Fatal("relative dir accepted")
	}
	for _, f := range []string{ledgerName, quarantineName, profilesName} {
		assertNoForms(t, f, hs.file(f), s1)
		assertNoForms(t, f, hs.file(f), s2)
	}
}

// TestStrictFilesFRRHZ174: decodeLine is fail-closed (a valid object
// followed by anything but whitespace is corrupt), and ResolveRun applies
// the plane's values-file checks (damage → ErrValuesCorrupt, no value).
func TestStrictFilesFRRHZ174(t *testing.T) {
	var v struct{ A int }
	for _, c := range []struct {
		in string
		ok bool
	}{
		{`{"A":1}`, true}, {"{\"A\":1}\n  ", true}, {`{"A":1}x`, false}, {`{"A":1}{"A":2}`, false},
		{`{"A":1}]`, false}, {`{"A":1} 2`, false}, {`{"B":1}`, false},
	} {
		if decodeLine([]byte(c.in), &v) != c.ok {
			t.Fatalf("decodeLine(%q) != %v", c.in, c.ok)
		}
	}

	s := sentinel(t)
	hs := newHarness(t)
	ref := hs.seed([]byte(s), 1)
	if err := hs.pl.AddProfile(Profile{Name: "p", Program: execFile(t), Env: map[string]string{"A_TOKEN": ref}}); err != nil {
		t.Fatal(err)
	}
	vpath := filepath.Join(hs.dir, valuesName)
	good := hs.file(valuesName)
	val := `"` + base64Std(s) + `"`
	opID := newOpIDT(t)
	for i, bad := range []string{
		string(bytes.TrimSpace(good)) + "garbage",
		`{"v":1,"values":[{"ref":"` + ref + `","version":0,"opId":"` + opID + `","value":` + val + `}]}`,
		`{"v":1,"values":[{"ref":"` + ref + `","version":1,"opId":"bad","value":` + val + `}]}`,
		`{"v":1,"values":[{"ref":"` + ref + `","version":1,"opId":"` + opID + `","value":""}]}`,
		`{"v":1,"values":[{"ref":"bad","version":1,"opId":"` + opID + `","value":` + val + `}]}`,
		`{"v":1,"values":[{"ref":"` + ref + `","version":1,"opId":"` + opID + `","value":` + val + `},` +
			`{"ref":"` + ref + `","version":2,"opId":"` + opID + `","value":` + val + `}]}`,
		`{"v":2,"values":[]}`,
	} {
		if err := os.WriteFile(vpath, []byte(bad+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		spec, err := ResolveRun(hs.dir, "p")
		if !errors.Is(err, ErrValuesCorrupt) || spec.Env != nil {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	os.WriteFile(vpath, good, 0o600)
	pf := filepath.Join(hs.dir, profilesName)
	pgood := hs.file(profilesName)
	os.WriteFile(pf, []byte(string(bytes.TrimSpace(pgood))+"{}\n"), 0o600)
	if _, err := ResolveRun(hs.dir, "p"); !errors.Is(err, ErrProfilesCorrupt) {
		t.Fatalf("trailing profiles data: %v", err)
	}
	os.WriteFile(pf, pgood, 0o600)
	if spec, err := ResolveRun(hs.dir, "p"); err != nil || string(spec.Env[0].Value) != s {
		t.Fatalf("restored files: %v", err)
	}
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// safeDir: see cmd/rhizome-secret-plane safeDir (t.TempDir on macOS; a
// private home directory where the temp dir fails the rule, e.g. Linux /tmp).
func safeDir(t *testing.T) string {
	t.Helper()
	ok := func(d string) bool {
		p := filepath.Join(d, ".probe")
		if os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700) != nil {
			return false
		}
		defer os.Remove(p)
		_, err := CheckProgram(p)
		return err == nil
	}
	if d := t.TempDir(); ok(d) {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		if d, err := os.MkdirTemp(home, ".rhz-plane-test-"); err == nil {
			t.Cleanup(func() { os.RemoveAll(d) })
			if ok(d) {
				return d
			}
		}
	}
	t.Skip("no directory satisfies the program ownership rule")
	return ""
}

// TestCheckProgramFRRHZ174: the program and every directory on its path
// (as given and resolved) must be owned by this user or root and not
// group/other-writable; symlinks are resolved and the resolved path is
// returned; /tmp (1777) fails.
func TestCheckProgramFRRHZ174(t *testing.T) {
	d := safeDir(t)
	prog := filepath.Join(d, "prog")
	os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755)
	os.Chmod(prog, 0o755)
	want, _ := filepath.EvalSymlinks(prog)
	if got, err := CheckProgram(prog); err != nil || got != want {
		t.Fatalf("safe program: %q %v", got, err)
	}

	// group-writable binary
	os.Chmod(prog, 0o775)
	if _, err := CheckProgram(prog); !errors.Is(err, ErrProgramUnsafe) {
		t.Fatal("group-writable binary accepted")
	}
	os.Chmod(prog, 0o755)
	// not executable
	os.Chmod(prog, 0o644)
	if _, err := CheckProgram(prog); !errors.Is(err, ErrProgramUnsafe) {
		t.Fatal("non-executable accepted")
	}
	os.Chmod(prog, 0o755)

	// other-writable parent directory
	sub := filepath.Join(d, "sub")
	os.Mkdir(sub, 0o755)
	inner := filepath.Join(sub, "prog")
	os.WriteFile(inner, []byte("#!/bin/sh\n"), 0o755)
	os.Chmod(sub, 0o757)
	if _, err := CheckProgram(inner); !errors.Is(err, ErrProgramUnsafe) {
		t.Fatal("other-writable parent accepted")
	}
	os.Chmod(sub, 0o755)
	if _, err := CheckProgram(inner); err != nil {
		t.Fatal(err)
	}

	// owned by another uid (seam): file and directories no longer trusted
	saved := trustedUID
	trustedUID = func(uid uint32) bool { return uid == 0 }
	if _, err := CheckProgram(prog); !errors.Is(err, ErrProgramUnsafe) {
		trustedUID = saved
		t.Fatal("foreign-owned binary accepted")
	}
	trustedUID = saved

	// symlinks: resolved; a link to a safe program is accepted and the
	// resolved path returned; a link into an unsafe directory is refused
	link := filepath.Join(d, "link")
	os.Symlink(inner, link)
	wantInner, _ := filepath.EvalSymlinks(inner)
	if got, err := CheckProgram(link); err != nil || got != wantInner {
		t.Fatalf("link: %q %v", got, err)
	}
	os.Chmod(sub, 0o775)
	if _, err := CheckProgram(link); !errors.Is(err, ErrProgramUnsafe) {
		t.Fatal("link into a group-writable directory accepted")
	}
	os.Chmod(sub, 0o755)
	// a link placed in an unsafe directory is refused (component check)
	usub := filepath.Join(d, "usub")
	os.Mkdir(usub, 0o755)
	ulink := filepath.Join(usub, "link")
	os.Symlink(prog, ulink)
	os.Chmod(usub, 0o777)
	if _, err := CheckProgram(ulink); !errors.Is(err, ErrProgramUnsafe) {
		t.Fatal("link in a world-writable directory accepted")
	}
	os.Chmod(usub, 0o755)

	// /tmp (sticky, world-writable) fails the directory rule
	if fi, err := os.Stat("/tmp"); err == nil && fi.Mode().Perm()&0o002 != 0 {
		tp, err := os.CreateTemp("/tmp", "rhzprog")
		if err == nil {
			tp.Close()
			defer os.Remove(tp.Name())
			os.Chmod(tp.Name(), 0o755)
			if _, err := CheckProgram(tp.Name()); !errors.Is(err, ErrProgramUnsafe) {
				t.Fatal("program under /tmp accepted")
			}
		}
	}
	for _, bad := range []string{"rel/prog", d + "/../" + filepath.Base(d) + "/prog", filepath.Join(d, "missing")} {
		if _, err := CheckProgram(bad); !errors.Is(err, ErrProgramUnsafe) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
