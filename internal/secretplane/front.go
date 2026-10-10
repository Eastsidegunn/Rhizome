package secretplane

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Value-free read helpers for the interactive front end (FR-RHZ-139, M5b).

// RefSummary is the value-free description of one ref known to the plane.
// Origin is "request" (introduced by a Rhizome register request),
// "plane-direct" or "attested".
type RefSummary struct {
	Ref, Name, RefKind, Origin string
	ScopeTags                  []string
	Version                    uint64
	Revoked                    bool
	// Busy is true while a rotate or revoke of the ref is in progress.
	Busy bool
}

// Attested reports whether the summary's ref is attested (record-only: the
// plane holds no value for it, Q2).
func (r RefSummary) Attested() bool { return r.Origin == originAttested }

// Refs lists the refs known to the plane, sorted by ref.
func (pl *Plane) Refs() ([]RefSummary, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return nil, err
	}
	out := make([]RefSummary, 0, len(pl.st.refs))
	for ref, r := range pl.st.refs {
		out = append(out, RefSummary{Ref: ref, Name: r.Name, RefKind: r.Kind, Origin: r.Origin,
			ScopeTags: cloneTags(r.Tags), Version: r.Version, Revoked: r.Revoked, Busy: r.Busy != ""})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// ListQuarantine reads the quarantine summary of the state directory dir
// without opening the plane: it creates nothing, writes nothing and runs no
// recovery (`quarantine list`, P5). The directory and file must pass the
// same ownership and mode checks as Open. If a plane process holds the
// directory lock it returns ErrBusy. An unterminated last line is not yet
// written (P5a) and is skipped, not truncated; any other damage is
// ErrQuarantineCorrupt.
func ListQuarantine(dir string) ([]QuarantineItem, error) {
	if !filepath.IsAbs(dir) {
		return nil, ErrStateDirUnsafe
	}
	dir = filepath.Clean(dir)
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, ErrStateDirUnsafe
	}
	m := fi.Mode()
	if m&os.ModeSymlink != 0 || !m.IsDir() || m.Perm() != 0o700 || m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !ownedBySelf(fi) {
		return nil, ErrStateDirUnsafe
	}
	df, err := os.Open(dir)
	if err != nil {
		return nil, ErrIO
	}
	defer df.Close()
	if err := lockDir(df); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, quarantineName)
	qi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []QuarantineItem{}, nil
	}
	if err != nil {
		return nil, ErrIO
	}
	if !safeFile(qi) {
		return nil, ErrFileUnsafe
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrIO
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, ErrIO
	}
	if n := len(data); n > 0 && data[n-1] != '\n' {
		i := len(data) - 1
		for i >= 0 && data[i] != '\n' {
			i--
		}
		data = data[:i+1]
	}
	recs, ok := parseQLines(data)
	if !ok {
		return nil, ErrQuarantineCorrupt
	}
	q := newQStore()
	out := make([]QuarantineItem, 0, len(recs))
	for _, r := range recs {
		if q.keys[r.key()] {
			continue
		}
		q.add(r)
		it := QuarantineItem{PlaneID: r.PlaneID, Seq: r.Seq, OpID: r.OpID, Ref: r.Ref, Reason: r.Reason}
		if r.Entry != nil {
			it.Fact = r.Entry.Fact
		}
		out = append(out, it)
	}
	return out, nil
}
