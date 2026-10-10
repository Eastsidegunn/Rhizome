package main

// RHZ-131 (FR-RHZ-172): `rhizome agent <verb>` — agent-facing board CLI.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/workspace"
)

// fakeBoard172 records every request and answers intents by kind.
type fakeBoard172 struct {
	mu       sync.Mutex
	intents  []map[string]any
	gets     []string
	reject   map[string]string // kind -> Reason
	status   map[string]int    // kind -> HTTP status
	contexts map[string]string // raw query -> JSON body
}

func newFakeBoard172(t *testing.T) (*fakeBoard172, *httptest.Server) {
	t.Helper()
	fb := &fakeBoard172{reject: map[string]string{}, status: map[string]int{}, contexts: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/intent":
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("FR-RHZ-172: invalid intent json %q", raw)
			}
			fb.intents = append(fb.intents, m)
			kind, _ := m["kind"].(string)
			if st := fb.status[kind]; st != 0 {
				http.Error(w, "boom", st)
				return
			}
			if reason, ok := fb.reject[kind]; ok {
				json.NewEncoder(w).Encode(map[string]any{"Accepted": false, "Reason": reason})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"Accepted": true, "Reason": ""})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/context":
			fb.gets = append(fb.gets, r.URL.RawQuery)
			body, ok := fb.contexts[r.URL.RawQuery]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return fb, srv
}

func (fb *fakeBoard172) kinds() []string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	var out []string
	for _, m := range fb.intents {
		out = append(out, m["kind"].(string))
	}
	return out
}

func (fb *fakeBoard172) intent(i int) map[string]any {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.intents[i]
}

var fixedNow172 = time.Date(2026, 10, 10, 8, 30, 0, 0, time.FixedZone("KST", 9*3600))

// trailer172 is the note trailer for actor sess-a at fixedNow172 (UTC).
func trailer172(mission string) string {
	return "\n\nactor=sess-a mission=" + mission + " at=2026-10-09T23:30:00Z"
}

func deps172(env map[string]string, files map[string]string) agentDeps {
	return agentDeps{
		getenv: func(k string) string { return env[k] },
		now:    func() time.Time { return fixedNow172 },
		client: &http.Client{Timeout: 10 * time.Second},
		readFile: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, errors.New("no such file")
		},
	}
}

func run172(t *testing.T, d agentDeps, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runAgent(args, &out, &errOut, d)
	return code, out.String(), errOut.String()
}

func env172(board string) map[string]string {
	return map[string]string{"RHIZOME_BOARD": board, "RHIZOME_SESSION": "sess-a"}
}

func jsonEq172(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("FR-RHZ-172 intent\n got: %s\nwant: %s", gb, wb)
	}
}

func taskBundle172(name, state, prompt string, memories string) string {
	return fmt.Sprintf(`{"task":{"id":"mission-x","name":%q,"state":%q,"prompt":%q,"hasProgress":false},"goal":{"id":"goal-g","description":"the goal","state":"active"},"memories":%s,"knowledge":[],"relations":[],"steps":[]}`, name, state, prompt, memories)
}

// ---- progress ----------------------------------------------------------------

func TestFRRHZ172_ProgressSendsFieldsAndActor(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	code, out, _ := run172(t, deps172(env172(srv.URL), nil), "progress", "m-0000abcd", "--action", "테스트 작성 중", "--pct", "40")
	if code != 0 || out != "ok\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	jsonEq172(t, fb.intent(0), map[string]any{"actor": "sess-a", "currentAction": "테스트 작성 중", "kind": "mission.progress", "missionId": "m-0000abcd", "progress": 0.4})
}

func TestFRRHZ172_ProgressAbsentPctOmitsKey(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "progress", "m-0000abcd", "--action", "a"); code != 0 {
		t.Fatal(code)
	}
	if _, ok := fb.intent(0)["progress"]; ok {
		t.Fatalf("FR-RHZ-172: absent --pct must omit progress: %v", fb.intent(0))
	}
	// --pct 0 is a real 0, distinct from absent.
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "progress", "m-0000abcd", "--action", "a", "--pct", "0"); code != 0 {
		t.Fatal(code)
	}
	if v, ok := fb.intent(1)["progress"]; !ok || v != 0.0 {
		t.Fatalf("FR-RHZ-172: --pct 0 must send 0: %v", fb.intent(1))
	}
}

