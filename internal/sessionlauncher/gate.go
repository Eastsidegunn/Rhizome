package sessionlauncher

// RHZ-124 S2 (FR-RHZ-124-S2): permission denials → one internal gate → the
// human's decision resumes the same claude session.
//
// A session that ended on its own with permission_denials raises exactly one
// gate (correlation "launcher:<executionId>") through AskGate and the
// mission waits for the human. The relay calls GateDecided after the
// decision is durable; the launcher re-derives everything it needs from the
// journal and the digest-verified session log — never from the gate text:
//
//	approve            → resume with ledger tools ∪ denied tool names (this
//	                     execution only; the human-approved widening is the
//	                     resume ceiling)
//	reject             → resume without extra tools, told to finish
//	reject "STOP …"    → no resume; a mission note says so
//
// The resume is a new execution (intent → claim → spawn → accept) keyed by
// (gate.resume, Target, gateId, decision): re-delivery never spawns twice.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rhizome/internal/domain"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
	"rhizome/internal/projector"
)

// GatePrefix prefixes the correlation id of every launcher denial gate; the
// rest is the execution id whose denials raised it.
const GatePrefix = "launcher:"

// MaxToolInputBytes bounds one denial's rendered tool input in the gate body.
const MaxToolInputBytes = 512

// maxToolNameBytes bounds a tool name the launcher will pass to
// --allowedTools; longer (or flag-shaped) names are never approved.
const maxToolNameBytes = 128

// maxParagraphBytes bounds the result-text paragraph quoted in the gate body.
const maxParagraphBytes = 1500

// GateRequest is the one gate a denied session raises (question.ask through
// the relay at the composition root).
type GateRequest struct {
	MissionID, ExecutionID, CorrelationID, Name, Body string
}

// GateDecision is a recorded internal gate decision handed over by the relay.
type GateDecision struct {
	GateID, CorrelationID, MissionID string
	// Decision is "approve" or "reject".
	Decision, Reason, Actor string
}

// toolNameRE is the shape of a grantable tool name: plain names as claude
// reports them in permission_denials (Bash, Read, mcp__x__y). Rule-style
// names (parentheses, spaces, commas) are never granted — one argv entry
// must widen exactly one tool.
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// validTool reports a denied tool name the launcher may pass on argv:
// toolNameRE, bounded, not flag-shaped.
func validTool(name string) bool {
	return len(name) <= maxToolNameBytes && !strings.HasPrefix(name, "-") && toolNameRE.MatchString(name)
}

// deniedTools is the distinct valid tool names of the denials, in first-seen
// order, at most MaxDenials; skipped counts the invalid ones. Both the gate
// body and the resume use this one derivation.
func deniedTools(ds []rawDenial) (names []string, skipped int) {
	seen := map[string]bool{}
	names = []string{}
	for _, d := range ds {
		if seen[d.ToolName] {
			continue
		}
		seen[d.ToolName] = true
		if !validTool(d.ToolName) {
			skipped++
			continue
		}
		if len(names) < MaxDenials {
			names = append(names, d.ToolName)
		}
	}
	return names, skipped
}

// truncBytes cuts s to at most n bytes on a rune boundary.
func truncBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	k := n
	for k > 0 && !utf8.RuneStart(s[k]) {
		k--
	}
	return s[:k], true
}

// toolInputSummary is the compact JSON of one tool input, at most
// MaxToolInputBytes.
func toolInputSummary(raw json.RawMessage) string {
	var b bytes.Buffer
	if len(raw) == 0 || json.Compact(&b, raw) != nil {
		return "null"
	}
	out, cut := truncBytes(b.String(), MaxToolInputBytes)
	if cut {
		return out + " …(잘림)"
	}
	return out
}

// lastParagraph is the last non-blank paragraph of the result text, bounded.
func lastParagraph(res *resultLine) string {
	if res.Result == nil {
		return ""
	}
	parts := strings.Split(strings.ReplaceAll(*res.Result, "\r\n", "\n"), "\n\n")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(parts[i]); p != "" {
			out, cut := truncBytes(p, maxParagraphBytes)
			if cut {
				out += " …(잘림)"
			}
			return out
		}
	}
	return ""
}

