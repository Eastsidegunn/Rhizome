package workspace

// RHZ-134 (FR-RHZ-175) "카드가 대화한다": question.ask card fields on the
// relay and /v1/workspace, note.create re links, note_blocked resolution by
// an answer, and the gate thread.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/memory"
)

const card175JSON = `"risk":{"level":"high","line":"prod data may be lost"},"ifSkipped":"release slips","whyHuman":"touches billing","options":[{"key":"go","label":"Ship it","meaning":"deploy now","consequence":"billing migrates"},{"key":"wait","label":"Wait"}],"rollback":"revert it","context":{"project":"rhizome","what":"migration","state":"staged"}`

type gate175 struct {
	ID        string          `json:"id"`
	Risk      json.RawMessage `json:"risk"`
	IfSkipped string          `json:"ifSkipped"`
	WhyHuman  string          `json:"whyHuman"`
	Options   json.RawMessage `json:"options"`
	Rollback  string          `json:"rollback"`
	Context   json.RawMessage `json:"context"`
	Thread    []struct {
		ID, Kind, Content, At string
	} `json:"thread"`
}

func gates175(t *testing.T, s events.Port) (map[string]gate175, map[string]map[string]json.RawMessage) {
	t.Helper()
	raw := getWorkspace071(t, s)
	var env struct {
		Body struct {
			Gates []json.RawMessage `json:"gates"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	typed, keys := map[string]gate175{}, map[string]map[string]json.RawMessage{}
	for _, g := range env.Body.Gates {
		var x gate175
		var k map[string]json.RawMessage
		if json.Unmarshal(g, &x) != nil || json.Unmarshal(g, &k) != nil {
			t.Fatalf("gate %s", g)
		}
		typed[x.ID], keys[x.ID] = x, k
	}
	return typed, keys
}

var cardKeys175 = []string{"risk", "ifSkipped", "whyHuman", "options", "rollback", "context", "thread"}

func TestQuestionAskCardOverHTTPFRRHZ175(t *testing.T) {
	s := store078(t)
	h := NewHTTP(s).Handler()
	code, body := post073(t, h, `{"kind":"question.ask","name":"carded","body":"b","recommendation":"r","missionId":"mission-078","actor":"tester",`+card175JSON+`}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("ask: %d %s", code, body)
	}
	plainID, _ := askBound078(t, s, "plain")
	id := mustQuestionID("carded", "b", "r")
	typed, keys := gates175(t, s)
	g := typed[id]
	if string(g.Risk) != `{"level":"high","line":"prod data may be lost"}` || g.IfSkipped != "release slips" || g.WhyHuman != "touches billing" || g.Rollback != "revert it" {
		t.Fatalf("gate card: %+v", g)
	}
	if string(g.Options) != `[{"key":"go","label":"Ship it","meaning":"deploy now","consequence":"billing migrates"},{"key":"wait","label":"Wait"}]` {
		t.Fatalf("options: %s", g.Options)
	}
	if string(g.Context) != `{"project":"rhizome","what":"migration","state":"staged"}` {
		t.Fatalf("context: %s", g.Context)
	}
	for _, k := range cardKeys175 {
		if _, ok := keys[plainID][k]; ok {
			t.Fatalf("card-less gate carries %s", k)
		}
	}
	// Replay from the journal reproduces the same projection.
	again, _ := gates175(t, s)
	if fmt.Sprint(again[id]) != fmt.Sprint(g) {
		t.Fatalf("projection not stable")
	}
}

func TestQuestionAskCardRejectsWithZeroWritesFRRHZ175(t *testing.T) {
	s := store078(t)
	h := NewHTTP(s).Handler()
	bad := []string{
		`"risk":{"level":"extreme","line":"x"}`,
		`"risk":{"level":"low","line":"x","why":"y"}`,
		`"risk":{"level":"low"}`,
		`"risk":"high"`,
		`"options":[{"key":"Bad","label":"x"}]`,
		`"options":[{"key":"a","label":"x"},{"key":"a","label":"y"}]`,
		`"options":[{"key":"a","label":"x","extra":"y"}]`,
		`"options":{"key":"a"}`,
		`"context":{"project":"p","owner":"o"}`,
		`"context":{"project":1}`,
		`"ifSkipped":"` + strings.Repeat("x", 1001) + `"`,
		`"whyHuman":"` + strings.Repeat("x", 1001) + `"`,
		`"rollback":"` + strings.Repeat("x", 1001) + `"`,
	}
	for _, field := range bad {
		before := len(s.All())
		code, body := post073(t, h, `{"kind":"question.ask","name":"n","body":"b","missionId":"mission-078","actor":"tester",`+field+`}`)
		if code != 200 || !strings.Contains(body, `"Accepted":false`) {
			t.Fatalf("%s: %d %s", field, code, body)
		}
		if len(s.All()) != before {
			t.Fatalf("%s: wrote events", field)
		}
	}
}

func TestQuestionReAskCardConflictFRRHZ175(t *testing.T) {
	s := store078(t)
	h := NewHTTP(s).Handler()
	ask := `{"kind":"question.ask","name":"n","body":"b","missionId":"mission-078","actor":"tester",` + card175JSON + `}`
	if code, body := post073(t, h, ask); code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("ask: %s", body)
	}
	before := len(s.All())
	if _, body := post073(t, h, ask); !strings.Contains(body, `"Accepted":true`) || len(s.All()) != before {
		t.Fatalf("idempotent re-ask: %s", body)
	}
	for _, other := range []string{
		`{"kind":"question.ask","name":"n","body":"b","missionId":"mission-078","actor":"tester"}`,
		`{"kind":"question.ask","name":"n","body":"b","missionId":"mission-078","actor":"tester","whyHuman":"other"}`,
	} {
		_, body := post073(t, h, other)
		if !strings.Contains(body, "question metadata conflicts with existing gate") || len(s.All()) != before {
			t.Fatalf("conflict: %s", body)
		}
	}
}

