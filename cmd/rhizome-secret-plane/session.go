package main

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"rhizome/internal/secretplane"
	"rhizome/internal/secretplane/server"
	"rhizome/internal/secretwire"
)

// Typed confirmations. Each human action needs its own phrase; there is no
// flag, configuration or API that supplies it (D19).
const (
	confirmYes      = "yes"
	confirmRevoke   = "revoke"
	confirmRotated  = "rotated externally"
	confirmAttest   = "attest"
	confirmReplace  = "replace epoch"
	confirmAddProf  = "add profile"
	confirmRmProf   = "remove profile"
	actionAccept    = "accept"
	actionReject    = "reject"
	actionSkip      = "skip"
	msgInvalidInput = "invalid input; nothing recorded"
	msgNotConfirmed = "not confirmed; nothing recorded"
	msgStaysPending = "not completed; the request stays in progress and can be handled again"
	msgVendorNote   = "the plane does not revoke the key at its vendor; do that by hand"
	msgAttestation  = "interactive caller attestation: the key is kept outside the plane and no value is stored. " +
		"This records your statement as the interactive caller; it is not verified by the plane."
)

const helpText = `commands:
  pending          list requests from Rhizome (values are never shown)
  next             act on the oldest actionable request
  handle <opId>    act on one request
  register         register a value directly at the plane (plane-direct)
  attest           record an interactive caller attestation (no value)
  observe <ref>    record whether the plane holds a value for ref
  refs             list refs known to the plane
  status           plane id, epoch phase, ack and stall state
  quarantine       list quarantined entries and abandoned operations
  unreconciled     list refs in the values file unknown to the ledger
  epoch-replace    freeze the plane and replace its epoch (P5)
  profiles         list run profiles (no values)
  profile-add      add a run profile: program + variables bound to refs
  profile-remove <name>  remove a run profile
  help             this text
  quit             stop the plane
`

// lockedWriter serializes the foreground loop and server notices.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// session is the foreground loop. Its input, output, password reader and
// queue are injected so tests can script it; the binary passes the terminal.
type session struct {
	pl       *secretplane.Plane
	queue    func() []server.Item
	in       *bufio.Reader
	out      io.Writer
	password func() ([]byte, error)
	fatal    func()
}

func newSession(pl *secretplane.Plane, queue func() []server.Item, in io.Reader, out io.Writer,
	password func() ([]byte, error), fatal func()) *session {
	return &session{pl: pl, queue: queue, in: bufio.NewReader(in), out: out, password: password, fatal: fatal}
}

func (s *session) say(format string, a ...any) { fmt.Fprintf(s.out, format+"\n", a...) }

// readLine prints prompt and reads one line; ok is false at end of input.
func (s *session) readLine(prompt string) (string, bool) {
	io.WriteString(s.out, prompt)
	line, err := s.in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// confirm asks for an exact typed phrase.
func (s *session) confirm(phrase string) bool {
	line, ok := s.readLine("type \"" + phrase + "\" to continue: ")
	return ok && line == phrase
}

// fail prints a fixed error code; ErrFatal ends the process.
func (s *session) fail(err error) {
	if errors.Is(err, secretplane.ErrFatal) {
		s.fatal()
		return
	}
	s.say("error: %s", errCode(err))
}

func (s *session) loop() {
	s.showStatus()
	s.say("type \"help\" for commands")
	for {
		line, ok := s.readLine("plane> ")
		if !ok {
			return
		}
		cmd, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		switch cmd {
		case "":
		case "help":
			io.WriteString(s.out, helpText)
		case "quit", "exit":
			return
		case "pending":
			s.showPending()
		case "next":
			s.next()
		case "handle":
			s.handle(arg)
		case "register":
			s.registerDirect()
		case "attest":
			s.attest()
		case "observe":
			s.observe(arg)
		case "refs":
			s.showRefs()
		case "status":
			s.showStatus()
		case "quarantine":
			s.showQuarantine()
		case "unreconciled":
			s.showUnreconciled()
		case "epoch-replace":
			s.epochReplace()
		case "profiles":
			s.showProfiles()
		case "profile-add":
			s.profileAdd()
		case "profile-remove":
			s.profileRemove(arg)
		default:
			s.say("unknown command; type \"help\"")
		}
	}
}

func statusLabel(st secretplane.PendingStatus) string {
	switch st {
	case secretplane.PendingNew:
		return "new"
	case secretplane.PendingInProgress:
		return "in-progress"
	case secretplane.PendingDisplayOnly:
		return "display-only (abandoned or quarantined; not processed)"
	case secretplane.PendingFrozen:
		return "ignored while the epoch is frozen"
	}
	return "-"
}

func describe(p secretwire.Pending) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s opId=%s ref=%s name=%s refKind=%s", p.Op, p.OpID, p.Ref, p.Name, p.RefKind)
	if len(p.ScopeTags) > 0 {
		b.WriteString(" scopeTags=" + strings.Join(p.ScopeTags, ","))
	}
	if p.ReasonCode != "" {
		b.WriteString(" reasonCode=" + p.ReasonCode)
	}
	return b.String()
}

