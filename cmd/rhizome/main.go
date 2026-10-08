package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/blob"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/janusadapter"
	"rhizome/internal/journal"
	"rhizome/internal/memory"
	"rhizome/internal/source"
	"rhizome/internal/workspace"
)

func validKind(k memory.Kind) bool {
	switch k {
	case memory.Fact, memory.Decision, memory.Preference, memory.Observation, memory.Hypothesis, memory.Reference:
		return true
	}
	return false
}

// acquireJournalLock takes the kernel advisory lock (flock LOCK_EX|LOCK_NB) on
// <journal>.lock (FR-RHZ-107). The kernel releases it when the holder dies, so a
// lock file left behind by a killed process is simply re-locked; a live holder
// is refused. "<pid> <timestamp>" is written for the refusal message only. The
// fd stays open for the holder's lifetime; a lock not held is never removed.
func acquireJournalLock(path string, errOut io.Writer) (func(), error) {
	lock := path + ".lock"
	for attempt := 0; attempt < 8; attempt++ {
		f, e := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			fmt.Fprintf(errOut, "journal lock %s: %v\n", lock, e)
			return nil, e
		}
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			f.Close()
			// No removal hint: deleting the file under a live holder is the one way
			// to end up with two writers (the holder's later unlink would also
			// remove the newcomer's file).
			if b, x := os.ReadFile(lock); x == nil && len(b) > 0 {
				fmt.Fprintf(errOut, "journal lock held by a running writer: %s (%s); stop that process first\n", lock, string(b))
			} else {
				fmt.Fprintf(errOut, "journal lock held by a running writer: %s; stop that process first\n", lock)
			}
			return nil, e
		}
		// A previous holder may have unlinked this inode between our open and
		// flock; a lock on an unlinked file protects nothing, so re-open.
		fi, e1 := f.Stat()
		di, e2 := os.Stat(lock)
		if e1 != nil || e2 != nil || !os.SameFile(fi, di) {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
			continue
		}
		_ = f.Truncate(0)
		fmt.Fprintf(f, "%d %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
		return func() {
			_ = os.Remove(lock)
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		}, nil
	}
	e := fmt.Errorf("journal lock %s: could not obtain a stable lock", lock)
	fmt.Fprintln(errOut, e)
	return nil, e
}

func ingest(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("ingest", flag.ContinueOnError)
	f.SetOutput(errOut)
	jpath := f.String("journal", "", "")
	bpath := f.String("blobs", "", "")
	dataDir := f.String("data-dir", "", "")
	file := f.String("file", "", "")
	kind := f.String("kind", string(memory.Reference), "")
	override := f.String("content", "", "")
	tagsArg := f.String("tags", "", "")
	media := f.String("media", "text/markdown", "")
	uri := f.String("uri", "", "")
	if f.Parse(args) != nil {
		return 2
	}
	journalSet, blobsSet := false, false
	f.Visit(func(v *flag.Flag) {
		switch v.Name {
		case "journal":
			journalSet = true
		case "blobs":
			blobsSet = true
		}
	})
	if !validKind(memory.Kind(*kind)) {
		fmt.Fprintln(errOut, "invalid memory kind")
		return 2
	}
	if *file == "" || (journalSet && *jpath == "") || (journalSet && !blobsSet) || (journalSet && *bpath == "") {
		fmt.Fprintln(errOut, "journal, blobs and file are required")
		return 2
	}
	var err error
	*jpath, _, _, err = resolveJournal(*dataDir, *jpath, journalSet)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if !journalSet && !blobsSet {
		dir, e := commandDataDir(*dataDir)
		if e != nil {
			fmt.Fprintln(errOut, e)
			return 2
		}
		*bpath = filepath.Join(dir, "blobs")
	}
	b, e := os.ReadFile(*file)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	if len(b) == 0 {
		fmt.Fprintln(errOut, "file content is empty")
		return 1
	}
	if *uri == "" {
		*uri = "note://" + filepath.Base(*file)
	}
	blobID, e := (blob.FileStore{Dir: *bpath}).Put(b)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	unlock, e := acquireJournalLock(*jpath, errOut)
	if e != nil {
		return 1
	}
	defer unlock()
	j, e := journal.Open(*jpath)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	defer j.Close()
	ref, e := (source.Service{Store: j}).Register(b, *media, *uri)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	if ref.BlobID != blobID {
		fmt.Fprintln(errOut, "blob/source identity mismatch")
		return 1
	}
	text := *override
	if text == "" {
		if len(b) <= 2048 {
			text = string(b)
		} else {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(line) != "" {
					text = line
					break
				}
			}
		}
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(errOut, "memory content is empty")
		return 1
	}
	h := strings.TrimPrefix(blobID, "sha256:")
	tags := []string{}
	for _, tag := range strings.Split(*tagsArg, ",") {
		if strings.TrimSpace(tag) != "" {
			tags = append(tags, strings.TrimSpace(tag))
		}
	}
	m, e := (memory.Service{Store: j}).Create(memory.Memory{ID: "note-" + h[:12], Kind: memory.Kind(*kind), Content: text, SourceType: "blob", SourceID: blobID, Confidence: 1, Tags: tags})
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	fmt.Fprintf(out, "%s %s %s\n", m.ID, m.Kind, m.SourceID)
	return 0
}

