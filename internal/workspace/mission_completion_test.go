package workspace

// RHZ-069 FR-RHZ-098: mission.complete / mission.fail — 운영자 미션 종결
// intent(goal.resolve/fail 짝, 결정 이벤트 append-only). 테스트 계획
// RHZ-069 C1/C2/C3/F1/T1/T2/R1/W1, 지정 Variant B
// (제한): running·waiting_for_result에서만 직접 전이, 그 외 비-terminal은
// "task.resume 먼저" 안내 거부(저널 쓰기 0), terminal은 ErrInvalidState.
// RHZ-079 (FR-RHZ-110)가 allow-list를 waiting_for_human·blocked로 확장
// (도메인 전이표 확장만) — 그 두 상태의 단언은 mission_terminal_exception_states_test.go C1/C2/C5로 이전.

import (
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

type terminalPayload069 struct{ To, DecisionID, Reason string }

func lastPayload069(t *testing.T, s events.Port) (events.Event, terminalPayload069) {
	t.Helper()
	all := s.All()
	last := all[len(all)-1]
	var p terminalPayload069
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return last, p
}

func journalBytes069(t *testing.T, s events.Port) string {
	t.Helper()
	b, err := json.Marshal(s.All())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func workspaceTaskStates069(t *testing.T, s events.Port) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/workspace %d", rec.Code)
	}
	var env struct {
		Body struct {
			Tasks []struct{ ID, State string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, tk := range env.Body.Tasks {
		out[tk.ID] = tk.State
	}
	return out
}

// missionIn069 is missionIn062 over an events.Port (journal round trip, R1).
func missionIn069(t *testing.T, s events.Port, id string, path ...domain.MissionState) {
	t.Helper()
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-"+id, "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create(id, "goal-"+id, "m", "done"); err != nil {
		t.Fatal(err)
	}
	rev := uint64(1)
	for _, to := range path {
		if _, err := ms.Transition(id, rev, to); err != nil {
			t.Fatalf("drive %s to %s: %v", id, to, err)
		}
		rev++
	}
}

func replay069(t *testing.T, s events.Port, id string) domain.Mission {
	t.Helper()
	m, err := projector.ReplayMission(s.List("mission", id))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// C1: running·waiting_for_result → 직접 Succeeded. 저널 증분 1, payload
// To/DecisionID/Reason, correlation 규약(verified·unverified), 재생
// TerminalDecisionID 일치, /v1/workspace "completed".
func TestMissionCompleteRunningFRRHZ098(t *testing.T) {
	cases := map[string][]domain.MissionState{
		"mission-running":       {domain.MissionReady, domain.MissionRunning},
		"mission-waitingresult": {domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult},
	}
	for id, path := range cases {
		s := &events.Store{}
		missionIn062(t, s, id, path...)
		n := len(s.All())
		res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: id, Reason: "done by reviewer"}, "tester", true)
		if err != nil || !res.Accepted {
			t.Fatalf("%s: %+v err=%v", id, res, err)
		}
		if got := len(s.All()); got != n+1 {
			t.Fatalf("%s: journal grew %d, want 1", id, got-n)
		}
		last, p := lastPayload069(t, s)
		if last.AggregateType != "mission" || last.AggregateID != id || last.Type != "mission.transitioned" {
			t.Fatalf("%s: event %+v", id, last)
		}
		if p.To != "succeeded" || p.DecisionID != "decision-mission.complete-"+id || p.Reason != "done by reviewer" {
			t.Fatalf("%s: payload %s", id, last.Payload)
		}
		if last.CorrelationID != "relay:tester" {
			t.Fatalf("%s: correlation %q", id, last.CorrelationID)
		}
		m := replay069(t, s, id)
		if m.State != domain.MissionSucceeded || m.TerminalDecisionID != p.DecisionID {
			t.Fatalf("%s: replayed %+v", id, m)
		}
		if st := workspaceTaskStates069(t, s)[id]; st != "completed" {
			t.Fatalf("%s: workspace state %q, want completed", id, st)
		}
	}
	// 변형 1: reason 없음 → payload Reason "" (Succeeded에 reason은 선택).
	s := &events.Store{}
	missionIn062(t, s, "mission-noreason", domain.MissionReady, domain.MissionRunning)
	if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-noreason"}, "tester", true); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if _, p := lastPayload069(t, s); p.To != "succeeded" || p.Reason != "" {
		t.Fatalf("no-reason payload %+v", p)
	}
	// 변형 2: 미검증 actor → correlation 프리픽스 규약(goal_lifecycle_test W2 규칙).
	s = &events.Store{}
	missionIn062(t, s, "mission-unverified", domain.MissionReady, domain.MissionRunning)
	if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-unverified"}, "op", false); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if last, _ := lastPayload069(t, s); last.CorrelationID != "relay:unverified-local-operator:op" {
		t.Fatalf("correlation %q", last.CorrelationID)
	}
}