// gateRequest renders the one denial gate of an execution. The body names
// the execution so two executions never collapse into one question id. It
// must stay deterministic (no time, no randomness): the resume binding
// recomputes the gate id from it. Changing the wording changes the content
// id and permanently invalidates every launcher gate still open across an
// upgrade (their decisions are then refused; TestGateRequestGoldenFRRHZ124S2
// pins the text).
func gateRequest(missionID, execID string, res *resultLine) GateRequest {
	names, skipped := deniedTools(res.RawDenials)
	list := "(없음)"
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	title, _ := truncBytes("RHZ 런처 승인 요청: "+list, 300)
	var b strings.Builder
	fmt.Fprintf(&b, "세션 런처 실행 %s (미션 %s)이 권한 거부로 끝났다. 거부 %d건", execID, missionID, len(res.RawDenials))
	if len(res.RawDenials) > MaxDenials {
		fmt.Fprintf(&b, " (앞 %d건 표시)", MaxDenials)
	}
	b.WriteString(":\n")
	for i, d := range res.RawDenials {
		if i >= MaxDenials {
			break
		}
		// A raw (invalid) name is quoted so it cannot forge body lines.
		name := d.ToolName
		if !validTool(name) {
			cut, _ := truncBytes(name, maxToolNameBytes)
			name = strconv.Quote(cut)
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, toolInputSummary(d.ToolInput))
	}
	if p := lastParagraph(res); p != "" {
		b.WriteString("\n결과 마지막 문단:\n" + p + "\n")
	}
	b.WriteString("\n승인 = 같은 세션을 재개하며 이번 재개 1회에 한해 다음 도구를 허용한다: " + list + "\n")
	if skipped > 0 {
		fmt.Fprintf(&b, "(형식이 잘못된 도구 이름 %d개는 승인해도 허용되지 않는다.)\n", skipped)
	}
	b.WriteString("거부 = 이 도구 없이 같은 세션을 재개해 마무리시킨다. 거부 사유의 첫 단어가 STOP이면 재개하지 않는다.\n(STOP 규칙: 공백으로 나눈 첫 단어가 정확히 대문자 STOP일 때만 — \"STOP\" 단독 또는 \"STOP 사유…\". \"STOP:\"·\"stop\"은 해당하지 않는다.)")
	return GateRequest{MissionID: missionID, ExecutionID: execID, CorrelationID: GatePrefix + execID, Name: title, Body: b.String()}
}

// firstWord is the reason's first whitespace-delimited word.
func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// logCwd is the cwd of the session's init line (claude stream-json
// {"type":"system","subtype":"init","cwd":…}); "" when absent.
func logCwd(b []byte) string {
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.Contains(line, []byte(`"init"`)) {
			continue
		}
		var x struct{ Type, Subtype, Cwd string }
		if json.Unmarshal(line, &x) == nil && x.Type == "system" && x.Subtype == "init" {
			return x.Cwd
		}
	}
	return ""
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

// ledgerWorkdir maps the original session's cwd back to a ledger workdir: a
// resumed claude session is only found from the directory it ran in.
func (c Config) ledgerWorkdir(cwd string) (string, bool) {
	if cwd == "" {
		return "", false
	}
	names := make([]string, 0, len(c.Workdirs))
	for n := range c.Workdirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if samePath(cwd, c.Workdirs[n]) {
			return c.Workdirs[n], true
		}
	}
	return "", false
}

func (l *Launcher) note(missionID, content string) {
	if l.Note == nil {
		return
	}
	l.fault("note", missionID, l.Note(missionID, content))
}

// GateDecided handles one recorded internal gate decision. Gates without the
// launcher prefix are ignored. It never reports back to the relay: the
// decision is already durable; a refused resume is a mission note plus
// OnError and the mission stays waiting_for_human. A decision that arrives
// while the original session's completion is still running (the gate is
// asked inside it) waits, asynchronously and bounded, for that completion.
func (l *Launcher) GateDecided(d GateDecision) { l.decide(d, true) }

// Retry is GateDecided for a journal re-scan (FR-RHZ-124-S2): a refusal is
// reported through OnError only — the first delivery already left the
// mission note.
func (l *Launcher) Retry(d GateDecision) { l.decide(d, false) }

// ResumeExecutionID is the id of the resume execution a gate decision owns
// (journal-derivable: the composition root's re-scan looks it up).
func ResumeExecutionID(gateID, decision string) string {
	return "exec-" + hashOf("gate.resume", Target, gateID, decision)
}