func memories(args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("memories", flag.ContinueOnError)
	f.SetOutput(errOut)
	jpath := f.String("journal", "", "")
	dataDir := f.String("data-dir", "", "")
	kind := f.String("kind", "", "")
	tag := f.String("tag", "", "")
	src := f.String("source", "", "")
	if f.Parse(args) != nil {
		return 2
	}
	journalSet := false
	f.Visit(func(v *flag.Flag) { journalSet = journalSet || v.Name == "journal" })
	var err error
	*jpath, _, _, err = resolveJournal(*dataDir, *jpath, journalSet)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if *kind != "" && !validKind(memory.Kind(*kind)) {
		fmt.Fprintln(errOut, "invalid memory kind")
		return 2
	}
	unlock, e := acquireJournalLock(*jpath, errOut)
	if e != nil {
		return 1
	}
	defer unlock()
	j, e := journal.Open(*jpath)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	defer j.Close()
	items, e := (memory.Service{Store: j}).Search(memory.Kind(*kind), *tag, *src)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	for _, m := range items {
		p := m.Content
		if len(p) > 80 {
			p = p[:80]
		}
		fmt.Fprintf(out, "%s %s %s %s\n", m.ID, m.Kind, m.SourceID, p)
	}
	return 0
}

// janusServeConfig is the operator-owned JANUS wiring for serve. nil means
// the adapter is fully disabled and serve behaves exactly as before.
type janusServeConfig struct {
	HX       string
	Cfg      janusadapter.RunConfig
	Interval time.Duration
	// IdleTimeout is -janus-idle-timeout (FR-RHZ-119): 0 = off.
	IdleTimeout time.Duration
	// Exec is the -janus-exec-config ledger (FR-RHZ-123): operating defaults
	// and the configured ceiling for mission.start. nil = mission.start rejected
	// with "execution config not loaded" (zero writes); the rest of the
	// adapter is unaffected.
	Exec *janusadapter.ExecConfig
	// Runner overrides the hx run process (tests only: a Fake Runner). nil =
	// RealRunner(HX, os.TempDir()).
	Runner janusadapter.Runner
}

// execStarter adapts janusadapter.Starter to the workspace seam at the
// composition root (workspace never imports the adapter).
type execStarter struct{ st janusadapter.Starter }

func toStartRequest(r workspace.ExecStartRequest) janusadapter.StartRequest {
	return janusadapter.StartRequest{MissionID: r.MissionID, Instruction: r.Instruction, SessionMode: r.SessionMode,
		Budget: janusadapter.BudgetOverride{Tokens: r.Budget.Tokens, TimeMs: r.Budget.TimeMs, MaxDepth: r.Budget.MaxDepth}, Actor: r.Actor}
}
func (x execStarter) Prepare(r workspace.ExecStartRequest) (workspace.ExecStartPlan, string, error) {
	p, reason, err := x.st.Prepare(toStartRequest(r))
	return workspace.ExecStartPlan{ExecutionID: p.ExecutionID, Existing: p.Existing}, reason, err
}
func (x execStarter) Start(r workspace.ExecStartRequest) (string, string, error) {
	return x.st.Start(toStartRequest(r))
}

// defaultJanusIdleTimeout is the -janus-idle-timeout default.
const defaultJanusIdleTimeout = 10 * time.Minute

// defaultJanusEnvMode preserves the pre-FR-RHZ-126 process environment until
// the allowlist has been verified on the production rootless-Podman runtime.
const defaultJanusEnvMode = janusadapter.EnvModeInherit

// janusIdleTimeout validates -janus-idle-timeout: 0 disables, negative is a
// configuration error. It only takes effect with the JANUS flag set (nil
// config = no loop = nothing to judge).
func janusIdleTimeout(d time.Duration) (time.Duration, error) {
	if d < 0 {
		return 0, fmt.Errorf("janus-idle-timeout must be >= 0 (0 = off)")
	}
	return d, nil
}

