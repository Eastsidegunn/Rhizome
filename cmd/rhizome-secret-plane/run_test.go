package main

// FR-RHZ-174.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"rhizome/internal/secretplane"
	"rhizome/internal/secretwire"
)

// The test binary doubles as the plane binary ("s9-plane": run() with the
// real environment and syscall.Exec) and as the child program ("s9-child").
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "s9-plane":
			os.Exit(run(os.Args[2:], realEnv()))
		case "s9-child":
			os.Exit(childMain(os.Args[2:]))
		}
	}
	os.Exit(m.Run())
}

// childDigest is a salted digest (not a §8.1 form) so argv and profiles.json
// never carry a sentinel form.
func childDigest(v string) string {
	sum := sha256.Sum256([]byte("s9-child:" + v))
	return hex.EncodeToString(sum[:])
}

// childMain checks its environment without printing any value: args are the
// expected sorted names (comma-separated), then NAME=digest pairs, then any
// extra args. It prints "child ok extra=<n>" or "child mismatch".
func childMain(args []string) int {
	if len(args) < 1 {
		return 3
	}
	var names []string
	for _, kv := range os.Environ() {
		n, _, _ := strings.Cut(kv, "=")
		names = append(names, n)
	}
	sort.Strings(names)
	ok := strings.Join(names, ",") == args[0]
	extra := 0
	for _, a := range args[1:] {
		n, d, found := strings.Cut(a, "=")
		if !found {
			extra++
			continue
		}
		v, set := os.LookupEnv(n)
		ok = ok && set && childDigest(v) == d
	}
	if !ok {
		os.Stdout.WriteString("child mismatch\n")
		return 1
	}
	os.Stdout.WriteString("child ok extra=" + strconv.Itoa(extra) + "\n")
	return 0
}

type runResult struct {
	code        int
	stdout, err string
}

func runPlane(t *testing.T, args ...string) runResult {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, append([]string{"s9-plane"}, args...)...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "LANG=C", "UNRELATED=1",
		"GH_TOKEN=caller-token", "ANTHROPIC_API_KEY=caller-key", "PATH=/second"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return runResult{code, out.String(), errb.String()}
}