// C2 (B-form): queued(planned·ready)에서 complete는 안내 거부 + 저널 불변;
// task.resume(→running) 후 complete 성공. 증분 resume 2(planned)/1(ready) +
// complete 1.
func TestMissionCompleteQueuedRejectedFRRHZ098(t *testing.T) {
	cases := []struct {
		id         string
		path       []domain.MissionState
		state      string
		resumeGrow int
	}{
		{"mission-planned", nil, "planned", 2},
		{"mission-ready", []domain.MissionState{domain.MissionReady}, "ready", 1},
	}
	for _, c := range cases {
		s := &events.Store{}
		missionIn062(t, s, c.id, c.path...)
		before := journalBytes069(t, s)
		res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: c.id}, "tester", true)
		if err != nil || res.Accepted || res.Reason != c.state+"에서 종결 불가: task.resume 먼저" {
			t.Fatalf("%s: %+v err=%v", c.id, res, err)
		}
		if journalBytes069(t, s) != before {
			t.Fatalf("%s: rejected complete changed journal", c.id)
		}
		n := len(s.All())
		if res, err = RelayIntent(s, Intent{Kind: "task.resume", TaskID: c.id}, "tester", true); err != nil || !res.Accepted {
			t.Fatalf("%s resume: %+v err=%v", c.id, res, err)
		}
		if got := len(s.All()) - n; got != c.resumeGrow {
			t.Fatalf("%s: resume grew %d, want %d", c.id, got, c.resumeGrow)
		}
		n = len(s.All())
		if res, err = RelayIntent(s, Intent{Kind: "mission.complete", MissionID: c.id}, "tester", true); err != nil || !res.Accepted {
			t.Fatalf("%s complete after resume: %+v err=%v", c.id, res, err)
		}
		if got := len(s.All()) - n; got != 1 {
			t.Fatalf("%s: complete grew %d, want 1", c.id, got)
		}
		if m := replay069(t, s, c.id); m.State != domain.MissionSucceeded {
			t.Fatalf("%s: replayed %+v", c.id, m)
		}
	}
}

