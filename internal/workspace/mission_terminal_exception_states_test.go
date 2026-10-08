package workspace

// RHZ-079 FR-RHZ-110: waiting_for_human·blocked 운영자 종결 — mission.complete /
// mission.fail이 두 상태에서 직접 ApplyMissionDecision 1건으로 닫는다(중간
// running/ready 날조 0). 계획 C1/C1-f/C2/C2'/C3/C4/C5/R1. planned/ready/paused
// 안내 거부·terminal ErrInvalidState·task.resume 가드는 불변(C4/C5가 재핀).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
)

// blockedIn079: goal+mission → ready → blocked(reason) over an events.Port.
func blockedIn079(t *testing.T, s events.Port, id, reason string) {
	t.Helper()
	missionIn069(t, s, id, domain.MissionReady)
	if _, err := (mission.Service{Store: s}).TransitionWithReason(id, 2, domain.MissionBlocked, reason); err != nil {
		t.Fatal(err)
	}
	if m := replay069(t, s, id); m.State != domain.MissionBlocked || m.BlockedReason != reason {
		t.Fatalf("%s: fixture %+v", id, m)
	}
}

func workspaceCounts079(t *testing.T, s events.Port) (needsYou, blocked int, attention int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/workspace %d", rec.Code)
	}
	var env struct {
		Body struct {
			Counts    struct{ NeedsYou, Blocked int }
			Attention []json.RawMessage
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Body.Counts.NeedsYou, env.Body.Counts.Blocked, len(env.Body.Attention)
}

// C1: waiting_for_human → mission.complete → succeeded. 저널 증분 정확히 1,
// payload To/DecisionID/Reason, correlation(verified·unverified), 재생
// Attention=false, /v1/workspace "completed", counts.needsYou 1→0.
// C1-f: 같은 상태에서 mission.fail → failed.
func TestMissionCompleteFromWaitingHumanFRRHZ110(t *testing.T) {
	const id = "mission-h"
	path := []domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman}
	for _, c := range []struct {
		kind  string
		to    domain.MissionState
		state string
	}{
		{"mission.complete", domain.MissionSucceeded, "completed"},
		{"mission.fail", domain.MissionFailed, "failed"},
	} {
		s := &events.Store{}
		missionIn062(t, s, id, path...)
		if ny, _, att := workspaceCounts079(t, s); ny != 1 || att != 1 {
			t.Fatalf("%s: needsYou=%d attention=%d before, want 1/1", c.kind, ny, att)
		}
		n := len(s.All())
		res, err := RelayIntent(s, Intent{Kind: c.kind, MissionID: id, Reason: "closed by reviewer"}, "tester", noAuthority())
		if err != nil || !res.Accepted {
			t.Fatalf("%s: %+v err=%v", c.kind, res, err)
		}
		if got := len(s.All()); got != n+1 {
			t.Fatalf("%s: journal grew %d, want exactly 1 (no intermediate running)", c.kind, got-n)
		}
		last, p := lastPayload069(t, s)
		if last.AggregateType != "mission" || last.AggregateID != id || last.Type != "mission.transitioned" {
			t.Fatalf("%s: event %+v", c.kind, last)
		}
		if p.To != string(c.to) || p.DecisionID != "decision-"+c.kind+"-"+id || p.Reason != "closed by reviewer" {
			t.Fatalf("%s: payload %s", c.kind, last.Payload)
		}
		if last.CorrelationID != "relay:unverified-local-operator:tester" {
			t.Fatalf("%s: correlation %q", c.kind, last.CorrelationID)
		}
		m := replay069(t, s, id)
		if m.State != c.to || m.TerminalDecisionID != p.DecisionID || m.Revision != 5 {
			t.Fatalf("%s: replayed %+v", c.kind, m)
		}
		if st := workspaceTaskStates069(t, s)[id]; st != c.state {
			t.Fatalf("%s: workspace state %q, want %s", c.kind, st, c.state)
		}
		if ny, _, att := workspaceCounts079(t, s); ny != 0 || att != 0 {
			t.Fatalf("%s: needsYou=%d attention=%d after, want 0/0", c.kind, ny, att)
		}
	}
	// 미검증 actor → correlation 프리픽스 규약(069 변형 2와 동일).
	s := &events.Store{}
	missionIn062(t, s, "mission-h-unv", path...)
	if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-h-unv"}, "op", noAuthority()); err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if last, p := lastPayload069(t, s); last.CorrelationID != "relay:unverified-local-operator:op" || p.Reason != "" {
		t.Fatalf("unverified: corr=%q payload=%+v", last.CorrelationID, p)
	}
}

