package sessionlauncher

// RHZ-142 (FR-RHZ-186): sessionMode "interactive" starts the mission as a
// claude background session (`claude --bg`, a pty daemon) instead of a
// claude -p child: the same env and prompt (as the first input), the same
// execution sequence (intent → dispatch_claimed → spawn → accepted), and the
// session is registered with its short id so the cockpit attaches to it at
// once. claude --bg returns as soon as the daemon is up: the launcher tracks
// no child and journals no outcome; the session ends when a human ends it,
// and serve's observation closes it in the registry.
//
// RHZ-143 (FR-RHZ-187): --max-budget-usd is a -p flag, so an interactive
// session has no dollar ceiling; its time ceiling is enforced here instead
// (EnforceCeilings: `claude stop <attachId>`, then session.closed
// {reason: ceiling-time}). Open launched sessions count against the ledger's
// maxInteractive (not maxConcurrent). startMu covers the checks, the
// intent/claim and the slot reservation only; claude --bg, claude agents and
// the acceptance run outside it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"rhizome/internal/execution"
	"rhizome/internal/session"
)

// ModeInteractive is the sessionMode of a claude --bg session.
const ModeInteractive = "interactive"

// BgTimeout bounds the claude --bg invocation (it exits once the daemon runs)
// and one claude stop.
const BgTimeout = 30 * time.Second

// CeilingInterval is how often serve checks interactive time ceilings.
const CeilingInterval = 15 * time.Second

// errStop is the fixed error of a failed claude stop (output never echoed).
var errStop = errors.New("claude stop failed")

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

func launchingReason(execID string) string {
	return "session launch in progress (" + execID + ")"
}

// openInteractive counts the open --bg sessions this launcher started (the
// registry links them to an execution) plus in-flight launches. startMu held.
func (l *Launcher) openInteractive() (int, error) {
	all, err := session.AllSessions(l.Store)
	if err != nil {
		return 0, err
	}
	n := len(l.launching)
	for _, s := range all {
		if s.State == session.Open && s.Kind == session.KindBackground && s.ExecutionID != "" {
			n++
		}
	}
	return n, nil
}

// interactiveBusyReason is the maxInteractive refusal ("" = a slot is free).
// startMu held.
func (l *Launcher) interactiveBusyReason() string {
	n, err := l.openInteractive()
	if err != nil {
		return "session launcher busy: interactive sessions unreadable"
	}
	if c := l.Cfg.InteractiveCap(); n >= c {
		return fmt.Sprintf("session launcher busy: %d interactive open (maxInteractive %d)", n, c)
	}
	return ""
}

// reserveInteractive does, under startMu, what must be serialized: the
// existing/busy checks, intent and dispatch claim, and the slot reservation.
// launch=false: nothing to spawn (refused, or already accepted).
func (l *Launcher) reserveInteractive(sp spec) (id, sid, reason string, launch bool, err error) {
	l.startMu.Lock()
	defer l.startMu.Unlock()
	if l.closed {
		return "", "", "session launcher shutting down", false, nil
	}
	if l.launching[sp.execID] {
		return sp.execID, "", launchingReason(sp.execID), false, nil
	}
	r, ok, err := l.existing(sp.execID, sp.key)
	if err != nil {
		return "", "", "", false, err
	}
	if ok {
		if reason := l.existingReason(r); reason != "" {
			return r.ID, "", reason, false, nil
		}
		if r.State == execution.Accepted || r.State == execution.Observing {
			return r.ID, "", "", false, nil
		}
	}
	if reason := l.interactiveBusyReason(); reason != "" {
		return sp.execID, "", reason, false, nil
	}
	es := execution.Service{Store: l.Store}
	if !ok {
		if r, err = es.IntentWithProvenance(sp.mission, sp.key, sp.requested, sp.ceiling, sp.prov); err != nil {
			return "", "", "", false, err
		}
	}
	id = r.ID
	if r.State == execution.Intent {
		if _, err = es.ClaimDispatch(id, Target, sp.claimCorr); err != nil {
			return id, "", "", false, err
		}
	}
	sid, _ = newUUID()
	if sid == "" {
		return id, "", "session spawn failed: no session id", false, nil
	}
	if l.launching == nil {
		l.launching = map[string]bool{}
	}
	l.launching[sp.execID] = true
	l.wg.Add(1) // Wait covers the launch: no acceptance after the journal closes
	return id, sid, "", true, nil
}

