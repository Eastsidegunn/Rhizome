package sessionlauncher

// RHZ-142 (FR-RHZ-186): sessionMode "interactive" starts the mission as a
// claude background session (`claude --bg`, a pty daemon) instead of a
// claude -p child: the same env and prompt (as the first input), the same
// execution sequence (intent → dispatch_claimed → spawn → accepted), and the
// session is registered with its short id so the cockpit attaches to it at
// once. claude --bg returns as soon as the daemon is up: the launcher tracks
// no child, keeps no slot and journals no outcome; the session ends when a
// human ends it, and serve's observation closes it in the registry.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"rhizome/internal/execution"
	"rhizome/internal/session"
)

// ModeInteractive is the sessionMode of a claude --bg session.
const ModeInteractive = "interactive"

// BgTimeout bounds the claude --bg invocation (it exits once the daemon runs).
const BgTimeout = 30 * time.Second

// BgArgs is the exact claude argv (without argv[0]) of an interactive
// session. The prompt is positional and precedes the variadic --allowedTools.
func (c Config) BgArgs(prompt, sessionID string) []string {
	a := []string{"--bg", prompt, "--session-id", sessionID, "--model", c.Model, "--permission-mode", c.PermissionMode}
	if len(c.AllowedTools) > 0 {
		a = append(a, "--allowedTools")
		a = append(a, c.AllowedTools...)
	}
	return a
}

// launchInteractiveLocked is Start for ModeInteractive; the caller holds
// startMu and has done the existing/busy checks.
func (l *Launcher) launchInteractiveLocked(sp spec, r execution.Ref, exists bool, sid string) (string, string, error) {
	es := execution.Service{Store: l.Store}
	var err error
	if !exists {
		if r, err = es.IntentWithProvenance(sp.mission, sp.key, sp.requested, sp.ceiling, sp.prov); err != nil {
			return "", "", err
		}
	}
	id := r.ID
	if r.State == execution.Intent {
		if _, err = es.ClaimDispatch(id, Target, sp.claimCorr); err != nil {
			return id, "", err
		}
	}
	if sid == "" {
		return id, "session spawn failed: no session id", nil
	}
	if err := l.spawnBg(sp, sid); err != nil {
		return id, "session spawn failed: " + err.Error(), nil
	}
	if _, err = es.Accept(id, sid); err != nil {
		// the daemon runs without an acceptance: the spawn marker stays, so a
		// re-submission asks for human resolution
		return id, "", err
	}
	p := session.Opened{SessionID: sid, Name: sp.mission, MissionID: sp.mission, Mode: session.Interactive, Cwd: sp.workdir, ExecutionID: id, Kind: session.KindBackground}
	if agents, err := session.ListAgents(l.Cfg.ClaudePath, 0); err == nil {
		if a, ok := session.FindAgent(agents, sid); ok && a.Attachable() {
			p.AttachID = a.ID
		}
	}
	// Without the short id yet, the next observation reopens it attachable.
	_, err = (session.Service{Store: l.Store}).Adopt(p)
	l.fault("session", id, err)
	return id, "", nil
}

// spawnBg runs claude --bg in the session's workdir and waits for it to
// exit. The spawn marker is kept on success (the daemon may be running).
func (l *Launcher) spawnBg(sp spec, sid string) error {
	c := l.Cfg
	marker := l.markerPath(sp.execID)
	mf, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("spawn marker: %w", err)
	}
	defer mf.Close()
	fail := func(err error) error {
		mf.Close()
		_ = os.Remove(marker)
		return err
	}
	if _, err := mf.WriteString("bg\n"); err != nil {
		return fail(err)
	}
	logf, err := os.OpenFile(filepath.Join(c.LogDir, sp.execID+".bg.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	defer logf.Close()
	environ := os.Environ
	if l.Environ != nil {
		environ = l.Environ
	}
	env := BuildEnv(environ(), EnvAllowlist)
	// the daemon drives a real pty an attach client renders: not TERM=dumb
	env[len(env)-1] = "TERM=xterm-256color"
	ctx, cancel := context.WithTimeout(context.Background(), BgTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.ClaudePath, c.BgArgs(Prompt(l.BoardURL, sp.mission, sp.text), sid)...)
	cmd.Dir = sp.workdir
	cmd.Env = append(env, SessionEnv(l.BoardURL, sp.mission, sid)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		// a timed-out --bg may still have started the daemon: keep the marker
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("claude --bg timed out")
		}
		return fail(fmt.Errorf("claude --bg failed"))
	}
	return nil
}
