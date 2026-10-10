package main

// RHZ-131 (FR-RHZ-172): `rhizome agent <verb>` — the agent-facing board
// client. An agent session (Claude Code, pi, Codex, ...) writes its own
// progress, blockers, completion, usage, notes and questions to a running
// board instead of hand-rolling HTTP. It is a thin client over two board
// surfaces only: POST /v1/intent and GET /v1/context (the task bundle is
// read with include=prompt so the mission prompt is shown). It never sends a
// terminal intent (mission.complete/fail): closing a mission is the
// operator's call.
//
// Exit codes (FR-RHZ-172): 0 accepted; 1 board rejected (stderr carries
// "rejected: " + the server's Reason verbatim) or an HTTP/network error;
// 2 usage error.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agentDefaultBoard is used when neither --board nor RHIZOME_BOARD is set.
const agentDefaultBoard = "http://127.0.0.1:8790"

// agentDeps holds every outside-world dependency of the agent CLI so tests
// inject env, clock, HTTP client and file reads (FR-RHZ-172).
type agentDeps struct {
	getenv   func(string) string
	now      func() time.Time
	client   *http.Client
	readFile func(string) ([]byte, error)
}

func defaultAgentDeps() agentDeps {
	return agentDeps{getenv: os.Getenv, now: time.Now, client: &http.Client{Timeout: 30 * time.Second}, readFile: os.ReadFile}
}

const agentUsage = `usage: rhizome agent <verb> [<mission>] [args] [flags]

verbs:
  context  <mission> [--no-record]           read the mission bundle; records the start
  start    <mission>                         record the start explicitly
  progress <mission> --action TEXT [--pct N] record what is happening now (N = 0-100)
  blocked  <mission> LINE [--body-file F] [--tag T]...
  done     <mission> LINE [--body-file F] [--tag T]...
  usage    <mission> --model M --in N --out N [--cache-read N] [--cache-write N]
  note     <mission> LINE [--kind K] [--body-file F] [--tag T]...
  ask      <mission> TITLE (--body TEXT | --body-file F) [--recommendation TEXT]

common flags:
  --board URL     board base URL (env RHIZOME_BOARD, default ` + agentDefaultBoard + `)
  --actor NAME    who is writing (env RHIZOME_SESSION; required)
  --mission ID    mission id or handle (env RHIZOME_MISSION) when not given positionally
`

func agentCmd(args []string, out, errOut io.Writer) int {
	return runAgent(args, out, errOut, defaultAgentDeps())
}

type agentStrings []string

func (s *agentStrings) String() string     { return strings.Join(*s, ",") }
func (s *agentStrings) Set(v string) error { *s = append(*s, v); return nil }

// agentCLI is one parsed invocation: common flags, verb flags and the
// positional arguments (flags may appear anywhere, "--" ends flags).
type agentCLI struct {
	deps    agentDeps
	out     io.Writer
	errOut  io.Writer
	board   string
	actor   string
	mission string
	pos     []string
	set     map[string]bool
}

// parseInterspersed parses flags placed before, between or after the
// positional arguments (the stdlib parser stops at the first positional).
func parseInterspersed(f *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := f.Parse(rest); err != nil {
			return nil, err
		}
		left := f.Args()
		consumed := len(rest) - len(left)
		if endedByTerminator(f, rest[:consumed]) {
			return append(pos, left...), nil
		}
		if len(left) == 0 {
			break
		}
		pos = append(pos, left[0])
		rest = left[1:]
	}
	return pos, nil
}

// endedByTerminator reports whether the parser consumed a standalone "--"
// terminator, as opposed to "--" taken as a flag's value (--action --).
// It replays the consumed tokens with the flag set's own arity rules.
func endedByTerminator(f *flag.FlagSet, consumed []string) bool {
	for i := 0; i < len(consumed); i++ {
		tok := consumed[i]
		if tok == "--" {
			return true
		}
		name := strings.TrimLeft(tok, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if fl := f.Lookup(name); fl != nil {
			if b, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
				continue
			}
			i++ // the next token is this flag's value
		}
	}
	return false
}

