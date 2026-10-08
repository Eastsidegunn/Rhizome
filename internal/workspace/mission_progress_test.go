package workspace

// RHZ-082 FR-RHZ-113: mission.progress relay → surface.progressed 1건, 상태
// 전이 없음. 투영(/v1/workspace tasks[].currentAction/progress/hasProgress/
// blockedReason, /v1/context task 동일 3필드), 범위 밖·빈 intent 거부(저널
// 불변), 상태 allow-list(running·waiting·blocked), blockedReason은 표시만,
// 동일 반복은 쓰기 0, NDJSON 왕복. 계획 P1–P5/R1.

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/surface"
)

func progress082(t *testing.T, s events.Port, id string, in Intent) RelayResult {
	t.Helper()
	in.Kind, in.MissionID = "mission.progress", id
	res, err := RelayIntent(s, in, "tester", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func progressedCount082(s events.Port, id string) int {
	n := 0
	for _, e := range s.List("surface", "surface-"+id) {
		if e.Type == "surface.progressed" {
			n++
		}
	}
	return n
}

func contextTask082(t *testing.T, s events.Port, id string) map[string]any {
	t.Helper()
	code, b := getContext(t, NewHTTP(s).Handler(), "?task="+id)
	if code != http.StatusOK {
		t.Fatalf("context %d: %s", code, b)
	}
	var env struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, []byte(`,"steps":[]}`+"\n")) {
		t.Fatalf("RHZ-068 steps pin broken: %s", b)
	}
	return env.Task
}

func f082(v float64) *float64 { return &v }

// P1: running → {currentAction, 0.5} → workspace tasks[] and /v1/context
// carry the same values, hasProgress true, state unchanged, exactly one
// surface.progressed event and no mission event.
func TestMissionProgressProjectedFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-run", domain.MissionReady, domain.MissionRunning)
	before := len(s.List("mission", "mission-run"))
	if res := progress082(t, s, "mission-run", Intent{CurrentAction: "writing tests", Progress: f082(.5)}); !res.Accepted {
		t.Fatalf("progress: %+v", res)
	}
	if progressedCount082(s, "mission-run") != 1 || len(s.List("mission", "mission-run")) != before {
		t.Fatal("expected exactly one surface.progressed and no mission event")
	}
	if st := missionState062(t, s, "mission-run"); st != domain.MissionRunning {
		t.Fatalf("state changed to %s", st)
	}
	tk := tasks080(t, workspaceBody080(t, s, ""))["mission-run"]
	if tk["currentAction"] != "writing tests" || tk["progress"] != .5 || tk["hasProgress"] != true || tk["state"] != "running" {
		t.Fatalf("task: %v", tk)
	}
	if _, ok := tk["blockedReason"]; ok {
		t.Fatalf("no blockedReason expected: %v", tk)
	}
	ct := contextTask082(t, s, "mission-run")
	if ct["currentAction"] != "writing tests" || ct["progress"] != .5 || ct["hasProgress"] != true {
		t.Fatalf("context task: %v", ct)
	}
}

