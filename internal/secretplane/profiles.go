package secretplane

// Run profiles (RHZ-107 S9, FR-RHZ-174, outside the SCP §9 approved
// surfaces: the M8 value-consumption path, kept minimal). A profile names one
// program (absolute path, no PATH lookup), a fixed argv prefix and the
// environment variables that receive a stored value at exec time, each
// bound to a ref. profiles.json holds no value. Profiles are added and
// removed only by the interactive session (the plane lock is held, like every
// other write); `run` reads profiles.json and values.json read-only without
// the lock (both are replaced atomically by rename).

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"rhizome/internal/secretwire"
)

const (
	profilesName      = "profiles.json"
	profilesTmpPrefix = "profiles.json.tmp-"

	// MaxProfileEnv, MaxProfileArgs and MaxProfileArgLen bound a profile.
	MaxProfileEnv    = 8
	MaxProfileArgs   = 32
	MaxProfileArgLen = 1024
)

// Profile errors (fixed text).
var (
	ErrProfileExists   = errors.New("secretplane: profile-exists")
	ErrUnknownProfile  = errors.New("secretplane: unknown-profile")
	ErrValueMissing    = errors.New("secretplane: value-missing")
	ErrProfilesCorrupt = errors.New("secretplane: profiles-corrupt")
)

// RunEnvAllowlist is the only part of the caller's environment that reaches
// a run child (the same names as the session launcher's allowlist; copied,
// not imported, per §7). Profile variables may not use these names.
var RunEnvAllowlist = []string{"HOME", "USER", "LOGNAME", "PATH", "LANG", "TMPDIR", "SHELL"}

var envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ValidEnvName reports whether s may name an injected variable: the
// ^[A-Z][A-Z0-9_]{0,63}$ form and not one of RunEnvAllowlist.
func ValidEnvName(s string) bool {
	if !envNameRe.MatchString(s) {
		return false
	}
	for _, n := range RunEnvAllowlist {
		if s == n {
			return false
		}
	}
	return true
}

// Profile is one run profile. Env maps variable name → ref.
type Profile struct {
	Name           string            `json:"name"`
	Program        string            `json:"program"`
	Args           []string          `json:"args,omitempty"`
	AllowExtraArgs bool              `json:"allowExtraArgs"`
	Env            map[string]string `json:"env"`
}

type profilesFile struct {
	V        int       `json:"v"`
	Profiles []Profile `json:"profiles"`
}

// validShape checks a profile without the file system or plane state.
func (p Profile) validShape() bool {
	if !secretwire.ValidName(p.Name) || !filepath.IsAbs(p.Program) || filepath.Clean(p.Program) != p.Program ||
		strings.IndexByte(p.Program, 0) >= 0 || len(p.Args) > MaxProfileArgs || len(p.Env) < 1 || len(p.Env) > MaxProfileEnv {
		return false
	}
	for _, a := range p.Args {
		if len(a) > MaxProfileArgLen || strings.IndexByte(a, 0) >= 0 {
			return false
		}
	}
	for n, r := range p.Env {
		if !ValidEnvName(n) || !secretwire.ValidRef(r) {
			return false
		}
	}
	return true
}

// ErrProgramUnsafe: the program path fails CheckProgram.
var ErrProgramUnsafe = errors.New("secretplane: program-unsafe")

// trustedUID is the owner rule of CheckProgram: this user or root. A
// variable so tests can simulate another owner.
var trustedUID = func(uid uint32) bool { return uid == 0 || uid == uint32(os.Geteuid()) }

// trustedEntry: owned by a trusted uid and not group- or other-writable.
func trustedEntry(fi os.FileInfo) bool {
	uid, ok := ownerUID(fi)
	return ok && trustedUID(uid) && fi.Mode().Perm()&0o022 == 0
}

// CheckProgram decides whether path may be exec'd by `run` and returns the
// path to exec. Symlink policy: symlinks are resolved, not refused.
//   - path must be absolute and clean;
//   - every component of path as given is checked with Lstat: a directory
//     must be owned by this user or root and not group/other-writable (so
//     sticky world-writable directories such as /tmp fail); a symlink must
//     be owned by this user or root (its target is checked next);
//   - path is resolved with filepath.EvalSymlinks and every directory of the
//     resolved path gets the same directory rule;
//   - the resolved file is opened with O_NOFOLLOW and checked on the open
//     descriptor: regular file, an execute bit, owned by this user or root,
//     not group/other-writable.
//
// The caller execs the returned resolved path. It is called at profile-add
// and again by run immediately before exec.
func CheckProgram(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return "", ErrProgramUnsafe
	}
	if err := checkComponents(path, true); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", ErrProgramUnsafe
	}
	if err := checkComponents(resolved, false); err != nil {
		return "", err
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return "", ErrProgramUnsafe
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || !trustedEntry(fi) {
		return "", ErrProgramUnsafe
	}
	return resolved, nil
}