func agentUsageErr(errOut io.Writer, msg string) int {
	fmt.Fprintln(errOut, msg)
	return 2
}

// newAgentCLI parses common + verb flags. textArgs is the number of
// positionals the verb takes after the mission; the mission itself is the
// first positional when one more is given, else --mission / RHIZOME_MISSION.
func newAgentCLI(verb string, args []string, out, errOut io.Writer, deps agentDeps, textArgs int, define func(*flag.FlagSet)) (*agentCLI, int) {
	f := flag.NewFlagSet("agent "+verb, flag.ContinueOnError)
	f.SetOutput(errOut)
	board := f.String("board", "", "")
	actor := f.String("actor", "", "")
	mission := f.String("mission", "", "")
	if define != nil {
		define(f)
	}
	pos, err := parseInterspersed(f, args)
	if err != nil {
		return nil, 2
	}
	c := &agentCLI{deps: deps, out: out, errOut: errOut, set: map[string]bool{}}
	f.Visit(func(v *flag.Flag) { c.set[v.Name] = true })
	c.board = *board
	if c.board == "" {
		c.board = deps.getenv("RHIZOME_BOARD")
	}
	if c.board == "" {
		c.board = agentDefaultBoard
	}
	c.board = strings.TrimRight(c.board, "/")
	c.actor = strings.TrimSpace(*actor)
	if c.actor == "" {
		c.actor = strings.TrimSpace(deps.getenv("RHIZOME_SESSION"))
	}
	if c.actor == "" {
		return nil, agentUsageErr(errOut, "actor required: set RHIZOME_SESSION or --actor")
	}
	switch len(pos) {
	case textArgs + 1:
		if c.set["mission"] {
			return nil, agentUsageErr(errOut, "mission given twice (positional and --mission)")
		}
		c.mission, c.pos = pos[0], pos[1:]
	case textArgs:
		c.mission, c.pos = *mission, pos
		if c.mission == "" {
			c.mission = deps.getenv("RHIZOME_MISSION")
		}
	default:
		return nil, agentUsageErr(errOut, fmt.Sprintf("agent %s: wrong number of arguments\n\n%s", verb, agentUsage))
	}
	c.mission = strings.TrimSpace(c.mission)
	if c.mission == "" {
		return nil, agentUsageErr(errOut, "mission required: give <mission>, --mission or RHIZOME_MISSION")
	}
	return c, 0
}

// errRejected marks a board rejection; its message is the Reason verbatim.
type errRejected struct{ reason string }

func (e errRejected) Error() string { return e.reason }

