package workspace

// RHZ-057 FR-RHZ-087 1단계: note.create dual-write(FK+about 엣지)·역방향 질의.
// 테스트 계획 RHZ-057 T5~T12 (T13은 T5·T7의 저널
// 증분 단언으로 충족 — 설계 원칙 항목).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
)

func rhz057Fixture(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.CreateGoal("goal-g2", "goal two", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-ms", "goal-g", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func noteID(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "note-" + hex.EncodeToString(sum[:])
}

func journalJSON(t *testing.T, s *events.Store) string {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func edgeEvents(s *events.Store) []events.Event {
	out := []events.Event{}
	for _, e := range s.All() {
		if e.AggregateType == "edge" {
			out = append(out, e)
		}
	}
	return out
}

// T5 (변이 probe 핵심): note.create{MissionID} → FK와 about 엣지의 dual-write.
// dual-write 제거 시 이 테스트와 T10이 FAIL한다(수용 기준 6).
func TestNoteCreateDualWritesAboutEdgeFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	n := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "linked note", MemoryKind: "observation", MissionID: "mission-ms"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	// source.registered + memory.created + edge.declared = 3.
	if got := len(s.All()) - n; got != 3 {
		t.Fatalf("journal grew %d, want 3", got)
	}
	id := noteID("linked note")
	m, err := memory.Replay(s.List("memory", id))
	if err != nil || m.MissionID != "mission-ms" {
		t.Fatalf("FK not coexisting: %+v err=%v", m, err)
	}
	ee := edgeEvents(s)
	if len(ee) != 1 {
		t.Fatalf("edge events %d, want 1", len(ee))
	}
	x, err := edge.Replay(ee)
	if err != nil {
		t.Fatal(err)
	}
	if x.Kind != edge.About || x.From != (edge.Endpoint{Type: "memory", ID: id}) || x.To != (edge.Endpoint{Type: "mission", ID: "mission-ms"}) {
		t.Fatalf("edge shape %+v", x)
	}
}

// T6: 무타깃 note.create는 기존 동작과 완전 동일 — 엣지 0, FK 빈 값.
func TestNoteCreateWithoutTargetUnchangedFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "plain note", MemoryKind: "observation"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if len(edgeEvents(s)) != 0 {
		t.Fatal("edge published without a target")
	}
	m, err := memory.Replay(s.List("memory", noteID("plain note")))
	if err != nil || m.GoalID != "" || m.MissionID != "" {
		t.Fatalf("FK unexpectedly set: %+v err=%v", m, err)
	}
}

// T7: 타깃 미실재는 어떤 append보다 먼저 거부 — 부분 쓰기 0 (단일 writer
// 경계의 저널 증분 단언, T13 원칙 포함).
func TestNoteCreateUnknownTargetNoPartialWriteFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	before := journalJSON(t, s)
	for _, in := range []Intent{
		{Kind: "note.create", Content: "ghost mission note", MemoryKind: "observation", MissionID: "mission-ghost"},
		{Kind: "note.create", Content: "ghost goal note", MemoryKind: "observation", GoalID: "goal-ghost"},
	} {
		res, err := RelayIntent(s, in, "tester", noAuthority())
		if err != nil || res.Accepted {
			t.Fatal(res, err)
		}
		if !strings.Contains(res.Reason, "unknown") {
			t.Fatalf("reason %q", res.Reason)
		}
	}
	if journalJSON(t, s) != before {
		t.Fatal("partial write: journal changed by rejected note.create")
	}
}

// T8: 동일 노트 재제출은 멱등 — 엣지 중복 미발행. 1차 보증은 기존 멱등
// 조기반환이 dual-write 이전에 return하는 것이고, 결정론 엣지 ID + revision-0
// append guard는 2차 백스톱이다(중복 거부 '규칙' 아님 — 2단계).
func TestNoteCreateResubmitNoDuplicateEdgeFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	in := Intent{Kind: "note.create", Content: "idempotent note", MemoryKind: "observation", MissionID: "mission-ms"}
	if res, err := RelayIntent(s, in, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	before := journalJSON(t, s)
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if journalJSON(t, s) != before {
		t.Fatal("resubmit appended events")
	}
}

