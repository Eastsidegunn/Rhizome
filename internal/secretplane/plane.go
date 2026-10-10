package secretplane

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"rhizome/internal/secretwire"
)

// State directory file names.
const (
	ledgerName      = "ledger.ndjson"
	valuesName      = "values.json"
	quarantineName  = "quarantine.ndjson"
	valuesTmpPrefix = "values.json.tmp-"
)

// Options configures a Plane.
type Options struct {
	// Now is the clock for planeObservedAt (default time.Now). It is never
	// compared with anything (G13).
	Now func() time.Time
	// OnFatal runs once when the plane first returns ErrFatal (an fsync or
	// durable-write failure). The caller must then exit the process (P3).
	OnFatal func()
}

// seams are test-only injection points; they are unexported so no caller
// outside this package can bypass durability or confirmation.
type seams struct {
	// hook is called at named points (crash injection: panic; failure
	// injection at "values-write": return an error).
	hook func(point string) error
	// syncFn replaces (*os.File).Sync (fsync failure injection).
	syncFn func(f *os.File) error
	rand   io.Reader
}

// Plane is an open plane state directory. All methods are safe for
// concurrent use; ledger appends are serialized (single writer).
type Plane struct {
	dir  string
	opts Options
	sm   seams

	mu       sync.Mutex // guards everything below except dead
	valuesMu sync.Mutex // serializes values-file rewrites
	dead     atomic.Bool
	fatalMu  sync.Once

	dirFile *os.File // holds the directory lock
	ledger  *os.File
	qfile   *os.File
	closed  bool

	st         *state
	q          *qstore
	halted     bool
	committing map[string]bool
}

// Open opens (creating if absent) the state directory dir, takes the
// single-writer lock and runs startup recovery (P4). dir must be absolute;
// an existing directory must be a real directory owned by this user with
// mode exactly 0700, and every plane file must be a regular file owned by
// this user with mode exactly 0600, else Open refuses to start.
func Open(dir string, opts Options) (*Plane, error) { return open(dir, opts, seams{}) }

func open(dir string, opts Options, sm seams) (*Plane, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if sm.syncFn == nil {
		sm.syncFn = (*os.File).Sync
	}
	if sm.rand == nil {
		sm.rand = rand.Reader
	}
	pl := &Plane{opts: opts, sm: sm, st: newState(), q: newQStore(), committing: map[string]bool{}}
	if !filepath.IsAbs(dir) {
		return nil, ErrStateDirUnsafe
	}
	pl.dir = filepath.Clean(dir)
	if err := pl.ensureDir(); err != nil {
		return nil, err
	}
	df, err := os.Open(pl.dir)
	if err != nil {
		return nil, ErrIO
	}
	pl.dirFile = df
	if err := lockDir(df); err != nil {
		df.Close()
		return nil, err
	}
	// A panic during startup (injected crash in tests) must not leak the
	// lock descriptor.
	defer func() {
		if r := recover(); r != nil {
			pl.closeFiles()
			panic(r)
		}
	}()
	if err := pl.startup(); err != nil {
		pl.closeFiles()
		if pl.dead.Load() {
			return nil, ErrFatal
		}
		return nil, err
	}
	return pl, nil
}

func (pl *Plane) startup() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	// Mode checks first: nothing is written into a directory holding an
	// unsafe plane file.
	for _, name := range []string{ledgerName, valuesName, quarantineName} {
		fi, err := os.Lstat(filepath.Join(pl.dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return ErrIO
		}
		if !safeFile(fi) {
			return ErrFileUnsafe
		}
	}
	if err := pl.removeTemps(); err != nil {
		return err
	}
	if err := pl.loadLedger(); err != nil {
		return err
	}
	if err := pl.loadQuarantine(); err != nil {
		return err
	}
	for id := range pl.q.abandoned {
		if op := pl.st.ops[id]; op == nil || op.Done {
			return ErrQuarantineCorrupt
		}
	}
	return pl.recover()
}

// ensureDir creates the state directory (0700, parent fsynced) or checks it.
func (pl *Plane) ensureDir() error {
	fi, err := os.Lstat(pl.dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(pl.dir, 0o700); err != nil {
			return ErrStateDirUnsafe
		}
		if err := os.Chmod(pl.dir, 0o700); err != nil {
			return ErrStateDirUnsafe
		}
		parent, perr := os.Open(filepath.Dir(pl.dir))
		if perr != nil {
			return ErrIO
		}
		perr = pl.sm.syncFn(parent)
		parent.Close()
		if perr != nil {
			return pl.fatal()
		}
		fi, err = os.Lstat(pl.dir)
	}
	if err != nil {
		return ErrStateDirUnsafe
	}
	m := fi.Mode()
	if m&os.ModeSymlink != 0 || !m.IsDir() || m.Perm() != 0o700 || m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !ownedBySelf(fi) {
		return ErrStateDirUnsafe
	}
	return nil
}

