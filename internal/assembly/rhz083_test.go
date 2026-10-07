package assembly

// RHZ-083 FR-RHZ-114: NeedsGate step → 게이트 question 자동 생성 (N1·N2·N3·
// N5·N6). 모든 쓰기는 question.Service.Ask 경유(단일 writer), 선검증으로
// 부분 쓰기 0.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/question"
)

func gatedSteps083() []procedure.Step {
	return []procedure.Step{
		{ID: "a", Action: "act-a", NeedsGate: true},
		{ID: "b", Action: "act-b", After: []string{"a"}},
		{ID: "c", Action: "act-c", After: []string{"b"}, NeedsGate: true, Recommendation: "ship"},
	}
}

func plainSteps083() []procedure.Step {
	return []procedure.Step{
		{ID: "a", Action: "act-a"},
		{ID: "b", Action: "act-b", After: []string{"a"}},
		{ID: "c", Action: "act-c", After: []string{"b"}},
	}
}

func spec083(runID string) RunSpec {
	return RunSpec{ProcedureID: "proc-dev", GoalID: "goal-dev", RunID: runID, Actor: "tester", Verified: true, Correlation: "corr-" + runID}
}

func questionEvents(s *events.Store, from int) []events.Event {
	out := []events.Event{}
	for _, e := range s.All()[from:] {
		if e.AggregateType == "question" {
			out = append(out, e)
		}
	}
	return out
}

func shape(evs []events.Event) string {
	parts := []string{}
	for _, e := range evs {
		parts = append(parts, e.AggregateType+":"+e.Type)
	}
	return strings.Join(parts, ",")
}

// N1: a(NeedsGate)·b·c(NeedsGate, recommendation "ship") → question.asked 정확
// 2건, 각각 mission-<run>-a / -c에 바인딩, title/body/recommendation/
// requestedBy/correlation 지정값. 저널 순서는 TopoOrder(a 다음 c).
func TestRunAsksGateQuestionPerNeedsGateStepFRRHZ114(t *testing.T) {
	s := fixture063(t, gatedSteps083())
	n := len(s.All())
	res, err := Run(s, spec083("r1"))
	if err != nil {
		t.Fatal(err)
	}
	qs := questionEvents(s, n)
	if len(qs) != 2 || qs[0].Type != "question.asked" || qs[1].Type != "question.asked" {
		t.Fatalf("question events %s, want exactly 2 question.asked", shape(qs))
	}
	want := []struct{ step, rec string }{{"a", ""}, {"c", "ship"}}
	for i, w := range want {
		stepMission := "mission-r1-" + w.step
		q, err := question.Replay(s.List("question", qs[i].AggregateID))
		if err != nil {
			t.Fatal(err)
		}
		if q.MissionID != stepMission || q.GoalID != "" {
			t.Fatalf("question %d bound to mission=%q goal=%q, want %q", i, q.MissionID, q.GoalID, stepMission)
		}
		if q.Title != "r1 · "+w.step+" 게이트" || q.Body != "act-"+w.step || q.Recommendation != w.rec {
			t.Fatalf("question %d title=%q body=%q rec=%q", i, q.Title, q.Body, q.Recommendation)
		}
		if q.RequestedBy != "unverified-local-operator:tester" || q.CorrelationID != "corr-r1" || qs[i].CorrelationID != "corr-r1" {
			t.Fatalf("question %d requestedBy=%q corr=%q/%q", i, q.RequestedBy, q.CorrelationID, qs[i].CorrelationID)
		}
		if q.ID != question.IDFor(q.Title, q.Body, q.Recommendation) || res.GateQuestionIDs[stepMission] != q.ID {
			t.Fatalf("question %d id %q result map %v", i, q.ID, res.GateQuestionIDs)
		}
		if q.Decision != "" {
			t.Fatalf("question %d not pending: %q", i, q.Decision)
		}
	}
	if _, ok := res.GateQuestionIDs["mission-r1-b"]; ok || len(res.GateQuestionIDs) != 2 {
		t.Fatalf("b must not get a gate: %v", res.GateQuestionIDs)
	}
	// 질문은 해당 step mission 다음에 기록된다(순서: mission → 엣지 → question).
	seqOf := func(agg, id string) uint64 { return s.List(agg, id)[0].Sequence }
	if !(seqOf("mission", "mission-r1-a") < qs[0].Sequence && qs[0].Sequence < seqOf("mission", "mission-r1-b") && seqOf("mission", "mission-r1-c") < qs[1].Sequence) {
		t.Fatalf("question ordering: a=%d qa=%d b=%d c=%d qc=%d", seqOf("mission", "mission-r1-a"), qs[0].Sequence, seqOf("mission", "mission-r1-b"), seqOf("mission", "mission-r1-c"), qs[1].Sequence)
	}
}