func TestFRRHZ172_ProgressUsageErrors(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	for _, args := range [][]string{
		{"progress", "m-0000abcd", "--action", "a", "--pct", "101"},
		{"progress", "m-0000abcd", "--action", "a", "--pct", "-1"},
		{"progress", "m-0000abcd", "--action", "a", "--pct", "x"},
		{"progress", "m-0000abcd"},
	} {
		if code, _, _ := run172(t, deps172(env172(srv.URL), nil), args...); code != 2 {
			t.Fatalf("FR-RHZ-172 %v: code %d, want 2", args, code)
		}
	}
	if n := len(fb.kinds()); n != 0 {
		t.Fatalf("FR-RHZ-172: usage errors must send nothing, sent %d", n)
	}
}

// ---- common: actor, board, mission, rejection --------------------------------

func TestFRRHZ172_ActorFromEnvFlagOverridesMissingIsUsage(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	env := env172(srv.URL)
	if code, _, _ := run172(t, deps172(env, nil), "progress", "m-0000abcd", "--action", "a"); code != 0 {
		t.Fatal(code)
	}
	if fb.intent(0)["actor"] != "sess-a" {
		t.Fatalf("FR-RHZ-172 env actor: %v", fb.intent(0))
	}
	if code, _, _ := run172(t, deps172(env, nil), "progress", "--actor", "flag-b", "m-0000abcd", "--action", "a"); code != 0 {
		t.Fatal(code)
	}
	if fb.intent(1)["actor"] != "flag-b" {
		t.Fatalf("FR-RHZ-172 flag actor: %v", fb.intent(1))
	}
	delete(env, "RHIZOME_SESSION")
	code, _, stderr := run172(t, deps172(env, nil), "progress", "m-0000abcd", "--action", "a")
	if code != 2 || !strings.Contains(stderr, "actor required: set RHIZOME_SESSION or --actor") {
		t.Fatalf("FR-RHZ-172 missing actor: code=%d stderr=%q", code, stderr)
	}
	if len(fb.kinds()) != 2 {
		t.Fatalf("FR-RHZ-172: missing actor must send nothing")
	}
}