func (l *Launcher) decide(d GateDecision, notify bool) {
	if !strings.HasPrefix(d.CorrelationID, GatePrefix) {
		return
	}
	switch d.Decision {
	case "approve":
	case "reject":
		if firstWord(d.Reason) == "STOP" {
			if notify {
				l.note(d.MissionID, fmt.Sprintf("세션 런처 gate %s 가 STOP으로 거부되었다: 재개하지 않았다 (실행 %s). 사유: %s", d.GateID, strings.TrimPrefix(d.CorrelationID, GatePrefix), d.Reason))
			}
			return
		}
	default:
		return
	}
	l.mu.Lock()
	orig := l.running[strings.TrimPrefix(d.CorrelationID, GatePrefix)]
	if orig != nil {
		// The original's completion still runs and is counted in wg; Add
		// under mu, before it can leave running.
		l.wg.Add(1)
	}
	l.mu.Unlock()
	if orig == nil {
		l.resumeOrRefuse(d, notify)
		return
	}
	go func() {
		defer l.wg.Done()
		t := time.NewTimer(l.runningWait() + l.grace())
		defer t.Stop()
		select {
		case <-orig.done:
		case <-t.C:
		}
		l.resumeOrRefuse(d, notify)
	}()
}

func (l *Launcher) runningWait() time.Duration {
	if l.RunningWait <= 0 {
		return 5 * time.Second
	}
	return l.RunningWait
}

func (l *Launcher) resumeOrRefuse(d GateDecision, notify bool) {
	reason := l.resume(d)
	if reason == "" {
		return
	}
	if notify {
		l.note(d.MissionID, fmt.Sprintf("세션 런처 gate %s (%s) 재개 거부: %s. 미션은 waiting_for_human에 머문다. 일시적 사유(busy·종료 중·spawn 실패)면 슬롯이 빌 때와 serve 시작 때 자동 재시도된다; 그 밖에는 사람이 미션 상태를 정하라.", d.GateID, d.Decision, reason))
	}
	l.fault("resume", d.GateID, errors.New(reason))
}