// N2: NeedsGate 없는 템플릿 → question 0건, 저널 모양은 RHZ-063 그대로
// (run mission + step별 mission/spawn/dep) — 변이 probe c(항상 ask)를 잡는다.
func TestRunWithoutNeedsGateAsksNothingFRRHZ114(t *testing.T) {
	s := fixture063(t, plainSteps083())
	n := len(s.All())
	if _, err := Run(s, spec083("r2")); err != nil {
		t.Fatal(err)
	}
	if got := len(questionEvents(s, n)); got != 0 {
		t.Fatalf("question events %d, want 0", got)
	}
	want := "mission:mission.created," + // run
		"mission:mission.created,edge:edge.declared," + // a + spawn
		"mission:mission.created,edge:edge.declared,edge:edge.declared," + // b + spawn + dep
		"mission:mission.created,edge:edge.declared,edge:edge.declared" // c + spawn + dep
	if got := shape(s.All()[n:]); got != want {
		t.Fatalf("journal shape\n got %s\nwant %s", got, want)
	}
}

// N3: 재제출 멱등 — 같은 spec의 두 번째 Run은 기존 run mission 규칙으로 어떤
// 쓰기도 전에 거부(저널 바이트 동일). 같은 질문의 직접 Ask도 멱등(기존 Ref).
func TestRunResubmissionWritesNothingFRRHZ114(t *testing.T) {
	s := fixture063(t, gatedSteps083())
	first, err := Run(s, spec083("r3"))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	if _, err := Run(s, spec083("r3")); err == nil {
		t.Fatal("re-submitted run accepted")
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("re-submitted run changed the journal")
	}
	// 직접 Ask로 핀: 동일 title/body/recommendation·동일 mission → 기존 Ref, 쓰기 0.
	q, err := (question.Service{Store: s}).Ask(GateTitle("r3", "c"), "act-c", "ship", "mission-r3-c", "", "tester", "corr-r3")
	if err != nil || q.ID != first.GateQuestionIDs["mission-r3-c"] {
		t.Fatalf("direct Ask: %+v err=%v want %q", q, err, first.GateQuestionIDs["mission-r3-c"])
	}
	again, _ := json.Marshal(s.All())
	if string(before) != string(again) {
		t.Fatal("idempotent Ask changed the journal")
	}
}

// N5: 충돌 — 같은 title/body/recommendation의 question이 다른 mission에 이미
// 바인딩돼 있으면 Run은 어떤 append도 하기 전에 거부(저널 바이트 동일).
func TestRunRefusesGateQuestionCollisionFRRHZ114(t *testing.T) {
	s := fixture063(t, gatedSteps083())
	if _, err := (mission.Service{Store: s}).Create("mission-other", "goal-dev", "other", "done"); err != nil {
		t.Fatal(err)
	}
	// step a의 질문과 ID가 같은 질문을 다른 mission에 미리 만든다.
	if _, err := (question.Service{Store: s}).Ask(GateTitle("r5", "a"), "act-a", "", "mission-other", "", "someone", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	_, err := Run(s, spec083("r5"))
	if err == nil || !strings.Contains(err.Error(), "mission-other") {
		t.Fatalf("collision accepted or wrong reason: %v", err)
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("collision left partial writes")
	}
	if len(s.List("mission", "mission-r5")) != 0 {
		t.Fatal("run mission written despite collision")
	}
}

// N6: 뒤쪽 step(c)의 질문만 충돌해도 선검증이 잡아 run mission·a·b 포함
// 아무것도 쓰이지 않는다(부분 쓰기 0). actor 부재도 같은 선검증.
func TestRunPrevalidatesLaterStepGateFRRHZ114(t *testing.T) {
	s := fixture063(t, gatedSteps083())
	if _, err := (mission.Service{Store: s}).Create("mission-other", "goal-dev", "other", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := (question.Service{Store: s}).Ask(GateTitle("r6", "c"), "act-c", "ship", "mission-other", "", "someone", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	if _, err := Run(s, spec083("r6")); err == nil {
		t.Fatal("later-step collision accepted")
	}
	// actor 불량: 빈 값·맨 접두사·접두사+공백 — edge는 통과시키지만 question.Ask가
	// 거부하는 값들. 선검증이 question.NormalizeActor 규칙 그대로 막아야
	// run mission·step a·spawn이 먼저 쓰이는 고아 run이 생기지 않는다.
	for i, actor := range []string{"", "unverified-local-operator:", "unverified-local-operator:   ", "   "} {
		bad := spec083(fmt.Sprintf("r7%d", i))
		bad.Actor, bad.Verified = actor, false
		if _, err := Run(s, bad); err == nil || !strings.Contains(err.Error(), "actor required") {
			t.Fatalf("actor %q with gated steps accepted: %v", actor, err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejected run left partial writes")
	}
	for _, id := range []string{"mission-r6", "mission-r6-a", "mission-r6-b", "mission-r6-c", "mission-r70", "mission-r71", "mission-r72", "mission-r73"} {
		if len(s.List("mission", id)) != 0 {
			t.Fatalf("%s written despite pre-validation failure", id)
		}
	}
	// 양성: 이미 접두사가 붙은 유효 actor는 그대로 통과하고 requestedBy에 보존된다.
	ok := spec083("r8")
	ok.Actor, ok.Verified = "unverified-local-operator:op", false
	res, err := Run(s, ok)
	if err != nil {
		t.Fatal(err)
	}
	q, err := question.Replay(s.List("question", res.GateQuestionIDs["mission-r8-a"]))
	if err != nil || q.RequestedBy != "unverified-local-operator:op" {
		t.Fatalf("prefixed actor: %+v err=%v", q, err)
	}
}