func TestFRRHZ172_BoardDefaultEnvFlag(t *testing.T) {
	fbEnv, srvEnv := newFakeBoard172(t)
	fbFlag, srvFlag := newFakeBoard172(t)
	env := env172(srvEnv.URL)
	if code, _, _ := run172(t, deps172(env, nil), "progress", "m-0000abcd", "--action", "a"); code != 0 || len(fbEnv.kinds()) != 1 {
		t.Fatalf("FR-RHZ-172 env board: code=%d", code)
	}
	if code, _, _ := run172(t, deps172(env, nil), "progress", "m-0000abcd", "--action", "a", "--board", srvFlag.URL+"/"); code != 0 || len(fbFlag.kinds()) != 1 || len(fbEnv.kinds()) != 1 {
		t.Fatalf("FR-RHZ-172 flag board must override env: code=%d", code)
	}
	// Default board: capture the URL the client is asked to reach.
	var hit string
	d := deps172(map[string]string{"RHIZOME_SESSION": "s"}, nil)
	d.client = &http.Client{Transport: roundTrip172(func(r *http.Request) (*http.Response, error) {
		hit = r.URL.String()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"Accepted":true}`)), Header: http.Header{}}, nil
	})}
	if code, _, _ := run172(t, d, "progress", "m-0000abcd", "--action", "a"); code != 0 || hit != "http://127.0.0.1:8790/v1/intent" {
		t.Fatalf("FR-RHZ-172 default board: code=%d hit=%q", code, hit)
	}
}

type roundTrip172 func(*http.Request) (*http.Response, error)

func (f roundTrip172) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFRRHZ172_MissionFromEnvAndFlag(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	env := env172(srv.URL)
	env["RHIZOME_MISSION"] = "m-env00000"
	if code, _, _ := run172(t, deps172(env, nil), "blocked", "한 줄"); code != 0 {
		t.Fatal(code)
	}
	if fb.intent(0)["missionId"] != "m-env00000" || fb.intent(0)["content"] != "한 줄"+trailer172("m-env00000") {
		t.Fatalf("FR-RHZ-172 env mission: %v", fb.intent(0))
	}
	if code, _, _ := run172(t, deps172(env, nil), "note", "--mission", "m-flag0000", "line"); code != 0 {
		t.Fatal(code)
	}
	if fb.intent(2)["missionId"] != "m-flag0000" {
		t.Fatalf("FR-RHZ-172 flag mission: %v", fb.intent(2))
	}
	delete(env, "RHIZOME_MISSION")
	if code, _, _ := run172(t, deps172(env, nil), "note", "line"); code != 2 {
		t.Fatalf("FR-RHZ-172 missing mission: code %d", code)
	}
	if code, _, _ := run172(t, deps172(env, nil), "bogus", "m-0000abcd"); code != 2 {
		t.Fatalf("FR-RHZ-172 unknown verb: code %d", code)
	}
}

func TestFRRHZ172_RejectionReasonVerbatimExit1(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	reason := "queued에서 진척 기록 불가: task.resume 먼저"
	fb.reject["mission.progress"] = reason
	code, out, stderr := run172(t, deps172(env172(srv.URL), nil), "progress", "m-0000abcd", "--action", "a")
	if code != 1 || out != "" || stderr != "rejected: "+reason+"\n" {
		t.Fatalf("FR-RHZ-172 rejection: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestFRRHZ172_HTTP500AndNetworkErrorExit1(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	fb.status["note.create"] = 500
	code, _, stderr := run172(t, deps172(env172(srv.URL), nil), "note", "m-0000abcd", "x")
	if code != 1 || !strings.Contains(stderr, "HTTP 500") || !strings.Contains(stderr, "boom") {
		t.Fatalf("FR-RHZ-172 500: code=%d stderr=%q", code, stderr)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if code, _, _ := run172(t, deps172(env172(dead.URL), nil), "note", "m-0000abcd", "x"); code != 1 {
		t.Fatalf("FR-RHZ-172 network error: code %d", code)
	}
}

// ---- blocked / done / note ---------------------------------------------------

func TestFRRHZ172_BlockedNoteThenProgress(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	files := map[string]string{"detail.md": "자세한 내용\n둘째 줄"}
	code, out, _ := run172(t, deps172(env172(srv.URL), files), "blocked", "m-0000abcd", "CI 대기 중", "--body-file", "detail.md", "--tag", "RHZ-131", "--tag", "ci")
	if code != 0 || out != "note: ok\nprogress: ok\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if got := fb.kinds(); strings.Join(got, ",") != "note.create,mission.progress" {
		t.Fatalf("FR-RHZ-172 order: %v", got)
	}
	jsonEq172(t, fb.intent(0), map[string]any{"actor": "sess-a", "content": "CI 대기 중\n\n자세한 내용\n둘째 줄" + trailer172("m-0000abcd"), "kind": "note.create", "memoryKind": "blocked", "missionId": "m-0000abcd", "tags": []string{"blocked", "RHZ-131", "ci"}})
	jsonEq172(t, fb.intent(1), map[string]any{"actor": "sess-a", "blockedReason": "CI 대기 중", "kind": "mission.progress", "missionId": "m-0000abcd"})
}

func TestFRRHZ172_BlockedRejectedNoteSendsNoProgress(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	fb.reject["note.create"] = "content exceeds 16KiB limit"
	code, _, stderr := run172(t, deps172(env172(srv.URL), nil), "blocked", "m-0000abcd", "x")
	if code != 1 || stderr != "rejected: content exceeds 16KiB limit\n" {
		t.Fatalf("FR-RHZ-172: code=%d stderr=%q", code, stderr)
	}
	if got := fb.kinds(); len(got) != 1 {
		t.Fatalf("FR-RHZ-172: rejected note must not be followed by progress: %v", got)
	}
}

func TestFRRHZ172_DoneNeverCompletes(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	code, _, _ := run172(t, deps172(env172(srv.URL), nil), "done", "m-0000abcd", "구현 완료", "--tag", "RHZ-131")
	if code != 0 {
		t.Fatal(code)
	}
	if got := fb.kinds(); strings.Join(got, ",") != "note.create,mission.progress" {
		t.Fatalf("FR-RHZ-172 done must send exactly note+progress (never complete/fail): %v", got)
	}
	jsonEq172(t, fb.intent(0), map[string]any{"actor": "sess-a", "content": "구현 완료" + trailer172("m-0000abcd"), "kind": "note.create", "memoryKind": "done", "missionId": "m-0000abcd", "tags": []string{"done", "RHZ-131"}})
	jsonEq172(t, fb.intent(1), map[string]any{"actor": "sess-a", "currentAction": "done: 구현 완료", "kind": "mission.progress", "missionId": "m-0000abcd", "progress": 1.0})
}

func TestFRRHZ172_DoneProgressRejectedIsWarningExit0(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	fb.reject["mission.progress"] = "queued에서 진척 기록 불가: task.resume 먼저"
	code, out, stderr := run172(t, deps172(env172(srv.URL), nil), "done", "m-0000abcd", "x")
	if code != 0 || out != "note: ok\n" || stderr != "warning: progress not recorded: rejected: queued에서 진척 기록 불가: task.resume 먼저\n" {
		t.Fatalf("FR-RHZ-172: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestFRRHZ172_NoteKindDefaultAndOverride(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "note", "m-0000abcd", "관찰"); code != 0 {
		t.Fatal(code)
	}
	jsonEq172(t, fb.intent(0), map[string]any{"actor": "sess-a", "content": "관찰" + trailer172("m-0000abcd"), "kind": "note.create", "memoryKind": "observation", "missionId": "m-0000abcd", "tags": []string{}})
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "note", "m-0000abcd", "넘김", "--kind", "handoff", "--tag", "handoff"); code != 0 {
		t.Fatal(code)
	}
	jsonEq172(t, fb.intent(1), map[string]any{"actor": "sess-a", "content": "넘김" + trailer172("m-0000abcd"), "kind": "note.create", "memoryKind": "handoff", "missionId": "m-0000abcd", "tags": []string{"handoff"}})
	if len(fb.kinds()) != 2 {
		t.Fatalf("FR-RHZ-172: note sends no progress: %v", fb.kinds())
	}
	// A multi-line summary or an unreadable body file is a usage error.
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "note", "m-0000abcd", "a\nb"); code != 2 {
		t.Fatalf("FR-RHZ-172 multi-line: %d", code)
	}
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "note", "m-0000abcd", "a", "--body-file", "missing"); code != 2 {
		t.Fatalf("FR-RHZ-172 missing body file: %d", code)
	}
}

// UTF-8 bytes (Korean, emoji-free) round-trip to the server exactly.
func TestFRRHZ172_UTF8RoundTripByteExact(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"Accepted":true}`)
	}))
	defer srv.Close()
	line := "한글 요약 — 따옴표\"와 <태그> & 탭\t"
	body := "본문 첫 줄\n둘째 줄: 가나다라마바사\n"
	if code, _, _ := run172(t, deps172(env172(srv.URL), map[string]string{"b": body}), "note", "m-0000abcd", line, "--body-file", "b"); code != 0 {
		t.Fatal(code)
	}
	var got struct{ Content string }
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if want := line + "\n\n" + body + trailer172("m-0000abcd"); !bytes.Equal([]byte(got.Content), []byte(want)) {
		t.Fatalf("FR-RHZ-172 utf8:\n got %q\nwant %q", got.Content, want)
	}
}