// C2: blocked → mission.fail: reason 공백은 "reason required"(저널 불변);
// reason 있으면 +1, To:"failed", 재생 Failed·BlockedReason "" , workspace
// "failed", counts.blocked 1→0. C2': blocked → mission.complete → succeeded.
func TestMissionFailFromBlockedFRRHZ110(t *testing.T) {
	s := &events.Store{}
	blockedIn079(t, s, "mission-b", "dependency unavailable")
	if _, bl, _ := workspaceCounts079(t, s); bl != 1 {
		t.Fatalf("blocked count %d before, want 1", bl)
	}
	before := journalBytes069(t, s)
	for _, reason := range []string{"", "  "} {
		res, err := RelayIntent(s, Intent{Kind: "mission.fail", MissionID: "mission-b", Reason: reason}, "tester", noAuthority())
		if err != nil || res.Accepted || res.Reason != "reason required" {
			t.Fatalf("reason %q: %+v err=%v", reason, res, err)
		}
	}
	if journalBytes069(t, s) != before {
		t.Fatal("rejected fail changed journal")
	}
	n := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "mission.fail", MissionID: "mission-b", Reason: "dependency never came"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if got := len(s.All()); got != n+1 {
		t.Fatalf("journal grew %d, want exactly 1 (no intermediate ready/running)", got-n)
	}
	last, p := lastPayload069(t, s)
	if p.To != "failed" || p.DecisionID != "decision-mission.fail-mission-b" || p.Reason != "dependency never came" || last.CorrelationID != "relay:unverified-local-operator:tester" {
		t.Fatalf("payload %+v corr=%q", p, last.CorrelationID)
	}
	m := replay069(t, s, "mission-b")
	if m.State != domain.MissionFailed || m.TerminalDecisionID != p.DecisionID || m.BlockedReason != "" || m.Revision != 4 {
		t.Fatalf("replayed %+v", m)
	}
	if st := workspaceTaskStates069(t, s)["mission-b"]; st != "failed" {
		t.Fatalf("workspace state %q, want failed", st)
	}
	if _, bl, _ := workspaceCounts079(t, s); bl != 0 {
		t.Fatalf("blocked count %d after, want 0", bl)
	}

	// C2': blocked → complete → succeeded (넓은 변형: 외부에서 의존성이 풀려
	// 결과가 난 blocked mission을 닫는 길).
	s = &events.Store{}
	blockedIn079(t, s, "mission-b2", "dependency unavailable")
	n = len(s.All())
	res, err = RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-b2"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatal(res, err)
	}
	if got := len(s.All()); got != n+1 {
		t.Fatalf("complete grew %d, want 1", got-n)
	}
	if _, p := lastPayload069(t, s); p.To != "succeeded" || p.DecisionID != "decision-mission.complete-mission-b2" || p.Reason != "" {
		t.Fatalf("payload %+v", p)
	}
	if m := replay069(t, s, "mission-b2"); m.State != domain.MissionSucceeded || m.BlockedReason != "" {
		t.Fatalf("replayed %+v", m)
	}
	if st := workspaceTaskStates069(t, s)["mission-b2"]; st != "completed" {
		t.Fatalf("workspace state %q, want completed", st)
	}
}