func relayReason175(t *testing.T, s events.Port, in Intent) string {
	t.Helper()
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted {
		return ""
	}
	return res.Reason
}

func TestNoteCreateReFRRHZ175(t *testing.T) {
	s := store078(t)
	gateID, _ := askBound078(t, s, "g")
	note := func(kind, re, content string, tags ...string) Intent {
		return Intent{Kind: "note.create", MemoryKind: kind, MissionID: "mission-078", Re: re, Content: content, Tags: tags}
	}
	rejects := map[string]Intent{
		"question without re":  note("question", "", "q0"),
		"unknown re":           note("question", "q-000000000000000000000000", "q1"),
		"unknown note re":      note("answer", "note-missing", "a1"),
		"mission is no target": note("answer", "mission-078", "a2"),
		"observation with re":  note("observation", gateID, "o1"),
		"blocked with re":      note("blocked", gateID, "b1"),
	}
	for name, in := range rejects {
		before := len(s.All())
		if reason := relayReason175(t, s, in); reason == "" {
			t.Fatalf("%s: accepted", name)
		}
		if len(s.All()) != before {
			t.Fatalf("%s: wrote events", name)
		}
	}
	if r := relayReason175(t, s, note("question", gateID, "why this?")); r != "" {
		t.Fatal(r)
	}
	qid := noteID132("why this?")
	m, err := memory.Replay(s.List("memory", qid))
	if err != nil || m.Kind != memory.Question || strings.Join(m.Tags, ",") != "re:"+gateID {
		t.Fatalf("stored question: %+v %v", m, err)
	}
	// Idempotent resubmit, also when the caller passes the tag itself.
	before := len(s.All())
	for _, in := range []Intent{note("question", gateID, "why this?"), note("question", gateID, "why this?", "re:"+gateID)} {
		if r := relayReason175(t, s, in); r != "" || len(s.All()) != before {
			t.Fatalf("resubmit: %q", r)
		}
	}
	// answer re: a note id; answer without re (legacy RHZ-132 shape) stays valid.
	if r := relayReason175(t, s, note("answer", qid, "because", "answer")); r != "" {
		t.Fatal(r)
	}
	if r := relayReason175(t, s, note("answer", "", "legacy answer", "answer", "re:note-x")); r != "" {
		t.Fatal(r)
	}
	// re by the gate's RHZ-073 handle resolves to the gate id.
	ws := getWorkspace071(t, s)
	handle := handleOf073(t, ws, "gates", gateID)
	if r := relayReason175(t, s, note("question", handle, "via handle")); r != "" {
		t.Fatal(r)
	}
	if m, _ := memory.Replay(s.List("memory", noteID132("via handle"))); strings.Join(m.Tags, ",") != "re:"+gateID {
		t.Fatalf("handle tag: %+v", m.Tags)
	}
}

