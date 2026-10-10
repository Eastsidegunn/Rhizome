package workspace

// RHZ-131 FR-RHZ-172: GET /v1/context?task=<id|handle>&include=prompt —
// opt-in task.prompt (the mission's prompt, domain.Mission.Success). Without
// include the bundle bytes are unchanged (pinned goldens); the UseTrace is
// the same with or without it; unknown include values are a 400.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

const prompt172 = "인수 기준: \"테스트\" 통과 <완료>\n둘째 줄"

func TestContextIncludePromptOptInByteExactFRRHZ172(t *testing.T) {
	s := goldenJournalWithoutVerificationFRRHZ131(t)
	h := NewHTTP(s).Handler()
	m, err := projector.ReplayMission(s.List("mission", "mission-x"))
	if err != nil || m.Success == "" {
		t.Fatalf("fixture mission-x: %+v %v", m, err)
	}
	// Without include (absent or empty): byte-identical to the pinned golden.
	for _, q := range []string{"?task=mission-x", "?task=mission-x&include="} {
		code, got := getContext(t, h, q)
		if code != http.StatusOK || !bytes.Equal(got, []byte(goldenPlainContextWithoutVerificationFRRHZ131)) {
			t.Fatalf("FR-RHZ-172 %s must equal the golden: code=%d\n%s", q, code, got)
		}
	}
	// With include=prompt: the golden plus exactly the nested prompt key.
	p, _ := json.Marshal(m.Success)
	want := strings.Replace(goldenPlainContextWithoutVerificationFRRHZ131, `"hasProgress":false},"goal"`, `"hasProgress":false,"prompt":`+string(p)+`},"goal"`, 1)
	if want == goldenPlainContextWithoutVerificationFRRHZ131 {
		t.Fatal("golden anchor not found")
	}
	code, got := getContext(t, h, "?task=mission-x&include=prompt")
	if code != http.StatusOK || string(got) != want {
		t.Fatalf("FR-RHZ-172 include=prompt:\n got: %s\nwant: %s", got, want)
	}
	if !bytes.HasSuffix(got, []byte(`,"steps":[]}`+"\n")) {
		t.Fatalf("FR-RHZ-172: steps suffix pin broken: %s", got)
	}
}

func TestContextIncludePromptValueAndHandleFRRHZ172(t *testing.T) {
	s := fixture064(t)
	if _, err := (mission.Service{Store: s}).Create("mission-p", "goal-g", "prompt mission", prompt172); err != nil {
		t.Fatal(err)
	}
	h := NewHTTP(s).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
	var ws struct {
		Body struct{ Tasks []struct{ ID, Handle string } }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	handle := ""
	for _, tk := range ws.Body.Tasks {
		if tk.ID == "mission-p" {
			handle = tk.Handle
		}
	}
	if !strings.HasPrefix(handle, "m-") {
		t.Fatalf("handle %q", handle)
	}
	for _, task := range []string{"mission-p", handle} {
		code, b := getContext(t, h, "?task="+task+"&include=prompt")
		var got struct {
			Task struct{ ID, Prompt string }
		}
		if code != http.StatusOK || json.Unmarshal(b, &got) != nil || got.Task.ID != "mission-p" || got.Task.Prompt != prompt172 {
			t.Fatalf("FR-RHZ-172 task=%s: code=%d %s", task, code, b)
		}
		code, b = getContext(t, h, "?task="+task)
		if code != http.StatusOK || bytes.Contains(b, []byte(`"prompt"`)) {
			t.Fatalf("FR-RHZ-172 task=%s without include must carry no prompt: %s", task, b)
		}
	}
}

// The opt-in never changes knowledge retrieval or the UseTrace: the same
// query sequence with and without include writes identical trace events.
func TestContextIncludePromptSameTraceFRRHZ172(t *testing.T) {
	plain, withPrompt := fixture064(t), fixture064(t)
	for i := 0; i < 2; i++ {
		if code, b := getContext(t, NewHTTP(plain).Handler(), "?task=mission-x"); code != http.StatusOK {
			t.Fatalf("%d %s", code, b)
		}
		if code, b := getContext(t, NewHTTP(withPrompt).Handler(), "?task=mission-x&include=prompt"); code != http.StatusOK {
			t.Fatalf("%d %s", code, b)
		}
	}
	a, b := traceEvents064(plain), traceEvents064(withPrompt)
	if len(a) != 2 || len(a) != len(b) {
		t.Fatalf("FR-RHZ-172 trace count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].AggregateID != b[i].AggregateID || a[i].Type != b[i].Type || !bytes.Equal(a[i].Payload, b[i].Payload) || a[i].CorrelationID != b[i].CorrelationID {
			t.Fatalf("FR-RHZ-172 trace %d differs:\n%+v\n%+v", i, a[i], b[i])
		}
	}
}

func TestContextIncludeBadValuesAre400FRRHZ172(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	before := len(s.All())
	for _, q := range []string{
		"?task=mission-x&include=bogus",
		"?task=mission-x&include=Prompt",
		"?goal=goal-g&include=prompt",
		"?mission=mission-x&include=prompt",
		"?include=prompt",
	} {
		if code, b := getContext(t, h, q); code != http.StatusBadRequest {
			t.Fatalf("FR-RHZ-172 %s: code=%d %s", q, code, b)
		}
	}
	if after := len(s.All()); after != before {
		t.Fatalf("FR-RHZ-172: a rejected include must write nothing (%d -> %d)", before, after)
	}
}

func TestContextIncludeRepeatedValuesFRRHZ172(t *testing.T) {
	h := NewHTTP(fixture064(t)).Handler()
	for _, q := range []string{
		"?task=mission-x&include=prompt&include=bogus",
		"?task=mission-x&include=bogus&include=prompt",
		"?task=mission-x&include=prompt&include=",
	} {
		if code, b := getContext(t, h, q); code != http.StatusBadRequest {
			t.Fatalf("FR-RHZ-172 %s: code=%d %s", q, code, b)
		}
	}
	if code, b := getContext(t, h, "?task=mission-x&include=prompt&include=prompt"); code != http.StatusOK || !bytes.Contains(b, []byte(`"prompt":"done"`)) {
		t.Fatalf("FR-RHZ-172 repeated prompt: code=%d %s", code, b)
	}
}
