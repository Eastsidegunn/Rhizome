package main

// `run` (RHZ-107 S9, FR-RHZ-174): exec one program with stored
// values injected into its environment, per a profile [H] added in the
// session. It consumes values and never manages or confirms anything: no
// prompt, no write, no plane lock. The process image is replaced by the
// program (syscall.Exec), so no plane process lingers holding a value. A
// value is never written to stdout, stderr, argv or a file; the []byte
// values it reads are wiped before exec, while the env strings handed to exec
// (and the runtime's copy) live until exec replaces the process or, on
// failure, until the immediate exit; errors are the
// fixed strings "rhizome-secret-plane: run: <code>" without any profile
// name, argument or path.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"rhizome/internal/secretplane"
	"rhizome/internal/secretwire"
)

// run error codes.
const (
	runUsage        = "usage"
	runUnknown      = "unknown-profile"
	runExtraRefused = "extra-args-refused"
	runValueMissing = "value-missing"
	runStateUnsafe  = "state-unsafe"
	runExecFailed   = "exec-failed"
)

func runFail(e *env, code string) int {
	fmt.Fprintln(e.stderr, progName+": run: "+code)
	if code == runUsage {
		return 2
	}
	return 1
}

// buildRunEnv keeps the allowlisted names of environ (first occurrence) and
// appends NAME=value for each injected variable. The returned strings are
// ordinary immutable copies: they (and the runtime's own copy made by
// syscall.Exec) cannot be wiped and live until exec replaces the process
// image or, on exec failure, until the immediate exit.
func buildRunEnv(environ []string, vars []secretplane.RunVar) []string {
	allow := map[string]bool{}
	for _, n := range secretplane.RunEnvAllowlist {
		allow[n] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, kv := range environ {
		name, _, found := strings.Cut(kv, "=")
		if !found || !allow[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, kv)
	}
	for _, v := range vars {
		out = append(out, v.Name+"="+string(v.Value))
	}
	return out
}

func runRun(args []string, e *env) int {
	f := flag.NewFlagSet("run", flag.ContinueOnError)
	dirFlag := f.String("state-dir", "", msgStateDirFlag)
	f.SetOutput(io.Discard)
	err := f.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		f.SetOutput(e.stderr)
		fmt.Fprintf(e.stderr, "Usage of %s:\n", f.Name())
		f.PrintDefaults()
		return 2
	case err != nil:
		return runFail(e, runUsage)
	}
	rest := f.Args()
	if len(rest) == 0 {
		return runFail(e, runUsage)
	}
	name, extra := rest[0], rest[1:]
	if len(extra) > 0 {
		if extra[0] != "--" {
			return runFail(e, runUsage)
		}
		extra = extra[1:]
	}
	dir, ok := stateDir(*dirFlag, e)
	if !ok {
		return runFail(e, runUsage)
	}
	if !secretwire.ValidName(name) {
		return runFail(e, runUnknown)
	}
	spec, err := secretplane.ResolveRun(dir, name)
	if err != nil {
		switch {
		case errors.Is(err, secretplane.ErrUnknownProfile):
			return runFail(e, runUnknown)
		case errors.Is(err, secretplane.ErrValueMissing):
			return runFail(e, runValueMissing)
		}
		return runFail(e, runStateUnsafe)
	}
	if len(extra) > 0 && !spec.AllowExtraArgs {
		spec.Wipe()
		return runFail(e, runExtraRefused)
	}
	if !secretplane.ExecutableFile(spec.Program) {
		spec.Wipe()
		return runFail(e, runExecFailed)
	}
	argv := append(append([]string{spec.Program}, spec.Args...), extra...)
	envv := buildRunEnv(e.environ(), spec.Env)
	spec.Wipe()                      // the []byte values this process owns
	e.exec(spec.Program, argv, envv) // returns only on failure
	// The env strings cannot be wiped; the caller exits right after this.
	return runFail(e, runExecFailed)
}
