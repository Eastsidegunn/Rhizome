package secretplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"rhizome/internal/secretwire"
)

// values.json is the only file that holds secret values (mode 0600). Each
// value is tagged with the ref, its plane version and the opId that wrote it,
// so recovery can tell whether an in-progress operation's effect happened
// (P4). Value bytes are base64 (encoding/json []byte).
type valuesFile struct {
	V      int        `json:"v"`
	Values []valueRec `json:"values"`
}

type valueRec struct {
	Ref     string `json:"ref"`
	Version uint64 `json:"version"`
	OpID    string `json:"opId"`
	Value   []byte `json:"value"`
}

// valueTag is the value-free part of a values entry.
type valueTag struct {
	Version uint64
	OpID    string
}

var errBackend = errors.New("backend")

// readValues parses the values file; a missing file is empty.
func (pl *Plane) readValues() ([]valueRec, error) {
	data, err := os.ReadFile(filepath.Join(pl.dir, valuesName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrIO
	}
	defer clear(data)
	return parseValues(data)
}

// parseValues strictly parses values-file bytes (shared by the plane and
// the lock-free run reader). On error nothing is returned and every decoded
// value is wiped.
func parseValues(data []byte) ([]valueRec, error) {
	var vf valuesFile
	if !decodeLine(data, &vf) || vf.V != 1 {
		wipeValues(vf.Values)
		return nil, ErrValuesCorrupt
	}
	seen := map[string]bool{}
	for _, v := range vf.Values {
		if !secretwire.ValidRef(v.Ref) || !secretwire.ValidOpID(v.OpID) || v.Version < 1 || len(v.Value) == 0 || seen[v.Ref] {
			wipeValues(vf.Values)
			return nil, ErrValuesCorrupt
		}
		seen[v.Ref] = true
	}
	return vf.Values, nil
}

// readTags returns the value-free index ref → tag.
func (pl *Plane) readTags() (map[string]valueTag, error) {
	vals, err := pl.readValues()
	if err != nil {
		return nil, err
	}
	defer wipeValues(vals)
	tags := make(map[string]valueTag, len(vals))
	for _, v := range vals {
		tags[v.Ref] = valueTag{Version: v.Version, OpID: v.OpID}
	}
	return tags, nil
}

func wipeValues(vals []valueRec) {
	for i := range vals {
		clear(vals[i].Value)
	}
}

// writeValues replaces the values file (P3 ③): same-directory 0600 temp file
// created O_EXCL → write → fsync → close → rename → directory fsync. A
// non-sync failure removes the temp file and returns errBackend; an fsync
// failure is fatal.
func (pl *Plane) writeValues(vals []valueRec) error {
	sort.Slice(vals, func(i, j int) bool { return vals[i].Ref < vals[j].Ref })
	if vals == nil {
		vals = []valueRec{}
	}
	data, err := json.Marshal(valuesFile{V: 1, Values: vals})
	if err != nil {
		return errBackend
	}
	defer clear(data)
	suffix, err := pl.randHex(8)
	if err != nil {
		return errBackend
	}
	tmp := filepath.Join(pl.dir, valuesTmpPrefix+suffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errBackend
	}
	fail := func(e error) error {
		f.Close()
		os.Remove(tmp)
		return e
	}
	pl.at("values-tmp-created")
	if err := f.Chmod(0o600); err != nil {
		return fail(errBackend)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fail(errBackend)
	}
	if err := pl.at("values-write"); err != nil {
		return fail(errBackend)
	}
	if err := pl.sync(f, "values-tmp"); err != nil {
		f.Close()
		return err // fatal; recovery removes the temp file
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return errBackend
	}
	pl.at("values-before-rename")
	if err := os.Rename(tmp, filepath.Join(pl.dir, valuesName)); err != nil {
		os.Remove(tmp)
		return errBackend
	}
	pl.at("values-renamed")
	return pl.syncDir()
}

// removeTemps deletes leftover temp values and profiles files (P4).
func (pl *Plane) removeTemps() error {
	ents, err := os.ReadDir(pl.dir)
	if err != nil {
		return ErrIO
	}
	removed := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), valuesTmpPrefix) || strings.HasPrefix(e.Name(), profilesTmpPrefix) {
			if err := os.Remove(filepath.Join(pl.dir, e.Name())); err != nil {
				return ErrIO
			}
			removed = true
		}
	}
	if removed {
		return pl.syncDir()
	}
	return nil
}

// applyValue performs an operation's external effect on the values file.
// value is copied; the caller may wipe its slice afterwards.
func (pl *Plane) applyValue(op, ref, opID string, version uint64, value []byte) error {
	pl.valuesMu.Lock()
	defer pl.valuesMu.Unlock()
	vals, err := pl.readValues()
	if err != nil {
		if errors.Is(err, ErrIO) || errors.Is(err, ErrValuesCorrupt) {
			return errBackend
		}
		return err
	}
	defer wipeValues(vals)
	out := vals[:0:0]
	for _, v := range vals {
		if v.Ref != ref {
			out = append(out, v)
		}
	}
	if op != secretwire.OpRevoke {
		out = append(out, valueRec{Ref: ref, Version: version, OpID: opID, Value: bytes.Clone(value)})
		defer clear(out[len(out)-1].Value)
	}
	return pl.writeValues(out)
}
