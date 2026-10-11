package codeindex

// RHZ-102a (FR-RHZ-178): the pinned git layer of the codeindex structure graph
// (design rev5 §4.1·§4.2·§5.1). Every git call runs with a scrubbed env (no
// inherited GIT_DIR/GIT_INDEX_FILE/config), --no-replace-objects and a
// deadline; repos whose history git can rewrite underneath us (shallow,
// grafts, partial clone, replace refs) are refused instead of modelled. The
// legacy Build/ResolveMain path above is untouched.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GitTimeout bounds one structure request (§5.1).
const GitTimeout = 60 * time.Second

// Resource limits (§5.1): entries in the tree, bytes of one read file, bytes
// read in total.
const (
	maxTreeEntries = 100_000
	maxFileBytes   = 1 << 20
	maxTotalBytes  = 256 << 20
)

// Error codes of the structure path. They are the CLI's stable-for-now
// vocabulary; the HTTP state words are SCP material (B6, Phase 1b).
const (
	CodeUnsupportedRepo   = "unsupported_repo"
	CodeCommitUnavailable = "commit_unavailable"
	CodeInvalidInput      = "invalid_input"
	CodeLimitExceeded     = "limit_exceeded"
	CodeTimeout           = "timeout"
	CodeGitError          = "git_error"
)

type CodeError struct {
	Code string
	Msg  string
}

func (e *CodeError) Error() string { return e.Code + ": " + e.Msg }

func codeErr(code, format string, a ...any) error {
	return &CodeError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// PinnedRepo is one local repository opened through the pinned layer. Open
// has already refused the unsupported repo kinds and fixed the object format.
type PinnedRepo struct {
	dir          string
	home         string
	ObjectFormat string // sha1 | sha256
	oidLen       int
}

// gitExit carries a non-zero git exit so callers can tell "answered no"
// (merge-base exit 1) from failure.
type gitExit struct {
	code   int
	args   []string
	stderr string
}

func (e *gitExit) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.args, " "), e.code, strings.TrimSpace(e.stderr))
}

func exitCode(err error) int {
	var ge *gitExit
	if errors.As(err, &ge) {
		return ge.code
	}
	return -1
}

// env is the whole environment git sees (§4.1): nothing is inherited but
// PATH, so GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_CONFIG_* and the user's
// config cannot leak into what we read.
func (r *PinnedRepo) env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func (r *PinnedRepo) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.dir, "--no-replace-objects"}, args...)...)
	cmd.Env = r.env()
	cmd.Dir = r.home
	return cmd
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return codeErr(CodeTimeout, "git did not finish within the deadline: %v", ctx.Err())
	}
	return err
}

func (r *PinnedRepo) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if c := ctxErr(ctx, nil); c != nil {
			return nil, c
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, &gitExit{code: ee.ExitCode(), args: args, stderr: stderr.String()}
		}
		return nil, codeErr(CodeGitError, "git %s: %v", strings.Join(args, " "), err)
	}
	return out, nil
}

func gitFail(err error) error {
	var ce *CodeError
	if errors.As(err, &ce) {
		return err
	}
	return codeErr(CodeGitError, "%v", err)
}

// OpenPinned opens dir and refuses what it cannot pin (B7): shallow clones,
// legacy grafts, partial clones (extensions.partialClone or a promisor
// remote) and any refs/replace/ ref. Close removes the empty HOME it made.
func OpenPinned(ctx context.Context, dir string) (*PinnedRepo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, codeErr(CodeInvalidInput, "repo path: %v", err)
	}
	home, err := os.MkdirTemp("", "rhizome-codeindex-home-")
	if err != nil {
		return nil, codeErr(CodeGitError, "empty HOME: %v", err)
	}
	r := &PinnedRepo{dir: abs, home: home}
	if err := r.check(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *PinnedRepo) Close() error { return os.RemoveAll(r.home) }