// intent POSTs one intent; actor is always attached. A nil error means
// Accepted. Rejections return errRejected; transport/HTTP failures other errors.
func (c *agentCLI) intent(kind string, fields map[string]any) error {
	body := map[string]any{"kind": kind, "actor": c.actor}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := c.deps.client.Post(c.board+"/v1/intent", "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("board: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("board: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var res struct {
		Accepted bool
		Reason   string
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("board: invalid response: %s", strings.TrimSpace(string(raw)))
	}
	if !res.Accepted {
		return errRejected{res.Reason}
	}
	return nil
}

// get fetches a board JSON document into v.
func (c *agentCLI) get(path string, q url.Values, v any) error {
	resp, err := c.deps.client.Get(c.board + path + "?" + q.Encode())
	if err != nil {
		return fmt.Errorf("board: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("board: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("board: invalid response: %v", err)
	}
	return nil
}

// report prints an intent failure and returns the exit code (1).
func (c *agentCLI) report(err error) int {
	var rj errRejected
	if errors.As(err, &rj) {
		fmt.Fprintln(c.errOut, "rejected: "+rj.reason)
	} else {
		fmt.Fprintln(c.errOut, err.Error())
	}
	return 1
}

// content builds "<line>" or "<line>\n\n<file>" for --body-file.
func (c *agentCLI) content(line, bodyFile string) (string, int) {
	if strings.TrimSpace(line) == "" {
		return "", agentUsageErr(c.errOut, "one-line summary required")
	}
	// The trailer makes every report a distinct note: the note id is
	// sha256(content), so without it the same summary on another mission
	// would conflict and a repeated blocker would be silently deduped. The
	// first line stays the summary (attention shows it as the cause).
	trailer := fmt.Sprintf("\n\nactor=%s mission=%s at=%s", c.actor, c.mission, c.deps.now().UTC().Format(time.RFC3339))
	if bodyFile == "" {
		return line + trailer, 0
	}
	b, err := c.deps.readFile(bodyFile)
	if err != nil {
		return "", agentUsageErr(c.errOut, fmt.Sprintf("body file: %v", err))
	}
	return line + "\n\n" + string(b) + trailer, 0
}

func runAgent(args []string, out, errOut io.Writer, deps agentDeps) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(errOut, agentUsage)
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "context":
		return agentContext(rest, out, errOut, deps)
	case "start":
		return agentStart(rest, out, errOut, deps)
	case "progress":
		return agentProgress(rest, out, errOut, deps)
	case "blocked", "done", "note":
		return agentNote(verb, rest, out, errOut, deps)
	case "usage":
		return agentUsageReport(rest, out, errOut, deps)
	case "ask":
		return agentAsk(rest, out, errOut, deps)
	}
	fmt.Fprintf(errOut, "unknown agent verb %q\n\n%s", verb, agentUsage)
	return 2
}

// ---- context / start -------------------------------------------------------

type agentContextBundle struct {
	Task struct {
		ID            string  `json:"id"`
		Name          string  `json:"name"`
		State         string  `json:"state"`
		Assignee      string  `json:"assignee"`
		CurrentAction string  `json:"currentAction"`
		Progress      float64 `json:"progress"`
		HasProgress   bool    `json:"hasProgress"`
		Prompt        string  `json:"prompt"`
	} `json:"task"`
	Goal struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		State       string `json:"state"`
	} `json:"goal"`
	Memories []agentMemory `json:"memories"`
	Steps    []struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		State string   `json:"state"`
		After []string `json:"after"`
		Gates []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			State          string `json:"state"`
			DecisionReason string `json:"decisionReason"`
		} `json:"gates"`
	} `json:"steps"`
}

type agentMemory struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	Seq     uint64   `json:"seq"`
}

// agentMissionCode is the task code (e.g. RHZ-131) in a mission name.
var agentMissionCode = regexp.MustCompile(`RHZ-\d+`)

const (
	agentNoteCap       = 10
	agentNoteLineLimit = 20
)

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return false
}

// relevantNotes picks the notes to show for a mission (FR-RHZ-172): the
// task bundle holds notes about the mission and about its goal. When the
// mission name carries a code (RHZ-n), goal-only notes are kept only if
// tagged with that code — sibling missions' notes stay out — while notes
// about this mission itself are always kept. Handoff-tagged notes first,
// then the rest newest-first by seq; capped at agentNoteCap.
func relevantNotes(all []agentMemory, code string, goalBound map[string]bool) []agentMemory {
	var handoff, others []agentMemory
	for _, m := range all {
		if code != "" && goalBound[m.ID] && !hasTag(m.Tags, code) {
			continue
		}
		if hasTag(m.Tags, "handoff") || m.Kind == "handoff" {
			handoff = append(handoff, m)
		} else {
			others = append(others, m)
		}
	}
	bySeq := func(xs []agentMemory) {
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].Seq > xs[j].Seq })
	}
	bySeq(handoff)
	bySeq(others)
	picked := append(handoff, others...)
	if len(picked) > agentNoteCap {
		picked = picked[:agentNoteCap]
	}
	return picked
}

func firstLines(s string, n int) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s, false
	}
	return strings.Join(lines[:n], "\n"), true
}