// janusEnvPassthrough parses the optional comma-separated list of environment
// variable names. Values are never accepted on this surface.
func janusEnvPassthrough(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	names := strings.Split(raw, ",")
	for i, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return nil, fmt.Errorf("janus-env-passthrough must be a comma-separated list of variable names")
		}
		names[i] = name
	}
	return names, nil
}

// janusServeFromFlags assembles the adapter config. All JANUS flags absent =
// adapter disabled (nil, nil); a partial set is a configuration error, never
// a silent disable (RHZ-046 D4). envModeSet distinguishes an explicit inherit
// from the default. The interval floor is 1s (D9).
func janusServeFromFlags(hx, endpoint, profile, acceptRoot, world, session, envMode string, envModeSet bool, envPassthrough string, interval time.Duration) (*janusServeConfig, error) {
	if hx == "" && endpoint == "" && profile == "" && acceptRoot == "" && world == "" && (envModeSet || envPassthrough != "") {
		return nil, fmt.Errorf("janus-env-mode/janus-env-passthrough requires the janus flag set (janus-hx, janus-approval-endpoint, janus-profile, janus-accept-root, janus-world-config)")
	}
	if envMode != janusadapter.EnvModeInherit && envMode != janusadapter.EnvModeAllowlist {
		return nil, fmt.Errorf("janus-env-mode must be %q or %q", janusadapter.EnvModeInherit, janusadapter.EnvModeAllowlist)
	}
	if envPassthrough != "" && envMode != janusadapter.EnvModeAllowlist {
		return nil, fmt.Errorf("janus-env-passthrough requires janus-env-mode=allowlist")
	}
	if hx == "" && endpoint == "" && profile == "" && acceptRoot == "" && world == "" && session == "" && envMode == defaultJanusEnvMode && envPassthrough == "" {
		return nil, nil
	}
	if hx == "" || endpoint == "" || profile == "" || acceptRoot == "" || world == "" {
		return nil, fmt.Errorf("incomplete janus configuration: janus-hx, janus-approval-endpoint, janus-profile, janus-accept-root and janus-world-config are all required (janus-session-db, janus-env-mode and janus-env-passthrough are optional)")
	}
	if interval < janusadapter.MinObserveInterval {
		return nil, fmt.Errorf("janus-observe-interval below %s", janusadapter.MinObserveInterval)
	}
	passthrough, err := janusEnvPassthrough(envPassthrough)
	if err != nil {
		return nil, err
	}
	return &janusServeConfig{HX: hx, Cfg: janusadapter.RunConfig{ProfilePath: profile, AcceptRoot: acceptRoot, WorldConfigPath: world, SessionDB: session, ApprovalEndpoint: endpoint, EnvMode: envMode, Passthrough: passthrough}, Interval: interval}, nil
}

