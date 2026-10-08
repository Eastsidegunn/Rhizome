package workspace

// RHZ-073 (FR-RHZ-103): short deterministic node handles — a pure function
// of the journal (zero new events). H1 determinism, H2 collision extension,
// H3 context by handle, H4 intent by handle, H5 additive golden, W1 no
// writes from resolution.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/deliverable"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
	"rhizome/internal/question"
	"rhizome/internal/source"
)

type wire073 struct {
	Body map[string]json.RawMessage `json:"body"`
}

// fixture073: goal + running mission + internal gate + deliverable.
func fixture073(t *testing.T) (*events.Store, question.Ref, deliverable.Deliverable) {
	t.Helper()
	s := &events.Store{}
	missionIn062(t, s, "mission-예시: [계약] 게이트 결정 슬롯", domain.MissionReady, domain.MissionRunning)
	q, err := (question.Service{Store: s}).Ask("q title", "q body", "approve", "mission-예시: [계약] 게이트 결정 슬롯", "", "tester", "corr-073")
	if err != nil {
		t.Fatal(err)
	}
	src, err := (source.Service{Store: s}).Register([]byte("body"), "text/plain", "note://d073")
	if err != nil {
		t.Fatal(err)
	}
	d, err := (deliverable.Service{Store: s}).Create(deliverable.Deliverable{ID: "deliv-073", Kind: "file", MissionID: "mission-예시: [계약] 게이트 결정 슬롯", SourceRef: src.BlobID, Summary: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return s, q, d
}

func items073(t *testing.T, b []byte, key string) []map[string]any {
	t.Helper()
	var w wire073
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	var out []map[string]any
	if err := json.Unmarshal(w.Body[key], &out); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return out
}

func handleOf073(t *testing.T, b []byte, key, id string) string {
	t.Helper()
	for _, it := range items073(t, b, key) {
		if it["id"] == id {
			h, _ := it["handle"].(string)
			return h
		}
	}
	t.Fatalf("%s %q not in %s", key, id, b)
	return ""
}

func sha8(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:8]
}

// H1: same journal → same handles (two bodies byte-equal); handle ==
// <tag>-<sha256 hex[:8]> for known IDs.
func TestHandlesDeterministicFRRHZ103(t *testing.T) {
	s, q, d := fixture073(t)
	const mid = "mission-예시: [계약] 게이트 결정 슬롯"
	b1, b2 := getWorkspace071(t, s), getWorkspace071(t, s)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("two snapshots differ:\n%s\n%s", b1, b2)
	}
	for _, c := range []struct{ key, id, want string }{
		{"tasks", mid, "m-" + sha8(mid)},
		{"missions", "goal-" + mid, "g-" + sha8("goal-"+mid)},
		{"gates", q.ID, "q-" + sha8(q.ID)},
		{"deliverables", d.ID, "d-" + sha8(d.ID)},
	} {
		if got := handleOf073(t, b1, c.key, c.id); got != c.want {
			t.Fatalf("%s %q handle = %q want %q", c.key, c.id, got, c.want)
		}
	}
	if got := handleFor("m", mid, 8); got != "m-"+sha8(mid) {
		t.Fatalf("handleFor = %q", got)
	}
}