// ---- usage -------------------------------------------------------------------

func TestFRRHZ172_UsageContentPinned(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	code, _, _ := run172(t, deps172(env172(srv.URL), nil), "usage", "m-0000abcd", "--model", "claude-x", "--in", "1200", "--out", "340", "--cache-read", "5000")
	if code != 0 {
		t.Fatal(code)
	}
	jsonEq172(t, fb.intent(0), map[string]any{
		"actor": "sess-a", "content": "usage model=claude-x in=1200 out=340 cacheRead=5000 cacheWrite=0\nactor=sess-a at=2026-10-09T23:30:00Z",
		"kind": "note.create", "memoryKind": "usage", "missionId": "m-0000abcd", "tags": []string{"usage", "model:claude-x"},
	})
}

func TestFRRHZ172_UsageBadNumbersExit2(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	for _, args := range [][]string{
		{"usage", "m-0000abcd", "--model", "m", "--in", "-1", "--out", "1"},
		{"usage", "m-0000abcd", "--model", "m", "--in", "1.5", "--out", "1"},
		{"usage", "m-0000abcd", "--model", "m", "--in", "1", "--out", "x"},
		{"usage", "m-0000abcd", "--model", "m", "--in", "1", "--out", "1", "--cache-write", "-3"},
		{"usage", "m-0000abcd", "--model", "m", "--in", "1"},
		{"usage", "m-0000abcd", "--in", "1", "--out", "1"},
	} {
		if code, _, _ := run172(t, deps172(env172(srv.URL), nil), args...); code != 2 {
			t.Fatalf("FR-RHZ-172 %v: code %d", args, code)
		}
	}
	if len(fb.kinds()) != 0 {
		t.Fatalf("FR-RHZ-172: bad usage must send nothing")
	}
}