// safeFile: regular file (not a symlink), owned by this user, mode 0600.
func safeFile(fi os.FileInfo) bool {
	m := fi.Mode()
	return m.IsRegular() && m.Perm() == 0o600 && m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && ownedBySelf(fi)
}

// Close releases the lock and file handles.
func (pl *Plane) Close() error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
		return nil
	}
	pl.closeFiles()
	return nil
}

func (pl *Plane) closeFiles() {
	pl.closed = true
	for _, f := range []*os.File{pl.ledger, pl.qfile, pl.dirFile} {
		if f != nil {
			f.Close()
		}
	}
	pl.ledger, pl.qfile, pl.dirFile = nil, nil, nil
}

// usable is checked first by every method (under mu).
func (pl *Plane) usable() error {
	if pl.dead.Load() {
		return ErrFatal
	}
	if pl.closed {
		return ErrClosed
	}
	return nil
}

func (pl *Plane) at(point string) error {
	if pl.sm.hook != nil {
		return pl.sm.hook(point)
	}
	return nil
}

// fatal latches the plane dead (P3: the process must exit; no retry).
func (pl *Plane) fatal() error {
	pl.dead.Store(true)
	pl.fatalMu.Do(func() {
		if pl.opts.OnFatal != nil {
			pl.opts.OnFatal()
		}
	})
	return ErrFatal
}

func (pl *Plane) sync(f *os.File, what string) error {
	pl.at("sync:" + what)
	if err := pl.sm.syncFn(f); err != nil {
		return pl.fatal()
	}
	return nil
}

func (pl *Plane) syncDir() error { return pl.sync(pl.dirFile, "dir") }

func (pl *Plane) randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(pl.sm.rand, b); err != nil {
		return "", ErrIO
	}
	return hex.EncodeToString(b), nil
}

func (pl *Plane) newOpID() (string, error) {
	h, err := pl.randHex(16)
	return "op-" + h, err
}

func (pl *Plane) newPlaneID() (string, error) {
	for {
		h, err := pl.randHex(16)
		if err != nil {
			return "", err
		}
		if id := "pl-" + h; !pl.st.planes[id] {
			return id, nil
		}
	}
}

// cutTornTail drops an unterminated last line (truncate + fsync, P4/P5a).
func (pl *Plane) cutTornTail(f *os.File, data []byte, what string) ([]byte, error) {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data, nil
	}
	n := bytes.LastIndexByte(data, '\n') + 1
	if err := f.Truncate(int64(n)); err != nil {
		return nil, pl.fatal()
	}
	if err := pl.sync(f, what); err != nil {
		return nil, err
	}
	return data[:n], nil
}

// loadLedger creates the ledger (P3 ①) or replays it with torn-tail repair.
func (pl *Plane) loadLedger() error {
	path := filepath.Join(pl.dir, ledgerName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
		if err != nil {
			return ErrIO
		}
		pl.ledger = f
		pl.at("ledger-created")
		if err := f.Chmod(0o600); err != nil {
			return ErrIO
		}
		return pl.initLedger()
	}
	if err != nil {
		return ErrIO
	}
	pl.ledger = f
	data, err := io.ReadAll(f)
	if err != nil {
		return ErrIO
	}
	if data, err = pl.cutTornTail(f, data, "ledger"); err != nil {
		return err
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var r rec
		if !decodeLine(data[:i], &r) || pl.st.apply(&r) != nil {
			return ErrLedgerCorrupt
		}
		data = data[i+1:]
	}
	if !pl.st.inited {
		return pl.initLedger()
	}
	return nil
}

// initLedger writes the first line (the persisted planeId, Q3), then file
// fsync and directory fsync (P3 ①).
func (pl *Plane) initLedger() error {
	id, err := pl.newPlaneID()
	if err != nil {
		return err
	}
	if err := pl.appendRec(&rec{T: recInit, PlaneID: id}); err != nil {
		return err
	}
	return pl.syncDir()
}

