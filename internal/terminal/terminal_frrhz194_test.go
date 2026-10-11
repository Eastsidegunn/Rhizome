//go:build darwin || linux

package terminal

// RHZ-150 (FR-RHZ-194): a launcher -p run's stream-json log is listed and
// opened as a read-only log terminal: rendered for people (text as is, a tool
// call and its result one line each, a denial in red, the result with its
// cost), followed while the run writes, masked, and ended by the result.
// Input and resize are 409 with the read-only reason; the log is never written.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/session"
)

// fake tokens are assembled so no scanner mistakes the source for a leak
var (
	fakeAnthropic = "sk-ant-" + "api03-" + "Zx9Qw8Er7Ty6Ui5Op4As3Df2"
	fakeHF        = "hf_" + "AbCdEfGhIjKlMnOpQrSt1234"
	fakeGitHub    = "ghp_" + "0123456789abcdefABCDEF0123456789abcd"
	fakeBlob      = "QWxhZGRpbjpvcGVuIHNlc2FtZQ9" + "xYz12ABcdEFgh34IJklMN"
)

const (
	initLine   = `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5","cwd":"/work","tools":["Bash"]}`
	resultLine = `{"type":"result","subtype":"success","is_error":false,"result":"끝냈다\n둘째 줄","total_cost_usd":0.1234,"num_turns":3,"duration_ms":4200,"permission_denials":[]}`
)

func assistantText(s string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}})
	return string(b)
}

func toolUse(name string, input map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tu1", "name": name, "input": input}}}})
	return string(b)
}

func toolResult(content any, isError bool) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": content, "is_error": isError}}}})
	return string(b)
}

func render(t *testing.T, line string) string {
	t.Helper()
	out, _, _ := RenderLine([]byte(line))
	return string(out)
}