// T9: 역방향 질의 단위 — goal·mission 타깃 정확 반환, 무관 엣지 미포함, 엣지
// 없는 memory는 빈 슬라이스.
func TestKnowledgeAboutReverseQueryFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	in := Intent{Kind: "note.create", Content: "both targets", MemoryKind: "observation", GoalID: "goal-g", MissionID: "mission-ms"}
	if res, err := RelayIntent(s, in, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	id := noteID("both targets")
	// 무관 엣지: contains(goal-g→goal-g2).
	if _, err := (edge.Service{Store: s}).Create(edge.Spec{ID: "e-contains", From: edge.Endpoint{Type: "goal", ID: "goal-g"}, To: edge.Endpoint{Type: "goal", ID: "goal-g2"}, Kind: edge.Contains, Actor: "tester", Correlation: "test"}, noAuthority()); err != nil {
		t.Fatal(err)
	}
	p, err := KnowledgeAbout(s, id)
	if err != nil {
		t.Fatal(err)
	}
	want := []AboutTarget{
		{Type: "goal", ID: "goal-g", EdgeID: "edge-about-" + strings.TrimPrefix(id, "note-")[:12] + "-goal-g"},
		{Type: "mission", ID: "mission-ms", EdgeID: "edge-about-" + strings.TrimPrefix(id, "note-")[:12] + "-mission-ms"},
	}
	if !reflect.DeepEqual(p.Targets, want) {
		t.Fatalf("targets %+v, want %+v", p.Targets, want)
	}
	// 엣지 없는 memory.
	if res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "lonely", MemoryKind: "observation"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	p2, err := KnowledgeAbout(s, noteID("lonely"))
	if err != nil || len(p2.Targets) != 0 {
		t.Fatalf("lonely targets %+v err=%v", p2.Targets, err)
	}
}

func postIntent057(t *testing.T, srv *httptest.Server, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/intent", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// T5′·T10: HTTP 왕복 — note.create{goalId} 와이어 발행 + /v1/knowledge?about=
// 역방향 질의(수용 기준 2 증거). 빈 결과는 [] 리터럴, 파람 없는 기존 응답
// 형태 불변.
func TestKnowledgeAboutHTTPFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	out := postIntent057(t, srv, map[string]any{"kind": "note.create", "content": "wire note", "memoryKind": "observation", "goalId": "goal-g"})
	if out["Accepted"] != true {
		t.Fatalf("intent rejected: %v", out)
	}
	id := noteID("wire note")
	resp, err := http.Get(srv.URL + "/v1/knowledge?about=" + id)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env struct {
		Revision uint64 `json:"revision"`
		Body     struct {
			About []struct{ Type, ID, EdgeID string } `json:"about"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Body.About) != 1 || env.Body.About[0].Type != "goal" || env.Body.About[0].ID != "goal-g" || env.Body.About[0].EdgeID == "" {
		t.Fatalf("about %+v", env.Body.About)
	}
	if env.Revision == 0 {
		t.Fatal("revision missing")
	}
	// 엣지 없는 memory → 빈 배열 리터럴.
	if res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "lonely wire", MemoryKind: "observation"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	resp2, err := http.Get(srv.URL + "/v1/knowledge?about=" + noteID("lonely wire"))
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatal(resp2, err)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(raw2), `"about":[]`) {
		t.Fatalf("empty about not literal []: %s", raw2)
	}
	// 파람 없는 기존 응답 형태 불변: notes 키 존재, about 키 부재.
	resp3, err := http.Get(srv.URL + "/v1/knowledge")
	if err != nil || resp3.StatusCode != http.StatusOK {
		t.Fatal(resp3, err)
	}
	raw3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if !strings.Contains(string(raw3), `"notes":`) || strings.Contains(string(raw3), `"about":`) {
		t.Fatalf("parameterless shape changed: %s", raw3)
	}
}

// T11: workspace projection 병존 — 새 Kind 엣지가 /v1/workspace body.edges에
// 나타나고 기존 스냅샷 재생이 오류 없이 동작.
func TestWorkspaceProjectionIncludesNewKindsFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	if res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "proj note", MemoryKind: "observation", MissionID: "mission-ms"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if _, err := (edge.Service{Store: s}).Create(edge.Spec{ID: "e-proj-contains", From: edge.Endpoint{Type: "goal", ID: "goal-g"}, To: edge.Endpoint{Type: "goal", ID: "goal-g2"}, Kind: edge.Contains, Actor: "tester", Correlation: "test"}, noAuthority()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"edgeKind":"about"`) || !strings.Contains(string(raw), `"edgeKind":"contains"`) {
		t.Fatalf("new kinds missing from workspace edges: %s", raw)
	}
}

// T12: 파생 재계산 — 저널 재생본에서 역방향 질의 결과 동일.
func TestAboutRecomputableFromEventsFRRHZ087(t *testing.T) {
	s := rhz057Fixture(t)
	if res, err := RelayIntent(s, Intent{Kind: "note.create", Content: "replay note", MemoryKind: "observation", GoalID: "goal-g", MissionID: "mission-ms"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	id := noteID("replay note")
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	orig, err := KnowledgeAbout(s, id)
	if err != nil {
		t.Fatal(err)
	}
	again, err := KnowledgeAbout(replayed, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orig, again) {
		t.Fatalf("replay diverged: %+v vs %+v", orig, again)
	}
}
