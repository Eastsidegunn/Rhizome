package workspace

// RHZ-086 FR-RHZ-116: GET /v1/context?goal=<id|handle> — goal의 about note
// 읽기 투영(최신순, 결정론, 저널 무변경) + task 번들의 additive seq.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
)

// fixture086: goal-a(note 3건, 생성 순서 n1→n2→n3) + goal-b(note 0건) +
// mission-x(goal-a 소속, note 1건 — goal 번들에 섞이면 안 된다).
func fixture086(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	for _, id := range []string{"goal-a", "goal-b"} {
		if _, err := ms.CreateGoal(id, "desc "+id, "done", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ms.Create("mission-x", "goal-a", "m", "done"); err != nil {
		t.Fatal(err)
	}
	mems := memory.Service{Store: s}
	es := edge.Service{Store: s}
	notes := []memory.Memory{
		{ID: "n1", Kind: memory.Decision, Content: "first decision", Tags: []string{"a", "b"}},
		{ID: "n2", Kind: memory.Observation, Content: "second obs"},
		{ID: "n3", Kind: memory.Reference, Content: "third ref", Tags: []string{"z"}},
		{ID: "n-mission", Kind: memory.Fact, Content: "about the mission"},
	}
	for _, n := range notes {
		n.SourceType, n.SourceID, n.Confidence = "note", "src-"+n.ID, .9
		if _, err := mems.Create(n); err != nil {
			t.Fatal(err)
		}
		to := edge.Endpoint{Type: "goal", ID: "goal-a"}
		if n.ID == "n-mission" {
			to = edge.Endpoint{Type: "mission", ID: "mission-x"}
		}
		if _, err := es.Create(edge.Edge{ID: "e-" + n.ID, From: edge.Endpoint{Type: "memory", ID: n.ID}, To: to, Kind: edge.About, Actor: "tester", Correlation: "test", Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

type goalBundle086 struct {
	Goal struct {
		ID, Description, State string
	}
	Memories []struct {
		ID, Kind, Content, SourceID string
		Tags                        []string
		Seq                         uint64
	}
}

// G1: note 3건 → 최신순(seq 엄감), kind/tags/content 원문, mission note 제외.
func TestGoalContextNewestFirstFRRHZ116(t *testing.T) {
	s := fixture086(t)
	code, b := getContext(t, NewHTTP(s).Handler(), "?goal=goal-a")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, b)
	}
	var got goalBundle086
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Goal.ID != "goal-a" || got.Goal.Description != "desc goal-a" || got.Goal.State == "" {
		t.Fatalf("goal %+v", got.Goal)
	}
	if len(got.Memories) != 3 {
		t.Fatalf("memories %d, want 3: %s", len(got.Memories), b)
	}
	want := []struct {
		id, kind, content string
		tags              []string
	}{
		{"n3", "reference", "third ref", []string{"z"}},
		{"n2", "observation", "second obs", []string{}},
		{"n1", "decision", "first decision", []string{"a", "b"}},
	}
	for i, m := range got.Memories {
		w := want[i]
		if m.ID != w.id || m.Kind != w.kind || m.Content != w.content || m.SourceID != "src-"+w.id || len(m.Tags) != len(w.tags) {
			t.Fatalf("memory %d = %+v, want %+v", i, m, w)
		}
		for j := range w.tags {
			if m.Tags[j] != w.tags[j] {
				t.Fatalf("memory %d tags %v want %v", i, m.Tags, w.tags)
			}
		}
		if m.Seq == 0 || (i > 0 && got.Memories[i-1].Seq <= m.Seq) {
			t.Fatalf("seq not strictly decreasing at %d: %+v", i, got.Memories)
		}
		if m.Seq != s.List("memory", m.ID)[0].Sequence {
			t.Fatalf("memory %s seq %d != first-event sequence", m.ID, m.Seq)
		}
	}
	if bytes.Contains(b, []byte(`"n-mission"`)) || bytes.Contains(b, []byte(`"knowledge"`)) || bytes.Contains(b, []byte(`"steps"`)) || bytes.Contains(b, []byte(`"task"`)) {
		t.Fatalf("goal bundle leaks mission note or task-only sections: %s", b)
	}
}

// G2: note 없는 goal → memories [] (null 아님).
func TestGoalContextEmptyFRRHZ116(t *testing.T) {
	code, b := getContext(t, NewHTTP(fixture086(t)).Handler(), "?goal=goal-b")
	if code != http.StatusOK || !bytes.Contains(b, []byte(`"memories":[]`)) {
		t.Fatalf("status %d body %s", code, b)
	}
}

// M1: ?mission= → mission 자신의 note + 소속 goal의 note, 최신순, trace 0.
func TestMissionContextNewestFirstFRRHZ116(t *testing.T) {
	s := fixture086(t)
	code, b := getContext(t, NewHTTP(s).Handler(), "?mission=mission-x")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, b)
	}
	var got struct {
		Mission  struct{ ID, Name, State string }
		Goal     *struct{ ID string }
		Memories []struct {
			ID  string
			Seq uint64
		}
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Mission.ID != "mission-x" || got.Mission.Name != "m" || got.Mission.State == "" || got.Goal != nil {
		t.Fatalf("head %s", b)
	}
	ids := []string{}
	for i, m := range got.Memories {
		ids = append(ids, m.ID)
		if m.Seq == 0 || (i > 0 && got.Memories[i-1].Seq <= m.Seq) {
			t.Fatalf("seq not strictly decreasing: %+v", got.Memories)
		}
	}
	if fmt.Sprint(ids) != "[n-mission n3 n2 n1]" {
		t.Fatalf("memories %v, want [n-mission n3 n2 n1]", ids)
	}
	if !bytes.HasPrefix(b, []byte(`{"mission":{`)) || bytes.Contains(b, []byte(`"knowledge"`)) || bytes.Contains(b, []byte(`"steps"`)) {
		t.Fatalf("mission bundle shape: %s", b)
	}
}

// M2 (핵심 핀): 확정 지식이 걸린 note라도 ?mission=은 저널에 0건 쓴다 —
// 같은 fixture에서 ?task=는 trace를 쓴다는 대조로 차이를 고정한다.
func TestMissionContextNoTraceWhereTaskWritesFRRHZ116(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	before, _ := json.Marshal(s.All())
	for i := 0; i < 5; i++ {
		code, b := getContext(t, h, "?mission=mission-x")
		if code != http.StatusOK || !bytes.Contains(b, []byte(`"id":"mem-a"`)) || !bytes.Contains(b, []byte(`"id":"mem-b"`)) {
			t.Fatalf("status %d: %s", code, b)
		}
	}
	after, _ := json.Marshal(s.All())
	if !bytes.Equal(before, after) || len(traceEvents064(s)) != 0 {
		t.Fatalf("?mission= wrote to the journal (+%d events)", len(s.All())-len(before))
	}
	// 대조군: 같은 저장소에서 ?task=는 정확 1건의 trace를 쓴다.
	if code, _ := getContext(t, h, "?task=mission-x"); code != http.StatusOK || len(traceEvents064(s)) != 1 {
		t.Fatalf("control: ?task= trace count %d, want 1", len(traceEvents064(s)))
	}
}

// M3·M4: task+mission, goal+mission 400; 미지 mission 404; mission handle 해석.
func TestMissionContextParamsAndHandleFRRHZ116(t *testing.T) {
	s := fixture086(t)
	h := NewHTTP(s).Handler()
	for q, want := range map[string]int{"?task=mission-x&mission=mission-x": 400, "?goal=goal-a&mission=mission-x": 400, "?goal=goal-a&task=mission-x&mission=mission-x": 400, "?mission=mission-nope": 404} {
		if code, _ := getContext(t, h, q); code != want {
			t.Fatalf("%q: status %d want %d", q, code, want)
		}
	}
	_, byID := getContext(t, h, "?mission=mission-x")
	code, byHandle := getContext(t, h, "?mission="+buildHandleIndex(s.All()).of("m", "mission-x"))
	if code != http.StatusOK || !bytes.Equal(byID, byHandle) {
		t.Fatalf("handle: status %d, bytes differ", code)
	}
}

// G3: 미지 goal 404, task+goal 동시 400, 둘 다 없음 400, goal handle 해석.
func TestGoalContextParamsAndHandleFRRHZ116(t *testing.T) {
	s := fixture086(t)
	h := NewHTTP(s).Handler()
	for q, want := range map[string]int{"?goal=goal-nope": 404, "?goal=goal-a&task=mission-x": 400, "": 400, "?goal=g-ffffffff": 404} {
		if code, _ := getContext(t, h, q); code != want {
			t.Fatalf("%q: status %d want %d", q, code, want)
		}
	}
	_, byID := getContext(t, h, "?goal=goal-a")
	handle := buildHandleIndex(s.All()).of("g", "goal-a")
	code, byHandle := getContext(t, h, "?goal="+handle)
	if code != http.StatusOK || !bytes.Equal(byID, byHandle) {
		t.Fatalf("handle %s: status %d, bytes differ:\n%s\n%s", handle, code, byID, byHandle)
	}
}

// G4: 결정론(두 GET 바이트 동일) + 읽기 전용(저널 길이·내용 불변, trace 0).
func TestGoalContextDeterministicNoWriteFRRHZ116(t *testing.T) {
	s := fixture086(t)
	before, _ := json.Marshal(s.All())
	h := NewHTTP(s).Handler()
	_, b1 := getContext(t, h, "?goal=goal-a")
	_, b2 := getContext(t, h, "?goal=goal-a")
	if !bytes.Equal(b1, b2) {
		t.Fatalf("bytes differ:\n%s\n%s", b1, b2)
	}
	after, _ := json.Marshal(s.All())
	if !bytes.Equal(before, after) || len(traceEvents064(s)) != 0 {
		t.Fatal("goal context wrote to the journal")
	}
}

// G5: task 번들 키 집합 핀 — memories[]에 seq만 추가, 나머지 키 집합 불변.
func TestTaskContextAdditiveSeqFRRHZ116(t *testing.T) {
	s := fixture086(t)
	code, b := getContext(t, NewHTTP(s).Handler(), "?task=mission-x")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, b)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"task", "goal", "memories", "knowledge", "relations", "steps"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("task bundle lost key %s", k)
		}
	}
	if len(top) != 6 {
		t.Fatalf("task bundle top-level keys %d, want 6", len(top))
	}
	var mems []map[string]json.RawMessage
	if err := json.Unmarshal(top["memories"], &mems); err != nil {
		t.Fatal(err)
	}
	// mission-x는 goal-a의 note 3건 + 자기 note 1건을 본다(기존 시드 규칙).
	if len(mems) != 4 {
		t.Fatalf("task memories %d, want 4", len(mems))
	}
	for _, m := range mems {
		for _, k := range []string{"id", "kind", "content", "sourceId", "tags", "seq"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("memory missing key %s: %v", k, m)
			}
		}
		if len(m) != 6 {
			t.Fatalf("memory keys %d, want 6 (id,kind,content,sourceId,tags,seq)", len(m))
		}
		var seq uint64
		if err := json.Unmarshal(m["seq"], &seq); err != nil || seq == 0 {
			t.Fatalf("seq %s", m["seq"])
		}
	}
	if !bytes.HasSuffix(b, []byte(`,"steps":[]}`+"\n")) {
		t.Fatal("RHZ-068 steps suffix pin broken")
	}
}