// H2: on a prefix collision the earlier node (journal first appearance)
// keeps the short handle and the later one is extended; both resolve.
func TestHandleCollisionExtendsLaterNodeFRRHZ103(t *testing.T) {
	old := handlePrefixLen
	handlePrefixLen = 1
	defer func() { handlePrefixLen = old }()
	first := "mission-h2-0"
	second := ""
	for i := 1; i < 1000; i++ {
		c := fmt.Sprintf("mission-h2-%d", i)
		if handleFor("m", c, 1) == handleFor("m", first, 1) && handleFor("m", c, 2) != handleFor("m", first, 2) {
			second = c
			break
		}
	}
	if second == "" {
		t.Fatal("no colliding id found")
	}
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-h2", "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first, second} {
		if _, err := ms.Create(id, "goal-h2", "m", "done"); err != nil {
			t.Fatal(err)
		}
	}
	hi := buildHandleIndex(s.All())
	hFirst, hSecond := hi.of("m", first), hi.of("m", second)
	if hFirst != handleFor("m", first, 1) {
		t.Fatalf("earlier node must keep the short handle: %q", hFirst)
	}
	if hSecond != handleFor("m", second, 2) {
		t.Fatalf("later node must be extended by one char: %q (short %q)", hSecond, handleFor("m", second, 1))
	}
	if hi.resolve(hFirst) != first || hi.resolve(hSecond) != second {
		t.Fatalf("resolve: %q→%q %q→%q", hFirst, hi.resolve(hFirst), hSecond, hi.resolve(hSecond))
	}
	// The wire agrees with the index, and the earlier handle survives a
	// third colliding arrival.
	b := getWorkspace071(t, s)
	if handleOf073(t, b, "tasks", first) != hFirst || handleOf073(t, b, "tasks", second) != hSecond {
		t.Fatalf("wire handles differ from index: %s", b)
	}
	third := ""
	for i := 1; i < 5000; i++ {
		c := fmt.Sprintf("mission-h2-x%d", i)
		if handleFor("m", c, 1) == hFirst && c != second {
			third = c
			break
		}
	}
	if third == "" {
		t.Fatal("no third colliding id found")
	}
	if _, err := ms.Create(third, "goal-h2", "m", "done"); err != nil {
		t.Fatal(err)
	}
	hi2 := buildHandleIndex(s.All())
	if hi2.of("m", first) != hFirst || hi2.of("m", second) != hSecond {
		t.Fatal("existing handles changed when a new node arrived")
	}
	if h3 := hi2.of("m", third); h3 == hFirst || h3 == hSecond || hi2.resolve(h3) != third {
		t.Fatalf("third handle %q", h3)
	}
}

// H3: GET /v1/context?task=<handle> is byte-identical to ?task=<id>, and
// resolution writes nothing.
func TestContextByHandleFRRHZ103(t *testing.T) {
	s, _, _ := fixture073(t)
	const mid = "mission-예시: [계약] 게이트 결정 슬롯"
	h := NewHTTP(s).Handler()
	handle := handleOf073(t, getWorkspace071(t, s), "tasks", mid)
	before := len(s.All())
	code, byID := getContext(t, h, "?task="+strings.ReplaceAll(mid, " ", "%20"))
	if code != http.StatusOK {
		t.Fatalf("by id: %d %s", code, byID)
	}
	code, byHandle := getContext(t, h, "?task="+handle)
	if code != http.StatusOK {
		t.Fatalf("by handle: %d %s", code, byHandle)
	}
	if !bytes.Equal(byID, byHandle) {
		t.Fatalf("context differs:\n id    %s\n handle %s", byID, byHandle)
	}
	if !strings.Contains(string(byHandle), `"id":"`+mid+`"`) {
		t.Fatalf("bundle must carry the real id: %s", byHandle)
	}
	if len(s.All()) != before {
		t.Fatal("context by handle wrote to the journal")
	}
	// Unknown handle shape → treated as a literal id → 404 as today.
	if code, _ := getContext(t, h, "?task=m-ffffffff"); code != http.StatusNotFound {
		t.Fatalf("unknown handle: %d", code)
	}
}