// C3 (B-form): paused → 안내 거부, 저널 불변; task.resume(→running) 후
// complete 성공. waiting_for_human·blocked 항목은 RHZ-079 (FR-RHZ-110)에서
// 운영자 종결 경로가 열려 mission_terminal_exception_states_test.go C1/C2/C5로 이전됐다(해당 단언 —
// 거부문·resume ErrInvalidState·저널 불변 — 은 그쪽에서 계속 핀).
func TestMissionCompleteFromPausedHumanBlockedRejectedFRRHZ098(t *testing.T) {
	cases := map[string]struct {
		path  []domain.MissionState
		state string
	}{
		"mission-paused": {[]domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionPaused}, "paused"},
	}
	for id, c := range cases {
		s := &events.Store{}
		missionIn062(t, s, id, c.path...)
		before := journalBytes069(t, s)
		for _, kind := range []string{"mission.complete", "mission.fail"} {
			res, err := RelayIntent(s, Intent{Kind: kind, MissionID: id, Reason: "x"}, "tester", true)
			if err != nil || res.Accepted || res.Reason != c.state+"에서 종결 불가: task.resume 먼저" {
				t.Fatalf("%s %s: %+v err=%v", id, kind, res, err)
			}
		}
		if journalBytes069(t, s) != before {
			t.Fatalf("%s: rejected terminal changed journal", id)
		}
		res, err := RelayIntent(s, Intent{Kind: "task.resume", TaskID: id}, "tester", true)
		if err != nil {
			t.Fatal(err)
		}
		if c.state == "paused" {
			if !res.Accepted {
				t.Fatalf("%s resume: %+v", id, res)
			}
			if res, err = RelayIntent(s, Intent{Kind: "mission.complete", MissionID: id}, "tester", true); err != nil || !res.Accepted {
				t.Fatalf("%s complete after resume: %+v err=%v", id, res, err)
			}
			if m := replay069(t, s, id); m.State != domain.MissionSucceeded {
				t.Fatalf("%s: replayed %+v", id, m)
			}
			continue
		}
		t.Fatalf("%s: unexpected state in C3 fixture (waiting_for_human·blocked moved to mission_terminal_exception_states_test.go)", id)
	}
}

// F1: mission.fail은 reason 필수(빈·공백 거부, 저널 불변); reason 있으면
// Failed + payload To/Reason/DecisionID.
func TestMissionFailReasonRequiredFRRHZ098(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-f", domain.MissionReady, domain.MissionRunning)
	before := journalBytes069(t, s)
	for _, reason := range []string{"", "  "} {
		res, err := RelayIntent(s, Intent{Kind: "mission.fail", MissionID: "mission-f", Reason: reason}, "tester", true)
		if err != nil || res.Accepted || res.Reason != "reason required" {
			t.Fatalf("reason %q: %+v err=%v", reason, res, err)
		}
	}
	if journalBytes069(t, s) != before {
		t.Fatal("rejected fail changed journal")
	}
	res, err := RelayIntent(s, Intent{Kind: "mission.fail", MissionID: "mission-f", Reason: "flaky"}, "tester", true)
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if m := replay069(t, s, "mission-f"); m.State != domain.MissionFailed || m.TerminalDecisionID != "decision-mission.fail-mission-f" {
		t.Fatalf("replayed %+v", m)
	}
	if _, p := lastPayload069(t, s); p.To != "failed" || p.Reason != "flaky" || p.DecisionID != "decision-mission.fail-mission-f" {
		t.Fatalf("payload %+v", p)
	}
}

// T1: terminal 3종 × {complete, fail} 6조합 + complete 후 재-complete — 전부
// ErrInvalidState 거부, 저널 불변, 직후 /v1/workspace 200(poison 없음).
func TestMissionTerminalRejectedNoPoisonFRRHZ098(t *testing.T) {
	setups := map[string]func(t *testing.T, s *events.Store, id string){
		"cancelled": func(t *testing.T, s *events.Store, id string) {
			if res := cancelMission062(t, s, id); !res.Accepted {
				t.Fatal(res)
			}
		},
		"succeeded": func(t *testing.T, s *events.Store, id string) { drive062Terminal(t, s, id, domain.MissionSucceeded) },
		"failed":    func(t *testing.T, s *events.Store, id string) { drive062Terminal(t, s, id, domain.MissionFailed) },
	}
	for state, setup := range setups {
		s := &events.Store{}
		id := "mission-" + state
		missionIn062(t, s, id)
		setup(t, s, id)
		before := journalBytes069(t, s)
		for _, kind := range []string{"mission.complete", "mission.fail"} {
			res, err := RelayIntent(s, Intent{Kind: kind, MissionID: id, Reason: "x"}, "tester", true)
			if err != nil || res.Accepted || res.Reason != domain.ErrInvalidState.Error() {
				t.Fatalf("%s on %s: %+v err=%v", kind, state, res, err)
			}
		}
		if journalBytes069(t, s) != before {
			t.Fatalf("rejected terminal on %s changed journal", state)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
		rec := httptest.NewRecorder()
		NewHTTP(s).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("workspace %d after rejected terminal on %s (poison?)", rec.Code, state)
		}
	}
	// 재제출: complete 후 complete — 멱등 수용 없음(mission.cancel과 동일 의미론).
	s := &events.Store{}
	missionIn062(t, s, "mission-x", domain.MissionReady, domain.MissionRunning)
	if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-x"}, "tester", true); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	before := journalBytes069(t, s)
	res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-x"}, "tester", true)
	if err != nil || res.Accepted || res.Reason != domain.ErrInvalidState.Error() {
		t.Fatalf("re-complete: %+v err=%v", res, err)
	}
	if journalBytes069(t, s) != before {
		t.Fatal("re-complete changed journal")
	}
	if st := workspaceTaskStates069(t, s)["mission-x"]; st != "completed" {
		t.Fatalf("workspace state %q", st)
	}
}

