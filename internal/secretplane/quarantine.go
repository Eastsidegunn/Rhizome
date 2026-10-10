package secretplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"rhizome/internal/secretwire"
)

// Quarantine reasons (value-free).
const (
	QuarantineAfterCutoff = "after-cutoff" // P5 ③: outbox entry with seq > C
	QuarantineSeqConflict = "seq-conflict" // P2 / P5 ③: the plane's entry at a conflicting seq
	QuarantineAbandoned   = "abandoned"    // P5 ③ / P4: operation in progress at freeze
)

// qrec is one quarantine.ndjson line, keyed (PlaneID, Seq) for entries and
// (PlaneID, OpID) for abandoned operations (P5a).
type qrec struct {
	K       string            `json:"k"` // "entry" | "abandoned"
	PlaneID string            `json:"planeId"`
	Seq     uint64            `json:"seq,omitempty"`
	OpID    string            `json:"opId,omitempty"`
	Op      string            `json:"op,omitempty"`
	Ref     string            `json:"ref,omitempty"`
	Reason  string            `json:"reason"`
	Entry   *secretwire.Entry `json:"entry,omitempty"`
}

const (
	qkEntry     = "entry"
	qkAbandoned = "abandoned"
)

func (q qrec) key() string {
	if q.K == qkEntry {
		return "s/" + q.PlaneID + "/" + strconv.FormatUint(q.Seq, 10)
	}
	return "o/" + q.PlaneID + "/" + q.OpID
}

func (q qrec) valid() bool {
	if !secretwire.ValidPlaneID(q.PlaneID) {
		return false
	}
	switch q.K {
	case qkEntry:
		return secretwire.ValidSeq(q.Seq) && q.Entry != nil && q.Entry.Seq == q.Seq && q.Op == "" &&
			q.OpID == q.Entry.OpID && q.Ref == q.Entry.Ref &&
			(q.Reason == QuarantineAfterCutoff || q.Reason == QuarantineSeqConflict)
	case qkAbandoned:
		return q.Seq == 0 && q.Entry == nil && secretwire.ValidOpID(q.OpID) && secretwire.ValidOp(q.Op) &&
			secretwire.ValidRef(q.Ref) && q.Reason == QuarantineAbandoned
	}
	return false
}

type qstore struct {
	keys       map[string]bool
	items      []qrec
	abandoned  map[string]bool // opIds
	outcomeOps map[string]bool // opIds whose outcome entry is quarantined
}

func newQStore() *qstore {
	return &qstore{keys: map[string]bool{}, abandoned: map[string]bool{}, outcomeOps: map[string]bool{}}
}

func (q *qstore) add(r qrec) {
	q.keys[r.key()] = true
	q.items = append(q.items, r)
	if r.K == qkAbandoned {
		q.abandoned[r.OpID] = true
	} else if r.Entry.Fact == secretwire.FactOutcome {
		q.outcomeOps[r.OpID] = true
	}
}

func (q *qstore) hasSeq(p string, seq uint64) bool {
	return q.keys[qrec{K: qkEntry, PlaneID: p, Seq: seq}.key()]
}

// parseQLines strictly parses complete quarantine lines.
func parseQLines(data []byte) ([]qrec, bool) {
	var out []qrec
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil, false
		}
		var r qrec
		if !decodeLine(data[:i], &r) || !r.valid() {
			return nil, false
		}
		out = append(out, r)
		data = data[i+1:]
	}
	return out, true
}

// loadQuarantine opens an existing quarantine file at startup (P5a): mode
// check, torn-tail truncate + fsync, refuse on mid-file corruption, then
// file and directory fsync so that every loaded key is durable.
func (pl *Plane) loadQuarantine() error {
	path := filepath.Join(pl.dir, quarantineName)
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrIO
	}
	if !safeFile(fi) {
		return ErrFileUnsafe
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return ErrIO
	}
	pl.qfile = f
	data, err := io.ReadAll(f)
	if err != nil {
		return ErrIO
	}
	if data, err = pl.cutTornTail(f, data, "quarantine"); err != nil {
		return err
	}
	recs, ok := parseQLines(data)
	if !ok {
		return ErrQuarantineCorrupt
	}
	for _, r := range recs {
		if !pl.q.keys[r.key()] {
			pl.q.add(r)
		}
	}
	if err := pl.sync(f, "quarantine"); err != nil {
		return err
	}
	return pl.syncDir()
}