// ---- ask ---------------------------------------------------------------------

func TestFRRHZ172_AskQuestion(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	code, _, _ := run172(t, deps172(env172(srv.URL), map[string]string{"q.md": "선택지 A/B"}), "ask", "m-0000abcd", "어느 쪽?", "--body-file", "q.md", "--recommendation", "A")
	if code != 0 {
		t.Fatal(code)
	}
	jsonEq172(t, fb.intent(0), map[string]any{"actor": "sess-a", "body": "선택지 A/B", "kind": "question.ask", "missionId": "m-0000abcd", "name": "어느 쪽?", "recommendation": "A"})
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "ask", "m-0000abcd", "t", "--body", "inline"); code != 0 {
		t.Fatal(code)
	}
	jsonEq172(t, fb.intent(1), map[string]any{"actor": "sess-a", "body": "inline", "kind": "question.ask", "missionId": "m-0000abcd", "name": "t"})
	if code, _, _ := run172(t, deps172(env172(srv.URL), nil), "ask", "m-0000abcd", "t"); code != 2 {
		t.Fatalf("FR-RHZ-172 ask without body: %d", code)
	}
}

// ---- context / start ---------------------------------------------------------

const memories172 = `[
 {"id":"note-a","kind":"observation","content":"sibling note","sourceId":"s","tags":["RHZ-999"],"seq":5},
 {"id":"note-b","kind":"observation","content":"older own note","sourceId":"s","tags":["RHZ-131"],"seq":3},
 {"id":"note-c","kind":"decision","content":"handoff text 핸드오프 본문\nline2","sourceId":"s","tags":["handoff","RHZ-131"],"seq":1},
 {"id":"note-d","kind":"blocked","content":"mission-bound blocked","sourceId":"s","tags":["blocked"],"seq":7},
 {"id":"note-e","kind":"observation","content":"newer own note","sourceId":"s","tags":["rhz-131"],"seq":9}
]`

func contextBoard172(t *testing.T, state string) (*fakeBoard172, string) {
	fb, srv := newFakeBoard172(t)
	fb.contexts["include=prompt&task=m-0000abcd"] = taskBundle172("RHZ-131 agent CLI", state, "프롬프트: 먼저 X를 확인하라", memories172)
	// goal-bound notes: a (sibling) and b (own, code-tagged).
	fb.contexts["goal=goal-g"] = `{"goal":{"id":"goal-g","description":"the goal","state":"active"},"memories":[{"id":"note-a","kind":"observation","content":"sibling note","sourceId":"s","tags":["RHZ-999"],"seq":5},{"id":"note-b","kind":"observation","content":"older own note","sourceId":"s","tags":["RHZ-131"],"seq":3}]}`
	return fb, srv.URL
}

