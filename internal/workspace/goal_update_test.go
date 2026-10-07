package workspace

// RHZ-087 FR-RHZ-117: goal.update relay → goal.updated 1건, 투영
// (/v1/workspace missions[].name·success, /v1/context?goal= description,
// mission detail items), 둘 다 빈값/terminal/미지 goal 거부·동일값 멱등
// (저널 바이트 불변), 핸들 goalId, HTTP wire(goalId/description/success
// json 키), 실 NDJSON 저널 왕복(legacy goal 스트림 + updated 스트림).
// 계획 U1–U5/R1.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

func goal087(t *testing.T, s events.Port, id, desc, success string) {
	t.Helper()
	if _, err := (mission.Service{Store: s}).CreateGoal(id, desc, success, ""); err != nil {
		t.Fatal(err)
	}
}

func update087(t *testing.T, s events.Port, in Intent) RelayResult {
	t.Helper()
	in.Kind = "goal.update"
	res, err := RelayIntent(s, in, "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// missions087 returns the raw mission (goal) objects of /v1/workspace by id.
func missions087(t *testing.T, s events.Port) map[string]map[string]any {
	t.Helper()
	var env struct {
		Body struct {
			Missions []map[string]any `json:"missions"`
		} `json:"body"`
	}
	if err := json.Unmarshal(workspaceBody080(t, s, ""), &env); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, m := range env.Body.Missions {
		out[m["id"].(string)] = m
	}
	return out
}

func contextGoal087(t *testing.T, s events.Port, id string) map[string]any {
	t.Helper()
	code, b := getContext(t, NewHTTP(s).Handler(), "?goal="+id)
	if code != http.StatusOK {
		t.Fatalf("context %d: %s", code, b)
	}
	var bundle struct {
		Goal map[string]any `json:"goal"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatal(err)
	}
	return bundle.Goal
}

func replayGoal087(t *testing.T, s events.Port, id string) domain.Goal {
	t.Helper()
	g, err := projector.ReplayGoal(s.List("goal", id))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// U1: description change → journal +1 goal.updated (no Success key),
// /v1/workspace missions[].name, /v1/context?goal= description and the
// mission detail (RHZ-072) all follow; the ID and success stay.
func TestGoalUpdateDescriptionProjectedFRRHZ117(t *testing.T) {
	s := &events.Store{}
	goal087(t, s, "goal-예시목표", "예시 목표", "예시 결과")
	goal087(t, s, "goal-other", "other", "o")
	n := len(s.All())
	if res := update087(t, s, Intent{GoalID: "goal-예시목표", Description: "예시 목표 (수정)", Reason: "다듬기"}); !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	if len(s.All()) != n+1 {
		t.Fatalf("journal grew %d, want 1", len(s.All())-n)
	}
	last := s.All()[len(s.All())-1]
	var p map[string]any
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if last.Type != "goal.updated" || last.AggregateType != "goal" || last.AggregateID != "goal-예시목표" || last.Revision != 2 || p["Description"] != "예시 목표 (수정)" || p["Reason"] != "다듬기" {
		t.Fatalf("event %+v %s", last, last.Payload)
	}
	if _, has := p["Success"]; has {
		t.Fatalf("description-only update must omit Success: %s", last.Payload)
	}
	if g := replayGoal087(t, s, "goal-예시목표"); g.ID != "goal-예시목표" || g.Description != "예시 목표 (수정)" || g.Success != "예시 결과" || g.State != domain.GoalActive {
		t.Fatalf("replay %+v", g)
	}
	ms := missions087(t, s)
	if ms["goal-예시목표"]["name"] != "예시 목표 (수정)" || ms["goal-예시목표"]["success"] != "예시 결과" {
		t.Fatalf("workspace mission: %v", ms["goal-예시목표"])
	}
	if ms["goal-other"]["name"] != "other" || len(ms) != 2 {
		t.Fatalf("other mission touched: %v", ms)
	}
	if g := contextGoal087(t, s, "goal-예시목표"); g["id"] != "goal-예시목표" || g["description"] != "예시 목표 (수정)" {
		t.Fatalf("context goal: %v", g)
	}
	// the adapter detail (RHZ-072/086) is built from missions[].name/success
	// of this same body, so it follows without code (see nodes.ts label = m.name).
}

// U2: success change → missions[].success follows, name unchanged, payload
// has no Description key; a second update to both fields is one event.
func TestGoalUpdateSuccessProjectedFRRHZ117(t *testing.T) {
	s := &events.Store{}
	goal087(t, s, "goal-sample", "샘플 목표", "성공 기준 추후")
	if res := update087(t, s, Intent{GoalID: "goal-sample", Success: "기준 A"}); !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	last := s.All()[len(s.All())-1]
	var p map[string]any
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if last.Type != "goal.updated" || p["Success"] != "기준 A" {
		t.Fatalf("event %+v %s", last, last.Payload)
	}
	if _, has := p["Description"]; has {
		t.Fatalf("success-only update must omit Description: %s", last.Payload)
	}
	if m := missions087(t, s)["goal-sample"]; m["success"] != "기준 A" || m["name"] != "샘플 목표" {
		t.Fatalf("workspace mission: %v", m)
	}
	n := len(s.All())
	if res := update087(t, s, Intent{GoalID: "goal-sample", Description: "샘플 목표 (수정)", Success: "기준 B"}); !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	if len(s.All()) != n+1 {
		t.Fatalf("journal grew %d, want 1", len(s.All())-n)
	}
	if m := missions087(t, s)["goal-sample"]; m["success"] != "기준 B" || m["name"] != "샘플 목표 (수정)" || m["id"] != "goal-sample" {
		t.Fatalf("workspace mission: %v", m)
	}
}

// U3: both fields empty/whitespace, terminal goal, missing/unknown goal →
// rejected with a reason, journal byte-unchanged; identical values →
// Accepted with zero writes (byte compare).
func TestGoalUpdateRejectedAndIdempotentNoWriteFRRHZ117(t *testing.T) {
	s := &events.Store{}
	goal087(t, s, "goal-live", "live", "done")
	goal087(t, s, "goal-cancelled", "c", "done")
	goal087(t, s, "goal-achieved", "a", "done")
	if res, err := RelayIntent(s, Intent{Kind: "goal.cancel", GoalID: "goal-cancelled"}, "tester", true); err != nil || !res.Accepted {
		t.Fatalf("cancel fixture: %+v err=%v", res, err)
	}
	if res, err := RelayIntent(s, Intent{Kind: "goal.resolve", GoalID: "goal-achieved"}, "tester", true); err != nil || !res.Accepted {
		t.Fatalf("resolve fixture: %+v err=%v", res, err)
	}
	before := journalBytes069(t, s)
	ws := workspaceBody080(t, s, "")
	cases := []struct {
		in   Intent
		want string
	}{
		{Intent{GoalID: "goal-live"}, "description or success required"},
		{Intent{GoalID: "goal-live", Description: "   ", Success: "\t\n"}, "description or success required"},
		{Intent{GoalID: "goal-cancelled", Description: "x"}, domain.ErrInvalidState.Error()},
		{Intent{GoalID: "goal-achieved", Success: "y"}, domain.ErrInvalidState.Error()},
		{Intent{GoalID: "", Description: "x"}, "goalId required"},
		{Intent{GoalID: "goal-nope", Description: "x"}, "goal event stream is empty"},
	}
	for _, c := range cases {
		res := update087(t, s, c.in)
		if res.Accepted || res.Reason != c.want {
			t.Fatalf("%+v: %+v (want reason %q)", c.in, res, c.want)
		}
	}
	if after := journalBytes069(t, s); after != before {
		t.Fatal("journal changed on rejected goal.update")
	}
	// identical values: accepted, zero writes, deterministic on resubmit.
	for i := 0; i < 2; i++ {
		for _, in := range []Intent{
			{GoalID: "goal-live", Description: "live", Success: "done"},
			{GoalID: "goal-live", Description: "live"},
			{GoalID: "goal-live", Success: " done "},
		} {
			if res := update087(t, s, in); !res.Accepted || res.Reason != "" {
				t.Fatalf("identical %+v: %+v", in, res)
			}
		}
	}
	if after := journalBytes069(t, s); after != before {
		t.Fatal("journal changed on identical goal.update")
	}
	if again := workspaceBody080(t, s, ""); !bytes.Equal(again, ws) {
		t.Fatalf("workspace changed:\n%s\n%s", ws, again)
	}
	if bytes.Contains([]byte(before), []byte("goal.updated")) {
		t.Fatal("no goal.updated expected")
	}
}

// U4: goalId may be an RHZ-073 handle; the event stores the ID.
func TestGoalUpdateByHandleFRRHZ117(t *testing.T) {
	s := &events.Store{}
	goal087(t, s, "goal-h", "h", "done")
	handle := handleFor("g", "goal-h", handlePrefixLen)
	if m := missions087(t, s)["goal-h"]; m["handle"] != handle {
		t.Fatalf("handle fixture %v", m)
	}
	if res := update087(t, s, Intent{GoalID: handle, Description: "renamed"}); !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	last := s.All()[len(s.All())-1]
	if last.Type != "goal.updated" || last.AggregateID != "goal-h" {
		t.Fatalf("event %+v", last)
	}
	if m := missions087(t, s)["goal-h"]; m["name"] != "renamed" || m["handle"] != handle || m["id"] != "goal-h" {
		t.Fatalf("mission after rename: %v", m)
	}
	// context by handle too.
	if g := contextGoal087(t, s, handle); g["description"] != "renamed" {
		t.Fatalf("context by handle: %v", g)
	}
}

// U5: HTTP wire — POST /v1/intent with the cockpit json keys
// (goalId/description/success/reason) lands on the relay; the adapter reads
// missions[].name/success unchanged (label = m.name in nodes.ts).
func TestGoalUpdateHTTPWireFRRHZ117(t *testing.T) {
	s := &events.Store{}
	goal087(t, s, "goal-w", "w", "done")
	h := NewHTTP(s).Handler()
	body := `{"kind":"goal.update","goalId":"goal-w","description":"wire name","success":"wire success","reason":"via http"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"Accepted":true`) {
		t.Fatalf("intent %d: %s", rec.Code, rec.Body.String())
	}
	if m := missions087(t, s)["goal-w"]; m["name"] != "wire name" || m["success"] != "wire success" {
		t.Fatalf("mission: %v", m)
	}
	var p struct{ Description, Success, Reason string }
	if err := json.Unmarshal(s.All()[len(s.All())-1].Payload, &p); err != nil || p.Reason != "via http" {
		t.Fatalf("payload %+v err=%v", p, err)
	}
	// a rejected wire update is 200 + reason, not an error, journal unchanged.
	before := journalBytes069(t, s)
	req = httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(`{"kind":"goal.update","goalId":"goal-w"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "description or success required") {
		t.Fatalf("intent %d: %s", rec.Code, rec.Body.String())
	}
	if journalBytes069(t, s) != before {
		t.Fatal("journal changed on rejected wire update")
	}
}

// R1: real NDJSON journal — a legacy goal stream (never updated) beside an
// updated one; rejects and idempotent resubmits; restart; replay and
// /v1/workspace byte-identical across the restart.
func TestGoalUpdateJournalRoundTripFRRHZ117(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	goal087(t, j, "goal-legacy", "legacy", "l")
	goal087(t, j, "goal-edit", "edit", "e")
	goal087(t, j, "goal-closed", "closed", "c")
	steps := []struct {
		in   Intent
		want bool
	}{
		{Intent{Kind: "goal.update", GoalID: "goal-edit", Description: "edit v2"}, true},
		{Intent{Kind: "goal.update", GoalID: "goal-edit", Success: "e2"}, true},
		{Intent{Kind: "goal.update", GoalID: "goal-edit", Description: "edit v2", Success: "e2"}, true}, // identical: no write
		{Intent{Kind: "goal.cancel", GoalID: "goal-closed"}, true},
		{Intent{Kind: "goal.update", GoalID: "goal-closed", Description: "x"}, false},
		{Intent{Kind: "goal.update", GoalID: "goal-legacy", Description: " "}, false},
	}
	for _, st := range steps {
		res, e := RelayIntent(j, st.in, "tester", true)
		if e != nil || res.Accepted != st.want {
			t.Fatalf("%+v: %+v err=%v", st.in, res, e)
		}
	}
	updated := 0
	for _, e := range j.All() {
		switch e.Type {
		case "goal.created", "goal.transitioned":
		case "goal.updated":
			updated++
			if e.AggregateID != "goal-edit" {
				t.Fatalf("goal.updated on %s", e.AggregateID)
			}
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
	}
	if updated != 2 {
		t.Fatalf("updated events %d, want 2", updated)
	}
	want := map[string]domain.Goal{}
	for _, id := range []string{"goal-legacy", "goal-edit", "goal-closed"} {
		want[id] = replayGoal087(t, j, id)
	}
	if want["goal-edit"].Description != "edit v2" || want["goal-edit"].Success != "e2" || want["goal-edit"].Revision != 3 || want["goal-legacy"].Description != "legacy" || want["goal-legacy"].Revision != 1 || want["goal-closed"].State != domain.GoalCancelled {
		t.Fatalf("before restart: %+v", want)
	}
	full1 := workspaceBody080(t, j, "")
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	for id, w := range want {
		if got := replayGoal087(t, j, id); got != w {
			t.Fatalf("%s after restart: got %+v want %+v", id, got, w)
		}
	}
	if full2 := workspaceBody080(t, j, ""); !bytes.Equal(full1, full2) {
		t.Fatalf("workspace diverged after restart:\n%s\n%s", full1, full2)
	}
	ms := missions087(t, j)
	if ms["goal-edit"]["name"] != "edit v2" || ms["goal-edit"]["success"] != "e2" || ms["goal-legacy"]["name"] != "legacy" || ms["goal-closed"]["name"] != "closed" || ms["goal-closed"]["state"] != "cancelled" {
		t.Fatalf("missions after restart: %v", ms)
	}
}
