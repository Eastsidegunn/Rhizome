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
)

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
		out, err := exec.Command(hxPath, ReplayArgv(sessionDB)...).Output()
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
		cmd := exec.Command(hxPath, RunArgv(cfg, requestPath)...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = os.Remove(requestPath)
			return nil, fmt.Errorf("hx run stdout: %w", err)
		}
		cmd.Stdin = os.Stdin
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			_ = os.Remove(requestPath)
			return nil, fmt.Errorf("hx run: %w", err)
		}
		go func() {
			_ = cmd.Wait()
			_ = os.Remove(requestPath)
		}()
		return stdout, nil
	}
}
