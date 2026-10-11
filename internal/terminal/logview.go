package terminal

// RHZ-150 (FR-RHZ-194): a launcher run's stream-json log rendered for people.
// A log terminal follows <logDir>/<executionId>.ndjson and publishes one
// rendered line per event: assistant text as is, a tool call as one line, a
// tool result as its first line, a permission denial in red, and the result as
// a summary with its cost before the exit. Token-shaped strings are masked in
// the rendering only; the log file is never written.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	clip = 120

	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

var (
	// prefixed provider tokens keep their prefix so a reader sees what was hidden
	prefixToken = regexp.MustCompile(`\b(sk-ant-|sk-proj-|sk-|sk_live_|sk_test_|rk_live_|pk_live_|hf_|ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|glpat-|xox[abprs]-|npm_|AKIA|ASIA|AIza)[A-Za-z0-9_\-]{8,}`)
	jwtToken    = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{4,}`)
	bearer      = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/\-]{12,}=*`)
	keyValue    = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:api[_-]?key|token|secret|password|passwd|credential)[A-Za-z0-9_]*)(\s*[:=]\s*["']?)([^\s"',;]{8,})`)
	longRun     = regexp.MustCompile(`[A-Za-z0-9+/_\-]{32,}={0,2}`)
)

// Mask hides token-shaped strings: provider prefixes (sk-, hf_, ghp_ …), JWTs,
// bearer values, key=value secrets and long mixed-case base64-like runs.
func Mask(s string) string {
	s = prefixToken.ReplaceAllString(s, "${1}****")
	s = jwtToken.ReplaceAllString(s, "eyJ****")
	s = bearer.ReplaceAllString(s, "${1} ****")
	s = keyValue.ReplaceAllString(s, "${1}${2}****")
	return longRun.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasSuffix(m, "****") || !secretShaped(m) {
			return m
		}
		return "****"
	})
}

// secretShaped: digits, upper and lower case together and not a path (a git
// hash or a long file path stays readable).
func secretShaped(m string) bool {
	if strings.Count(m, "/") > 1 {
		return false
	}
	var d, u, l bool
	for _, r := range m {
		switch {
		case r >= '0' && r <= '9':
			d = true
		case r >= 'A' && r <= 'Z':
			u = true
		case r >= 'a' && r <= 'z':
			l = true
		}
	}
	return d && u && l
}

// one is s as a single line clipped to n runes.
func one(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// firstLine is the first non-empty line of s, clipped.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return one(l, clip)
		}
	}
	return ""
}

type logEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Cwd     string `json:"cwd"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	TotalCostUSD      *float64          `json:"total_cost_usd"`
	NumTurns          *int              `json:"num_turns"`
	DurationMs        *int64            `json:"duration_ms"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func blocks(raw json.RawMessage) []block {
	var bs []block
	if json.Unmarshal(raw, &bs) == nil {
		return bs
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

// resultText is a tool_result content: a string or text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// toolLine is a tool call as one line.
func toolLine(name string, raw json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	if name == "Bash" {
		return "▶ Bash: " + one(str("command"), clip)
	}
	if editTools[name] {
		p := str("file_path")
		if p == "" {
			p = str("notebook_path")
		}
		return "✎ " + name + " " + one(p, clip)
	}
	for _, k := range []string{"file_path", "pattern", "url", "query", "description", "prompt"} {
		if v := str(k); v != "" {
			return "▶ " + name + ": " + one(v, clip)
		}
	}
	if len(in) == 0 {
		return "▶ " + name
	}
	return "▶ " + name + ": " + one(string(raw), clip)
}

func denied(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "permission") && (strings.Contains(l, "denied") || strings.Contains(l, "requested") || strings.Contains(l, "haven't granted") || strings.Contains(l, "not allowed"))
}

// crlf turns text into terminal lines.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
}

// RenderLine renders one stream-json line. final is true for the result event,
// with the exit code (0 success, 1 error). An unknown or unparseable line
// renders as nothing. The output is masked.
func RenderLine(line []byte) (out []byte, final bool, code int) {
	var e logEvent
	if json.Unmarshal(line, &e) != nil {
		return nil, false, 0
	}
	var b strings.Builder
	switch e.Type {
	case "system":
		if e.Subtype == "init" {
			b.WriteString(ansiDim + "● 세션 시작" + optional(" · ", e.Model) + optional(" · ", e.Cwd) + ansiReset + "\r\n")
		}
	case "assistant":
		for _, c := range blocks(e.Message.Content) {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					b.WriteString(crlf(strings.TrimRight(c.Text, "\n")))
				}
			case "tool_use":
				b.WriteString(ansiBold + toolLine(c.Name, c.Input) + ansiReset + "\r\n")
			}
		}
	case "user":
		for _, c := range blocks(e.Message.Content) {
			if c.Type != "tool_result" {
				continue
			}
			text := resultText(c.Content)
			switch {
			case c.IsError && denied(text):
				b.WriteString(ansiRed + ansiBold + "  ⛔ 권한 거부: " + firstLine(text) + ansiReset + "\r\n")
			case c.IsError:
				b.WriteString(ansiRed + "  ⎿ ✗ " + firstLine(text) + ansiReset + "\r\n")
			default:
				b.WriteString(ansiDim + "  ⎿ " + firstLine(text) + ansiReset + "\r\n")
			}
		}
	case "result":
		final = true
		if e.IsError || (e.Subtype != "" && e.Subtype != "success") {
			code = 1
		}
		if n := len(e.PermissionDenials); n > 0 {
			b.WriteString(ansiRed + ansiBold + fmt.Sprintf("⛔ 권한 거부 %d건", n) + ansiReset + "\r\n")
		}
		sum := "■ 결과(" + orDefault(e.Subtype, "result") + ")"
		if r := firstLine(e.Result); r != "" {
			sum += ": " + r
		}
		if e.TotalCostUSD != nil {
			sum += fmt.Sprintf(" · $%.4f", *e.TotalCostUSD)
		}
		if e.NumTurns != nil {
			sum += fmt.Sprintf(" · %d턴", *e.NumTurns)
		}
		if e.DurationMs != nil {
			sum += fmt.Sprintf(" · %ds", *e.DurationMs/1000)
		}
		b.WriteString(ansiBold + sum + ansiReset + "\r\n")
	}
	if b.Len() == 0 {
		return nil, final, code
	}
	return []byte(Mask(b.String())), final, code
}

func optional(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