// appendRec validates r against the state (apply), then writes it as one
// line and fsyncs. A record that fails validation is a bug and never reaches
// disk; any write or fsync failure is fatal.
func (pl *Plane) appendRec(r *rec) error {
	if err := pl.st.apply(r); err != nil {
		return pl.fatal()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return pl.fatal()
	}
	b = append(b, '\n')
	if _, err := pl.ledger.Write(b[:len(b)/2]); err != nil {
		return pl.fatal()
	}
	pl.at("ledger-mid-line:" + r.T)
	if _, err := pl.ledger.Write(b[len(b)/2:]); err != nil {
		return pl.fatal()
	}
	if err := pl.sync(pl.ledger, "ledger"); err != nil {
		return err
	}
	pl.at("ledger-appended:" + r.T)
	return nil
}

// recover is P4 plus the P5 restart behavior.
func (pl *Plane) recover() error {
	s := pl.st
	switch s.phase {
	case phaseFrozen:
		// Stay frozen; in-progress ops are abandoned at ③.
	case phaseCutoff:
		if err := pl.finishSplit(); err != nil {
			return err
		}
	case phaseSplit:
		// Operations of P′ left in progress are not resolved in this stage
		// (P4 exception); they are quarantined as abandoned.
		var exp []qrec
		for _, id := range s.inProgress() {
			if pl.q.abandoned[id] {
				continue
			}
			op := s.ops[id]
			exp = append(exp, qrec{K: qkAbandoned, PlaneID: op.PlaneID, OpID: id, Op: op.Op, Ref: op.Ref, Reason: QuarantineAbandoned})
		}
		for _, r := range exp {
			if err := pl.quarantine(r); err != nil {
				return err
			}
		}
		if len(exp) > 0 {
			if err := pl.barrier(exp); err != nil {
				return err
			}
		}
	default:
		if err := pl.resolveInProgress(); err != nil {
			return err
		}
	}
	return pl.quarantineConflicts()
}

// resolveInProgress deterministically finishes in-progress operations from
// the values file, never prompting again (P4): value tagged by the opId →
// ok, else failed/crash-before-effect; revoke by value removal.
// Record-only operations on attested refs have no value evidence and resolve
// to failed/crash-before-effect.
func (pl *Plane) resolveInProgress() error {
	ids := pl.st.inProgress()
	var pending []string
	for _, id := range ids {
		if !pl.q.abandoned[id] {
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	tags, err := pl.readTags()
	if err != nil {
		return err
	}
	for _, id := range pending {
		op := pl.st.ops[id]
		ok := false
		if !op.Attested {
			t, has := tags[op.Ref]
			if op.Op == secretwire.OpRevoke {
				ok = !has
			} else {
				ok = has && t.OpID == id
			}
		}
		pl.at("recover-before-terminal")
		if err := pl.appendEnd(op, ok, secretwire.FailureCrashBeforeEffect); err != nil {
			return err
		}
	}
	return nil
}

// appendEnd writes the terminal + outbox line for op.
func (pl *Plane) appendEnd(op *opRec, ok bool, failure string) error {
	result := secretwire.ResultOK
	if !ok {
		result = secretwire.ResultFailed
	} else {
		failure = ""
	}
	return pl.appendResult(op, result, failure)
}

func (pl *Plane) appendResult(op *opRec, result, failure string) error {
	r := &rec{T: recEnd, OpID: op.OpID, Result: result, Failure: failure, Entry: pl.st.terminalEntry(op, result, failure)}
	return pl.appendRec(r)
}

// quarantineConflicts copies every recorded seq-conflict entry still missing
// from the quarantine file (P2 immediate quarantine; redone after a crash
// before its barrier).
func (pl *Plane) quarantineConflicts() error {
	for _, p := range pl.st.order {
		var seqs []uint64
		for seq := range pl.st.conflicts[p] {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs {
			if err := pl.quarantineConflict(p, seq); err != nil {
				return err
			}
		}
	}
	return nil
}

func (pl *Plane) quarantineConflict(p string, seq uint64) error {
	if seq > pl.st.maxSeq(p) || pl.q.hasSeq(p, seq) {
		return nil
	}
	r := entryQ(p, pl.st.outbox[p][seq-1], QuarantineSeqConflict)
	if err := pl.quarantine(r); err != nil {
		return err
	}
	return pl.barrier([]qrec{r})
}

func entryQ(p string, e secretwire.Entry, reason string) qrec {
	ec := cloneEntry(e)
	return qrec{K: qkEntry, PlaneID: p, Seq: e.Seq, OpID: e.OpID, Ref: e.Ref, Reason: reason, Entry: &ec}
}