// checkComponents Lstats "/" and every directory prefix of p (and, when
// allowLinks, the final component if it is a symlink). Without allowLinks
// (a resolved path) any symlink is refused.
func checkComponents(p string, allowLinks bool) error {
	parts := strings.Split(p, "/")[1:]
	cur := "/"
	check := func(name string, final bool) error {
		fi, err := os.Lstat(name)
		if err != nil {
			return ErrProgramUnsafe
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			uid, ok := ownerUID(fi)
			if !allowLinks || !ok || !trustedUID(uid) {
				return ErrProgramUnsafe
			}
		case final:
			// the file itself is checked on its descriptor
		case !fi.IsDir() || !trustedEntry(fi):
			return ErrProgramUnsafe
		}
		return nil
	}
	if err := check(cur, false); err != nil {
		return err
	}
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		if err := check(cur, i == len(parts)-1); err != nil {
			return err
		}
	}
	return nil
}

func cloneProfile(p Profile) Profile {
	c := p
	c.Args = append([]string(nil), p.Args...)
	c.Env = make(map[string]string, len(p.Env))
	for k, v := range p.Env {
		c.Env[k] = v
	}
	return c
}

func parseProfiles(data []byte) ([]Profile, error) {
	var pf profilesFile
	if !decodeLine(data, &pf) || pf.V != 1 {
		return nil, ErrProfilesCorrupt
	}
	seen := map[string]bool{}
	for _, p := range pf.Profiles {
		if !p.validShape() || seen[p.Name] {
			return nil, ErrProfilesCorrupt
		}
		seen[p.Name] = true
	}
	return pf.Profiles, nil
}

// readProfiles reads profiles.json of an open plane (missing = none).
func (pl *Plane) readProfiles() ([]Profile, error) {
	data, err := os.ReadFile(filepath.Join(pl.dir, profilesName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrIO
	}
	return parseProfiles(data)
}

// writeProfiles replaces profiles.json like the values file: same-directory
// 0600 temp file (O_EXCL) → write → fsync → close → rename → directory
// fsync. An fsync failure is fatal.
func (pl *Plane) writeProfiles(ps []Profile) error {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	if ps == nil {
		ps = []Profile{}
	}
	data, err := json.Marshal(profilesFile{V: 1, Profiles: ps})
	if err != nil {
		return ErrIO
	}
	suffix, err := pl.randHex(8)
	if err != nil {
		return ErrIO
	}
	tmp := filepath.Join(pl.dir, profilesTmpPrefix+suffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrIO
	}
	fail := func(e error) error {
		f.Close()
		os.Remove(tmp)
		return e
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(ErrIO)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fail(ErrIO)
	}
	if err := pl.sync(f, "profiles-tmp"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return ErrIO
	}
	if err := os.Rename(tmp, filepath.Join(pl.dir, profilesName)); err != nil {
		os.Remove(tmp)
		return ErrIO
	}
	return pl.syncDir()
}

// Profiles lists the run profiles (value-free), sorted by name.
func (pl *Plane) Profiles() ([]Profile, error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return nil, err
	}
	ps, err := pl.readProfiles()
	if err != nil {
		return nil, err
	}
	out := make([]Profile, len(ps))
	for i, p := range ps {
		out[i] = cloneProfile(p)
	}
	return out, nil
}

// AddProfile records a new profile. It must be driven only by the
// interactive session. The program must pass CheckProgram; every
// ref must be a current ref of this plane with a stored value (attested and
// revoked refs have none: ErrUnknownRef).
func (pl *Plane) AddProfile(p Profile) error {
	p = cloneProfile(p)
	if !p.validShape() {
		return ErrInvalidInput
	}
	if _, err := CheckProgram(p.Program); err != nil {
		return ErrInvalidInput
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return err
	}
	pl.valuesMu.Lock()
	tags, err := pl.readTags()
	pl.valuesMu.Unlock()
	if err != nil {
		return err
	}
	for _, ref := range p.Env {
		r := pl.st.refs[ref]
		if r == nil || r.Revoked || r.Origin == originAttested {
			return ErrUnknownRef
		}
		if _, ok := tags[ref]; !ok {
			return ErrValueMissing
		}
	}
	ps, err := pl.readProfiles()
	if err != nil {
		return err
	}
	for _, q := range ps {
		if q.Name == p.Name {
			return ErrProfileExists
		}
	}
	return pl.writeProfiles(append(ps, p))
}