// C3: terminal(succeeded/failed/cancelled) from-state는 여전히 ErrInvalidState,
// 저널 불변 — waiting_for_human/blocked에서 닫힌 뒤 재제출 포함.
func TestMissionCompleteTerminalStillRejectedFRRHZ110(t *testing.T) {
	setups := map[string]func(t *testing.T, s *events.Store, id string){
		"cancelled": func(t *testing.T, s *events.Store, id string) {
			if res := cancelMission062(t, s, id); !res.Accepted {
				t.Fatal(res)
			}
		},
		"succeeded": func(t *testing.T, s *events.Store, id string) { drive062Terminal(t, s, id, domain.MissionSucceeded) },
		"failed":    func(t *testing.T, s *events.Store, id string) { drive062Terminal(t, s, id, domain.MissionFailed) },
		"human-closed": func(t *testing.T, s *events.Store, id string) {
			ms := mission.Service{Store: s}
			for rev, to := range []domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman} {
				if _, err := ms.Transition(id, uint64(rev+1), to); err != nil {
					t.Fatal(err)
				}
			}
			if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: id}, "tester", noAuthority()); err != nil || !res.Accepted {
				t.Fatal(res, err)
			}
		},
		"blocked-closed": func(t *testing.T, s *events.Store, id string) {
			ms := mission.Service{Store: s}
			if _, err := ms.Transition(id, 1, domain.MissionReady); err != nil {
				t.Fatal(err)
			}
			if _, err := ms.TransitionWithReason(id, 2, domain.MissionBlocked, "dep"); err != nil {
				t.Fatal(err)
			}
			if res, err := RelayIntent(s, Intent{Kind: "mission.fail", MissionID: id, Reason: "gave up"}, "tester", noAuthority()); err != nil || !res.Accepted {
				t.Fatal(res, err)
			}
		},
	}
	for state, setup := range setups {
		s := &events.Store{}
		id := "mission-" + state
		missionIn062(t, s, id)
		setup(t, s, id)
		before := journalBytes069(t, s)
		for _, kind := range []string{"mission.complete", "mission.fail"} {
			res, err := RelayIntent(s, Intent{Kind: kind, MissionID: id, Reason: "x"}, "tester", noAuthority())
			if err != nil || res.Accepted || res.Reason != domain.ErrInvalidState.Error() {
				t.Fatalf("%s on %s: %+v err=%v", kind, state, res, err)
			}
		}
		if journalBytes069(t, s) != before {
			t.Fatalf("rejected terminal on %s changed journal", state)
		}
	}
}

// C4: planned/ready/paused → complete/fail은 여전히 "<state>에서 종결 불가:
// task.resume 먼저", 저널 불변(불변 재핀).
func TestMissionCompleteQueuedPausedStillRejectedFRRHZ110(t *testing.T) {
	cases := map[string]struct {
		path  []domain.MissionState
		state string
	}{
		"mission-planned": {nil, "planned"},
		"mission-ready":   {[]domain.MissionState{domain.MissionReady}, "ready"},
		"mission-paused":  {[]domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionPaused}, "paused"},
	}
	for id, c := range cases {
		s := &events.Store{}
		missionIn062(t, s, id, c.path...)
		before := journalBytes069(t, s)
		for _, kind := range []string{"mission.complete", "mission.fail"} {
			res, err := RelayIntent(s, Intent{Kind: kind, MissionID: id, Reason: "x"}, "tester", noAuthority())
			if err != nil || res.Accepted || res.Reason != c.state+"에서 종결 불가: task.resume 먼저" {
				t.Fatalf("%s %s: %+v err=%v", id, kind, res, err)
			}
		}
		if journalBytes069(t, s) != before {
			t.Fatalf("%s: rejected terminal changed journal", id)
		}
	}
}

