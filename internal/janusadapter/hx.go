package janusadapter

// Real hx process assembly (RHZ-046). Only the pure argv builders and
// the absent-socket error path are unit tested; live hx contact is exercised
// only by integration smoke checks, never an automated test dependency.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

const (
	EnvModeInherit   = "inherit"
	EnvModeAllowlist = "allowlist"
)

func defaultHxEnvironment(name string) bool {
	switch name {
	case "PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
		"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HX_RUNTIME_DIR":
		return true
	}
	return false
}

// hxEnv returns the subset of parent explicitly allowed for hx. Passthrough
// entries are operator-supplied variable names, matched exactly and
// case-sensitively. Missing variables are never invented.
func hxEnv(parent []string, passthrough []string) []string {
	allowed := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		allowed[name] = struct{}{}
	}

	values := make(map[string]string, len(allowed))
	for _, entry := range parent {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, ok := allowed[name]; defaultHxEnvironment(name) || ok {
			values[name] = entry
		}
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, values[name])
	}
	return env
}

// hxCommand is the single process-construction path for hx run and replay.
func hxCommand(hxPath string, cfg RunConfig, args ...string) *exec.Cmd {
	cmd := exec.Command(hxPath, args...)
	if cfg.EnvMode == EnvModeAllowlist {
		cmd.Env = hxEnv(os.Environ(), cfg.Passthrough)
	}
	return cmd
}

// RunArgv assembles the hx run argv from the operator config (contract v1.1
// confirmed surface). The request travels as a file path; --session is
// appended only when the operator configured one.
func RunArgv(cfg RunConfig, requestPath string) []string {
	args := []string{
		"run", "--request", requestPath,
		"--profile", cfg.ProfilePath,
		"--accept-root", cfg.AcceptRoot,
		"--world-config", cfg.WorldConfigPath,
		"--approval-endpoint", cfg.ApprovalEndpoint,
	}
	if cfg.SessionDB != "" {
		args = append(args, "--session", cfg.SessionDB)
	}
	return args
}

// ReplayArgv assembles the hx replay argv. v1 always reads the full log
// (ParseReplay skips the cursor prefix), so no --to bound is passed.
func ReplayArgv(sessionDB string) []string {
	return []string{"replay", "--session", sessionDB}
}

// RealDialer connects to the JANUS approval/control unix socket. An absent
// socket surfaces through the Client as UNAVAILABLE.
func RealDialer(path string) Dialer {
	return func() (net.Conn, error) { return net.Dial("unix", path) }
}

// RealReplay runs hx replay to completion and hands the captured stdout to
// ParseReplay. The trace identity is verified by the parser, not trusted here.
func RealReplay(hxPath string) ReplaySource {
	return func(cfg RunConfig, sessionDB, traceID string) (io.Reader, error) {
		out, err := hxCommand(hxPath, cfg, ReplayArgv(sessionDB)...).Output()
		if err != nil {
			return nil, fmt.Errorf("hx replay: %w", err)
		}
		return bytes.NewReader(out), nil
	}
}

// RealRunner materializes the strict v1 request bytes into requestDir and
// invokes hx run. It exists for the explicit start path only — the serve loop
// never invokes a Runner (RHZ-046/D7), so no automatic path reaches it.
func RealRunner(hxPath, requestDir string) Runner {
	return func(cfg RunConfig, requestJSON []byte) (io.Reader, error) {
		f, err := os.CreateTemp(requestDir, "request-*.json")
		if err != nil {
			return nil, err
		}
		if _, err = f.Write(requestJSON); err != nil {
			f.Close()
			_ = os.Remove(f.Name())
			return nil, err
		}
		if err = f.Close(); err != nil {
			_ = os.Remove(f.Name())
			return nil, err
		}
		requestPath := f.Name()
		cmd := hxCommand(hxPath, cfg, RunArgv(cfg, requestPath)...)
		// An explicit os.Pipe, not cmd.StdoutPipe: Wait closes a StdoutPipe
		// as soon as the child exits, which races with a reader that is
		// still draining it ("read |0: file already closed"). The parent
		// owns the read end here, so Wait never touches it; the reader
		// sees EOF when the child exits and closes the end itself.
		pr, pw, err := os.Pipe()
		if err != nil {
			_ = os.Remove(requestPath)
			return nil, fmt.Errorf("hx run stdout: %w", err)
		}
		cmd.Stdout = pw
		cmd.Stdin = os.Stdin
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			_ = pw.Close()
			_ = pr.Close()
			_ = os.Remove(requestPath)
			return nil, fmt.Errorf("hx run: %w", err)
		}
		_ = pw.Close() // the child holds its own copy of the write end
		go func() {
			_ = cmd.Wait()
			// The production consumer reads only the first control line, so
			// nothing else references pr while hx runs. Pin it until the child
			// has exited: otherwise the finalizer could close the read end
			// early and a later write by hx would hit EPIPE.
			runtime.KeepAlive(pr)
			_ = os.Remove(requestPath)
		}()
		return &eofCloser{File: pr}, nil
	}
}

// eofCloser closes the parent's read end of the hx stdout pipe once the
// stream is exhausted, so a fully drained run leaks no descriptor.
type eofCloser struct {
	*os.File
	closed bool
}

func (c *eofCloser) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	if err != nil && !c.closed {
		c.closed = true
		_ = c.File.Close()
	}
	return n, err
}
