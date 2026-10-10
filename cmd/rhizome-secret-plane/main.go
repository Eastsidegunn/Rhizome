// Command rhizome-secret-plane is the secret plane process (RHZ-107 G1,
// missions M5a/M5b, FR-RHZ-138/139): it holds secret values in its own state
// directory, answers Rhizome's channel on a Unix socket, and lets the human
// at the terminal act on Rhizome's register/rotate/revoke requests.
//
// Commands:
//
//	rhizome-secret-plane session [-state-dir <abs>] [-socket <abs>]
//	rhizome-secret-plane quarantine list [-state-dir <abs>]
//
// -state-dir is the plane state directory. It must be absolute and must be a
// directory of its own, separate from any Rhizome data directory (the plane
// files are not Rhizome's; Rhizome never reads them). Default:
// <os.UserConfigDir()>/rhizome-secret-plane. It is created with mode 0700.
//
// -socket is the Unix socket Rhizome dials (serve -secret-plane-socket).
// The path is configuration only: the plane never prints or records it.
// Default: <state-dir>/run/plane.sock. Its parent directory is created with
// mode 0700 when absent and must be a real directory owned by this user
// without group/other permission bits. The path must be at most 103 bytes
// (macOS sun_path); with a long state directory pass a shorter -socket.
//
// session requires an interactive terminal on stdin and refuses all work
// otherwise. There is no non-interactive completion path: no --yes, no
// auto-approve configuration, no API that confirms, no batch rotate/revoke
// (split plan §4, D19). quarantine list is read-only and needs no terminal.
//
// Flag parse errors print only "rhizome-secret-plane: usage" (exit 2); -h
// prints "Usage of <command>:" and the flag defaults (exit 2) (SCP §1.8
// MJ-2, P6). Values are never printed and never part of an error message.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"rhizome/internal/peercred"
	"rhizome/internal/secretplane"
	"rhizome/internal/secretplane/server"
	"rhizome/internal/termio"
)

const (
	progName        = "rhizome-secret-plane"
	stateDirName    = "rhizome-secret-plane"
	socketSubdir    = "run"
	socketName      = "plane.sock"
	msgUsage        = progName + ": usage"
	msgInvalidFlag  = progName + ": invalid flag value"
	msgNoTerminal   = progName + ": refused: an interactive terminal is required"
	msgFatal        = progName + ": fatal: plane storage failed; restart required"
	msgStateDirFlag = "absolute plane state directory, separate from any Rhizome data directory (default <user config dir>/" + stateDirName + ")"
	msgSocketFlag   = "absolute Unix socket path for Rhizome, at most 103 bytes (default <state-dir>/" + socketSubdir + "/" + socketName + ")"
)

// env is the process environment; tests replace parts of it.
type env struct {
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	stdinFd int

	isTerminal   func(fd int) bool
	readPassword func(fd int) ([]byte, error)
	configDir    func() (string, error)
	// exit ends the process at once (fatal plane errors, P3).
	exit func(code int)
	// verify is the server's W2 seam; nil means peercred.Verify.
	verify func(*net.UnixConn) (peercred.Verified, error)
	// started is a test hook called once the server listens.
	started func(*server.Server)
}

func realEnv() *env {
	return &env{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, stdinFd: int(os.Stdin.Fd()),
		isTerminal: termio.IsTerminal, readPassword: termio.ReadPassword, configDir: os.UserConfigDir, exit: os.Exit}
}

func main() { os.Exit(run(os.Args[1:], realEnv())) }

func run(args []string, e *env) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, msgUsage)
		return 2
	}
	switch args[0] {
	case "session":
		return runSession(args[1:], e)
	case "quarantine":
		if len(args) < 2 || args[1] != "list" {
			fmt.Fprintln(e.stderr, msgUsage)
			return 2
		}
		return runQuarantineList(args[2:], e)
	}
	fmt.Fprintln(e.stderr, msgUsage)
	return 2
}

// parse applies the P6 / MJ-2 FlagSet rules: nothing is echoed; -h prints
// the static usage. ok is false when the caller must return code.
func parse(f *flag.FlagSet, args []string, e *env) (code int, ok bool) {
	f.SetOutput(io.Discard)
	err := f.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		f.SetOutput(e.stderr)
		if f.Usage != nil {
			f.Usage()
		} else {
			fmt.Fprintf(e.stderr, "Usage of %s:\n", f.Name())
			f.PrintDefaults()
		}
		return 2, false
	case err != nil, f.NArg() != 0:
		fmt.Fprintln(e.stderr, msgUsage)
		return 2, false
	}
	return 0, true
}