func post073(t *testing.T, h http.Handler, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// H4: /v1/intent accepts handles for missionId / goalId / gateId / taskId;
// the journal carries the real IDs; an unknown handle shape passes through
// as a literal id with the existing error.
func TestIntentByHandleFRRHZ103(t *testing.T) {
	s, q, _ := fixture073(t)
	const mid = "mission-예시: [계약] 게이트 결정 슬롯"
	gid := "goal-" + mid
	ws := getWorkspace071(t, s)
	mh, gh, qh := handleOf073(t, ws, "tasks", mid), handleOf073(t, ws, "missions", gid), handleOf073(t, ws, "gates", q.ID)
	h := NewHTTP(s).Handler()

	// question.ask {missionId: handle}
	code, body := post073(t, h, `{"kind":"question.ask","name":"via handle","body":"b","recommendation":"approve","missionId":"`+mh+`","actor":"tester"}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("question.ask by handle: %d %s", code, body)
	}
	asked := s.All()[len(s.All())-1]
	if asked.AggregateType != "question" || !strings.Contains(string(asked.Payload), `"MissionID":"`+mid+`"`) || strings.Contains(string(asked.Payload), mh) {
		t.Fatalf("question event must carry the real mission id: %s", asked.Payload)
	}

	// task.pause {taskId: handle} on a running mission
	code, body = post073(t, h, `{"kind":"task.pause","taskId":"`+mh+`","actor":"tester"}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("task.pause by handle: %d %s", code, body)
	}
	if m, err := projector.ReplayMission(s.List("mission", mid)); err != nil || m.State != domain.MissionPaused {
		t.Fatalf("task.pause did not reach %q: %+v %v", mid, m, err)
	}
	code, body = post073(t, h, `{"kind":"task.resume","taskId":"`+mh+`","actor":"tester"}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("task.resume by handle: %d %s", code, body)
	}

	// gate.approve {gateId: handle}
	code, body = post073(t, h, `{"kind":"gate.approve","gateId":"`+qh+`","digest":"`+q.Digest+`","actor":"tester"}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("gate.approve by handle: %d %s", code, body)
	}
	if got, err := (question.Service{Store: s}).Get(q.ID); err != nil || got.Decision != question.Approve {
		t.Fatalf("gate not approved: %+v %v", got, err)
	}

	// mission.complete {missionId: handle} on a running mission
	code, body = post073(t, h, `{"kind":"mission.complete","missionId":"`+mh+`","actor":"tester"}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("mission.complete by handle: %d %s", code, body)
	}
	last := s.All()[len(s.All())-1]
	if last.AggregateType != "mission" || last.AggregateID != mid || strings.Contains(string(last.Payload), mh) {
		t.Fatalf("decision event must carry the real id: %s %s %s", last.AggregateType, last.AggregateID, last.Payload)
	}
	if m, err := projector.ReplayMission(s.List("mission", mid)); err != nil || m.State != domain.MissionSucceeded {
		t.Fatalf("mission not completed: %+v %v", m, err)
	}

	// goal.resolve {goalId: handle}
	code, body = post073(t, h, `{"kind":"goal.resolve","goalId":"`+gh+`","actor":"tester"}`)
	if code != 200 || !strings.Contains(body, `"Accepted":true`) {
		t.Fatalf("goal.resolve by handle: %d %s", code, body)
	}
	if g, err := projector.ReplayGoal(s.List("goal", gid)); err != nil || g.State != domain.GoalAchieved {
		t.Fatalf("goal not resolved: %+v %v", g, err)
	}
	for _, e := range s.All() {
		if strings.Contains(string(e.Payload), mh) || strings.Contains(string(e.Payload), gh) || strings.Contains(string(e.Payload), qh) || e.AggregateID == mh || e.AggregateID == gh || e.AggregateID == qh {
			t.Fatalf("a handle leaked into the journal: %+v", e)
		}
	}

	// Unknown handle shape → literal id → the existing error, zero writes.
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "m-ffffffff"}, "tester", noAuthority())
	_, wantErr := projector.ReplayMission(s.List("mission", "m-ffffffff"))
	if err != nil || res.Accepted || wantErr == nil || res.Reason != wantErr.Error() {
		t.Fatalf("unknown handle must behave as a literal id: %+v %v (want %v)", res, err, wantErr)
	}
	if len(s.All()) != before {
		t.Fatal("rejected intent wrote to the journal")
	}
}

// H5: additive golden — top-level body keys unchanged; every item in
// missions/tasks/gates/deliverables carries a tagged `handle`.
func TestWorkspaceHandleAdditiveFRRHZ103(t *testing.T) {
	s, _, _ := fixture073(t)
	b := getWorkspace071(t, s)
	var raw struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw.Body))
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		t.Fatal("body not an object")
	}
	var keys []string
	for dec.More() {
		tok, _ := dec.Token()
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"missions", "tasks", "gates", "deliverables", "edges", "counts", "attention", "capabilities", "gateCapabilities"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("top-level keys changed: %v", keys)
	}
	for key, tag := range map[string]string{"missions": "g-", "tasks": "m-", "gates": "q-", "deliverables": "d-"} {
		items := items073(t, b, key)
		if len(items) == 0 {
			t.Fatalf("%s empty", key)
		}
		for _, it := range items {
			h, ok := it["handle"].(string)
			if !ok || !strings.HasPrefix(h, tag) || len(h) != len(tag)+8 || !handleShape(h) {
				t.Fatalf("%s item lacks a tagged handle: %v", key, it)
			}
		}
	}
	// Empty journal golden (RHZ-070 A1) is untouched: no item, no handle key.
	if got := string(getWorkspace071(t, &events.Store{})); got != `{"revision":0,"body":{"missions":[],"tasks":[],"gates":[],"deliverables":[],"edges":[],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{},"gateCapabilities":{}}}`+"\n" {
		t.Fatalf("empty golden changed: %s", got)
	}
}

// W1: resolution never writes — GET /v1/workspace, GET /v1/context by
// handle and rejected handle intents leave the journal length unchanged.
func TestHandleResolutionNoWritesFRRHZ103(t *testing.T) {
	s, q, _ := fixture073(t)
	const mid = "mission-예시: [계약] 게이트 결정 슬롯"
	h := NewHTTP(s).Handler()
	before := len(s.All())
	ws := getWorkspace071(t, s)
	mh, qh := handleOf073(t, ws, "tasks", mid), handleOf073(t, ws, "gates", q.ID)
	getWorkspace071(t, s)
	if code, _ := getContext(t, h, "?task="+mh); code != http.StatusOK {
		t.Fatalf("context: %d", code)
	}
	for _, body := range []string{
		`{"kind":"mission.fail","missionId":"` + mh + `"}`,                        // reason required
		`{"kind":"gate.approve","gateId":"` + qh + `"}`,                           // digest required
		`{"kind":"question.ask","name":"x","missionId":"m-ffffffff"}`,             // unknown → literal → not found
		`{"kind":"mission.create","name":"n","prompt":"p","goalId":"g-ffffffff"}`, // goal not found
	} {
		code, res := post073(t, h, body)
		if code != 200 || strings.Contains(res, `"Accepted":true`) {
			t.Fatalf("expected rejection for %s: %d %s", body, code, res)
		}
	}
	if len(s.All()) != before {
		t.Fatalf("journal grew from %d to %d without an accepted write", before, len(s.All()))
	}
}

// H6 (FR-RHZ-103): 리터럴 ID가 다른 노드의 핸들과 철자가 같아도
// ID가 우선한다 — 기존 ID의 의미는 절대 바뀌지 않는다. 저널 이벤트를 직접
// 구성해(스토어 불필요) 인덱스 규칙만 핀한다. probe: resolve의 ids 우선 검사
// 제거 → FAIL.
func TestHandleLiteralIDWinsOverHandleFRRHZ103(t *testing.T) {
	a := "mission-a"
	ha := handleFor("m", a, handlePrefixLen)
	all := []events.Event{
		{Sequence: 1, AggregateType: "mission", AggregateID: a, Type: "mission.created"},
		{Sequence: 2, AggregateType: "mission", AggregateID: ha, Type: "mission.created"}, // ID == A's handle
	}
	hi := buildHandleIndex(all)
	if got := hi.resolve(ha); got != ha {
		t.Fatalf("literal id %q resolved to %q — a known ID must never be read as a handle", ha, got)
	}
	if got := hi.resolve(a); got != a {
		t.Fatalf("plain id changed: %q", got)
	}
	if hi.of("m", a) != ha {
		t.Fatalf("A keeps its handle %q (got %q)", ha, hi.of("m", a))
	}
}
