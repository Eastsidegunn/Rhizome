package assembly

// RHZ-063 FR-RHZ-092: Procedure 인스턴스화 runner. 테스트 계획
// RHZ-063 A1·A2·A4·A5·A6·A7.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/projector"
)

func fixture063(t *testing.T, steps []procedure.Step) *events.Store {
	t.Helper()
	s := &events.Store{}
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "src", SourceType: "note", SourceID: "n-1", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	if _, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: "k-dev", Kind: knowledge.Procedure, Statement: "dev loop", SourceMemoryID: "mem-1", Confidence: .8}); err != nil {
		t.Fatal(err)
	}
	if _, err := (procedure.Service{Store: s}).Create(procedure.Procedure{ID: "proc-dev", SourceKnowledgeID: "k-dev", Trigger: "manual", Steps: steps, SuccessConditions: []string{"ci green"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := (mission.Service{Store: s}).CreateGoal("goal-dev", "dev goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

func devSteps() []procedure.Step {
	return []procedure.Step{
		{ID: "impl", Action: "implement the change"},
		{ID: "review", Action: "review the change", After: []string{"impl"}, NeedsGate: true},
		{ID: "merge", Action: "merge the change", After: []string{"review"}},
	}
}

func spec063(runID string) RunSpec {
	return RunSpec{ProcedureID: "proc-dev", GoalID: "goal-dev", RunID: runID, Params: map[string]string{"branch": "feat-x"}, Actor: "tester", Verified: true}
}

type createdPayload struct {
	ID                string            `json:"ID"`
	ProcedureID       string            `json:"procedure_id"`
	ProcedureRevision uint64            `json:"procedure_revision"`
	Params            map[string]string `json:"params"`
}

// A1 (수용 1·2, 변이 probe): 정상 인스턴스화 — run mission 1 + step mission 3
// + spawn 3 + dependency 2 = 9 이벤트, 여기에 RHZ-083(FR-RHZ-114)부터
// NeedsGate step(review) 1건당 question.asked 1 = 정확 10 이벤트. run payload에
// pin 전부 기록.
func TestRunSpawnsInstanceFRRHZ092(t *testing.T) {
	s := fixture063(t, devSteps())
	n := len(s.All())
	res, err := Run(s, spec063("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.All()) - n; got != 10 {
		t.Fatalf("journal grew %d, want exactly 10 (9 structure + 1 gate question)", got)
	}
	questions := 0
	for _, e := range s.All()[n:] {
		if e.AggregateType == "question" {
			questions++
		}
	}
	if questions != 1 {
		t.Fatalf("gate questions %d, want 1 (review is the only NeedsGate step)", questions)
	}
	if res.MissionID != "mission-r1" || !reflect.DeepEqual(res.StepMissionIDs, []string{"mission-r1-impl", "mission-r1-review", "mission-r1-merge"}) {
		t.Fatalf("result %+v", res)
	}
	// run mission.created에 procedure_id·revision·params 기록(A5의 pin 기반).
	run := s.List("mission", "mission-r1")
	var p createdPayload
	if err := json.Unmarshal(run[0].Payload, &p); err != nil || p.ProcedureID != "proc-dev" || p.ProcedureRevision != 1 || p.Params["branch"] != "feat-x" {
		t.Fatalf("run payload %s err=%v", run[0].Payload, err)
	}
	// step mission 실재 + 이름=Action.
	m, err := projector.ReplayMission(s.List("mission", "mission-r1-review"))
	if err != nil || m.Description != "review the change" {
		t.Fatalf("step mission %+v err=%v", m, err)
	}
	// 엣지 실재: spawn 3(run→step), dependency 2(후행→선행).
	spawns, deps := 0, 0
	for _, e := range s.All() {
		if e.AggregateType != "edge" {
			continue
		}
		var ep struct {
			Kind     string
			From, To struct{ Type, ID string }
		}
		if err := json.Unmarshal(e.Payload, &ep); err != nil {
			t.Fatal(err)
		}
		switch ep.Kind {
		case "spawn":
			spawns++
			if ep.From.ID != "mission-r1" {
				t.Fatalf("spawn from %+v", ep)
			}
		case "dependency":
			deps++
		}
	}
	if spawns != 3 || deps != 2 {
		t.Fatalf("spawn=%d dep=%d", spawns, deps)
	}
}

// A2 (수용 1): after 위상 순서 — 동순위 사전순 결정론, dependency 방향
// 후행→선행.
func TestRunTopologicalOrderFRRHZ092(t *testing.T) {
	s := fixture063(t, []procedure.Step{
		{ID: "c", Action: "c work", After: []string{"a"}},
		{ID: "a", Action: "a work"},
		{ID: "b", Action: "b work", After: []string{"a"}},
	})
	res, err := Run(s, spec063("r2"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.StepMissionIDs, []string{"mission-r2-a", "mission-r2-b", "mission-r2-c"}) {
		t.Fatalf("order %v", res.StepMissionIDs)
	}
	// 저널 sequence로도 a < b < c.
	seq := map[string]uint64{}
	for _, e := range s.All() {
		if e.AggregateType == "mission" && strings.HasPrefix(e.AggregateID, "mission-r2-") {
			seq[e.AggregateID] = e.Sequence
		}
	}
	if !(seq["mission-r2-a"] < seq["mission-r2-b"] && seq["mission-r2-b"] < seq["mission-r2-c"]) {
		t.Fatalf("spawn sequence %v", seq)
	}
	// dependency는 후행→선행.
	for _, e := range s.All() {
		if e.AggregateType != "edge" {
			continue
		}
		var ep struct {
			Kind     string
			From, To struct{ Type, ID string }
		}
		if json.Unmarshal(e.Payload, &ep) == nil && ep.Kind == "dependency" {
			if ep.To.ID != "mission-r2-a" {
				t.Fatalf("dependency target %+v", ep)
			}
		}
	}
}

// A4 (수용 2): 재생 재계산 — 재생본의 run·step·엣지·파라미터 동일.
func TestRunRecomputableFRRHZ092(t *testing.T) {
	s := fixture063(t, devSteps())
	if _, err := Run(s, spec063("r3")); err != nil {
		t.Fatal(err)
	}
	replayed := &events.Store{}
	for _, e := range s.All() {
		if err := replayed.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := json.Marshal(s.All())
	b, _ := json.Marshal(replayed.All())
	if string(a) != string(b) {
		t.Fatal("replay diverged")
	}
	if _, err := projector.ReplayMission(replayed.List("mission", "mission-r3-merge")); err != nil {
		t.Fatal(err)
	}
}

// A5 (수용 2, 변이 probe): pin — 같은 spec의 두 run은 동형 구조(산출 = spec
// 만의 함수)이고, 모든 mission.created가 procedure_revision을 기록해 run이
// procedure 스트림 없이 재계산 가능하다.
func TestRunPinnedToSpecFRRHZ092(t *testing.T) {
	s := fixture063(t, devSteps())
	r1, err := Run(s, spec063("ra"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(s, spec063("rb"))
	if err != nil {
		t.Fatal(err)
	}
	strip := func(ids []string, run string) []string {
		out := []string{}
		for _, id := range ids {
			out = append(out, strings.TrimPrefix(id, "mission-"+run))
		}
		return out
	}
	if !reflect.DeepEqual(strip(r1.StepMissionIDs, "ra"), strip(r2.StepMissionIDs, "rb")) {
		t.Fatalf("same spec produced different shapes: %v vs %v", r1.StepMissionIDs, r2.StepMissionIDs)
	}
	for _, id := range append([]string{r1.MissionID}, r1.StepMissionIDs...) {
		var p createdPayload
		if err := json.Unmarshal(s.List("mission", id)[0].Payload, &p); err != nil || p.ProcedureRevision != 1 || p.ProcedureID != "proc-dev" {
			t.Fatalf("%s: pin missing: %+v err=%v", id, p, err)
		}
	}
}

// A6: 선검증 — 미존재 procedure/goal·사용 중 RunID 전부 거부 + 저널 완전
// 불변(부분 쓰기 0). needs_gate 보존도 확인.
func TestRunPrevalidationNoPartialWriteFRRHZ092(t *testing.T) {
	s := fixture063(t, devSteps())
	if _, err := Run(s, spec063("r4")); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	bad := []RunSpec{
		{ProcedureID: "proc-ghost", GoalID: "goal-dev", RunID: "x1", Actor: "t", Verified: true},
		{ProcedureID: "proc-dev", GoalID: "goal-ghost", RunID: "x2", Actor: "t", Verified: true},
		{ProcedureID: "proc-dev", GoalID: "goal-dev", RunID: "r4", Actor: "t", Verified: true}, // 사용 중
		{ProcedureID: "proc-dev", GoalID: "goal-dev", RunID: "", Actor: "t", Verified: true},
	}
	for _, spec := range bad {
		if _, err := Run(s, spec); err == nil {
			t.Fatalf("%+v accepted", spec)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("rejected run left partial writes")
	}
	p, err := (procedure.Service{Store: s}).Get("proc-dev")
	if err != nil || !p.Steps[1].NeedsGate {
		t.Fatalf("needs_gate not preserved: %+v err=%v", p.Steps, err)
	}
}

// A7(미니 경계 테스트)은 RHZ-055 archtest가 흡수했다: procedure⊄exec = R1,
// procedure⊄assembly = R7, assembly의 양 커널 import 합법성 = R6 + 실저장소
// 스캔(TestRepositoryObeysKernelBoundaryFRRHZ085). 중복 제거이지 약화 아님.