func TestFRRHZ172_ContextPrintsPromptHandoffAndFiltersByCode(t *testing.T) {
	fb, board := contextBoard172(t, "running")
	code, out, stderr := run172(t, deps172(env172(board), nil), "context", "m-0000abcd", "--no-record")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"mission: RHZ-131 agent CLI (mission-x)", "state: running", "프롬프트: 먼저 X를 확인하라", "handoff text 핸드오프 본문\nline2", "mission-bound blocked", "newer own note", "older own note"} {
		if !strings.Contains(out, want) {
			t.Fatalf("FR-RHZ-172 context missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sibling note") {
		t.Fatalf("FR-RHZ-172: goal-bound note of another code must be filtered:\n%s", out)
	}
	// handoff first, then by seq desc: e(9), d(7), b(3).
	order := []string{"handoff text", "newer own note", "mission-bound blocked", "older own note"}
	last := -1
	for _, s := range order {
		i := strings.Index(out, s)
		if i < last {
			t.Fatalf("FR-RHZ-172 note order wrong at %q:\n%s", s, out)
		}
		last = i
	}
	if n := len(fb.kinds()); n != 0 {
		t.Fatalf("FR-RHZ-172: --no-record must send no intent, sent %v", fb.kinds())
	}
}

func TestFRRHZ172_ContextTruncatesNonHandoffNotes(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	long := strings.Repeat("x\n", 30) + "TAIL"
	mem, _ := json.Marshal([]map[string]any{
		{"id": "n1", "kind": "observation", "content": long, "tags": []string{}, "seq": 2},
		{"id": "n2", "kind": "handoff", "content": long + "-H", "tags": []string{}, "seq": 1},
	})
	fb.contexts["include=prompt&task=m-0000abcd"] = taskBundle172("no code", "running", "", string(mem))
	code, out, _ := run172(t, deps172(env172(srv.URL), nil), "context", "m-0000abcd", "--no-record")
	if code != 0 || !strings.Contains(out, "TAIL-H") || strings.Count(out, "TAIL") != 1 || !strings.Contains(out, "[... truncated]") {
		t.Fatalf("FR-RHZ-172 truncation:\n%s", out)
	}
	if strings.Contains(out, "== prompt ==") {
		t.Fatalf("FR-RHZ-172: empty prompt prints no section")
	}
}

func TestFRRHZ172_ContextRecordsStartByState(t *testing.T) {
	for _, tc := range []struct {
		state string
		kinds string
	}{
		{"queued", "task.resume,mission.progress"},
		{"paused", "task.resume,mission.progress"},
		{"running", "mission.progress"},
		{"waiting", "mission.progress"},
		{"blocked", "mission.progress"},
		{"completed", ""},
		{"cancelled", ""},
		{"failed", ""},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fb, board := contextBoard172(t, tc.state)
			code, _, _ := run172(t, deps172(env172(board), nil), "context", "m-0000abcd")
			if code != 0 {
				t.Fatal(code)
			}
			if got := strings.Join(fb.kinds(), ","); got != tc.kinds {
				t.Fatalf("FR-RHZ-172 %s: intents %q want %q", tc.state, got, tc.kinds)
			}
			k := fb.kinds()
			for i, kind := range k {
				m := fb.intent(i)
				switch kind {
				case "task.resume":
					jsonEq172(t, m, map[string]any{"actor": "sess-a", "kind": "task.resume", "taskId": "mission-x"})
				case "mission.progress":
					jsonEq172(t, m, map[string]any{"actor": "sess-a", "currentAction": "착수 — context 읽음 (sess-a)", "kind": "mission.progress", "missionId": "mission-x"})
				}
			}
		})
	}
}

func TestFRRHZ172_ContextRecordingRejectedStillPrintsExit0(t *testing.T) {
	fb, board := contextBoard172(t, "queued")
	fb.reject["task.resume"] = "상태 전이 거부"
	code, out, stderr := run172(t, deps172(env172(board), nil), "context", "m-0000abcd")
	if code != 0 || !strings.Contains(out, "프롬프트") || stderr != "warning: start not recorded: rejected: 상태 전이 거부\n" {
		t.Fatalf("FR-RHZ-172: code=%d stderr=%q out=%q", code, stderr, out)
	}
	if got := strings.Join(fb.kinds(), ","); got != "task.resume" {
		t.Fatalf("FR-RHZ-172: failed resume must not be followed by progress: %s", got)
	}
}

func TestFRRHZ172_ContextNotFoundExit1(t *testing.T) {
	_, srv := newFakeBoard172(t)
	code, _, stderr := run172(t, deps172(env172(srv.URL), nil), "context", "m-0000abcd")
	if code != 1 || !strings.Contains(stderr, "HTTP 404") {
		t.Fatalf("FR-RHZ-172: code=%d stderr=%q", code, stderr)
	}
}

func TestFRRHZ172_StartRecordsAndPrintsOk(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	fb.contexts["mission=m-0000abcd"] = `{"mission":{"id":"mission-x","name":"n","state":"paused"},"memories":[]}`
	code, out, _ := run172(t, deps172(env172(srv.URL), nil), "start", "m-0000abcd")
	if code != 0 || out != "ok\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if got := strings.Join(fb.kinds(), ","); got != "task.resume,mission.progress" {
		t.Fatalf("FR-RHZ-172 start order: %s", got)
	}
	jsonEq172(t, fb.intent(1), map[string]any{"actor": "sess-a", "currentAction": "착수 (sess-a)", "kind": "mission.progress", "missionId": "mission-x"})
	fb.reject["task.resume"] = "거부됨"
	code, _, stderr := run172(t, deps172(env172(srv.URL), nil), "start", "m-0000abcd")
	if code != 1 || stderr != "rejected: 거부됨\n" {
		t.Fatalf("FR-RHZ-172 start rejection: code=%d stderr=%q", code, stderr)
	}
}

// ---- end-to-end against the real board handler ------------------------------