func agentContext(args []string, out, errOut io.Writer, deps agentDeps) int {
	var noRecord bool
	c, code := newAgentCLI("context", args, out, errOut, deps, 0, func(f *flag.FlagSet) {
		f.BoolVar(&noRecord, "no-record", false, "")
	})
	if c == nil {
		return code
	}
	var b agentContextBundle
	if err := c.get("/v1/context", url.Values{"task": {c.mission}, "include": {"prompt"}}, &b); err != nil {
		return c.report(err)
	}
	// Goal-bound notes come from the read-only goal notes bundle (no trace).
	// They are used only to drop sibling notes when the mission has a code;
	// a failed lookup degrades to "keep everything". A note bound to both
	// the goal and the mission counts as goal-bound here (no endpoint tells
	// them apart); the CLI itself never binds notes to a goal.
	missionCode := agentMissionCode.FindString(b.Task.Name)
	goalBound := map[string]bool{}
	if missionCode != "" && b.Goal.ID != "" {
		var gb struct {
			Memories []agentMemory `json:"memories"`
		}
		if c.get("/v1/context", url.Values{"goal": {b.Goal.ID}}, &gb) == nil {
			for _, m := range gb.Memories {
				goalBound[m.ID] = true
			}
		}
	}
	writeAgentContext(out, b, relevantNotes(b.Memories, missionCode, goalBound))
	if !noRecord {
		if err := c.recordStart(b.Task.ID, b.Task.State, "착수 — context 읽음 ("+c.actor+")"); err != nil {
			var rj errRejected
			if errors.As(err, &rj) {
				fmt.Fprintln(errOut, "warning: start not recorded: rejected: "+rj.reason)
			} else {
				fmt.Fprintln(errOut, "warning: start not recorded: "+err.Error())
			}
		}
	}
	return 0
}

func writeAgentContext(w io.Writer, b agentContextBundle, notes []agentMemory) {
	t := b.Task
	fmt.Fprintf(w, "mission: %s (%s)\n", t.Name, t.ID)
	fmt.Fprintf(w, "state: %s\n", t.State)
	if t.Assignee != "" {
		fmt.Fprintf(w, "assignee: %s\n", t.Assignee)
	}
	if t.CurrentAction != "" {
		fmt.Fprintf(w, "current: %s\n", t.CurrentAction)
	}
	if t.HasProgress {
		fmt.Fprintf(w, "progress: %d%%\n", int(math.Round(t.Progress*100)))
	}
	fmt.Fprintf(w, "goal: %s (%s, %s)\n", b.Goal.Description, b.Goal.ID, b.Goal.State)
	if t.Prompt != "" {
		fmt.Fprintf(w, "\n== prompt ==\n%s\n", t.Prompt)
	}
	fmt.Fprintf(w, "\n== notes (%d) ==\n", len(notes))
	for _, n := range notes {
		tags := strings.Join(n.Tags, ", ")
		fmt.Fprintf(w, "\n-- %s %s seq=%d tags=[%s]\n", n.Kind, n.ID, n.Seq, tags)
		body := n.Content
		if !(hasTag(n.Tags, "handoff") || n.Kind == "handoff") {
			if cut, more := firstLines(body, agentNoteLineLimit); more {
				body = cut + "\n[... truncated]"
			}
		}
		fmt.Fprintln(w, body)
	}
	if len(b.Steps) > 0 {
		fmt.Fprintf(w, "\n== steps ==\n")
		for _, s := range b.Steps {
			after := ""
			if len(s.After) > 0 {
				after = " after=" + strings.Join(s.After, ",")
			}
			fmt.Fprintf(w, "- %s (%s) %s%s\n", s.Name, s.ID, s.State, after)
			for _, g := range s.Gates {
				reason := ""
				if g.DecisionReason != "" {
					reason = ": " + g.DecisionReason
				}
				fmt.Fprintf(w, "    gate %s (%s) %s%s\n", g.Name, g.ID, g.State, reason)
			}
		}
	}
}