func TestBlockedNoteResolvedByAnswerFRRHZ175(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-b", domain.MissionReady, domain.MissionRunning)
	b1 := blockedNote132(t, s, "mission-b", "", "blocked", nil, "need a key")
	b2 := blockedNote132(t, s, "mission-b", "", "observation", []string{"h-request"}, "need a decision")
	ny, items := wire132(t, s)
	if ny != 2 || len(ofKind132(items, "note_blocked")) != 2 {
		t.Fatalf("before: %d %+v", ny, items)
	}
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "answer", MissionID: "mission-b", Re: b1, Content: "here is the key", Tags: []string{"answer"}})
	ny, items = wire132(t, s)
	got := ofKind132(items, "note_blocked")
	if ny != 1 || len(got) != 1 || got[0].RefID != b2 {
		t.Fatalf("after answer: %d %+v", ny, items)
	}
	// A non-answer note re: the blocked note does not resolve it.
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "question", MissionID: "mission-b", Re: b2, Content: "which decision?"})
	if ny, _ := wire132(t, s); ny != 1 {
		t.Fatalf("question resolved a blocked note: %d", ny)
	}
	// Legacy RHZ-132 answer carrying only the re: tag resolves too.
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "answer", MissionID: "mission-b", Content: "decided", Tags: []string{"answer", "re:" + b2}})
	if ny, items := wire132(t, s); ny != 0 || len(items) != 0 {
		t.Fatalf("legacy answer: %d %+v", ny, items)
	}
}

func TestGateThreadFRRHZ175(t *testing.T) {
	s := store078(t)
	gateID, _ := askBound078(t, s, "g")
	other, _ := askBound078(t, s, "other")
	relay := func(kind, re, content string, tags ...string) string {
		relay132(t, s, Intent{Kind: "note.create", MemoryKind: kind, MissionID: "mission-078", Re: re, Content: content, Tags: tags})
		return noteID132(content)
	}
	q := relay("question", gateID, "q?")
	yID := noteID132("y")
	// x: legacy tag pointing at y before y exists (a re: cycle x <-> y).
	x := relay("answer", "", "x", "re:"+yID)
	y := relay("answer", q, "y", "re:"+x)
	relay("observation", "", "unrelated", "re:"+q) // not a thread kind
	relay("question", other, "elsewhere")
	typed, _ := gates175(t, s)
	th := typed[gateID].Thread
	if len(th) != 3 || th[0].ID != q || th[1].ID != x || th[2].ID != y {
		t.Fatalf("thread: %+v", th)
	}
	if th[0].Kind != "question" || th[0].Content != "q?" || th[2].Kind != "answer" || th[0].At == "" {
		t.Fatalf("entries: %+v", th)
	}
	if len(typed[other].Thread) != 1 {
		t.Fatalf("other thread: %+v", typed[other].Thread)
	}
	// A superseded original drops out of the thread.
	old, err := memory.Replay(s.List("memory", y))
	if err != nil {
		t.Fatal(err)
	}
	next := memory.Memory{ID: "note-y2", Kind: memory.Answer, Content: "y2", SourceType: "blob", SourceID: old.SourceID, Confidence: 1, MissionID: "mission-078", Tags: []string{"re:" + q}}
	if _, err := (memory.Service{Store: s}).Supersede(old, next); err != nil {
		t.Fatal(err)
	}
	typed, _ = gates175(t, s)
	ids := []string{}
	for _, e := range typed[gateID].Thread {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != q+",note-y2" {
		t.Fatalf("after supersede: %v", ids)
	}
}

func TestGateThreadCapFRRHZ175(t *testing.T) {
	s := store078(t)
	gateID, _ := askBound078(t, s, "g")
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "question", MissionID: "mission-078", Re: gateID, Content: "root"})
	root := noteID132("root")
	// root + threadCap+5 answers reachable: the root and the 5 oldest answers drop.
	var answers []string
	for i := 0; i < threadCap+5; i++ {
		c := fmt.Sprintf("answer %d", i)
		relay132(t, s, Intent{Kind: "note.create", MemoryKind: "answer", MissionID: "mission-078", Re: root, Content: c})
		answers = append(answers, noteID132(c))
	}
	typed, _ := gates175(t, s)
	th := typed[gateID].Thread
	if len(th) != threadCap || th[0].ID != answers[5] || th[len(th)-1].ID != answers[len(answers)-1] {
		t.Fatalf("cap: len=%d first=%s", len(th), th[0].ID)
	}
}

func TestGateThreadContentCutFRRHZ175(t *testing.T) {
	s := store078(t)
	gateID, _ := askBound078(t, s, "g")
	long := strings.Repeat("가", threadContentMaxRunes+10)
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "question", MissionID: "mission-078", Re: gateID, Content: long})
	relay132(t, s, Intent{Kind: "note.create", MemoryKind: "answer", MissionID: "mission-078", Re: noteID132(long), Content: "short"})
	typed, _ := gates175(t, s)
	th := typed[gateID].Thread
	if len(th) != 2 {
		t.Fatalf("thread len=%d", len(th))
	}
	if want := strings.Repeat("가", threadContentMaxRunes) + threadContentCut; th[0].Content != want {
		t.Fatalf("long content not cut: %d runes", len([]rune(th[0].Content)))
	}
	if th[1].Content != "short" {
		t.Fatalf("short content changed: %q", th[1].Content)
	}
}