// P1': progress 0 is recorded as 0 (hasProgress true, pointer semantics) and
// progress 1 is accepted; a currentAction-only update keeps the progress.
func TestMissionProgressZeroAndOneFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-run", domain.MissionReady, domain.MissionRunning)
	if res := progress082(t, s, "mission-run", Intent{Progress: f082(0)}); !res.Accepted {
		t.Fatalf("zero: %+v", res)
	}
	tk := tasks080(t, workspaceBody080(t, s, ""))["mission-run"]
	if tk["hasProgress"] != true {
		t.Fatalf("progress 0 must set hasProgress: %v", tk)
	}
	if _, ok := tk["progress"]; ok && tk["progress"] != float64(0) {
		t.Fatalf("progress 0: %v", tk)
	}
	if res := progress082(t, s, "mission-run", Intent{CurrentAction: "step 2"}); !res.Accepted {
		t.Fatalf("action only: %+v", res)
	}
	if res := progress082(t, s, "mission-run", Intent{Progress: f082(1)}); !res.Accepted {
		t.Fatalf("one: %+v", res)
	}
	tk = tasks080(t, workspaceBody080(t, s, ""))["mission-run"]
	if tk["progress"] != float64(1) || tk["currentAction"] != "step 2" || tk["hasProgress"] != true {
		t.Fatalf("task: %v", tk)
	}
	// Wire shape: "progress":0 via HTTP JSON is recorded (pointer, not omitted).
	var in Intent
	if err := json.Unmarshal([]byte(`{"kind":"mission.progress","missionId":"mission-run","progress":0}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Progress == nil || *in.Progress != 0 {
		t.Fatalf("progress 0 must decode as present: %+v", in.Progress)
	}
	var absent Intent
	if err := json.Unmarshal([]byte(`{"kind":"mission.progress","missionId":"mission-run"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Progress != nil {
		t.Fatal("absent progress must decode as nil")
	}
}

// P2: out-of-range / NaN / empty intent / oversize text are rejected with a
// reason and the journal is byte-identical.
func TestMissionProgressValidationNoWriteFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-run", domain.MissionReady, domain.MissionRunning)
	before := journalBytes069(t, s)
	cases := []struct {
		in   Intent
		want string
	}{
		{Intent{Progress: f082(1.2)}, "progress must be within [0,1]"},
		{Intent{Progress: f082(-0.1)}, "progress must be within [0,1]"},
		{Intent{Progress: f082(math.NaN())}, "progress must be within [0,1]"},
		{Intent{CurrentAction: "x", Progress: f082(math.Inf(1))}, "progress must be within [0,1]"},
		{Intent{}, "at least one of currentAction, progress, blockedReason required"},
		{Intent{CurrentAction: "   ", BlockedReason: " "}, "at least one of currentAction, progress, blockedReason required"},
		{Intent{CurrentAction: strings.Repeat("a", surface.MaxProgressText+1)}, "currentAction/blockedReason exceed 1KiB limit"},
		{Intent{BlockedReason: strings.Repeat("b", surface.MaxProgressText+1)}, "currentAction/blockedReason exceed 1KiB limit"},
	}
	for _, c := range cases {
		res := progress082(t, s, "mission-run", c.in)
		if res.Accepted || res.Reason != c.want {
			t.Fatalf("%+v: %+v (want %q)", c.in, res, c.want)
		}
	}
	if res, err := RelayIntent(s, Intent{Kind: "mission.progress", CurrentAction: "x"}, "tester", noAuthority()); err != nil || res.Accepted || res.Reason != "missionId required" {
		t.Fatalf("missing id: %+v %v", res, err)
	}
	if after := journalBytes069(t, s); after != before {
		t.Fatal("journal changed on rejected mission.progress")
	}
}

// P3: planned/ready/paused → "task.resume 먼저"; terminal → ErrInvalidState;
// unknown mission → reason. Zero writes. waiting_for_result/human and blocked
// accept.
func TestMissionProgressStateAllowListFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-planned")
	missionIn062(t, s, "mission-ready", domain.MissionReady)
	missionIn062(t, s, "mission-paused", domain.MissionReady, domain.MissionRunning, domain.MissionPaused)
	missionIn062(t, s, "mission-done", domain.MissionReady)
	if res := cancelMission062(t, s, "mission-done"); !res.Accepted {
		t.Fatalf("cancel fixture: %+v", res)
	}
	before := journalBytes069(t, s)
	cases := []struct{ id, want string }{
		{"mission-planned", "planned에서 진척 기록 불가: task.resume 먼저"},
		{"mission-ready", "ready에서 진척 기록 불가: task.resume 먼저"},
		{"mission-paused", "paused에서 진척 기록 불가: task.resume 먼저"},
		{"mission-done", domain.ErrInvalidState.Error()},
		{"mission-nope", "mission event stream is empty"},
	}
	for _, c := range cases {
		res := progress082(t, s, c.id, Intent{CurrentAction: "x", Progress: f082(.1)})
		if res.Accepted || res.Reason != c.want {
			t.Fatalf("%s: %+v (want %q)", c.id, res, c.want)
		}
	}
	if after := journalBytes069(t, s); after != before {
		t.Fatal("journal changed on rejected mission.progress")
	}
	for _, id := range []string{"mission-done", "mission-planned", "mission-ready", "mission-paused"} {
		if n := len(s.List("surface", "surface-"+id)); n != 0 {
			t.Fatalf("%s: surface stream written (%d)", id, n)
		}
	}
	missionIn062(t, s, "mission-wr", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult)
	missionIn062(t, s, "mission-wh", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	missionIn062(t, s, "mission-bl", domain.MissionReady, domain.MissionRunning, domain.MissionBlocked)
	for _, id := range []string{"mission-wr", "mission-wh", "mission-bl"} {
		st := missionState062(t, s, id)
		if res := progress082(t, s, id, Intent{CurrentAction: "still on it"}); !res.Accepted {
			t.Fatalf("%s: %+v", id, res)
		}
		if missionState062(t, s, id) != st {
			t.Fatalf("%s: state moved", id)
		}
	}
}

// P4: blockedReason is display-only — state stays running; once a real
// blocked transition lands, the transition reason wins in the DTO; after the
// mission leaves blocked the display reason is visible again.
func TestMissionProgressBlockedReasonDisplayOnlyFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-run", domain.MissionReady, domain.MissionRunning)
	if res := progress082(t, s, "mission-run", Intent{BlockedReason: "waiting on CI"}); !res.Accepted {
		t.Fatalf("blockedReason: %+v", res)
	}
	if st := missionState062(t, s, "mission-run"); st != domain.MissionRunning {
		t.Fatalf("display-only blockedReason moved state to %s", st)
	}
	tk := tasks080(t, workspaceBody080(t, s, ""))["mission-run"]
	if tk["blockedReason"] != "waiting on CI" || tk["state"] != "running" || tk["hasProgress"] != false {
		t.Fatalf("task: %v", tk)
	}
	m := replay069(t, s, "mission-run")
	if m.BlockedReason != "" {
		t.Fatalf("domain BlockedReason must stay empty: %q", m.BlockedReason)
	}
	if _, err := (mission.Service{Store: s}).TransitionWithReason("mission-run", m.Revision, domain.MissionBlocked, "dependency missing"); err != nil {
		t.Fatal(err)
	}
	tk = tasks080(t, workspaceBody080(t, s, ""))["mission-run"]
	if tk["blockedReason"] != "dependency missing" || tk["state"] != "blocked" {
		t.Fatalf("transition reason must win while blocked: %v", tk)
	}
	m = replay069(t, s, "mission-run")
	if _, err := (mission.Service{Store: s}).Transition("mission-run", m.Revision, domain.MissionReady); err != nil {
		t.Fatal(err)
	}
	tk = tasks080(t, workspaceBody080(t, s, ""))["mission-run"]
	if tk["blockedReason"] != "waiting on CI" || tk["state"] != "queued" {
		t.Fatalf("display reason after unblock: %v", tk)
	}
}