// T2: missionId 없음(fail+빈 reason이어도 missionId가 먼저)·미존재 거부, 저널 불변.
func TestMissionCompleteUnknownTargetRejectedFRRHZ098(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-x", domain.MissionReady, domain.MissionRunning)
	before := journalBytes069(t, s)
	for _, c := range []struct {
		in   Intent
		want string
	}{
		{Intent{Kind: "mission.complete"}, "missionId required"},
		{Intent{Kind: "mission.fail"}, "missionId required"},
		{Intent{Kind: "mission.complete", MissionID: "mission-ghost"}, "mission event stream is empty"},
		{Intent{Kind: "mission.fail", MissionID: "mission-ghost", Reason: "x"}, "mission event stream is empty"},
	} {
		res, err := RelayIntent(s, c.in, "tester", true)
		if err != nil || res.Accepted || res.Reason != c.want {
			t.Fatalf("%+v: %+v err=%v", c.in, res, err)
		}
	}
	if journalBytes069(t, s) != before {
		t.Fatal("rejected intents changed journal")
	}
}

// fixture069Journal: C1·F1·C2·C3(B) 시나리오를 journal에 적용(R1·W1 공용).
func fixture069Journal(t *testing.T, j *journal.Journal) []string {
	t.Helper()
	ids := []string{"mission-c1", "mission-c1w", "mission-f1", "mission-c2", "mission-c3"}
	missionIn069(t, j, "mission-c1", domain.MissionReady, domain.MissionRunning)
	missionIn069(t, j, "mission-c1w", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult)
	missionIn069(t, j, "mission-f1", domain.MissionReady, domain.MissionRunning)
	missionIn069(t, j, "mission-c2")
	missionIn069(t, j, "mission-c3", domain.MissionReady, domain.MissionRunning, domain.MissionPaused)
	steps := []struct {
		in   Intent
		want bool
	}{
		{Intent{Kind: "mission.complete", MissionID: "mission-c1", Reason: "done"}, true},
		{Intent{Kind: "mission.complete", MissionID: "mission-c1w"}, true},
		{Intent{Kind: "mission.fail", MissionID: "mission-f1", Reason: "flaky"}, true},
		{Intent{Kind: "mission.complete", MissionID: "mission-c2"}, false},
		{Intent{Kind: "task.resume", TaskID: "mission-c2"}, true},
		{Intent{Kind: "mission.complete", MissionID: "mission-c2"}, true},
		{Intent{Kind: "mission.fail", MissionID: "mission-c3", Reason: "x"}, false},
		{Intent{Kind: "task.resume", TaskID: "mission-c3"}, true},
		{Intent{Kind: "mission.fail", MissionID: "mission-c3", Reason: "gave up"}, true},
	}
	for _, st := range steps {
		res, err := RelayIntent(j, st.in, "tester", true)
		if err != nil || res.Accepted != st.want {
			t.Fatalf("%+v: %+v err=%v", st.in, res, err)
		}
	}
	return ids
}

