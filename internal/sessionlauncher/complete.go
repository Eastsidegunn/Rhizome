package sessionlauncher

// RHZ-124 S1 (FR-RHZ-TBD(124-S1)): completion of one launched session.
// Wait (bounded by the effective timeMs or a serve shutdown: stop request →
// SIGTERM group → grace → SIGKILL group; after every exit the remaining
// process group is swept the same way), parse the last "result" line of the stream-json
// log, journal the outcome on the execution, move the mission running →
// waiting_for_result (never auto-succeeded) and hand the usage to Report.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

// Denial is one permission denial the harness reported (tool input is not
// carried: it may hold file contents or commands).
type Denial struct {
	ToolName  string `json:"toolName"`
	ToolUseID string `json:"toolUseId"`
}

// Outcome is one finished session. Usage fields are copied as the harness
// reported them (json.Number / raw JSON — never recomputed or priced); nil
// = the session log had no result line (usage unavailable).
type Outcome struct {
	MissionID, ExecutionID, SessionID, Model string
	State                                    execution.State
	TimedOut                                 bool
	// Stopped is why the launcher stopped the session: "timeout",
	// "shutdown" (serve stopping) or "" (it exited on its own).
	Stopped                            string
	UsageAvailable                     bool
	ModelUsage                         map[string]json.RawMessage
	TotalCostUSD, NumTurns, DurationMs *json.Number
	TerminalReason                     *string
	IsError                            *bool
	// PermissionDenials holds at most MaxDenials entries;
	// PermissionDenialCount is the total the harness reported.
	PermissionDenials     []Denial
	PermissionDenialCount int
	LogDigest             string
}

// MaxDenials bounds the denials carried into the usage note.
const MaxDenials = 20

// MaxNoteBytes is the relay's note.create content limit.
const MaxNoteBytes = 16 * 1024

type resultLine struct {
	Type           string                     `json:"type"`
	Subtype        string                     `json:"subtype"`
	SessionID      string                     `json:"session_id"`
	IsError        bool                       `json:"is_error"`
	Result         *string                    `json:"result"`
	TotalCostUSD   *json.Number               `json:"total_cost_usd"`
	NumTurns       *json.Number               `json:"num_turns"`
	DurationMs     *json.Number               `json:"duration_ms"`
	TerminalReason *string                    `json:"terminal_reason"`
	RawDenials     []rawDenial                `json:"permission_denials"`
	ModelUsage     map[string]json.RawMessage `json:"modelUsage"`
}

type rawDenial struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

// parseLog returns the last well-formed "result" line of a stream-json log
// (nil when there is none) and the log's sha256 digest.
func parseLog(b []byte) (*resultLine, string) {
	sum := sha256.Sum256(b)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var last *resultLine
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.Contains(line, []byte(`"result"`)) {
			continue
		}
		var r resultLine
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if dec.Decode(&r) != nil || r.Type != "result" {
			continue
		}
		last = &r
	}
	return last, digest
}