// RemoveProfile deletes a profile. Interactive session only.
func (pl *Plane) RemoveProfile(name string) error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if err := pl.usable(); err != nil {
		return err
	}
	ps, err := pl.readProfiles()
	if err != nil {
		return err
	}
	out := ps[:0:0]
	for _, q := range ps {
		if q.Name != name {
			out = append(out, q)
		}
	}
	if len(out) == len(ps) {
		return ErrUnknownProfile
	}
	return pl.writeProfiles(out)
}

// RunSpec is what `run` executes: the profile and the values for its
// variables. Values must be wiped by the caller (Wipe) on every path.
type RunSpec struct {
	Program        string
	Args           []string
	AllowExtraArgs bool
	// Env holds name and value, sorted by name.
	Env []RunVar
}

// RunVar is one injected variable.
type RunVar struct {
	Name  string
	Value []byte
}

// Wipe zeroes every value.
func (s *RunSpec) Wipe() {
	for i := range s.Env {
		clear(s.Env[i].Value)
	}
}

// readSafeFile reads a plane file that must be a regular 0600 file owned by
// this user, opened without following a symlink and checked on the open
// descriptor. missing reports a file that does not exist.
func readSafeFile(path string) (data []byte, missing bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, ErrFileUnsafe
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !safeFile(fi) {
		return nil, false, ErrFileUnsafe
	}
	data, err = io.ReadAll(f)
	if err != nil {
		clear(data)
		return nil, false, ErrIO
	}
	return data, false, nil
}

// ResolveRun loads profile name from the state directory dir for `run`. It
// takes no lock and writes nothing: profiles.json and values.json are read
// read-only (a concurrent session replaces them by rename). Errors:
// ErrStateDirUnsafe / ErrFileUnsafe for modes, ErrProfilesCorrupt,
// ErrValuesCorrupt, ErrUnknownProfile, ErrValueMissing (a variable's ref has
// no stored value). On error no value is returned (all are wiped).
func ResolveRun(dir, name string) (RunSpec, error) {
	if !filepath.IsAbs(dir) {
		return RunSpec{}, ErrStateDirUnsafe
	}
	dir = filepath.Clean(dir)
	fi, err := os.Lstat(dir)
	if err != nil {
		return RunSpec{}, ErrStateDirUnsafe
	}
	m := fi.Mode()
	if m&os.ModeSymlink != 0 || !m.IsDir() || m.Perm() != 0o700 || m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !ownedBySelf(fi) {
		return RunSpec{}, ErrStateDirUnsafe
	}
	pdata, missing, err := readSafeFile(filepath.Join(dir, profilesName))
	if err != nil {
		return RunSpec{}, err
	}
	if missing {
		return RunSpec{}, ErrUnknownProfile
	}
	ps, err := parseProfiles(pdata)
	if err != nil {
		return RunSpec{}, err
	}
	var prof *Profile
	for i := range ps {
		if ps[i].Name == name {
			prof = &ps[i]
		}
	}
	if prof == nil {
		return RunSpec{}, ErrUnknownProfile
	}
	vdata, missing, err := readSafeFile(filepath.Join(dir, valuesName))
	if err != nil {
		return RunSpec{}, err
	}
	if missing {
		return RunSpec{}, ErrValueMissing
	}
	defer clear(vdata)
	vals, err := parseValues(vdata)
	if err != nil {
		return RunSpec{}, err
	}
	defer wipeValues(vals)
	byRef := map[string][]byte{}
	for _, v := range vals {
		byRef[v.Ref] = v.Value
	}
	spec := RunSpec{Program: prof.Program, Args: append([]string(nil), prof.Args...), AllowExtraArgs: prof.AllowExtraArgs}
	names := make([]string, 0, len(prof.Env))
	for n := range prof.Env {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := byRef[prof.Env[n]]
		if v == nil {
			spec.Wipe()
			return RunSpec{}, ErrValueMissing
		}
		spec.Env = append(spec.Env, RunVar{Name: n, Value: append([]byte(nil), v...)})
	}
	return spec, nil
}