// R1: 실 NDJSON journal 왕복 — Close → 재Open 후 각 mission의 state·Revision·
// TerminalDecisionID와 /v1/workspace 바이트가 동일.
func TestMissionCompleteJournalRoundTripFRRHZ098(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ids := fixture069Journal(t, j)
	want := map[string]domain.Mission{}
	for _, id := range ids {
		want[id] = replay069(t, j, id)
	}
	wantStates := map[string]domain.MissionState{"mission-c1": domain.MissionSucceeded, "mission-c1w": domain.MissionSucceeded, "mission-f1": domain.MissionFailed, "mission-c2": domain.MissionSucceeded, "mission-c3": domain.MissionFailed}
	for id, st := range wantStates {
		if want[id].State != st || want[id].TerminalDecisionID == "" {
			t.Fatalf("%s before restart: %+v", id, want[id])
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	NewHTTP(j).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/workspace %d", rec.Code)
	}
	b1 := rec.Body.String()
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	for _, id := range ids {
		got := replay069(t, j, id)
		w := want[id]
		if got.State != w.State || got.Revision != w.Revision || got.TerminalDecisionID != w.TerminalDecisionID {
			t.Fatalf("%s after restart: got %+v want %+v", id, got, w)
		}
	}
	rec = httptest.NewRecorder()
	NewHTTP(j).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != b1 {
		t.Fatalf("workspace diverged after restart: %d\n%s\n%s", rec.Code, b1, rec.Body.String())
	}
}

// W1: writer boundary — R1 시나리오가 남긴 mission 전이 이벤트 전수 스캔:
// 신규 이벤트 타입 0(mission.transitioned만), succeeded/failed To는 전부
// DecisionID 보유(projector "terminal decision missing" 정합).
func TestMissionCompleteWriterBoundaryFRRHZ098(t *testing.T) {
	j, err := journal.Open(t.TempDir() + "/j.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	fixture069Journal(t, j)
	terminals := 0
	for _, e := range j.All() {
		switch e.Type {
		case "goal.created", "mission.created", "mission.transitioned":
		default:
			t.Fatalf("unexpected event type %q (new type?)", e.Type)
		}
		if e.Type != "mission.transitioned" {
			continue
		}
		var p terminalPayload069
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.To == "succeeded" || p.To == "failed" {
			terminals++
			if p.DecisionID == "" || !strings.HasPrefix(p.DecisionID, "decision-mission.") {
				t.Fatalf("terminal without decision id: %+v %s", e, e.Payload)
			}
			if e.CorrelationID != "relay:tester" {
				t.Fatalf("terminal correlation %q", e.CorrelationID)
			}
		}
	}
	if terminals != 5 {
		t.Fatalf("terminal events %d, want 5", terminals)
	}
}

// C1' (FR-RHZ-098): 명시 CorrelationID는 relayCorrelation의
// given 분기로 그대로 실린다 — mission.complete와 goal.resolve가 같은 헬퍼를
// 공유하므로 두 표면 모두 핀한다(이전엔 어느 쪽도 이 분기를 검사하지 않았다).
func TestMissionCompleteExplicitCorrelationFRRHZ098(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-corr", domain.MissionReady, domain.MissionRunning)
	res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-corr", CorrelationID: "corr-x"}, "tester", true)
	if err != nil || !res.Accepted {
		t.Fatalf("complete: %+v %v", res, err)
	}
	last, _ := lastPayload069(t, s)
	if last.CorrelationID != "corr-x" {
		t.Fatalf("mission correlation=%q want corr-x", last.CorrelationID)
	}
	res, err = RelayIntent(s, Intent{Kind: "goal.resolve", GoalID: "goal-mission-corr", CorrelationID: "corr-y"}, "tester", true)
	if err != nil || !res.Accepted {
		t.Fatalf("goal.resolve: %+v %v", res, err)
	}
	all := s.All()
	if g := all[len(all)-1]; g.AggregateType != "goal" || g.CorrelationID != "corr-y" {
		t.Fatalf("goal correlation=%q (type %s) want corr-y", g.CorrelationID, g.AggregateType)
	}
}