// resumeSpec derives the resume execution from the original one. A
// non-empty string is the refusal.
func (l *Launcher) resumeSpec(d GateDecision) (spec, string) {
	if l.Cfg == nil || l.Store == nil {
		return spec{}, "session launcher config not loaded"
	}
	c := l.Cfg
	origID := strings.TrimPrefix(d.CorrelationID, GatePrefix)
	log := l.Store.List("execution", origID)
	if origID == "" || len(log) == 0 {
		return spec{}, "original execution not found (" + origID + ")"
	}
	orig, err := execution.Replay(log)
	if err != nil {
		return spec{}, "original execution invalid: " + err.Error()
	}
	switch {
	case orig.Target != Target:
		return spec{}, "original execution is not a " + Target + " execution"
	case orig.State != execution.Succeeded && orig.State != execution.Failed && orig.State != execution.Cancelled:
		return spec{}, "original execution not terminal: " + string(orig.State)
	case !uuidRE.MatchString(orig.ExternalID):
		return spec{}, "original execution has no session uuid"
	case orig.MissionID == "" || orig.MissionID != d.MissionID:
		return spec{}, "gate mission does not match the original execution"
	case orig.Provenance == nil || orig.Provenance.Effective.Units != Units:
		return spec{}, "original execution has no launcher policy provenance"
	}
	b, err := os.ReadFile(filepath.Join(c.LogDir, origID+".ndjson"))
	if err != nil {
		return spec{}, "original session log unreadable"
	}
	res, digest := parseLog(b)
	if orig.SourceRef == "" || digest != orig.SourceRef {
		return spec{}, "original session log digest mismatch"
	}
	if res == nil || len(res.RawDenials) == 0 {
		return spec{}, "original session log has no permission denials"
	}
	// M1 binding: only the gate this original raised can resume it. Its id
	// is recomputed from the verified log (gateRequest is deterministic).
	if l.GateID == nil {
		return spec{}, "gate binding unavailable"
	}
	if want, err := l.GateID(gateRequest(orig.MissionID, origID, res)); err != nil || want == "" || want != d.GateID {
		return spec{}, "gate " + d.GateID + " is not the denial gate of " + origID
	}
	dir, ok := c.ledgerWorkdir(logCwd(b))
	if !ok {
		return spec{}, "original session workdir unknown or not in ledger"
	}
	tools := append([]string{}, c.AllowedTools...)
	names, _ := deniedTools(res.RawDenials)
	var text string
	if d.Decision == "approve" && len(names) == 0 {
		return spec{}, "no grantable tool name among the denials"
	}
	if d.Decision == "approve" {
		have := map[string]bool{}
		for _, t := range tools {
			have[t] = true
		}
		for _, n := range names {
			if !have[n] {
				have[n] = true
				tools = append(tools, n)
			}
		}
		text = "승인됨"
		if strings.TrimSpace(d.Reason) != "" {
			text += ": " + d.Reason
		}
		text += " — 이번 재개에 한해 다음 도구가 허용되었다: " + strings.Join(names, ", ") + ". 거부되었던 작업을 이어서 마무리하라."
	} else {
		text = "거부됨: " + d.Reason + " — 이 도구 없이 마무리하고 끝내라"
	}
	// The human-approved widening is this execution's ceiling (Merge would
	// otherwise intersect the approved tools away); budget and time are the
	// original execution's effective values, still capped by the ledger.
	ceiling := c.ceilingPolicy()
	ceiling.Capabilities = toolCapabilities(tools)
	ceiling = policy.Merge(ceiling, ceiling).Policy
	requested := ceiling
	requested.Budget, requested.Timeout = orig.Provenance.Effective.Budget, orig.Provenance.Effective.Timeout
	merged := policy.Merge(requested, ceiling)
	if merged.Invalid || merged.Empty || merged.Policy.Units != Units {
		return spec{}, "POLICY_DENIED: effective policy empty"
	}
	eff := merged.Policy
	who := d.Actor
	if !strings.HasPrefix(who, "unverified-local-operator:") {
		who = "unverified-local-operator:" + who
	}
	key := strings.TrimPrefix(ResumeExecutionID(d.GateID, d.Decision), "exec-")
	digestC := c.Digest()
	prov := execution.Provenance{Ceiling: ceiling, Requested: requested, Effective: eff, ProfileID: Target, ProfileHash: strings.TrimPrefix(digestC, "sha256:"), ExecConfigDigest: digestC, Actor: who}
	return spec{key: key, execID: "exec-" + key, requested: requested, ceiling: ceiling, prov: prov, workdir: dir, cents: eff.Budget, timeMs: eff.Timeout, mode: "oneshot",
		mission: orig.MissionID, text: text, claimCorr: "gate.resume:" + d.GateID, resumeSession: orig.ExternalID, resumeOf: origID, tools: tools}, ""
}

// resume starts the resume execution; "" = started or already started by
// an earlier delivery of the same decision.
func (l *Launcher) resume(d GateDecision) string {
	sp, reason := l.resumeSpec(d)
	if reason != "" {
		return reason
	}
	l.startMu.Lock()
	defer l.startMu.Unlock()
	if l.closed {
		return "session launcher shutting down"
	}
	r, ok, err := l.existing(sp.execID, sp.key)
	if err != nil {
		return "store: " + err.Error()
	}
	if ok {
		switch r.State {
		case execution.Accepted, execution.Observing, execution.Succeeded, execution.Failed, execution.Cancelled:
			return "" // this decision already resumed the session
		}
		if reason := l.existingReason(r); reason != "" {
			return reason
		}
	}
	m, err := projector.ReplayMission(l.Store.List("mission", sp.mission))
	if err != nil {
		return "mission: " + err.Error()
	}
	if m.State != domain.MissionWaitingHuman {
		return "mission is " + string(m.State) + ", not waiting_for_human"
	}
	if reason := l.busyReason(); reason != "" {
		return reason
	}
	id, ch, reason, err := l.launchLocked(sp, r, ok, sp.resumeSession)
	if err != nil {
		return "store: " + err.Error() + " (" + id + ")"
	}
	if reason != "" {
		return reason + " (" + id + ")"
	}
	// Accepted: the mission runs again before the completion can observe it.
	if m, err = projector.ReplayMission(l.Store.List("mission", sp.mission)); err == nil {
		_, err = (mission.Service{Store: l.Store}).Transition(sp.mission, m.Revision, domain.MissionRunning)
	}
	if err != nil {
		l.fault("mission", sp.mission, fmt.Errorf("resume %s accepted but mission not moved to running: %w", id, err))
	}
	l.track(ch)
	return ""
}