func (r *PinnedRepo) check(ctx context.Context) error {
	out, err := r.git(ctx, "rev-parse", "--show-object-format")
	if err != nil {
		return gitFail(err)
	}
	switch f := strings.TrimSpace(string(out)); f {
	case "sha1":
		r.ObjectFormat, r.oidLen = f, 40
	case "sha256":
		r.ObjectFormat, r.oidLen = f, 64
	default:
		return codeErr(CodeUnsupportedRepo, "object format %q", f)
	}
	out, err = r.git(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return gitFail(err)
	}
	if strings.TrimSpace(string(out)) != "false" {
		return codeErr(CodeUnsupportedRepo, "shallow repository")
	}
	out, err = r.git(ctx, "rev-parse", "--git-path", "info/grafts")
	if err != nil {
		return gitFail(err)
	}
	grafts := strings.TrimSpace(string(out))
	if !filepath.IsAbs(grafts) {
		grafts = filepath.Join(r.dir, grafts)
	}
	if _, err := os.Lstat(grafts); err == nil {
		return codeErr(CodeUnsupportedRepo, "legacy grafts file present")
	} else if !errors.Is(err, os.ErrNotExist) {
		return codeErr(CodeGitError, "grafts: %v", err)
	}
	// config --get* exits 1 when nothing matches: that is the "no" answer.
	out, err = r.git(ctx, "config", "--get", "extensions.partialClone")
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return codeErr(CodeUnsupportedRepo, "partial clone (extensions.partialClone)")
	} else if err != nil && exitCode(err) != 1 {
		return gitFail(err)
	}
	out, err = r.git(ctx, "config", "--get-regexp", `^remote\..*\.promisor$`)
	if err == nil && len(bytes.TrimSpace(out)) != 0 {
		return codeErr(CodeUnsupportedRepo, "partial clone (promisor remote)")
	} else if err != nil && exitCode(err) != 1 {
		return gitFail(err)
	}
	out, err = r.git(ctx, "for-each-ref", "--format=%(refname)", "refs/replace/")
	if err != nil {
		return gitFail(err)
	}
	if len(bytes.TrimSpace(out)) != 0 {
		return codeErr(CodeUnsupportedRepo, "replace refs present (refs/replace/)")
	}
	return nil
}