// TestRunInjectsValueFRRHZ174 execs the child through `run` while a plane
// holds the state-directory lock: the child gets exactly the stored values
// and only the allowlisted caller variables plus the injected names; extra
// args only when allowed; fixed errors otherwise. ZV over stdout, stderr and
// every plane file except values.json.
func TestRunInjectsValueFRRHZ174(t *testing.T) {
	s1, s2 := sentinel(t), sentinel(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self = filepath.Clean(self)
	dir := filepath.Join(t.TempDir(), "st")
	pl, err := secretplane.Open(dir, secretplane.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	r1, err := pl.RegisterDirect("hf", secretwire.RefKindProviderKey, nil, []byte(s1))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := pl.RegisterDirect("other", secretwire.RefKindOther, nil, []byte(s2))
	if err != nil {
		t.Fatal(err)
	}
	expect := "HF_TOKEN,HOME,LANG,OTHER_SECRET,PATH"
	childArgs := []string{"s9-child", expect, "HF_TOKEN=" + childDigest(s1), "OTHER_SECRET=" + childDigest(s2)}
	add := func(p secretplane.Profile) {
		t.Helper()
		if err := pl.AddProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"HF_TOKEN": r1.Ref, "OTHER_SECRET": r2.Ref}
	add(secretplane.Profile{Name: "fixed", Program: self, Args: childArgs, Env: env})
	add(secretplane.Profile{Name: "open", Program: self, Args: childArgs, AllowExtraArgs: true, Env: env})
	garbage := filepath.Join(t.TempDir(), "noexec")
	os.WriteFile(garbage, []byte("not a program\n"), 0o755)
	add(secretplane.Profile{Name: "broken", Program: garbage, Env: map[string]string{"HF_TOKEN": r1.Ref}})
	gone := filepath.Join(t.TempDir(), "gone")
	os.WriteFile(gone, []byte("#!/bin/sh\n"), 0o755)
	add(secretplane.Profile{Name: "gone", Program: gone, Env: map[string]string{"HF_TOKEN": r1.Ref}})
	os.Remove(gone)

	var outputs []string
	check := func(r runResult, code int, stdout, stderr string) {
		t.Helper()
		outputs = append(outputs, r.stdout, r.err)
		if r.code != code || r.stdout != stdout || r.err != stderr {
			t.Fatalf("got %d %q %q", r.code, r.stdout, r.err)
		}
	}
	check(runPlane(t, "run", "-state-dir", dir, "fixed"), 0, "child ok extra=0\n", "")
	check(runPlane(t, "run", "-state-dir", dir, "open", "--", "x", "y"), 0, "child ok extra=2\n", "")
	check(runPlane(t, "run", "-state-dir", dir, "fixed", "--", s1), 1, "", "rhizome-secret-plane: run: extra-args-refused\n")
	check(runPlane(t, "run", "-state-dir", dir, s1), 1, "", "rhizome-secret-plane: run: unknown-profile\n")
	check(runPlane(t, "run", "-state-dir", dir, "Bad/"+s1), 1, "", "rhizome-secret-plane: run: unknown-profile\n")
	check(runPlane(t, "run", "-state-dir", dir, "broken"), 1, "", "rhizome-secret-plane: run: exec-failed\n")
	check(runPlane(t, "run", "-state-dir", dir, "gone"), 1, "", "rhizome-secret-plane: run: exec-failed\n")
	for _, args := range [][]string{{"run"}, {"run", "-" + s1}, {"run", "-state-dir", dir, "fixed", s1},
		{"run", "-state-dir", s1, "fixed"}} {
		check(runPlane(t, args...), 2, "", "rhizome-secret-plane: run: usage\n")
	}

	// unsafe modes
	pf := filepath.Join(dir, "profiles.json")
	os.Chmod(pf, 0o644)
	check(runPlane(t, "run", "-state-dir", dir, "fixed"), 1, "", "rhizome-secret-plane: run: state-unsafe\n")
	os.Chmod(pf, 0o600)
	os.Chmod(filepath.Join(dir, "values.json"), 0o640)
	check(runPlane(t, "run", "-state-dir", dir, "fixed"), 1, "", "rhizome-secret-plane: run: state-unsafe\n")
	os.Chmod(filepath.Join(dir, "values.json"), 0o600)

	// value missing: the values file no longer holds one of the refs
	pl.Close()
	vf := filepath.Join(dir, "values.json")
	data, _ := os.ReadFile(vf)
	os.WriteFile(vf, []byte(`{"v":1,"values":[]}`+"\n"), 0o600)
	check(runPlane(t, "run", "-state-dir", dir, "fixed"), 1, "", "rhizome-secret-plane: run: value-missing\n")
	os.WriteFile(vf, data, 0o600)

	all := []byte(strings.Join(outputs, "\n"))
	assertNoSentinel(t, "run output", all, s1, s2)
	for _, f := range []string{"ledger.ndjson", "quarantine.ndjson", "profiles.json"} {
		assertNoSentinel(t, f, readFile(t, filepath.Join(dir, f)), s1, s2)
	}
}

// TestRunEnvFRRHZ174: the child environment is the allowlist (first
// occurrence) plus the injected variables, nothing else.
func TestRunEnvFRRHZ174(t *testing.T) {
	vars := []secretplane.RunVar{{Name: "HF_TOKEN", Value: []byte("v1")}}
	env := buildRunEnv([]string{"HOME=/h", "GH_TOKEN=x", "PATH=/a", "PATH=/b", "X=1", "SHELL=/s", "BAD"}, vars)
	if strings.Join(env, " ") != "HOME=/h PATH=/a SHELL=/s HF_TOKEN=v1" {
		t.Fatalf("env %q", env)
	}
}

// TestRunExecFailureFRRHZ174: when exec fails, run prints only the fixed
// error and returns at once (main exits). It claims no more: the env
// strings passed to exec are immutable copies that live until that exit.
func TestRunExecFailureFRRHZ174(t *testing.T) {
	s := sentinel(t)
	dir := filepath.Join(t.TempDir(), "st")
	pl, err := secretplane.Open(dir, secretplane.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := pl.RegisterDirect("k", secretwire.RefKindOther, nil, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(t.TempDir(), "p")
	os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755)
	if err := pl.AddProfile(secretplane.Profile{Name: "p", Program: prog, Env: map[string]string{"K": r.Ref}}); err != nil {
		t.Fatal(err)
	}
	pl.Close()
	calls := 0
	e := newCmdEnv(t, false, "")
	e.environ = func() []string { return nil }
	e.exec = func(argv0 string, argv, envv []string) error {
		calls++
		if argv0 != prog || len(argv) != 1 || len(envv) != 1 || envv[0] != "K="+s {
			t.Error("exec arguments")
		}
		return os.ErrPermission
	}
	if code := run([]string{"run", "-state-dir", dir, "p"}, &e.env); code != 1 || calls != 1 ||
		e.errb.String() != "rhizome-secret-plane: run: exec-failed\n" || e.out.Len() != 0 {
		t.Fatalf("%d %q", code, e.errb.String())
	}
	assertNoSentinel(t, "stderr", e.errb.Bytes(), s)
}

// TestSessionProfilesFRRHZ174: profile-add / profiles / profile-remove in
// the scripted session: typed confirmations, attested refs refused, no value
// prompt, value-free output and files.
func TestSessionProfilesFRRHZ174(t *testing.T) {
	s := sentinel(t)
	u := newUnit(t)
	prog := filepath.Join(t.TempDir(), "hf")
	os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755)
	u.pw.push(s, s)
	u.run("register", "hf-token", "provider-key", "", "yes", "attest", "broker", "provider-key", "", "attest")
	var stored, att string
	refs, _ := u.pl.Refs()
	for _, r := range refs {
		if r.Attested() {
			att = r.Ref
		} else {
			stored = r.Ref
		}
	}

	// attested ref: refused by the plane after confirmation
	out := u.run("profile-add", "hf", prog, "", "no", "HF_TOKEN", att, "", "add profile")
	mustContain(t, out, "error: unknown-ref")
	// allowlisted variable name and relative program: invalid input
	out = u.run("profile-add", "hf", prog, "", "no", "PATH", stored,
		"profile-add", "hf", "rel/hf")
	if strings.Count(out, msgInvalidInput) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	// wrong phrase: nothing recorded
	out = u.run("profile-add", "hf", prog, "download", "--quiet", "", "no", "HF_TOKEN", stored, "", "yes")
	mustContain(t, out, msgNotConfirmed)
	if ps, _ := u.pl.Profiles(); len(ps) != 0 {
		t.Fatal("unconfirmed profile recorded")
	}
	out = u.run("profile-add", "hf", prog, "download", "--quiet", "", "no", "HF_TOKEN", stored, "", "add profile", "profiles")
	mustContain(t, out, "profile added: hf")
	mustContain(t, out, `hf program="`+prog+`" args="download" "--quiet" extra-args=refused HF_TOKEN=`+stored)
	out = u.run("profile-remove hf", "nope", "profile-remove hf", "remove profile", "profiles")
	mustContain(t, out, msgNotConfirmed)
	mustContain(t, out, "profile removed: hf")
	mustContain(t, out, "no profiles")

	assertNoSentinel(t, "terminal", []byte(u.out.String()), s)
	assertWording(t, "terminal", u.out.String())
	for _, f := range []string{"ledger.ndjson", "quarantine.ndjson", "profiles.json"} {
		assertNoSentinel(t, f, u.file(f), s)
	}
}