// P5: an identical repeat is accepted with zero writes (deterministic); a
// changed field writes exactly one more event.
func TestMissionProgressIdempotentFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-run", domain.MissionReady, domain.MissionRunning)
	in := Intent{CurrentAction: "step 1", Progress: f082(.25), BlockedReason: "r"}
	if res := progress082(t, s, "mission-run", in); !res.Accepted {
		t.Fatalf("first: %+v", res)
	}
	before := journalBytes069(t, s)
	for i := 0; i < 3; i++ {
		if res := progress082(t, s, "mission-run", in); !res.Accepted {
			t.Fatalf("repeat %d: %+v", i, res)
		}
	}
	if res := progress082(t, s, "mission-run", Intent{Progress: f082(.25)}); !res.Accepted {
		t.Fatalf("subset repeat: %+v", res)
	}
	if journalBytes069(t, s) != before || progressedCount082(s, "mission-run") != 1 {
		t.Fatal("identical repeat must not write")
	}
	if res := progress082(t, s, "mission-run", Intent{Progress: f082(.3)}); !res.Accepted {
		t.Fatalf("changed: %+v", res)
	}
	if progressedCount082(s, "mission-run") != 2 {
		t.Fatal("changed progress must write one event")
	}
}

// R1: NDJSON journal round trip — workspace and context bytes identical after
// reopen; a legacy surface stream (progress_reported/instructed only) and a
// mission with no surface stream replay unchanged.
func TestMissionProgressJournalRoundTripFRRHZ113(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	missionIn069(t, j, "mission-1", domain.MissionReady, domain.MissionRunning)
	missionIn069(t, j, "mission-2", domain.MissionReady, domain.MissionRunning)
	missionIn069(t, j, "mission-3", domain.MissionReady, domain.MissionRunning)
	// legacy surface stream on mission-2 (pre-RHZ-082 event types only)
	if _, err = (surface.Service{Store: j}).ReportProgress("mission-2", "legacy step", f082(.4), "coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err = (surface.Service{Store: j}).Instruct("mission-2", "keep going", "test-operator", noAuthority(), "c-1"); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		in   Intent
		want bool
	}{
		{Intent{Kind: "mission.progress", MissionID: "mission-1", CurrentAction: "a", Progress: f082(.5)}, true},
		{Intent{Kind: "mission.progress", MissionID: "mission-1", BlockedReason: "flaky"}, true},
		{Intent{Kind: "mission.progress", MissionID: "mission-1", Progress: f082(2)}, false},
		{Intent{Kind: "mission.progress", MissionID: "mission-3", CurrentAction: "x"}, true},
		{Intent{Kind: "mission.complete", MissionID: "mission-3"}, true},
		{Intent{Kind: "mission.progress", MissionID: "mission-3", CurrentAction: "y"}, false},
	}
	for _, st := range steps {
		res, e := RelayIntent(j, st.in, "tester", noAuthority())
		if e != nil || res.Accepted != st.want {
			t.Fatalf("%+v: %+v err=%v", st.in, res, e)
		}
	}
	progressed := 0
	for _, e := range j.All() {
		switch e.Type {
		case "goal.created", "mission.created", "mission.transitioned", "surface.progress_reported", "surface.instructed":
		case "surface.progressed":
			progressed++
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
	}
	if progressed != 3 {
		t.Fatalf("progressed events %d, want 3", progressed)
	}
	ws1 := workspaceBody080(t, j, "")
	tk := tasks080(t, ws1)
	if tk["mission-1"]["currentAction"] != "a" || tk["mission-1"]["progress"] != .5 || tk["mission-1"]["blockedReason"] != "flaky" || tk["mission-2"]["currentAction"] != "legacy step" || tk["mission-2"]["progress"] != .4 {
		t.Fatalf("before restart: %v", tk)
	}
	ctx1 := contextTask082(t, j, "mission-1")
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	if j, err = openTestJournal(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if ws2 := workspaceBody080(t, j, ""); !bytes.Equal(ws1, ws2) {
		t.Fatalf("workspace diverged after restart:\n%s\n%s", ws1, ws2)
	}
	ctx2 := contextTask082(t, j, "mission-1")
	if ctx1["currentAction"] != ctx2["currentAction"] || ctx1["progress"] != ctx2["progress"] || ctx1["hasProgress"] != ctx2["hasProgress"] || ctx2["hasProgress"] != true {
		t.Fatalf("context diverged: %v vs %v", ctx1, ctx2)
	}
	sv, err := (surface.Service{Store: j}).ByMission("mission-1")
	if err != nil || sv.Revision != 2 || sv.ProgressSource != "relay:unverified-local-operator:tester" || sv.BlockedReason != "flaky" {
		t.Fatalf("surface replay: %+v err=%v", sv, err)
	}
}

// P6 (FR-RHZ-113): 표시용 blockedReason은 live 상태에서만 투영되고
// terminal 뒤에는 사라진다(저널엔 남음). probe: Snapshot의 상태 가드 제거 → FAIL.
func TestMissionProgressBlockedReasonNotAfterTerminalFRRHZ113(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-p6", domain.MissionReady, domain.MissionRunning)
	if res := progress082(t, s, "mission-p6", Intent{BlockedReason: "waiting on CI"}); !res.Accepted {
		t.Fatalf("progress: %+v", res)
	}
	if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-p6"}, "tester", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("complete: %v %+v", err, res)
	}
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range p.Tasks {
		if task.ID == "mission-p6" {
			if task.State != "completed" || task.BlockedReason != "" {
				t.Fatalf("display reason must not outlive the mission: %+v", task)
			}
			return
		}
	}
	t.Fatal("task missing")
}