func (r *PinnedRepo) validOID(s string) bool {
	if len(s) != r.oidLen {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (r *PinnedRepo) peelCommit(ctx context.Context, rev string) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		if exitCode(err) >= 0 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Resolve pins the request (§4.2): the branch tip is read once; sha, when
// given, must be a full canonical OID of a commit reachable from that tip
// (merged side-branch commits included, unmerged ones refused). An empty sha
// means the tip itself.
func (r *PinnedRepo) Resolve(ctx context.Context, branch, sha string) (commit string, err error) {
	if !strings.HasPrefix(branch, "refs/heads/") || len(branch) == len("refs/heads/") {
		return "", codeErr(CodeInvalidInput, "branch must be a full refs/heads/ name, got %q", branch)
	}
	tip, err := r.peelCommit(ctx, branch)
	if err != nil {
		return "", gitFail(err)
	}
	if tip == "" {
		return "", codeErr(CodeCommitUnavailable, "branch %s does not resolve to a commit", branch)
	}
	if !r.validOID(tip) {
		return "", codeErr(CodeGitError, "tip %q is not a %s OID", tip, r.ObjectFormat)
	}
	if sha == "" {
		return tip, nil
	}
	if !r.validOID(sha) {
		return "", codeErr(CodeInvalidInput, "sha must be a full lowercase %s OID (%d hex), got %q", r.ObjectFormat, r.oidLen, sha)
	}
	peeled, err := r.peelCommit(ctx, sha)
	if err != nil {
		return "", gitFail(err)
	}
	if peeled != sha {
		return "", codeErr(CodeCommitUnavailable, "%s is not a commit in this repository", sha)
	}
	if _, err := r.git(ctx, "merge-base", "--is-ancestor", sha, tip); err != nil {
		if exitCode(err) == 1 {
			return "", codeErr(CodeCommitUnavailable, "%s is not reachable from %s (%s)", sha, branch, tip)
		}
		return "", gitFail(err)
	}
	return sha, nil
}

// TreeEntry is one ls-tree record. Size is -1 for non-blobs (gitlinks).
type TreeEntry struct {
	Mode string
	Type string
	OID  string
	Size int64
	Path string
}

// Regular reports a plain file blob (not a symlink, not a submodule).
func (e TreeEntry) Regular() bool {
	return e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755")
}

// ListTree lists commit's full tree, NUL-separated so any path byte survives.
func (r *PinnedRepo) ListTree(ctx context.Context, commit string) ([]TreeEntry, error) {
	out, err := r.git(ctx, "ls-tree", "-r", "-z", "-l", "--full-tree", commit)
	if err != nil {
		return nil, gitFail(err)
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		if len(entries) == maxTreeEntries {
			return nil, codeErr(CodeLimitExceeded, "tree has more than %d entries", maxTreeEntries)
		}
		meta, p, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 4 {
			return nil, codeErr(CodeGitError, "ls-tree record %q", rec)
		}
		e := TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Size: -1, Path: string(p)}
		if f[3] != "-" {
			if e.Size, err = strconv.ParseInt(f[3], 10, 64); err != nil {
				return nil, codeErr(CodeGitError, "ls-tree size %q", f[3])
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// ReadBlobs reads the given blobs through one `cat-file --batch` process, in
// order. The size limits are checked from the tree before anything is read.
func (r *PinnedRepo) ReadBlobs(ctx context.Context, entries []TreeEntry) ([][]byte, error) {
	var total int64
	for _, e := range entries {
		if e.Size > maxFileBytes {
			return nil, codeErr(CodeLimitExceeded, "%s is %d bytes (limit %d)", e.Path, e.Size, maxFileBytes)
		}
		if total += e.Size; total > maxTotalBytes {
			return nil, codeErr(CodeLimitExceeded, "reads exceed %d bytes", maxTotalBytes)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	cmd := r.command(ctx, "cat-file", "--batch")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, codeErr(CodeGitError, "cat-file stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, codeErr(CodeGitError, "cat-file stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, codeErr(CodeGitError, "cat-file start: %v", err)
	}
	// Feed requests concurrently: git answers as it reads, so writing every
	// OID before reading could fill both pipes and deadlock.
	go func() {
		w := bufio.NewWriter(stdin)
		for _, e := range entries {
			if _, err := w.WriteString(e.OID + "\n"); err != nil {
				break
			}
		}
		w.Flush()
		stdin.Close()
	}()
	blobs, readErr := readBatch(bufio.NewReader(stdout), entries)
	if readErr != nil {
		_ = cmd.Process.Kill()
	} else {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, ctxErr(ctx, readErr)
	}
	if waitErr != nil {
		return nil, ctxErr(ctx, codeErr(CodeGitError, "cat-file: %v: %s", waitErr, strings.TrimSpace(stderr.String())))
	}
	return blobs, nil
}

func readBatch(rd *bufio.Reader, entries []TreeEntry) ([][]byte, error) {
	blobs := make([][]byte, 0, len(entries))
	for _, e := range entries {
		header, err := rd.ReadString('\n')
		if err != nil {
			return nil, codeErr(CodeGitError, "cat-file header for %s: %v", e.Path, err)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != e.OID || f[1] != "blob" {
			return nil, codeErr(CodeGitError, "cat-file answered %q for %s", strings.TrimSpace(header), e.Path)
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size != e.Size {
			return nil, codeErr(CodeGitError, "cat-file size %q for %s (tree says %d)", f[2], e.Path, e.Size)
		}
		buf := make([]byte, size+1)
		if _, err := io.ReadFull(rd, buf); err != nil || buf[size] != '\n' {
			return nil, codeErr(CodeGitError, "cat-file body for %s: %v", e.Path, err)
		}
		blobs = append(blobs, buf[:size])
	}
	return blobs, nil
}