// C5: waiting_for_human·blocked에서 task.resume은 여전히 ErrInvalidState·저널
// 불변(relay_resume_test 가드 불변) — 두 상태를 닫는 길은 complete/fail뿐이다.
func TestResumeFromWaitingHumanBlockedStillRejectedFRRHZ110(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-h", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	blockedIn079(t, s, "mission-b", "dep")
	before := journalBytes069(t, s)
	for _, id := range []string{"mission-h", "mission-b"} {
		res, err := RelayIntent(s, Intent{Kind: "task.resume", TaskID: id}, "tester", noAuthority())
		if err != nil || res.Accepted || res.Reason != domain.ErrInvalidState.Error() {
			t.Fatalf("%s resume: %+v err=%v", id, res, err)
		}
	}
	if journalBytes069(t, s) != before {
		t.Fatal("rejected resume changed journal")
	}
	for id, kind := range map[string]string{"mission-h": "mission.complete", "mission-b": "mission.fail"} {
		if res, err := RelayIntent(s, Intent{Kind: kind, MissionID: id, Reason: "closed"}, "tester", noAuthority()); err != nil || !res.Accepted {
			t.Fatalf("%s %s: %+v err=%v", id, kind, res, err)
		}
	}
}

// R1: 실 NDJSON journal 왕복 — h1 complete, h2 fail, b1 fail, b2 complete, 거부
// 1건(blocked fail 공백 reason) 섞음. Close→Open 후 state·Revision·
// TerminalDecisionID·/v1/workspace 바이트 동일; 이벤트 타입은 goal.created/
// mission.created/mission.transitioned만(신규 타입 0).
func TestMissionCompleteWaitingHumanBlockedJournalRoundTripFRRHZ110(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	missionIn069(t, j, "mission-h1", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	missionIn069(t, j, "mission-h2", domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman)
	blockedIn079(t, j, "mission-b1", "dep")
	blockedIn079(t, j, "mission-b2", "dep")
	steps := []struct {
		in   Intent
		want bool
	}{
		{Intent{Kind: "mission.complete", MissionID: "mission-h1", Reason: "ok"}, true},
		{Intent{Kind: "mission.fail", MissionID: "mission-h2", Reason: "no"}, true},
		{Intent{Kind: "mission.fail", MissionID: "mission-b1"}, false},
		{Intent{Kind: "mission.fail", MissionID: "mission-b1", Reason: "dead"}, true},
		{Intent{Kind: "mission.complete", MissionID: "mission-b2"}, true},
	}
	for _, st := range steps {
		res, e := RelayIntent(j, st.in, "tester", noAuthority())
		if e != nil || res.Accepted != st.want {
			t.Fatalf("%+v: %+v err=%v", st.in, res, e)
		}
	}
	wantStates := map[string]domain.MissionState{"mission-h1": domain.MissionSucceeded, "mission-h2": domain.MissionFailed, "mission-b1": domain.MissionFailed, "mission-b2": domain.MissionSucceeded}
	want := map[string]domain.Mission{}
	for id, st := range wantStates {
		want[id] = replay069(t, j, id)
		if want[id].State != st || want[id].TerminalDecisionID == "" || want[id].BlockedReason != "" {
			t.Fatalf("%s before restart: %+v", id, want[id])
		}
	}
	terminals := 0
	for _, e := range j.All() {
		switch e.Type {
		case "goal.created", "mission.created", "mission.transitioned":
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
		if e.Type == "mission.transitioned" {
			var p terminalPayload069
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.To == "succeeded" || p.To == "failed" {
				terminals++
				if p.DecisionID == "" || e.CorrelationID != "relay:unverified-local-operator:tester" {
					t.Fatalf("terminal without decision/correlation: %+v %s", e, e.Payload)
				}
			}
		}
	}
	if terminals != 4 {
		t.Fatalf("terminal events %d, want 4", terminals)
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
	j, err = openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	for id, w := range want {
		got := replay069(t, j, id)
		if got.State != w.State || got.Revision != w.Revision || got.TerminalDecisionID != w.TerminalDecisionID || got.BlockedReason != "" {
			t.Fatalf("%s after restart: got %+v want %+v", id, got, w)
		}
	}
	rec = httptest.NewRecorder()
	NewHTTP(j).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != b1 {
		t.Fatalf("workspace diverged after restart: %d\n%s\n%s", rec.Code, b1, rec.Body.String())
	}
	if ny, bl, _ := workspaceCounts079(t, j); ny != 0 || bl != 0 {
		t.Fatalf("counts after restart needsYou=%d blocked=%d", ny, bl)
	}
}