// quarantine appends r unless its key is already present (no-op rewrite,
// P5a). The file is created on first use: 0600, O_EXCL, file fsync, then
// directory fsync.
func (pl *Plane) quarantine(r qrec) error {
	if pl.q.keys[r.key()] {
		return nil
	}
	if err := pl.qAppend(r); err != nil {
		return err
	}
	pl.q.add(r)
	return nil
}

func (pl *Plane) qAppend(r qrec) error {
	if pl.qfile == nil {
		f, err := os.OpenFile(filepath.Join(pl.dir, quarantineName), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
		if err != nil {
			return pl.fatal()
		}
		pl.qfile = f
		pl.at("quarantine-created")
		if err := f.Chmod(0o600); err != nil {
			return pl.fatal()
		}
		if err := pl.sync(f, "quarantine"); err != nil {
			return err
		}
		if err := pl.syncDir(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return pl.fatal()
	}
	b = append(b, '\n')
	if _, err := pl.qfile.Write(b[:len(b)/2]); err != nil {
		return pl.fatal()
	}
	pl.at("quarantine-mid-line")
	if _, err := pl.qfile.Write(b[len(b)/2:]); err != nil {
		return pl.fatal()
	}
	return pl.sync(pl.qfile, "quarantine")
}

// barrier is the P5a barrier, run before any record that depends on
// quarantine content (epoch-split, P2 (b) removal): ① expected keys are the
// arguments; ② reopen the file, read it to the end and append any missing
// key; ③ fsync the file and the directory. Any failure is fatal.
func (pl *Plane) barrier(expected []qrec) error {
	pl.at("barrier-start")
	path := filepath.Join(pl.dir, quarantineName)
	if pl.qfile == nil && len(expected) == 0 {
		return nil
	}
	var onDisk map[string]bool
	if pl.qfile != nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return pl.fatal()
		}
		recs, ok := parseQLines(data)
		if !ok {
			return pl.fatal()
		}
		onDisk = make(map[string]bool, len(recs))
		for _, r := range recs {
			onDisk[r.key()] = true
		}
	}
	for _, r := range expected {
		if onDisk[r.key()] {
			continue
		}
		if err := pl.qAppend(r); err != nil {
			return err
		}
		if !pl.q.keys[r.key()] {
			pl.q.add(r)
		}
		if onDisk == nil {
			onDisk = map[string]bool{}
		}
		onDisk[r.key()] = true
	}
	pl.at("barrier-before-sync")
	f, err := os.Open(path)
	if err != nil {
		return pl.fatal()
	}
	defer f.Close()
	if err := pl.sync(f, "quarantine-barrier"); err != nil {
		return err
	}
	if err := pl.syncDir(); err != nil {
		return err
	}
	pl.at("barrier-done")
	return nil
}

// QuarantineItem is the value-free summary of one quarantine line
// (P5 `quarantine list`): Seq and Fact are empty for abandoned operations.
type QuarantineItem struct {
	PlaneID string
	Seq     uint64
	Fact    string
	OpID    string
	Ref     string
	Reason  string
}

// QuarantineList returns the quarantine summary in file order.
func (pl *Plane) QuarantineList() ([]QuarantineItem, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return nil, err
	}
	out := make([]QuarantineItem, 0, len(pl.q.items))
	for _, r := range pl.q.items {
		it := QuarantineItem{PlaneID: r.PlaneID, Seq: r.Seq, OpID: r.OpID, Ref: r.Ref, Reason: r.Reason}
		if r.Entry != nil {
			it.Fact = r.Entry.Fact
		}
		out = append(out, it)
	}
	return out, nil
}

// FormatQuarantineList renders items one per line as
// "planeId seq fact opId ref reason" ("-" for an absent field).
func FormatQuarantineList(items []QuarantineItem) string {
	var b strings.Builder
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, it := range items {
		seq := "-"
		if it.Seq != 0 {
			seq = strconv.FormatUint(it.Seq, 10)
		}
		b.WriteString(strings.Join([]string{it.PlaneID, seq, dash(it.Fact), dash(it.OpID), dash(it.Ref), it.Reason}, " "))
		b.WriteByte('\n')
	}
	return b.String()
}