// recordStart writes "work started" for a mission in the given wire state
// (FR-RHZ-172): queued/paused → task.resume then mission.progress;
// running/waiting/blocked → mission.progress only; terminal → nothing.
func (c *agentCLI) recordStart(missionID, state, action string) error {
	switch state {
	case "queued", "paused":
		if err := c.intent("task.resume", map[string]any{"taskId": missionID}); err != nil {
			return err
		}
	case "running", "waiting", "blocked":
	default:
		return nil
	}
	return c.intent("mission.progress", map[string]any{"missionId": missionID, "currentAction": action})
}

func agentStart(args []string, out, errOut io.Writer, deps agentDeps) int {
	c, code := newAgentCLI("start", args, out, errOut, deps, 0, nil)
	if c == nil {
		return code
	}
	// The read-only mission notes bundle: no knowledge trace is written.
	var mb struct {
		Mission struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"mission"`
	}
	if err := c.get("/v1/context", url.Values{"mission": {c.mission}}, &mb); err != nil {
		return c.report(err)
	}
	switch mb.Mission.State {
	case "completed", "cancelled", "failed":
		return c.report(errRejected{"mission is " + mb.Mission.State})
	}
	if err := c.recordStart(mb.Mission.ID, mb.Mission.State, "착수 ("+c.actor+")"); err != nil {
		return c.report(err)
	}
	fmt.Fprintln(out, "ok")
	return 0
}

// ---- progress --------------------------------------------------------------

func agentProgress(args []string, out, errOut io.Writer, deps agentDeps) int {
	var action, pct string
	c, code := newAgentCLI("progress", args, out, errOut, deps, 0, func(f *flag.FlagSet) {
		f.StringVar(&action, "action", "", "")
		f.StringVar(&pct, "pct", "", "")
	})
	if c == nil {
		return code
	}
	if strings.TrimSpace(action) == "" {
		return agentUsageErr(errOut, "--action required")
	}
	fields := map[string]any{"missionId": c.mission, "currentAction": action}
	if c.set["pct"] {
		p, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil || math.IsNaN(p) || p < 0 || p > 100 {
			return agentUsageErr(errOut, "--pct must be a number from 0 to 100")
		}
		fields["progress"] = p / 100
	}
	if err := c.intent("mission.progress", fields); err != nil {
		return c.report(err)
	}
	fmt.Fprintln(out, "ok")
	return 0
}

// ---- blocked / done / note -------------------------------------------------

func agentNote(verb string, args []string, out, errOut io.Writer, deps agentDeps) int {
	var bodyFile, kind string
	var tags agentStrings
	c, code := newAgentCLI(verb, args, out, errOut, deps, 1, func(f *flag.FlagSet) {
		f.StringVar(&bodyFile, "body-file", "", "")
		f.Var(&tags, "tag", "")
		if verb == "note" {
			f.StringVar(&kind, "kind", "observation", "")
		}
	})
	if c == nil {
		return code
	}
	line := c.pos[0]
	if strings.ContainsAny(line, "\r\n") {
		return agentUsageErr(errOut, "summary must be one line (use --body-file for details)")
	}
	content, code := c.content(line, bodyFile)
	if code != 0 {
		return code
	}
	allTags := []string{}
	switch verb {
	case "blocked", "done":
		kind = verb
		allTags = append(allTags, verb)
	}
	allTags = append(allTags, tags...)
	if err := c.intent("note.create", map[string]any{"memoryKind": kind, "missionId": c.mission, "content": content, "tags": allTags}); err != nil {
		return c.report(err)
	}
	fmt.Fprintln(out, "note: ok")
	// The display-only progress companion. The note is already durable, so a
	// rejected progress (e.g. mission not started) is a warning, exit 0.
	var progress map[string]any
	switch verb {
	case "blocked":
		progress = map[string]any{"missionId": c.mission, "blockedReason": line}
	case "done":
		// Never mission.complete: the terminal state is the operator's call.
		progress = map[string]any{"missionId": c.mission, "currentAction": "done: " + line, "progress": 1.0}
	}
	if progress != nil {
		if err := c.intent("mission.progress", progress); err != nil {
			var rj errRejected
			if errors.As(err, &rj) {
				fmt.Fprintln(errOut, "warning: progress not recorded: rejected: "+rj.reason)
			} else {
				fmt.Fprintln(errOut, "warning: progress not recorded: "+err.Error())
			}
		} else {
			fmt.Fprintln(out, "progress: ok")
		}
	}
	return 0
}

// ---- usage -----------------------------------------------------------------

func agentUsageReport(args []string, out, errOut io.Writer, deps agentDeps) int {
	var model, in, outTok, cacheRead, cacheWrite string
	c, code := newAgentCLI("usage", args, out, errOut, deps, 0, func(f *flag.FlagSet) {
		f.StringVar(&model, "model", "", "")
		f.StringVar(&in, "in", "", "")
		f.StringVar(&outTok, "out", "", "")
		f.StringVar(&cacheRead, "cache-read", "0", "")
		f.StringVar(&cacheWrite, "cache-write", "0", "")
	})
	if c == nil {
		return code
	}
	model = strings.TrimSpace(model)
	if model == "" || strings.ContainsAny(model, " \t\r\n") {
		return agentUsageErr(errOut, "--model required (no spaces)")
	}
	if !c.set["in"] || !c.set["out"] {
		return agentUsageErr(errOut, "--in and --out required")
	}
	nums := make([]uint64, 4)
	for i, v := range []struct{ name, val string }{{"in", in}, {"out", outTok}, {"cache-read", cacheRead}, {"cache-write", cacheWrite}} {
		n, err := strconv.ParseUint(strings.TrimSpace(v.val), 10, 63)
		if err != nil {
			return agentUsageErr(errOut, fmt.Sprintf("--%s must be a non-negative integer", v.name))
		}
		nums[i] = n
	}
	content := fmt.Sprintf("usage model=%s in=%d out=%d cacheRead=%d cacheWrite=%d\nactor=%s at=%s",
		model, nums[0], nums[1], nums[2], nums[3], c.actor, c.deps.now().UTC().Format(time.RFC3339))
	if err := c.intent("note.create", map[string]any{"memoryKind": "usage", "missionId": c.mission, "content": content, "tags": []string{"usage", "model:" + model}}); err != nil {
		return c.report(err)
	}
	fmt.Fprintln(out, "ok")
	return 0
}

// ---- ask -------------------------------------------------------------------

func agentAsk(args []string, out, errOut io.Writer, deps agentDeps) int {
	var body, bodyFile, rec string
	c, code := newAgentCLI("ask", args, out, errOut, deps, 1, func(f *flag.FlagSet) {
		f.StringVar(&body, "body", "", "")
		f.StringVar(&bodyFile, "body-file", "", "")
		f.StringVar(&rec, "recommendation", "", "")
	})
	if c == nil {
		return code
	}
	title := c.pos[0]
	if strings.TrimSpace(title) == "" {
		return agentUsageErr(errOut, "title required")
	}
	if c.set["body"] == c.set["body-file"] {
		return agentUsageErr(errOut, "exactly one of --body or --body-file required")
	}
	if c.set["body-file"] {
		b, err := c.deps.readFile(bodyFile)
		if err != nil {
			return agentUsageErr(errOut, fmt.Sprintf("body file: %v", err))
		}
		body = string(b)
	}
	if strings.TrimSpace(body) == "" {
		return agentUsageErr(errOut, "body is empty")
	}
	fields := map[string]any{"name": title, "body": body, "missionId": c.mission}
	if rec != "" {
		fields["recommendation"] = rec
	}
	if err := c.intent("question.ask", fields); err != nil {
		return c.report(err)
	}
	fmt.Fprintln(out, "ok")
	return 0
}
