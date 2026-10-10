// Package sessionlauncher starts one local `claude -p` session per
// mission.start (RHZ-124 S1, FR-RHZ-124-S1). It is the second
// execution backend beside janusadapter: same seam (Prepare/Start), same
// kernel sequence (intent → dispatch_claimed → accepted, durable before the
// child is contacted), zero writes on every rejection.
//
// Operating defaults and the policy ceiling come from one strict JSON ledger
// (-session-launcher-config). Changing it is a governance act: edit the file
// and restart serve — never an intent. The ledger carries no secrets.
//
// Layer: exec. This package never imports the knowledge kernel or the
// surface; the usage note is written by the composition root through Report.
package sessionlauncher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Ceiling is the configured per-execution ceiling: every effective policy is
// the minimum of this and the request.
type Ceiling struct {
	USD    float64 `json:"usd"`
	TimeMs int64   `json:"timeMs"`
}

// Config is the operator-owned session-launcher ledger. Unknown keys are
// rejected on load so nothing undeclared (a credential) can ride in.
type Config struct {
	ClaudePath     string            `json:"claudePath"`
	Workdirs       map[string]string `json:"workdirs"`
	DefaultWorkdir string            `json:"defaultWorkdir"`
	Model          string            `json:"model"`
	PermissionMode string            `json:"permissionMode"`
	AllowedTools   []string          `json:"allowedTools"`
	LogDir         string            `json:"logDir"`
	MaxConcurrent  int               `json:"maxConcurrent"`
	Ceiling        Ceiling           `json:"ceiling"`

	digest string
}

// permissionModes is the accepted --permission-mode vocabulary.
// bypassPermissions is deliberately absent: an unattended session must never
// skip the harness's own permission checks (rejected explicitly on load).
// The set is the installed CLI's --permission-mode choices (claude 2.1.x);
// "default" is not one of them.
var permissionModes = map[string]bool{"acceptEdits": true, "auto": true, "manual": true, "dontAsk": true, "plan": true}

// MaxTimeMs caps ceiling.timeMs at one day.
const MaxTimeMs = 24 * 60 * 60 * 1000

// writableByOthers reports a file that a group or other user could rewrite.
func writableByOthers(fi os.FileInfo) bool { return fi.Mode().Perm()&0o022 != 0 }

// Digest is the canonical ledger digest ("sha256:<hex>") journaled as the
// intent's ExecConfigDigest: the validated struct (nil lists as []) is
// marshalled, decoded into a generic map and re-marshalled, so key order and
// whitespace in the file never change it while any value change does.
func (c Config) Digest() string {
	if c.digest != "" {
		return c.digest
	}
	return canonicalDigest(c)
}

func canonicalDigest(c Config) string {
	c.digest = ""
	if c.AllowedTools == nil {
		c.AllowedTools = []string{}
	}
	if c.Workdirs == nil {
		c.Workdirs = map[string]string{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // strings, ints, floats from a validated file: unreachable
	}
	var m any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		panic(err)
	}
	if b, err = json.Marshal(m); err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// Parse decodes and validates the ledger bytes (strict: unknown fields and
// trailing data fail).
func Parse(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("session launcher config: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("session launcher config: trailing data")
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("session launcher config: %w", err)
	}
	c.digest = canonicalDigest(c)
	return c, nil
}

// Load reads and validates the ledger file.
func Load(path string) (Config, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Config{}, fmt.Errorf("session launcher config: %w", err)
	}
	if writableByOthers(fi) {
		return Config{}, fmt.Errorf("session launcher config: file is group- or world-writable")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("session launcher config: %w", err)
	}
	return Parse(b)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (c *Config) validate() error {
	if !filepath.IsAbs(c.ClaudePath) {
		return fmt.Errorf("claudePath must be an absolute path")
	}
	target, err := filepath.EvalSymlinks(c.ClaudePath)
	if err != nil {
		return fmt.Errorf("claudePath must be an executable file")
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("claudePath must be an executable file")
	}
	if writableByOthers(fi) {
		return fmt.Errorf("claudePath target is group- or world-writable")
	}
	if len(c.Workdirs) == 0 {
		return fmt.Errorf("workdirs required")
	}
	for name, dir := range c.Workdirs {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("workdirs: empty name")
		}
		if !filepath.IsAbs(dir) || !isDir(dir) {
			return fmt.Errorf("workdirs.%s must be an absolute existing directory", name)
		}
	}
	if _, ok := c.Workdirs[c.DefaultWorkdir]; !ok {
		return fmt.Errorf("defaultWorkdir must name a workdirs entry")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("model required")
	}
	if c.PermissionMode == "bypassPermissions" {
		return fmt.Errorf("permissionMode bypassPermissions is not allowed")
	}
	if !permissionModes[c.PermissionMode] {
		return fmt.Errorf("unsupported permissionMode %q", c.PermissionMode)
	}
	for _, t := range c.AllowedTools {
		if strings.TrimSpace(t) == "" || strings.HasPrefix(t, "-") {
			return fmt.Errorf("allowedTools: invalid entry %q", t)
		}
	}
	if !filepath.IsAbs(c.LogDir) || !isDir(c.LogDir) {
		return fmt.Errorf("logDir must be an absolute existing directory")
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("maxConcurrent must be >= 1")
	}
	if _, err := usdCents("ceiling.usd", c.Ceiling.USD); err != nil {
		return err
	}
	if c.Ceiling.TimeMs <= 0 || c.Ceiling.TimeMs > MaxTimeMs {
		return fmt.Errorf("ceiling.timeMs must be in 1..%d", MaxTimeMs)
	}
	return nil
}

// usdCents converts a dollar amount to integer cents (the policy Budget
// unit). It refuses non-positive amounts and sub-cent precision, so the
// journaled number is exactly what the operator wrote.
func usdCents(name string, usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 {
		return 0, fmt.Errorf("%s must be > 0", name)
	}
	c := math.Round(usd * 100)
	if math.Abs(usd*100-c) > 1e-6 {
		return 0, fmt.Errorf("%s must have at most 2 decimals", name)
	}
	if c < 1 || c > math.MaxInt32 {
		return 0, fmt.Errorf("%s out of range", name)
	}
	return int64(c), nil
}

// formatUSD renders cents as the --max-budget-usd argument ("5.00").
func formatUSD(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