func (s *session) showPending() {
	q := s.queue()
	if len(q) == 0 {
		s.say("no pending requests")
		return
	}
	for i, it := range q {
		s.say("%d %s [%s]", i+1, describe(it.Pending), statusLabel(it.Status))
	}
}

func (s *session) next() {
	for _, it := range s.queue() {
		if it.Actionable() {
			s.act(it.Pending)
			return
		}
	}
	s.say("no actionable request")
}

func (s *session) handle(opID string) {
	for _, it := range s.queue() {
		if it.Pending.OpID == opID {
			if !it.Actionable() {
				s.say("request is %s", statusLabel(it.Status))
				return
			}
			s.act(it.Pending)
			return
		}
	}
	s.say("no such request")
}

// refOf returns the plane's summary of ref.
func (s *session) refOf(ref string) (secretplane.RefSummary, bool, error) {
	refs, err := s.pl.Refs()
	if err != nil {
		return secretplane.RefSummary{}, false, err
	}
	for _, r := range refs {
		if r.Ref == ref {
			return r, true, nil
		}
	}
	return secretplane.RefSummary{}, false, nil
}

// act shows one request and collects the human's decision. One prompt at a
// time: other requests wait in the queue.
func (s *session) act(p secretwire.Pending) {
	st, err := s.pl.Classify(p)
	if err != nil {
		s.fail(err)
		return
	}
	if st != secretplane.PendingNew && st != secretplane.PendingInProgress {
		s.say("request is %s", statusLabel(st))
		return
	}
	attested := false
	if p.Op != secretwire.OpRegister {
		r, ok, err := s.refOf(p.Ref)
		if err != nil {
			s.fail(err)
			return
		}
		attested = ok && r.Attested()
	}
	s.say("request: %s", describe(p))
	if attested {
		s.say("this ref is attested: the plane holds no value for it; the operation is record-only")
	}
	action, ok := s.readLine("action (accept, reject, skip): ")
	switch {
	case !ok || action == actionSkip:
		s.say("skipped")
		return
	case action == actionReject:
		if !s.confirm(confirmYes) {
			s.say(msgNotConfirmed)
			return
		}
		s.commit(p, secretplane.Decision{Reject: true})
		return
	case action != actionAccept:
		s.say(msgInvalidInput)
		return
	}
	switch {
	case p.Op == secretwire.OpRevoke:
		s.say(msgVendorNote)
		if !s.confirm(confirmRevoke) {
			s.say(msgNotConfirmed)
			return
		}
		s.commit(p, secretplane.Decision{})
	case attested: // rotate of an attested ref (Q2): no value
		s.say("confirm that the key was rotated outside the plane; no value is entered")
		if !s.confirm(confirmRotated) {
			s.say(msgNotConfirmed)
			return
		}
		s.commit(p, secretplane.Decision{})
	default: // register, or rotate of a stored ref
		if err := s.pl.Begin(p); err != nil {
			s.fail(err)
			return
		}
		v, ok := s.readValue()
		if !ok {
			s.say(msgStaysPending)
			return
		}
		defer clear(v)
		if !s.confirm(confirmYes) {
			s.say(msgStaysPending)
			return
		}
		res, err := s.pl.Commit(p.OpID, secretplane.Decision{Value: v})
		s.report(res, err)
	}
}

// commit runs Begin + Commit for a decision collected and confirmed before
// anything is written (reject, revoke, attested rotate).
func (s *session) commit(p secretwire.Pending, d secretplane.Decision) {
	if err := s.pl.Begin(p); err != nil {
		s.fail(err)
		return
	}
	res, err := s.pl.Commit(p.OpID, d)
	s.report(res, err)
}

func (s *session) report(res secretplane.Result, err error) {
	if err != nil {
		if errors.Is(err, secretplane.ErrFrozen) {
			s.say("the epoch is frozen: the operation was not completed (it is listed as abandoned)")
			return
		}
		s.fail(err)
		return
	}
	line := "outcome: " + res.Result
	if res.FailureCode != "" {
		line += " " + res.FailureCode
	}
	if res.Entry != nil {
		line += " version " + strconv.FormatUint(res.Entry.Version, 10)
	}
	s.say("%s ref %s", line, res.Ref)
}