func TestRenderLineFRRHZ194(t *testing.T) {
	if got := render(t, initLine); !strings.Contains(got, "세션 시작") || !strings.Contains(got, "claude-opus-5-5") {
		t.Fatalf("init: %q", got)
	}
	if got := render(t, assistantText("한 줄\n두 줄")); got != "한 줄\r\n두 줄\r\n" {
		t.Fatalf("text: %q", got)
	}
	long := "echo " + strings.Repeat("a", 200)
	got := render(t, toolUse("Bash", map[string]any{"command": long}))
	if !strings.Contains(got, "▶ Bash: echo aaa") || !strings.Contains(got, "…") || strings.Contains(got, strings.Repeat("a", 121)) {
		t.Fatalf("bash: %q", got)
	}
	if got := render(t, toolUse("Edit", map[string]any{"file_path": "/work/a.go", "old_string": "x"})); !strings.Contains(got, "✎ Edit /work/a.go") {
		t.Fatalf("edit: %q", got)
	}
	if got := render(t, toolResult("\nok first\nsecond", false)); !strings.Contains(got, "  ⎿ ok first") || strings.Contains(got, "second") {
		t.Fatalf("result: %q", got)
	}
	if got := render(t, toolResult([]any{map[string]any{"type": "text", "text": "blocks first"}}, false)); !strings.Contains(got, "  ⎿ blocks first") {
		t.Fatalf("result blocks: %q", got)
	}
	denial := render(t, toolResult("Claude requested permissions to use Bash, but you haven't granted it yet.", true))
	if !strings.Contains(denial, "⛔ 권한 거부") || !strings.Contains(denial, ansiRed) {
		t.Fatalf("denial: %q", denial)
	}
	out, final, code := RenderLine([]byte(resultLine))
	if !final || code != 0 || !strings.Contains(string(out), "■ 결과(success): 끝냈다") || !strings.Contains(string(out), "$0.1234") || !strings.Contains(string(out), "3턴") || strings.Contains(string(out), "둘째") {
		t.Fatalf("result: %q %v %d", out, final, code)
	}
	out, final, code = RenderLine([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"permission_denials":[{"tool_name":"Bash"}]}`))
	if !final || code != 1 || !strings.Contains(string(out), "권한 거부 1건") {
		t.Fatalf("error result: %q %v %d", out, final, code)
	}
	for _, junk := range []string{"not json", `{"type":"stream_event"}`, `{"type":"user","message":{"content":"plain prompt"}}`} {
		if out, final, _ := RenderLine([]byte(junk)); out != nil || final {
			t.Fatalf("junk %q rendered %q", junk, out)
		}
	}
}

func TestMaskFRRHZ194(t *testing.T) {
	in := strings.Join([]string{fakeAnthropic, fakeHF, fakeGitHub, fakeBlob, "Authorization: Bearer abcDEF123456ghiJKL", "API_KEY=hunter2hunter2"}, " ")
	got := Mask(in)
	for _, s := range []string{fakeAnthropic, fakeHF, fakeGitHub, fakeBlob, "abcDEF123456ghiJKL", "hunter2hunter2"} {
		if strings.Contains(got, s) {
			t.Fatalf("not masked %q in %q", s, got)
		}
	}
	for _, p := range []string{"sk-ant-****", "hf_****", "ghp_****", "API_KEY=****"} {
		if !strings.Contains(got, p) {
			t.Fatalf("want %q in %q", p, got)
		}
	}
	// readable things stay: a long path, a git hash, prose
	keep := "/Users/me/Gunnsplayground/Rhizome/internal/terminal/terminal_frrhz184_test.go 7a49333c0ffee1234567890abcdef0123456789a max tokens used"
	if Mask(keep) != keep {
		t.Fatalf("over-masked: %q", Mask(keep))
	}
}

type logFixture struct {
	store *events.Store
	h     *Host
	url   string
	srv   *httptest.Server
	dir   string
	cwd   string
}

func newLogFixture(t *testing.T) *logFixture {
	store := &events.Store{}
	h, srv := newHost(t, store, "")
	f := &logFixture{store: store, h: h, url: srv.URL, srv: srv, dir: t.TempDir(), cwd: t.TempDir()}
	h.LogDir = f.dir
	return f
}

// run records a launcher -p session of an execution with a log body.
func (f *logFixture) run(t *testing.T, exec, sid string, open bool, lines ...string) string {
	t.Helper()
	svc := session.Service{Store: f.store}
	if _, err := svc.Open(session.Opened{SessionID: sid, Name: "m1", MissionID: "m1", Mode: session.Background, Cwd: f.cwd, ExecutionID: exec}); err != nil {
		t.Fatal(err)
	}
	if !open {
		if _, err := svc.Close(sid, "succeeded"); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(f.dir, exec+".ndjson")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *logFixture) open(t *testing.T, exec string) (int, string) {
	return do(t, http.MethodPost, f.url+"/v1/terminals", `{"target":{"execution":{"executionId":"`+exec+`"}},"cols":80,"rows":24}`)
}

func appendLines(t *testing.T, p string, lines ...string) {
	t.Helper()
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		fh.WriteString(l + "\n")
	}
}

func TestLogTerminalFRRHZ194(t *testing.T) {
	f := newLogFixture(t)
	p := f.run(t, "exec-live", "uuid-live", true, initLine, assistantText("키는 "+fakeAnthropic+" 이다"))
	f.run(t, "exec-old", "uuid-old", false, initLine, resultLine)
	old := time.Now().Add(-2 * RecentLogs)
	os.Chtimes(filepath.Join(f.dir, "exec-old.ndjson"), old, old)
	// an interactive or observed session is no log target
	if _, err := (session.Service{Store: f.store}).Open(session.Opened{SessionID: "uuid-bg", Name: "bg", Mode: session.Interactive, Cwd: f.cwd, ExecutionID: "exec-bg", Kind: session.KindBackground}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.dir, "exec-bg.ndjson"), nil, 0o600)

	code, out := do(t, http.MethodGet, f.url+"/v1/terminals", "")
	var l listDTO
	if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil || len(l.Logs) != 1 {
		t.Fatalf("list: %d %s", code, out)
	}
	if lg := l.Logs[0]; lg.ExecutionID != "exec-live" || lg.SessionID != "uuid-live" || lg.MissionID != "m1" || lg.Name != "m1" || !lg.Running {
		t.Fatalf("log target %+v", lg)
	}

	before, _ := os.ReadFile(p)
	code, out = f.open(t, "exec-live")
	if code != http.StatusCreated {
		t.Fatalf("open: %d %s", code, out)
	}
	var r struct{ TerminalID string }
	json.Unmarshal([]byte(out), &r)
	s := subscribe(t, f.srv, r.TerminalID)
	s.waitFor(t, "키는 sk-ant-****")

	for _, op := range []struct{ path, body string }{{"input", `{"data":"eA=="}`}, {"resize", `{"cols":100,"rows":30}`}} {
		code, out := do(t, http.MethodPost, f.url+"/v1/terminals/"+r.TerminalID+"/"+op.path, op.body)
		if code != http.StatusConflict || !strings.Contains(out, "읽기 전용") {
			t.Fatalf("%s: %d %s", op.path, code, out)
		}
	}
	code, out = do(t, http.MethodGet, f.url+"/v1/terminals", "")
	if json.Unmarshal([]byte(out), &l); len(l.Terminals) != 1 || l.Terminals[0].Kind != KindLog || l.Terminals[0].ExecutionID != "exec-live" {
		t.Fatalf("live terminals: %s", out)
	}

	// the tail follows lines written after the open, then the result ends it
	appendLines(t, p, toolUse("Bash", map[string]any{"command": "export HF=" + fakeHF}), toolResult("ok", false))
	s.waitFor(t, "▶ Bash: export HF=hf_****")
	s.waitFor(t, "  ⎿ ok")
	appendLines(t, p, resultLine)
	if c := s.waitExit(t); c != 0 {
		t.Fatalf("exit %d", c)
	}
	text := s.String()
	if !strings.Contains(text, "$0.1234") || strings.Contains(text, fakeAnthropic) || strings.Contains(text, fakeHF) {
		t.Fatalf("stream: %q", text)
	}
	after, _ := os.ReadFile(p)
	if !bytes.HasPrefix(after, before) || !strings.Contains(string(after), fakeAnthropic) {
		t.Fatal("log file changed")
	}
	closed := false
	for _, e := range f.store.All() {
		if e.AggregateType == AggregateType && e.AggregateID == r.TerminalID && e.Type == ClosedType {
			closed = true
		}
		if e.AggregateType == AggregateType && bytes.Contains(e.Payload, []byte("sk-ant")) {
			t.Fatal("terminal bytes in the journal")
		}
	}
	if !closed {
		t.Fatal("terminal.closed not recorded")
	}

	// a finished run without a result drains and exits 1
	f.run(t, "exec-cut", "uuid-cut", false, initLine, assistantText("중간"))
	code, out = f.open(t, "exec-cut")
	json.Unmarshal([]byte(out), &r)
	if code != http.StatusCreated {
		t.Fatalf("open cut: %d %s", code, out)
	}
	s = subscribe(t, f.srv, r.TerminalID)
	if c := s.waitExit(t); c != 1 || !strings.Contains(s.String(), "중간") || !strings.Contains(s.String(), "결과 기록 없음") {
		t.Fatalf("cut: %d %q", c, s.String())
	}

	// unknown or malformed execution 404, no writes
	n := f.store.Len()
	for _, id := range []string{"exec-none", "..", "exec-bg"} {
		if code, _ := f.open(t, id); code != http.StatusNotFound || f.store.Len() != n {
			t.Fatalf("%s: %d", id, code)
		}
	}
	// no launcher logDir: no log targets, open 409
	f.h.LogDir = ""
	if code, out := do(t, http.MethodGet, f.url+"/v1/terminals", ""); json.Unmarshal([]byte(out), &l) != nil || code != http.StatusOK || len(l.Logs) != 0 {
		t.Fatalf("list without logDir: %s", out)
	}
	if code, _ := f.open(t, "exec-live"); code != http.StatusConflict {
		t.Fatalf("open without logDir: %d", code)
	}
}

func TestLogTerminalCloseFRRHZ194(t *testing.T) {
	f := newLogFixture(t)
	f.run(t, "exec-a", "uuid-a", true, initLine)
	code, out := f.open(t, "exec-a")
	if code != http.StatusCreated {
		t.Fatalf("open: %d %s", code, out)
	}
	var r struct{ TerminalID string }
	json.Unmarshal([]byte(out), &r)
	s := subscribe(t, f.srv, r.TerminalID)
	s.waitFor(t, "세션 시작")
	if code, _ := do(t, http.MethodDelete, f.url+"/v1/terminals/"+r.TerminalID, ""); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if c := s.waitExit(t); c != 0 {
		t.Fatalf("viewer close exit %d", c)
	}
}