func summarize(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (l *Launcher) fault(scope, id string, err error) {
	if err != nil && l.OnError != nil {
		l.OnError(scope, id, err)
	}
}

func (l *Launcher) grace() time.Duration {
	if l.KillGrace <= 0 {
		return 5 * time.Second
	}
	return l.KillGrace
}

// reapGroup ends whatever is left of the child's process group after the
// leader exited: SIGTERM, wait up to the grace for the group to empty, then
// SIGKILL. A group that is already empty costs nothing.
func (l *Launcher) reapGroup(ch *child) {
	if syscall.Kill(-ch.pgid, syscall.SIGTERM) != nil {
		return // ESRCH: no member left
	}
	deadline := time.Now().Add(l.grace())
	for time.Now().Before(deadline) {
		if syscall.Kill(-ch.pgid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	ch.signal(syscall.SIGKILL)
}

func (l *Launcher) complete(ch *child) {
	defer l.wg.Done()
	defer func() {
		l.mu.Lock()
		delete(l.running, ch.execID)
		l.mu.Unlock()
	}()
	es := execution.Service{Store: l.Store}
	done := make(chan error, 1)
	go func() { done <- ch.cmd.Wait() }()
	timer := time.NewTimer(ch.timeout)
	defer timer.Stop()
	stopped, reason := "", ""
	select {
	case <-done:
	case <-timer.C:
		stopped, reason = "timeout", "budget_exceeded"
	case <-ch.stop:
		stopped, reason = "shutdown", "user"
	}
	if stopped != "" {
		// The durable stop intent precedes the kill (same order as the
		// JANUS stop path); a journal fault never keeps the child alive.
		_, err := es.RequestStop(ch.execID, reason, 0, Actor)
		l.fault("stop", ch.execID, err)
		ch.signal(syscall.SIGTERM)
		g := time.NewTimer(l.grace())
		select {
		case <-done:
			g.Stop()
		case <-g.C:
			ch.signal(syscall.SIGKILL)
			<-done
		}
	}
	// Background descendants never outlive the session.
	l.reapGroup(ch)
	l.finish(ch, stopped)
}

func (l *Launcher) finish(ch *child, stopped string) {
	es := execution.Service{Store: l.Store}
	b, err := os.ReadFile(ch.stdoutPath)
	l.fault("log", ch.execID, err)
	res, digest := parseLog(b)
	o := Outcome{MissionID: ch.missionID, ExecutionID: ch.execID, SessionID: ch.sessionID, Model: l.Cfg.Model, TimedOut: stopped == "timeout", Stopped: stopped, LogDigest: digest, State: execution.Failed}
	summary := "no result line in session log"
	if res != nil {
		o.UsageAvailable = true
		o.ModelUsage = res.ModelUsage
		if o.ModelUsage == nil {
			o.ModelUsage = map[string]json.RawMessage{}
		}
		o.TotalCostUSD, o.NumTurns, o.DurationMs, o.TerminalReason = res.TotalCostUSD, res.NumTurns, res.DurationMs, res.TerminalReason
		isErr := res.IsError
		o.IsError = &isErr
		o.PermissionDenials = []Denial{}
		o.PermissionDenialCount = len(res.RawDenials)
		for i, d := range res.RawDenials {
			if i >= MaxDenials {
				break
			}
			o.PermissionDenials = append(o.PermissionDenials, Denial{ToolName: d.ToolName, ToolUseID: d.ToolUseID})
		}
		if res.Result != nil {
			summary = summarize(*res.Result, 200)
		} else {
			summary = "result line without result text"
		}
		if res.Result != nil && !res.IsError {
			o.State = execution.Succeeded
		}
		if res.SessionID != "" && res.SessionID != ch.sessionID {
			l.fault("session", ch.execID, fmt.Errorf("result session_id %q differs from launched session %q", res.SessionID, ch.sessionID))
		}
	}
	switch stopped {
	case "timeout":
		o.State = execution.Cancelled
		summary = summarize(fmt.Sprintf("stopped: timeout after %dms; %s", ch.timeout.Milliseconds(), summary), 200)
	case "shutdown":
		o.State = execution.Cancelled
		summary = summarize("stopped: serve shutdown; "+summary, 200)
	}
	_, err = es.ObserveState(ch.execID, "", summary, digest, o.State)
	l.fault("observe", ch.execID, err)
	if err == nil {
		// Terminal and durable: the spawn marker has done its job.
		l.fault("marker", ch.execID, os.Remove(l.markerPath(ch.execID)))
	}
	l.fault("mission", ch.missionID, l.toWaiting(ch.missionID))
	if l.Report != nil {
		l.Report(o)
	}
}

// toWaiting moves the mission running → waiting_for_result. The relay moves
// it to running right after Start returns, so a very short session may finish
// first: a mission still on the way (planned/ready/paused) is polled for a
// bounded time. Any other state means an operator moved it — skip silently.
func (l *Launcher) toWaiting(id string) error {
	ms := mission.Service{Store: l.Store}
	wait := l.RunningWait
	if wait <= 0 {
		wait = 5 * time.Second
	}
	deadline := time.Now().Add(wait)
	for attempt := 0; ; attempt++ {
		m, err := projector.ReplayMission(l.Store.List("mission", id))
		if err != nil {
			return err
		}
		switch m.State {
		case domain.MissionRunning:
			_, err = ms.Transition(id, m.Revision, domain.MissionWaitingResult)
			if err == nil || !errors.Is(err, events.ErrRevisionConflict) || attempt > 8 {
				return err
			}
			continue
		case domain.MissionPlanned, domain.MissionReady, domain.MissionPaused:
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return nil
	}
}

type noteBody struct {
	ExecutionID           string                     `json:"executionId"`
	SessionID             string                     `json:"sessionId"`
	Model                 string                     `json:"model"`
	ModelUsage            map[string]json.RawMessage `json:"modelUsage"`
	TotalCostUSD          *json.Number               `json:"totalCostUsd"`
	NumTurns              *json.Number               `json:"numTurns"`
	DurationMs            *json.Number               `json:"durationMs"`
	TerminalReason        *string                    `json:"terminalReason"`
	IsError               *bool                      `json:"isError"`
	PermissionDenials     []Denial                   `json:"permissionDenials"`
	PermissionDenialCount *int                       `json:"permissionDenialsCount"`
	ResumeOf              *string                    `json:"resumeOf"`
	LogDigest             string                     `json:"logDigest"`
}

// NoteContent is the usage note body: one human line plus a fenced JSON
// block. Unavailable usage is null, never estimated. It stays within the
// relay's 16KiB note limit: denials are already capped at MaxDenials, and
// if the note is still too large it is re-rendered compact without the
// denial list (the count stays; modelUsage is never dropped).
func (o Outcome) NoteContent() string {
	body := noteBody{ExecutionID: o.ExecutionID, SessionID: o.SessionID, Model: o.Model, ModelUsage: o.ModelUsage, TotalCostUSD: o.TotalCostUSD, NumTurns: o.NumTurns,
		DurationMs: o.DurationMs, TerminalReason: o.TerminalReason, IsError: o.IsError, PermissionDenials: o.PermissionDenials, LogDigest: o.LogDigest}
	if o.UsageAvailable {
		n := o.PermissionDenialCount
		if n < len(o.PermissionDenials) {
			n = len(o.PermissionDenials)
		}
		body.PermissionDenialCount = &n
	}
	var line string
	if o.UsageAvailable {
		cost, turns := "?", "?"
		if o.TotalCostUSD != nil {
			cost = o.TotalCostUSD.String()
		}
		if o.NumTurns != nil {
			turns = o.NumTurns.String()
		}
		line = fmt.Sprintf("claude-local session usage for %s (model %s): total cost %s USD as reported by the harness, %s turns, is_error=%t, execution %s.", o.MissionID, o.Model, cost, turns, o.IsError != nil && *o.IsError, o.State)
	} else {
		line = fmt.Sprintf("claude-local session usage unavailable for %s (model %s): no result line in the session log, execution %s.", o.MissionID, o.Model, o.State)
	}
	switch o.Stopped {
	case "timeout":
		line += " Stopped on timeout."
	case "shutdown":
		line += " Stopped on serve shutdown."
	}
	render := func(b noteBody, indent bool) string {
		var out []byte
		var err error
		if indent {
			out, err = json.MarshalIndent(b, "", "  ")
		} else {
			out, err = json.Marshal(b)
		}
		if err != nil {
			// Raw model usage that is not valid JSON cannot come out of the
			// decoder; keep the note honest anyway.
			b.ModelUsage = nil
			out, _ = json.Marshal(b)
		}
		return line + "\n\n```json\n" + strings.TrimSpace(string(out)) + "\n```\n"
	}
	note := render(body, true)
	if len(note) > MaxNoteBytes {
		body.PermissionDenials = nil
		note = render(body, false)
	}
	return note
}