func TestFRRHZ172_EndToEndAgainstRealBoard(t *testing.T) {
	store := &events.Store{}
	srv := httptest.NewServer(workspace.NewHTTP(store).Handler())
	defer srv.Close()
	post := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct {
			Accepted bool
			Reason   string
		}
		json.NewDecoder(resp.Body).Decode(&res)
		if !res.Accepted {
			t.Fatalf("setup intent %s rejected: %s", body, res.Reason)
		}
	}
	post(`{"kind":"mission.create","name":"RHZ-900-demo","prompt":"인수 기준: 테스트 통과","actor":"ops"}`)
	const mid = "mission-RHZ-900-demo"
	getJSON := func(path string, v any) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	type task struct {
		State, CurrentAction string
	}
	ctxTask := func() task {
		var b struct{ Task task }
		getJSON("/v1/context?task="+url.QueryEscape(mid), &b)
		return b.Task
	}
	if got := ctxTask(); got.State != "queued" {
		t.Fatalf("setup state %q", got.State)
	}
	d := deps172(map[string]string{"RHIZOME_BOARD": srv.URL, "RHIZOME_SESSION": "sess-e2e", "RHIZOME_MISSION": mid}, nil)

	code, out, stderr := run172(t, d, "context")
	if code != 0 || stderr != "" || !strings.Contains(out, "mission: RHZ-900-demo") || !strings.Contains(out, "== prompt ==\n인수 기준: 테스트 통과\n") {
		t.Fatalf("FR-RHZ-172 e2e context: code=%d stderr=%q\n%s", code, stderr, out)
	}
	if got := ctxTask(); got.State != "running" || got.CurrentAction != "착수 — context 읽음 (sess-e2e)" {
		t.Fatalf("FR-RHZ-172 e2e: context must move queued→running and record: %+v", got)
	}

	if code, _, stderr := run172(t, d, "blocked", "외부 승인 대기"); code != 0 || stderr != "" {
		t.Fatalf("FR-RHZ-172 e2e blocked: code=%d stderr=%q", code, stderr)
	}
	var ws struct {
		Body struct {
			Attention []struct{ Kind, RefID, MissionID string }
		}
	}
	getJSON("/v1/workspace", &ws)
	found := false
	for _, a := range ws.Body.Attention {
		if a.Kind == "note_blocked" && a.MissionID == mid {
			found = true
		}
	}
	if !found {
		t.Fatalf("FR-RHZ-172 e2e: blocked note must surface as note_blocked: %+v", ws.Body.Attention)
	}

	if code, _, stderr := run172(t, d, "done", "구현 끝"); code != 0 || stderr != "" {
		t.Fatalf("FR-RHZ-172 e2e done: code=%d stderr=%q", code, stderr)
	}
	if got := ctxTask(); got.State != "running" || got.CurrentAction != "done: 구현 끝" {
		t.Fatalf("FR-RHZ-172 e2e: done must leave the mission non-terminal: %+v", got)
	}

	if code, _, stderr := run172(t, d, "usage", "--model", "m1", "--in", "10", "--out", "2"); code != 0 || stderr != "" {
		t.Fatalf("FR-RHZ-172 e2e usage: code=%d stderr=%q", code, stderr)
	}
	// Handles work too: a second context by handle reads the same mission
	// and (running) records progress only.
	var wsT struct {
		Body struct{ Tasks []struct{ ID, Handle string } }
	}
	getJSON("/v1/workspace", &wsT)
	handle := ""
	for _, tk := range wsT.Body.Tasks {
		if tk.ID == mid {
			handle = tk.Handle
		}
	}
	code, out, stderr = run172(t, d, "context", handle)
	if code != 0 || stderr != "" || !strings.Contains(out, "("+mid+")") || !strings.Contains(out, "인수 기준: 테스트 통과") || !strings.Contains(out, "외부 승인 대기") || !strings.Contains(out, "usage model=m1") {
		t.Fatalf("FR-RHZ-172 e2e context by handle %q: code=%d stderr=%q\n%s", handle, code, stderr, out)
	}
}

// ---- review follow-ups -------------------------------------------------------

// Same summary re-reported later on the same mission is a new note (the
// trailer's timestamp changes the content, so the id changes).
func TestFRRHZ172_ReBlockLaterIsNewNoteContent(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	d := deps172(env172(srv.URL), nil)
	if code, _, _ := run172(t, d, "blocked", "m-0000abcd", "CI 대기"); code != 0 {
		t.Fatal(code)
	}
	d.now = func() time.Time { return fixedNow172.Add(time.Hour) }
	if code, _, _ := run172(t, d, "blocked", "m-0000abcd", "CI 대기"); code != 0 {
		t.Fatal(code)
	}
	a, b := fb.intent(0)["content"].(string), fb.intent(2)["content"].(string)
	if a == b || !strings.HasPrefix(a, "CI 대기\n\n") || b != "CI 대기\n\nactor=sess-a mission=m-0000abcd at=2026-10-10T00:30:00Z" {
		t.Fatalf("FR-RHZ-172 re-block content: %q vs %q", a, b)
	}
}