func (l *Launcher) releaseInteractive(execID string) {
	l.startMu.Lock()
	delete(l.launching, execID)
	l.startMu.Unlock()
	l.wg.Done()
}

// startInteractive is Start for ModeInteractive.
func (l *Launcher) startInteractive(sp spec) (string, string, error) {
	id, sid, reason, launch, err := l.reserveInteractive(sp)
	if err != nil || !launch {
		return id, reason, err
	}
	defer l.releaseInteractive(sp.execID)
	if err := l.spawnBg(sp, sid); err != nil {
		return id, "session spawn failed: " + err.Error(), nil
	}
	if l.beforeAccept != nil {
		l.beforeAccept()
	}
	if _, err = (execution.Service{Store: l.Store}).Accept(id, sid); err != nil {
		// the daemon runs without an acceptance: the spawn marker stays, so a
		// re-submission asks for human resolution
		return id, "", err
	}
	p := session.Opened{SessionID: sid, Name: sp.mission, MissionID: sp.mission, Mode: session.Interactive, Cwd: sp.workdir, ExecutionID: id, Kind: session.KindBackground}
	if agents, err := session.ListAgents(context.Background(), l.Cfg.ClaudePath, 0); err == nil {
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

// ceilingDeadline is when a launched session's execution reaches its
// effective time ceiling: the acceptance's journal time + policy timeout.
func (l *Launcher) ceilingDeadline(execID string) (time.Time, bool) {
	log := l.Store.List("execution", execID)
	r, err := execution.Replay(log)
	if err != nil || r.Policy == nil || r.Policy.Timeout <= 0 {
		return time.Time{}, false
	}
	for _, e := range log {
		if e.Type == "execution.accepted" && !e.CreatedAt.IsZero() {
			return e.CreatedAt.Add(time.Duration(r.Policy.Timeout) * time.Millisecond), true
		}
	}
	return time.Time{}, false
}

// stopBg runs claudePath stop <attachId>.
func (l *Launcher) stopBg(ctx context.Context, attachID string) error {
	ctx, cancel := context.WithTimeout(ctx, BgTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.Cfg.ClaudePath, "stop", attachID)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if cmd.Run() != nil {
		return errStop
	}
	return nil
}

// EnforceCeilings stops every open launched --bg session past its time
// ceiling (`claude stop <attachId>`) and closes it with reason ceiling-time;
// it returns the sessions closed. A failed stop, or a session whose short id
// is not known, writes nothing and is retried on the next call. Scanning the
// journal (not a timer) keeps the ceiling across serve restarts.
func (l *Launcher) EnforceCeilings(ctx context.Context, now time.Time) (int, error) {
	all, err := session.AllSessions(l.Store)
	if err != nil {
		return 0, err
	}
	var agents []session.Agent
	listed := false
	closed := 0
	var firstErr error
	for _, s := range all {
		if s.State != session.Open || s.Kind != session.KindBackground || s.ExecutionID == "" {
			continue
		}
		if dl, ok := l.ceilingDeadline(s.ExecutionID); !ok || now.Before(dl) {
			continue
		}
		ref := s.AttachID
		if ref == "" {
			if !listed {
				agents, _ = session.ListAgents(ctx, l.Cfg.ClaudePath, 0)
				listed = true
			}
			if a, ok := session.FindAgent(agents, s.SessionID); ok && a.Attachable() {
				ref = a.ID
			}
		}
		if ref == "" {
			continue // not listed: the observation closes it as gone
		}
		did, err := (session.Service{Store: l.Store}).Retire(s.SessionID, session.ReasonCeilingTime, func() error { return l.stopBg(ctx, ref) })
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if did {
			closed++
		}
	}
	return closed, firstErr
}

// RunCeilings calls EnforceCeilings every interval until ctx ends; onWrite
// (nil = nothing) runs after a call that closed a session.
func (l *Launcher) RunCeilings(ctx context.Context, interval time.Duration, onWrite func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n, err := l.EnforceCeilings(ctx, now)
			if ctx.Err() == nil {
				l.fault("ceiling", "interactive", err)
			}
			if n > 0 && onWrite != nil {
				onWrite()
			}
		}
	}
}