// janusExecConfig loads -janus-exec-config (FR-RHZ-123). It is only legal
// with the JANUS flag set (D4: a ledger without an adapter is a
// configuration error, never silently ignored); absent with the adapter on
// = nil (mission.start rejected, zero writes). An invalid file fails serve.
func janusExecConfig(janusOn bool, path string) (*janusadapter.ExecConfig, error) {
	if path == "" {
		return nil, nil
	}
	if !janusOn {
		return nil, fmt.Errorf("janus-exec-config requires the janus flag set (janus-hx, janus-approval-endpoint, janus-profile, janus-accept-root, janus-world-config)")
	}
	c, err := janusadapter.LoadExecConfig(path)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// blobStoreFromFlag turns the serve -blobs flag into the handle for GET
// /v1/blob (RHZ-056, FR-RHZ-086) and, because blob.FileStore also implements
// workspace.BlobPutter, POST /v1/blob (RHZ-081, FR-RHZ-112). Empty flag =
// routes disabled (nil).
func blobStoreFromFlag(dir string) workspace.BlobGetter {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return blob.FileStore{Dir: dir}
}

// assembleServe builds the HTTP handler and, when configured, the JANUS loop.
// The HTTP surface is identical with or without the adapter: /v1/execution/*
// stays 501 in RHZ-046 part 1 (no new contract surface here).
// assembleServeInspect exposes the assembled workspace wiring to package tests.
var assembleServeInspect func(*workspace.HTTPServer)

func assembleServe(store events.Port, blobs workspace.BlobGetter, indexRepo, indexOut string, jc *janusServeConfig, errOut io.Writer) (http.Handler, *janusadapter.Loop) {
	h := workspace.NewHTTP(store)
	h.Blobs = blobs
	// RHZ-059 (FR-RHZ-089): both empty = /v1/codeindex disabled; partial
	// config is rejected in serve() before assembly (D4 관례).
	h.IndexRepo, h.IndexOut = indexRepo, indexOut
	if jc == nil {
		if assembleServeInspect != nil {
			assembleServeInspect(h)
		}
		return h.Handler(), nil
	}
	replay := janusadapter.RealReplay(jc.HX)
	// FR-RHZ-083 (T25): the /v1/execution outbound emit projects each bound
	// session's JANUS log — a read-only projection over the same hx replay
	// stream the loop observes, with no second writer or store. The seam type is
	// adapted into the surface type here at the composition root.
	h.ExecEvents = func(sessionDB, traceID string) ([]workspace.SessionEvent, error) {
		stream, err := replay(jc.Cfg, sessionDB, traceID)
		if err != nil {
			return nil, err
		}
		evs, err := janusadapter.ProjectSessionEvents(stream, traceID)
		if err != nil {
			return nil, err
		}
		out := make([]workspace.SessionEvent, 0, len(evs))
		for _, e := range evs {
			out = append(out, workspace.SessionEvent{Seq: e.Seq, Kind: e.Kind, Actor: e.Actor, TS: e.TS, UsageIn: e.UsageIn, UsageOut: e.UsageOut})
		}
		return out, nil
	}
	client := janusadapter.Client{Dial: janusadapter.RealDialer(jc.Cfg.ApprovalEndpoint)}
	// FR-RHZ-119: task.instruct → send_message on the mission's running
	// session. Rejections become the relay's Reason verbatim; a dial failure
	// is UNAVAILABLE. The seam error type is translated here, at the root.
	h.ExecInject = func(traceID, text string) (int64, string, error) {
		res, err := client.SendMessage(traceID, text)
		var rej janusadapter.ErrSendMessage
		switch {
		case err == nil:
			return res.MessageSeq, "", nil
		case errors.As(err, &rej):
			return rej.MessageSeq, rej.Reason, nil
		default:
			return 0, "", err
		}
	}
	// FR-RHZ-123: mission.start → intent → claim → hx run through the
	// configured Runner. Without a ledger the starter rejects before any
	// write; the loop below never starts executions (D7).
	runner := jc.Runner
	if runner == nil {
		runner = janusadapter.RealRunner(jc.HX, os.TempDir())
	}
	h.ExecStart = execStarter{janusadapter.Starter{Store: store, Run: runner, Cfg: jc.Cfg, Exec: jc.Exec}}
	loop := &janusadapter.Loop{
		ES:     execution.Service{Store: store},
		AS:     approval.Service{Store: store},
		Client: client,
		Replay: replay,
		// FR-RHZ-119: events_tail incremental observation with hx replay fallback.
		Tail: client.EventsTail,
		// FR-RHZ-119: idle-timeout stop (tail path only), process clock.
		IdleTimeout: jc.IdleTimeout,
		Cfg:         jc.Cfg,
		OnError: func(scope, id string, err error) {
			fmt.Fprintf(errOut, "janus loop %s %s: %v\n", scope, id, err)
		},
		Broadcast: func() {
			if p, e := workspace.Snapshot(store); e == nil {
				h.Broadcast(p)
			}
		},
	}
	if assembleServeInspect != nil {
		assembleServeInspect(h)
	}
	return h.Handler(), loop
}

// serve runs serveCtx until SIGINT/SIGTERM (FR-RHZ-107): the signal cancels
// the context, the HTTP server shuts down and the deferred journal unlock runs,
// so no .lock file is left behind for the next start.
func serve(args []string, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second SIGINT/SIGTERM
	// terminates a hung shutdown (safe: the kernel releases the flock on death).
	go func() { <-ctx.Done(); stop() }()
	return serveCtx(ctx, args, out, errOut)
}

func journalPoisonLogger(errOut io.Writer) func(error) {
	var once sync.Once
	return func(error) {
		once.Do(func() { fmt.Fprintln(errOut, events.ErrPoisoned) })
	}
}

// serveCtx is serve with the lifetime supplied by the caller (tests cancel it
// in-process). Cancellation is a clean exit (0), not an error.
func serveCtx(ctx context.Context, args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	jp := f.String("journal", "", "")
	bp := f.String("blobs", "", "")
	dataDir := f.String("data-dir", "", "")
	idxRepo := f.String("index-repo", "", "")
	idxOut := f.String("index-out", "", "")
	addr := f.String("addr", "127.0.0.1:8080", "")
	jHX := f.String("janus-hx", "", "")
	jEndpoint := f.String("janus-approval-endpoint", "", "")
	jProfile := f.String("janus-profile", "", "")
	jAcceptRoot := f.String("janus-accept-root", "", "")
	jWorld := f.String("janus-world-config", "", "")
	jSession := f.String("janus-session-db", "", "")
	jEnvMode := f.String("janus-env-mode", defaultJanusEnvMode, "")
	jEnvPassthrough := f.String("janus-env-passthrough", "", "")
	jInterval := f.Duration("janus-observe-interval", 5*time.Second, "")
	jIdle := f.Duration("janus-idle-timeout", defaultJanusIdleTimeout, "")
	jExec := f.String("janus-exec-config", "", "")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.SetOutput(errOut)
			if f.Usage != nil {
				f.Usage()
			} else {
				fmt.Fprintf(errOut, "Usage of %s:\n", f.Name())
				f.PrintDefaults()
			}
		} else {
			fmt.Fprintln(errOut, "serve: usage")
		}
		return 2
	}
	journalSet, blobsSet, janusEnvModeSet := false, false, false
	f.Visit(func(v *flag.Flag) {
		switch v.Name {
		case "journal":
			journalSet = true
		case "blobs":
			blobsSet = true
		case "janus-env-mode":
			janusEnvModeSet = true
		}
	})
	// Preserve the original silent usage failure for an explicitly empty
	// journal flag. In particular, do not resolve the default data directory.
	if journalSet && *jp == "" {
		return 2
	}
	var dataPath string
	var err error
	*jp, dataPath, _, err = resolveJournal(*dataDir, *jp, journalSet)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if !journalSet {
		fmt.Fprintf(errOut, "rhizome: data dir %s\n", dataPath)
		if !blobsSet {
			*bp = filepath.Join(dataPath, "blobs")
			if err := os.MkdirAll(*bp, 0700); err != nil {
				fmt.Fprintln(errOut, err)
				return 1
			}
		}
	}
	jc, e := janusServeFromFlags(*jHX, *jEndpoint, *jProfile, *jAcceptRoot, *jWorld, *jSession, *jEnvMode, janusEnvModeSet, *jEnvPassthrough, *jInterval)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 2
	}
	exec, e := janusExecConfig(jc != nil, *jExec)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 2
	}
	if jc != nil {
		jc.Exec = exec
	}
	idle, e := janusIdleTimeout(*jIdle)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 2
	}
	if jc != nil {
		jc.IdleTimeout = idle
	}
	if (*idxRepo == "") != (*idxOut == "") {
		// 부분 설정 = 오류, silent disable 금지 (janus D4 관례와 동일).
		fmt.Fprintln(errOut, "incomplete codeindex configuration: index-repo and index-out are both required")
		return 2
	}
	unlock, e := acquireJournalLock(*jp, errOut)
	if e != nil {
		return 1
	}
	defer unlock()
	j, e := journal.Open(*jp)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	j.OnPoison = journalPoisonLogger(errOut)
	defer j.Close()
	handler, loop := assembleServe(j, blobStoreFromFlag(*bp), *idxRepo, *idxOut, jc, errOut)
	if serveHandlerWrap != nil {
		handler = serveHandlerWrap(handler)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if loop != nil {
		wg.Add(1)
		go func() { defer wg.Done(); _ = loop.Run(ctx, jc.Interval) }()
	}
	ln, e := net.Listen("tcp", *addr)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	// Request contexts end with ctx, so long-lived SSE handlers (which exit
	// only on r.Context()) release their connections instead of stalling
	// Shutdown until its timeout. Handlers must therefore not tie journal
	// writes to r.Context(): an in-flight write completes under the drain below.
	srv := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return ctx }}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutdownCtx)
	}()
	e = srv.Serve(ln)
	// Serve returns as soon as Shutdown is called; drain in-flight handlers and
	// the JANUS loop before the deferred journal close / unlock run. A handler
	// still running past the 5s Shutdown deadline sees its Append fail with
	// "closed journal" — a safe failure, never a partial write.
	cancel()
	<-shutdownDone
	wg.Wait()
	if e != nil && e != http.ErrServerClosed {
		fmt.Fprintln(errOut, e)
		return 1
	}
	return 0
}

// serveHandlerWrap, when set (tests only), wraps the assembled handler so a
// slow request can be put in flight during shutdown. nil in production.
var serveHandlerWrap func(http.Handler) http.Handler

func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "ingest":
		return ingest(args[1:], out, errOut)
	case "serve":
		return serve(args[1:], out, errOut)
	case "memories":
		return memories(args[1:], out, errOut)
	case "index":
		return indexCmd(args[1:], out, errOut)
	case "graph":
		return graphCmd(args[1:], out, errOut)
	default:
		return 2
	}
}
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