// readValue reads the value twice without echo. The returned slice must be
// wiped by the caller; on mismatch both reads are wiped here.
func (s *session) readValue() ([]byte, bool) {
	io.WriteString(s.out, "value (not echoed): ")
	v1, err := s.password()
	io.WriteString(s.out, "\n")
	if err != nil || len(v1) == 0 {
		clear(v1)
		s.say("no value entered")
		return nil, false
	}
	io.WriteString(s.out, "value again (not echoed): ")
	v2, err := s.password()
	io.WriteString(s.out, "\n")
	same := err == nil && subtle.ConstantTimeCompare(v1, v2) == 1
	clear(v2)
	if !same {
		clear(v1)
		s.say("the two entries differ")
		return nil, false
	}
	return v1, true
}

// readMeta prompts for name, refKind and scopeTags (no value).
func (s *session) readMeta() (name, kind string, tags []string, ok bool) {
	name, ok = s.readLine("name: ")
	if !ok || !secretwire.ValidName(name) {
		return "", "", nil, false
	}
	kind, ok = s.readLine("refKind (provider-key, gateway-credential, other): ")
	if !ok || !secretwire.ValidRefKind(kind) {
		return "", "", nil, false
	}
	line, ok := s.readLine("scopeTags (comma-separated, empty for none): ")
	if !ok {
		return "", "", nil, false
	}
	for _, t := range strings.Split(line, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	sort.Strings(tags)
	if tags != nil && !secretwire.ValidScopeTags(tags) {
		return "", "", nil, false
	}
	return name, kind, tags, true
}

func (s *session) registerDirect() {
	name, kind, tags, ok := s.readMeta()
	if !ok {
		s.say(msgInvalidInput)
		return
	}
	v, ok := s.readValue()
	if !ok {
		s.say("nothing recorded")
		return
	}
	defer clear(v)
	if !s.confirm(confirmYes) {
		s.say(msgNotConfirmed)
		return
	}
	res, err := s.pl.RegisterDirect(name, kind, tags, v)
	s.report(res, err)
}

func (s *session) attest() {
	s.say(msgAttestation)
	name, kind, tags, ok := s.readMeta()
	if !ok {
		s.say(msgInvalidInput)
		return
	}
	if !s.confirm(confirmAttest) {
		s.say(msgNotConfirmed)
		return
	}
	res, err := s.pl.DeclareAttested(name, kind, tags)
	s.report(res, err)
}

func (s *session) observe(ref string) {
	if !secretwire.ValidRef(ref) {
		s.say(msgInvalidInput)
		return
	}
	e, err := s.pl.Observe(ref)
	if err != nil {
		s.fail(err)
		return
	}
	s.say("observed: ref %s present=%t version %d at %s", e.Ref, e.Present, e.Version, e.PlaneObservedAt)
}

func (s *session) showRefs() {
	refs, err := s.pl.Refs()
	if err != nil {
		s.fail(err)
		return
	}
	if len(refs) == 0 {
		s.say("no refs")
		return
	}
	for _, r := range refs {
		tags := "-"
		if len(r.ScopeTags) > 0 {
			tags = strings.Join(r.ScopeTags, ",")
		}
		state := "current"
		switch {
		case r.Revoked:
			state = "revoked"
		case r.Busy:
			state = "in-progress"
		}
		s.say("%s name=%s refKind=%s origin=%s version %d scopeTags=%s %s", r.Ref, r.Name, r.RefKind, r.Origin, r.Version, tags, state)
	}
}

func (s *session) showStatus() {
	st, err := s.pl.Status()
	if err != nil {
		s.fail(err)
		return
	}
	s.say("plane %s phase %s ack %d outbox %d", st.PlaneID, st.Phase, st.RecordedAck, st.OutboxMax)
	if st.LastRejected != nil {
		s.say("stalled: Rhizome rejected entry %d (%s); recovery is epoch-replace", st.LastRejected.Seq, st.LastRejected.Reason)
	}
	if st.Halted {
		s.say("halted: Rhizome's ack contradicted the plane records; no entries are sent until restart")
	}
}

func (s *session) showQuarantine() {
	items, err := s.pl.QuarantineList()
	if err != nil {
		s.fail(err)
		return
	}
	if len(items) == 0 {
		s.say("quarantine is empty")
		return
	}
	io.WriteString(s.out, secretplane.FormatQuarantineList(items))
}

func (s *session) showUnreconciled() {
	refs, err := s.pl.UnreconciledValueRefs()
	if err != nil {
		s.fail(err)
		return
	}
	if len(refs) == 0 {
		s.say("no unreconciled refs")
		return
	}
	for _, r := range refs {
		s.say("%s (display only)", r)
	}
}

func (s *session) epochReplace() {
	s.showStatus()
	s.say("epoch replacement freezes the plane: operations in progress are abandoned, " +
		"entries Rhizome has not acknowledged are quarantined and never resent, and a new plane id starts with an empty outbox")
	if !s.confirm(confirmReplace) {
		s.say(msgNotConfirmed)
		return
	}
	if err := s.pl.Freeze(); err != nil {
		s.fail(err)
		return
	}
	s.say("phase frozen: the next polls from Rhizome record the cutoff, quarantine and switch to the new plane id (see status)")
}

func formatProfile(p secretplane.Profile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s program=%s", p.Name, strconv.Quote(p.Program))
	if len(p.Args) > 0 {
		q := make([]string, len(p.Args))
		for i, a := range p.Args {
			q[i] = strconv.Quote(a)
		}
		b.WriteString(" args=" + strings.Join(q, " "))
	}
	if p.AllowExtraArgs {
		b.WriteString(" extra-args=allowed")
	} else {
		b.WriteString(" extra-args=refused")
	}
	names := make([]string, 0, len(p.Env))
	for n := range p.Env {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString(" " + n + "=" + p.Env[n])
	}
	return b.String()
}

func (s *session) showProfiles() {
	ps, err := s.pl.Profiles()
	if err != nil {
		s.fail(err)
		return
	}
	if len(ps) == 0 {
		s.say("no profiles")
		return
	}
	for _, p := range ps {
		s.say("%s", formatProfile(p))
	}
}

// profileAdd collects a run profile. Variables bind to refs only; a value is
// never entered here.
func (s *session) profileAdd() {
	var p secretplane.Profile
	var ok bool
	if p.Name, ok = s.readLine("profile name: "); !ok || !secretwire.ValidName(p.Name) {
		s.say(msgInvalidInput)
		return
	}
	if p.Program, ok = s.readLine("program (absolute path, no PATH lookup): "); !ok || !programOK(p.Program) ||
		filepath.Clean(p.Program) != p.Program || !filepath.IsAbs(p.Program) {
		s.say(msgInvalidInput)
		return
	}
	for {
		a, ok := s.readLine("fixed argument (empty line to finish): ")
		if !ok {
			s.say(msgInvalidInput)
			return
		}
		if a == "" {
			break
		}
		if len(p.Args) == secretplane.MaxProfileArgs {
			s.say(msgInvalidInput)
			return
		}
		p.Args = append(p.Args, a)
	}
	switch x, _ := s.readLine("allow extra arguments at run time? (yes/no): "); x {
	case "yes":
		p.AllowExtraArgs = true
	case "no":
	default:
		s.say(msgInvalidInput)
		return
	}
	p.Env = map[string]string{}
	for {
		n, ok := s.readLine("variable name (empty line to finish): ")
		if !ok {
			s.say(msgInvalidInput)
			return
		}
		if n == "" {
			break
		}
		r, ok := s.readLine("ref for the variable: ")
		if !ok || !secretplane.ValidEnvName(n) || !secretwire.ValidRef(r) || p.Env[n] != "" || len(p.Env) == secretplane.MaxProfileEnv {
			s.say(msgInvalidInput)
			return
		}
		p.Env[n] = r
	}
	if len(p.Env) == 0 {
		s.say(msgInvalidInput)
		return
	}
	s.say("profile: %s", formatProfile(p))
	s.say("run gives this program the stored values in these variables; a program that prints them exposes them")
	if !s.confirm(confirmAddProf) {
		s.say(msgNotConfirmed)
		return
	}
	if err := s.pl.AddProfile(p); err != nil {
		s.fail(err)
		return
	}
	s.say("profile added: %s", p.Name)
}

func (s *session) profileRemove(name string) {
	if !secretwire.ValidName(name) {
		s.say(msgInvalidInput)
		return
	}
	if !s.confirm(confirmRmProf) {
		s.say(msgNotConfirmed)
		return
	}
	if err := s.pl.RemoveProfile(name); err != nil {
		s.fail(err)
		return
	}
	s.say("profile removed: %s", name)
}

func programOK(path string) bool {
	_, err := secretplane.CheckProgram(path)
	return err == nil
}