func TestFRRHZ172_StartOnTerminalMissionExit1NoIntent(t *testing.T) {
	for _, state := range []string{"completed", "cancelled", "failed"} {
		fb, srv := newFakeBoard172(t)
		fb.contexts["mission=m-0000abcd"] = `{"mission":{"id":"mission-x","name":"n","state":"` + state + `"},"memories":[]}`
		code, out, stderr := run172(t, deps172(env172(srv.URL), nil), "start", "m-0000abcd")
		if code != 1 || out != "" || stderr != "rejected: mission is "+state+"\n" {
			t.Fatalf("FR-RHZ-172 start %s: code=%d out=%q stderr=%q", state, code, out, stderr)
		}
		if n := len(fb.kinds()); n != 0 {
			t.Fatalf("FR-RHZ-172 start %s sent %v", state, fb.kinds())
		}
	}
}

func TestFRRHZ172_ArgumentEdgeCases(t *testing.T) {
	fb, srv := newFakeBoard172(t)
	d := deps172(env172(srv.URL), nil)
	// A carriage return makes the summary multi-line too.
	for _, line := range []string{"a\rb", "a\r\nb"} {
		if code, _, _ := run172(t, d, "note", "m-0000abcd", line); code != 2 {
			t.Fatalf("FR-RHZ-172 summary %q: code %d", line, code)
		}
	}
	// Positional mission plus --mission is ambiguous.
	if code, _, _ := run172(t, d, "note", "m-0000abcd", "x", "--mission", "m-other000"); code != 2 {
		t.Fatalf("FR-RHZ-172 mission twice: code %d", code)
	}
	if n := len(fb.kinds()); n != 0 {
		t.Fatalf("FR-RHZ-172 usage errors sent %v", fb.kinds())
	}
	// "--" ends flags: what follows is text even if it looks like a flag.
	// The mission comes after "--" too, so the parser must not resume flag
	// parsing for the remaining tokens.
	if code, _, stderr := run172(t, d, "note", "--", "m-0000abcd", "--looks-like-flag"); code != 0 {
		t.Fatalf("FR-RHZ-172 terminator: code=%d stderr=%q", code, stderr)
	}
	if got := fb.intent(0)["content"]; got != "--looks-like-flag"+trailer172("m-0000abcd") {
		t.Fatalf("FR-RHZ-172 terminator content %q", got)
	}
	// A flag value of exactly "--" is a value, not the terminator.
	if code, _, stderr := run172(t, d, "progress", "--action", "--", "m-0000abcd", "--pct", "50"); code != 0 {
		t.Fatalf("FR-RHZ-172 --action --: code=%d stderr=%q", code, stderr)
	}
	jsonEq172(t, fb.intent(1), map[string]any{"actor": "sess-a", "currentAction": "--", "kind": "mission.progress", "missionId": "m-0000abcd", "progress": 0.5})
}

// Against the real board: the same summary on two missions is two distinct
// accepted notes (without the trailer the second is a metadata conflict).
func TestFRRHZ172_SameSummaryTwoMissionsRealBoard(t *testing.T) {
	store := &events.Store{}
	srv := httptest.NewServer(workspace.NewHTTP(store).Handler())
	defer srv.Close()
	for _, name := range []string{"RHZ-901-a", "RHZ-902-b"} {
		resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(`{"kind":"mission.create","name":"`+name+`","prompt":"p","actor":"ops"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	d := deps172(env172(srv.URL), nil)
	for _, mid := range []string{"mission-RHZ-901-a", "mission-RHZ-902-b"} {
		if code, _, stderr := run172(t, d, "note", mid, "구현 완료"); code != 0 {
			t.Fatalf("FR-RHZ-172 note on %s: code=%d stderr=%q", mid, code, stderr)
		}
	}
	notes := 0
	for _, e := range store.All() {
		if e.AggregateType == "memory" && e.Revision == 1 {
			notes++
		}
	}
	if notes != 2 {
		t.Fatalf("FR-RHZ-172: want 2 distinct notes, got %d", notes)
	}
}
