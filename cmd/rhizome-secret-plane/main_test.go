package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rhizome/internal/secretplane"
	"rhizome/internal/secretwire"
)

type cmdEnv struct {
	env
	out, errb bytes.Buffer
	exited    []int
}

func newCmdEnv(t *testing.T, tty bool, configDir string) *cmdEnv {
	c := &cmdEnv{}
	c.env = env{stdin: strings.NewReader(""), stdout: &c.out, stderr: &c.errb, stdinFd: -1,
		isTerminal:   func(int) bool { return tty },
		readPassword: func(int) ([]byte, error) { t.Error("password read"); return nil, errors.New("x") },
		configDir:    func() (string, error) { return configDir, nil },
		exit:         func(code int) { c.exited = append(c.exited, code) }}
	return c
}

// TestFlagErrorsNonEchoFRRHZ139: parse errors print only the fixed usage
// phrase (P6 / MJ-2); sentinels in flag names, values and extra arguments
// never reach the output, and nothing is created.
func TestFlagErrorsNonEchoFRRHZ139(t *testing.T) {
	s := sentinel(t)
	cfg := filepath.Join(t.TempDir(), "cfg")
	abs := filepath.Join(t.TempDir(), "st")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, msgUsage},
		{[]string{s}, msgUsage},
		{[]string{"-" + s}, msgUsage},
		{[]string{"session", "-" + s}, msgUsage},
		{[]string{"session", "--" + s + "=" + s}, msgUsage},
		{[]string{"session", "-state-dir"}, msgUsage},
		{[]string{"session", "-state-dir", abs, s}, msgUsage},
		{[]string{"session", "-state-dir=" + s}, msgInvalidFlag},
		{[]string{"session", "-state-dir", abs, "-socket", s}, msgInvalidFlag},
		{[]string{"quarantine"}, msgUsage},
		{[]string{"quarantine", s}, msgUsage},
		{[]string{"quarantine", "list", "-" + s}, msgUsage},
		{[]string{"quarantine", "list", "-state-dir", abs, s}, msgUsage},
		{[]string{"quarantine", "list", "-state-dir", s}, msgInvalidFlag},
	}
	for _, c := range cases {
		e := newCmdEnv(t, true, cfg)
		if code := run(c.args, &e.env); code != 2 {
			t.Fatalf("exit %d", code)
		}
		if e.errb.String() != c.want+"\n" || e.out.Len() != 0 {
			t.Fatalf("stderr %q stdout %q", e.errb.String(), e.out.String())
		}
		assertNoSentinel(t, "stderr", e.errb.Bytes(), s)
	}
	for _, p := range []string{cfg, abs} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a parse error created state")
		}
	}
}

// TestHelpOutputFRRHZ139: -h prints "Usage of <name>:" and the static flag
// defaults to stderr, exit 2 (literal golden).
func TestHelpOutputFRRHZ139(t *testing.T) {
	session := "Usage of session:\n" +
		"  -socket string\n" +
		"    \tabsolute Unix socket path for Rhizome, at most 103 bytes (default <state-dir>/run/plane.sock)\n" +
		"  -state-dir string\n" +
		"    \tabsolute plane state directory, separate from any Rhizome data directory (default <user config dir>/rhizome-secret-plane)\n"
	list := "Usage of quarantine list:\n" +
		"  -state-dir string\n" +
		"    \tabsolute plane state directory, separate from any Rhizome data directory (default <user config dir>/rhizome-secret-plane)\n"
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"session", "-h"}, session},
		{[]string{"session", "-help"}, session},
		{[]string{"quarantine", "list", "-h"}, list},
	} {
		e := newCmdEnv(t, false, t.TempDir())
		if code := run(c.args, &e.env); code != 2 {
			t.Fatalf("exit %d", code)
		}
		if e.errb.String() != c.want || e.out.Len() != 0 {
			t.Fatalf("stderr:\n%s", e.errb.String())
		}
	}
}

// TestNonTTYRefusalFRRHZ139: without a terminal on stdin the session does no
// work: fixed message, nonzero exit, no state created or changed.
func TestNonTTYRefusalFRRHZ139(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "cfg")
	e := newCmdEnv(t, false, cfg)
	if code := run([]string{"session"}, &e.env); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if e.errb.String() != msgNoTerminal+"\n" || e.out.Len() != 0 {
		t.Fatalf("stderr %q", e.errb.String())
	}
	if _, err := os.Lstat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("state created without a terminal")
	}

	// an existing plane is untouched
	dir := filepath.Join(t.TempDir(), "st")
	pl, err := secretplane.Open(dir, secretplane.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pl.Close()
	led := readFile(t, filepath.Join(dir, "ledger.ndjson"))
	e = newCmdEnv(t, false, cfg)
	if code := run([]string{"session", "-state-dir", dir, "-socket", filepath.Join(dir, "run", "p.sock")}, &e.env); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !bytes.Equal(led, readFile(t, filepath.Join(dir, "ledger.ndjson"))) {
		t.Fatal("ledger changed")
	}
	if _, err := os.Lstat(filepath.Join(dir, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket dir created without a terminal")
	}
}

// TestQuarantineListCommandFRRHZ139: quarantine list works without a
// terminal, is value-free, and is refused with "busy" while a plane holds the
// state directory.
func TestQuarantineListCommandFRRHZ139(t *testing.T) {
	s := sentinel(t)
	dir := filepath.Join(t.TempDir(), "st")
	pl, err := secretplane.Open(dir, secretplane.Options{})
	if err != nil {
		t.Fatal(err)
	}
	op := newOpID(t)
	p := secretwire.Pending{OpID: op, Op: secretwire.OpRegister, Ref: secretwire.DeriveRef(op), Name: "k",
		RefKind: secretwire.RefKindOther, JournalSeq: 1}
	if _, err := pl.Apply(p, secretplane.Decision{Value: []byte(s)}); err != nil {
		t.Fatal(err)
	}
	pid, _ := pl.HelloPlaneID()
	if err := pl.Freeze(); err != nil {
		t.Fatal(err)
	}
	if err := pl.OnPoll(pid, secretwire.Ack{PlaneID: pid}, nil); err != nil {
		t.Fatal(err)
	}

	e := newCmdEnv(t, false, "")
	if code := run([]string{"quarantine", "list", "-state-dir", dir}, &e.env); code != 1 ||
		e.errb.String() != "rhizome-secret-plane: busy\n" || e.out.Len() != 0 {
		t.Fatalf("busy: %d %q", code, e.errb.String())
	}
	pl.Close()

	e = newCmdEnv(t, false, "")
	if code := run([]string{"quarantine", "list", "-state-dir", dir}, &e.env); code != 0 || e.errb.Len() != 0 {
		t.Fatalf("list: %d %q", code, e.errb.String())
	}
	want := pid + " 1 outcome " + op + " " + p.Ref + " after-cutoff\n"
	if e.out.String() != want {
		t.Fatalf("list output %q", e.out.String())
	}
	assertNoSentinel(t, "list", e.out.Bytes(), s)

	// default state dir comes from the user config dir
	cfg := t.TempDir()
	e = newCmdEnv(t, false, cfg)
	if code := run([]string{"quarantine", "list"}, &e.env); code != 1 || e.errb.String() != "rhizome-secret-plane: state-dir-unsafe\n" {
		t.Fatalf("missing default dir: %d %q", code, e.errb.String())
	}
}
