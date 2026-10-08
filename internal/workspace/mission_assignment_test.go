package workspace

// RHZ-080 FR-RHZ-111: mission.assign relay → mission.assigned 1건, 투영
// (/v1/workspace tasks[].assignee, /v1/context task.assignee), 재배정은
// 마지막 값(이력 2건), terminal/빈값/미지 mission은 저널 불변 거부,
// GET /v1/workspace?assignee=<x>는 tasks만 정확 일치 필터(다른 컬렉션·
// counts 바이트 동일), 핸들 missionId, 저널 왕복. 계획 A1–A5/R1.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

func workspaceBody080(t *testing.T, s events.Port, query string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace"+query, nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/workspace%s %d: %s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// tasks080 returns the raw task objects by id and the sorted key set of each.
func tasks080(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var env struct {
		Body struct {
			Tasks []map[string]any `json:"tasks"`
		} `json:"body"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, tk := range env.Body.Tasks {
		out[tk["id"].(string)] = tk
	}
	return out
}

func keys080(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func assignedCount080(s events.Port, id string) int {
	n := 0
	for _, e := range s.List("mission", id) {
		if e.Type == "mission.assigned" {
			n++
		}
	}
	return n
}

func assign080(t *testing.T, s events.Port, id, assignee string) RelayResult {
	t.Helper()
	res, err := RelayIntent(s, Intent{Kind: "mission.assign", MissionID: id, Assignee: assignee, Reason: "handoff"}, "tester", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A1: relay assign → journal +1 mission.assigned; /v1/workspace tasks[].assignee
// and /v1/context task.assignee carry it; the unassigned task has no
// "assignee" key at all (omitempty byte evidence) and the RHZ-068 suffix holds.
func TestMissionAssignProjectedFRRHZ111(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady)
	missionIn062(t, s, "mission-b")
	n := len(s.All())
	if res := assign080(t, s, "mission-a", "agent-a"); !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	if len(s.All()) != n+1 {
		t.Fatalf("journal grew %d, want 1", len(s.All())-n)
	}
	last := s.All()[len(s.All())-1]
	var p struct{ Assignee, Reason string }
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if last.Type != "mission.assigned" || last.AggregateID != "mission-a" || p.Assignee != "agent-a" || p.Reason != "handoff" {
		t.Fatalf("event %+v %s", last, last.Payload)
	}
	if m := replay069(t, s, "mission-a"); m.Assignee != "agent-a" || m.State != domain.MissionReady {
		t.Fatalf("replay %+v", m)
	}
	tasks := tasks080(t, workspaceBody080(t, s, ""))
	if tasks["mission-a"]["assignee"] != "agent-a" {
		t.Fatalf("task a: %v", tasks["mission-a"])
	}
	if _, has := tasks["mission-b"]["assignee"]; has {
		t.Fatalf("unassigned task must omit assignee: %v", tasks["mission-b"])
	}
	// assignee is the last key of an assigned task (additive suffix).
	raw := workspaceBody080(t, s, "")
	if !bytes.Contains(raw, []byte(`,"handle":"`+handleFor("m", "mission-a", handlePrefixLen)+`","assignee":"agent-a"}`)) {
		t.Fatalf("assignee must trail handle in the task object: %s", raw)
	}
	h := NewHTTP(s).Handler()
	code, b := getContext(t, h, "?task=mission-a")
	if code != http.StatusOK {
		t.Fatalf("context %d: %s", code, b)
	}
	var bundle struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Task["assignee"] != "agent-a" || bundle.Task["id"] != "mission-a" {
		t.Fatalf("context task: %v", bundle.Task)
	}
	if !bytes.HasSuffix(b, []byte(`,"steps":[]}`+"\n")) {
		t.Fatalf("RHZ-068 suffix pin broken: %s", b)
	}
	code, b = getContext(t, h, "?task=mission-b")
	if code != http.StatusOK {
		t.Fatalf("context %d: %s", code, b)
	}
	bundle.Task = nil // Unmarshal merges into an existing map; start clean.
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatal(err)
	}
	if _, has := bundle.Task["assignee"]; has {
		t.Fatalf("unassigned context task must omit assignee: %v", bundle.Task)
	}
}

// A2: reassign → projection shows the last value; history keeps 2 events.
func TestMissionReassignLastWinsFRRHZ111(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady, domain.MissionRunning)
	if res := assign080(t, s, "mission-a", "agent-a"); !res.Accepted {
		t.Fatalf("first: %+v", res)
	}
	if res := assign080(t, s, "mission-a", "agent-b"); !res.Accepted {
		t.Fatalf("second: %+v", res)
	}
	if got := assignedCount080(s, "mission-a"); got != 2 {
		t.Fatalf("assigned events %d, want 2", got)
	}
	if m := replay069(t, s, "mission-a"); m.Assignee != "agent-b" || m.State != domain.MissionRunning {
		t.Fatalf("replay %+v", m)
	}
	if tk := tasks080(t, workspaceBody080(t, s, ""))["mission-a"]; tk["assignee"] != "agent-b" {
		t.Fatalf("task: %v", tk)
	}
}

// A3: terminal mission, empty/whitespace assignee, unknown mission → rejected
// with a reason, journal byte-unchanged.
func TestMissionAssignRejectedNoWriteFRRHZ111(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-live")
	missionIn062(t, s, "mission-done")
	if res := cancelMission062(t, s, "mission-done"); !res.Accepted {
		t.Fatalf("cancel fixture: %+v", res)
	}
	before := journalBytes069(t, s)
	cases := []struct {
		in   Intent
		want string
	}{
		{Intent{Kind: "mission.assign", MissionID: "mission-done", Assignee: "agent-a"}, domain.ErrInvalidState.Error()},
		{Intent{Kind: "mission.assign", MissionID: "mission-live", Assignee: ""}, "assignee required"},
		{Intent{Kind: "mission.assign", MissionID: "mission-live", Assignee: "   "}, "assignee required"},
		{Intent{Kind: "mission.assign", MissionID: "", Assignee: "agent-a"}, "missionId required"},
		{Intent{Kind: "mission.assign", MissionID: "mission-nope", Assignee: "agent-a"}, "mission event stream is empty"},
	}
	for _, c := range cases {
		res, err := RelayIntent(s, c.in, "tester", noAuthority())
		if err != nil || res.Accepted || res.Reason != c.want {
			t.Fatalf("%+v: %+v err=%v (want reason %q)", c.in, res, err, c.want)
		}
	}
	if after := journalBytes069(t, s); after != before {
		t.Fatal("journal changed on rejected mission.assign")
	}
	if ws := workspaceBody080(t, s, ""); bytes.Contains(ws, []byte(`"assignee"`)) {
		t.Fatalf("no assignee expected: %s", ws)
	}
}

// A4: ?assignee=<x> narrows tasks only (exact match); every other collection,
// counts and capabilities are byte-identical to the unfiltered body; unknown
// assignee → tasks []; no parameter on a journal without assignments has no
// "assignee" key anywhere and the task key set is the pre-RHZ-080 one.
func TestWorkspaceAssigneeFilterFRRHZ111(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-1", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	missionIn062(t, s, "mission-2", domain.MissionReady, domain.MissionRunning)
	missionIn062(t, s, "mission-3")
	wantKeys := []string{"attention", "handle", "hasProgress", "id", "missionId", "name", "state"}
	plain := workspaceBody080(t, s, "")
	if bytes.Contains(plain, []byte(`"assignee"`)) {
		t.Fatalf("pre-assignment body must not mention assignee: %s", plain)
	}
	for id, tk := range tasks080(t, plain) {
		if got := keys080(tk); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
			t.Fatalf("%s key set %v, want %v", id, got, wantKeys)
		}
	}
	if filtered := workspaceBody080(t, s, "?assignee=agent-a"); len(tasks080(t, filtered)) != 0 {
		t.Fatalf("no assignments yet, want tasks []: %s", filtered)
	}
	for id, who := range map[string]string{"mission-1": "agent-a", "mission-2": "agent-b", "mission-3": "agent-a"} {
		if res := assign080(t, s, id, who); !res.Accepted {
			t.Fatalf("%s: %+v", id, res)
		}
	}
	full := workspaceBody080(t, s, "")
	filtered := workspaceBody080(t, s, "?assignee=agent-a")
	ft := tasks080(t, filtered)
	if len(ft) != 2 || ft["mission-1"] == nil || ft["mission-3"] == nil {
		t.Fatalf("filtered tasks: %v", ft)
	}
	// exact match only: prefix/case variants match nothing.
	for _, q := range []string{"agent", "Agent-a", "agent-a%20", "nobody"} {
		if got := tasks080(t, workspaceBody080(t, s, "?assignee="+q)); len(got) != 0 {
			t.Fatalf("%q matched %v", q, got)
		}
	}
	var none struct {
		Body struct {
			Tasks json.RawMessage `json:"tasks"`
		} `json:"body"`
	}
	if err := json.Unmarshal(workspaceBody080(t, s, "?assignee=nobody"), &none); err != nil || string(none.Body.Tasks) != "[]" {
		t.Fatalf("unknown assignee tasks = %s err=%v", none.Body.Tasks, err)
	}
	// every non-task section byte-identical (counts stay global by design).
	var a, b map[string]json.RawMessage
	var ea, eb struct {
		Revision json.RawMessage `json:"revision"`
		Body     json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(full, &ea); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(filtered, &eb); err != nil {
		t.Fatal(err)
	}
	if string(ea.Revision) != string(eb.Revision) {
		t.Fatalf("revision diverged %s vs %s", ea.Revision, eb.Revision)
	}
	if err := json.Unmarshal(ea.Body, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(eb.Body, &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("key sets differ: %d vs %d", len(a), len(b))
	}
	for k := range a {
		if k == "tasks" {
			continue
		}
		if string(a[k]) != string(b[k]) {
			t.Fatalf("%s diverged under filter:\n%s\n%s", k, a[k], b[k])
		}
	}
	if !bytes.Contains(a["counts"], []byte(`"needsYou":1`)) || !bytes.Contains(a["counts"], []byte(`"running":1`)) {
		t.Fatalf("counts %s", a["counts"])
	}
	// the filtered tasks are the same bytes as the matching unfiltered ones.
	fullTasks := tasks080(t, full)
	for id, tk := range ft {
		x, _ := json.Marshal(tk)
		y, _ := json.Marshal(fullTasks[id])
		if string(x) != string(y) {
			t.Fatalf("%s differs: %s vs %s", id, x, y)
		}
	}
	// no parameter: unchanged by the filter (same bytes twice, all 3 tasks).
	if again := workspaceBody080(t, s, ""); !bytes.Equal(again, full) || len(tasks080(t, again)) != 3 {
		t.Fatalf("unfiltered body not stable")
	}
	// the SSE stream ignores the parameter: the snapshot frame is the full body.
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace/stream?assignee=agent-a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	cancel()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	frame := rec.Body.String()
	if !strings.HasPrefix(frame, "event: snapshot\ndata: ") || !strings.Contains(frame, strings.TrimSpace(string(full))) {
		t.Fatalf("stream frame must be the unfiltered snapshot:\n%s", frame)
	}
}

// A5: missionId may be an RHZ-073 handle; the event stores the ID.
func TestMissionAssignByHandleFRRHZ111(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-h", domain.MissionReady)
	handle := handleFor("m", "mission-h", handlePrefixLen)
	if tk := tasks080(t, workspaceBody080(t, s, ""))["mission-h"]; tk["handle"] != handle {
		t.Fatalf("handle fixture %v", tk)
	}
	res, err := RelayIntent(s, Intent{Kind: "mission.assign", MissionID: handle, Assignee: "agent-a"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("%+v err=%v", res, err)
	}
	last := s.All()[len(s.All())-1]
	if last.Type != "mission.assigned" || last.AggregateID != "mission-h" {
		t.Fatalf("event %+v", last)
	}
	if m := replay069(t, s, "mission-h"); m.Assignee != "agent-a" {
		t.Fatalf("replay %+v", m)
	}
}

// R1: real NDJSON journal — assign·reassign·reject, restart, replay and
// /v1/workspace (filtered and not) byte-identical across the restart.
func TestMissionAssignJournalRoundTripFRRHZ111(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	missionIn069(t, j, "mission-1", domain.MissionReady)
	missionIn069(t, j, "mission-2", domain.MissionReady, domain.MissionRunning)
	missionIn069(t, j, "mission-3")
	steps := []struct {
		in   Intent
		want bool
	}{
		{Intent{Kind: "mission.assign", MissionID: "mission-1", Assignee: "agent-a", Reason: "r"}, true},
		{Intent{Kind: "mission.assign", MissionID: "mission-2", Assignee: "agent-a"}, true},
		{Intent{Kind: "mission.assign", MissionID: "mission-2", Assignee: "agent-b"}, true},
		{Intent{Kind: "mission.cancel", MissionID: "mission-3"}, true},
		{Intent{Kind: "mission.assign", MissionID: "mission-3", Assignee: "agent-a"}, false},
		{Intent{Kind: "mission.assign", MissionID: "mission-1", Assignee: " "}, false},
	}
	for _, st := range steps {
		res, e := RelayIntent(j, st.in, "tester", noAuthority())
		if e != nil || res.Accepted != st.want {
			t.Fatalf("%+v: %+v err=%v", st.in, res, e)
		}
	}
	assigned := 0
	for _, e := range j.All() {
		switch e.Type {
		case "goal.created", "mission.created", "mission.transitioned":
		case "mission.assigned":
			assigned++
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
	}
	if assigned != 3 {
		t.Fatalf("assigned events %d, want 3", assigned)
	}
	want := map[string]domain.Mission{}
	for _, id := range []string{"mission-1", "mission-2", "mission-3"} {
		want[id] = replay069(t, j, id)
	}
	if want["mission-1"].Assignee != "agent-a" || want["mission-2"].Assignee != "agent-b" || want["mission-3"].Assignee != "" || want["mission-3"].State != domain.MissionCancelled {
		t.Fatalf("before restart: %+v", want)
	}
	full1 := workspaceBody080(t, j, "")
	mine1 := workspaceBody080(t, j, "?assignee=agent-a")
	if len(tasks080(t, mine1)) != 1 {
		t.Fatalf("filtered before restart: %s", mine1)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	for id, w := range want {
		got := replay069(t, j, id)
		if got.Assignee != w.Assignee || got.State != w.State || got.Revision != w.Revision {
			t.Fatalf("%s after restart: got %+v want %+v", id, got, w)
		}
	}
	if full2 := workspaceBody080(t, j, ""); !bytes.Equal(full1, full2) {
		t.Fatalf("workspace diverged after restart:\n%s\n%s", full1, full2)
	}
	if mine2 := workspaceBody080(t, j, "?assignee=agent-a"); !bytes.Equal(mine1, mine2) {
		t.Fatalf("filtered workspace diverged after restart:\n%s\n%s", mine1, mine2)
	}
}