// stateDir resolves -state-dir (absolute, cleaned) or its default.
func stateDir(v string, e *env) (string, bool) {
	if v == "" {
		base, err := e.configDir()
		if err != nil || !filepath.IsAbs(base) {
			return "", false
		}
		return filepath.Join(base, stateDirName), true
	}
	if !filepath.IsAbs(v) {
		return "", false
	}
	return filepath.Clean(v), true
}

// errCode maps library errors to fixed codes; anything else is "error".
func errCode(err error) string {
	for _, k := range []error{secretplane.ErrStateDirUnsafe, secretplane.ErrFileUnsafe, secretplane.ErrBusy,
		secretplane.ErrUnsupported, secretplane.ErrIO, secretplane.ErrLedgerCorrupt, secretplane.ErrQuarantineCorrupt,
		secretplane.ErrValuesCorrupt, secretplane.ErrFatal, secretplane.ErrClosed, secretplane.ErrInvalidPending,
		secretplane.ErrInvalidInput, secretplane.ErrInvalidDecision, secretplane.ErrUnknownOp, secretplane.ErrUnknownRef,
		secretplane.ErrDone, secretplane.ErrOpBusy, secretplane.ErrDisplayOnly, secretplane.ErrFrozen,
		secretplane.ErrEpochPhase, secretplane.ErrHalted, secretplane.ErrStalePlane} {
		if errors.Is(err, k) {
			return strings.TrimPrefix(k.Error(), "secretplane: ")
		}
	}
	for _, k := range []error{server.ErrSocketPath, server.ErrSocketDirUnsafe, server.ErrSocketInUse, server.ErrListen} {
		if errors.Is(err, k) {
			return k.Error()
		}
	}
	return "error"
}

func runQuarantineList(args []string, e *env) int {
	f := flag.NewFlagSet("quarantine list", flag.ContinueOnError)
	dirFlag := f.String("state-dir", "", msgStateDirFlag)
	if code, ok := parse(f, args, e); !ok {
		return code
	}
	dir, ok := stateDir(*dirFlag, e)
	if !ok {
		fmt.Fprintln(e.stderr, msgInvalidFlag)
		return 2
	}
	items, err := secretplane.ListQuarantine(dir)
	if err != nil {
		fmt.Fprintln(e.stderr, progName+": "+errCode(err))
		return 1
	}
	io.WriteString(e.stdout, secretplane.FormatQuarantineList(items))
	return 0
}

func runSession(args []string, e *env) int {
	f := flag.NewFlagSet("session", flag.ContinueOnError)
	dirFlag := f.String("state-dir", "", msgStateDirFlag)
	sockFlag := f.String("socket", "", msgSocketFlag)
	if code, ok := parse(f, args, e); !ok {
		return code
	}
	// P6 / split plan M5b: no terminal, no work (nothing is created).
	if !e.isTerminal(e.stdinFd) {
		fmt.Fprintln(e.stderr, msgNoTerminal)
		return 1
	}
	dir, ok := stateDir(*dirFlag, e)
	if !ok {
		fmt.Fprintln(e.stderr, msgInvalidFlag)
		return 2
	}
	sock := filepath.Join(dir, socketSubdir, socketName)
	if *sockFlag != "" {
		if !filepath.IsAbs(*sockFlag) {
			fmt.Fprintln(e.stderr, msgInvalidFlag)
			return 2
		}
		sock = filepath.Clean(*sockFlag)
	}
	if len(sock) > server.MaxSocketPath {
		fmt.Fprintln(e.stderr, progName+": "+server.ErrSocketPath.Error())
		return 2
	}

	out := &lockedWriter{w: e.stdout}
	fatal := func() {
		fmt.Fprintln(e.stderr, msgFatal)
		e.exit(1)
	}
	fmt.Fprintln(out, secretplane.Banner)
	pl, err := secretplane.Open(dir, secretplane.Options{OnFatal: fatal})
	if err != nil {
		fmt.Fprintln(e.stderr, progName+": "+errCode(err))
		if errors.Is(err, secretplane.ErrFatal) {
			e.exit(1)
		}
		return 1
	}
	defer pl.Close()
	srv, err := server.Listen(server.Config{Plane: pl, SocketPath: sock, Verify: e.verify, OnFatal: fatal,
		OnNotice: func(n string) { fmt.Fprintln(out, "\nnotice: "+n) }})
	if err != nil {
		fmt.Fprintln(e.stderr, progName+": "+errCode(err))
		return 1
	}
	served := make(chan struct{})
	go func() {
		srv.Serve()
		close(served)
	}()
	defer func() {
		srv.Close()
		<-served
	}()
	// The socket path is configuration and is never printed (SCP S4).
	fmt.Fprintln(out, "socket ready")
	if e.started != nil {
		e.started(srv)
	}
	s := newSession(pl, srv.Queue, e.stdin, out, func() ([]byte, error) { return e.readPassword(e.stdinFd) }, fatal)
	s.loop()
	return 0
}
