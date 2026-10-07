package mission

// RHZ-080 FR-RHZ-111 K1: 커널 writer Assign — mission.assigned 1건 append,
// 재배정은 이력 2건·재생은 마지막 값, terminal/빈 assignee/revision 불일치는
// 모두 append 전 거부(저널 불변).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

func fixtureMission080(t *testing.T, s *events.Store, id string) Service {
	t.Helper()
	svc := Service{Store: s}
	if _, err := svc.CreateGoal("g", "g", "d", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(id, "g", "m", "d"); err != nil {
		t.Fatal(err)
	}
	return svc
}

func assignedEvents080(s *events.Store, id string) []events.Event {
	var out []events.Event
	for _, e := range s.List("mission", id) {
		if e.Type == "mission.assigned" {
			out = append(out, e)
		}
	}
	return out
}

func TestAssignAndReassignFRRHZ111(t *testing.T) {
	s := &events.Store{}
	svc := fixtureMission080(t, s, "m1")
	n := len(s.All())
	m, err := svc.Assign("m1", 1, "agent-a", "first")
	if err != nil {
		t.Fatal(err)
	}
	if m.Assignee != "agent-a" || m.Revision != 2 || m.State != domain.MissionPlanned {
		t.Fatalf("after assign: %+v", m)
	}
	if len(s.All()) != n+1 {
		t.Fatalf("journal grew %d, want 1", len(s.All())-n)
	}
	last := s.All()[len(s.All())-1]
	var p struct{ Assignee, Reason string }
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if last.AggregateType != "mission" || last.AggregateID != "m1" || last.Type != "mission.assigned" || last.Revision != 2 || p.Assignee != "agent-a" || p.Reason != "first" {
		t.Fatalf("event %+v payload %s", last, last.Payload)
	}
	// reassignment: allowed, last wins, history keeps both.
	m, err = svc.Assign("m1", 2, "agent-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Assignee != "agent-b" || m.Revision != 3 {
		t.Fatalf("after reassign: %+v", m)
	}
	if got := assignedEvents080(s, "m1"); len(got) != 2 {
		t.Fatalf("assigned history %d, want 2", len(got))
	}
	if r, err := projector.ReplayMission(s.List("mission", "m1")); err != nil || r.Assignee != "agent-b" {
		t.Fatalf("replay %+v err=%v", r, err)
	}
	// assignee survives a state transition (value copy).
	m, err = svc.Transition("m1", 3, domain.MissionReady)
	if err != nil || m.Assignee != "agent-b" || m.State != domain.MissionReady {
		t.Fatalf("transition kept assignee? %+v err=%v", m, err)
	}
}

func TestAssignTerminalRejectedFRRHZ111(t *testing.T) {
	s := &events.Store{}
	svc := fixtureMission080(t, s, "m1")
	if _, err := svc.Transition("m1", 1, domain.MissionCancelled); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.All())
	_, err := svc.Assign("m1", 2, "agent-a", "")
	if !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on rejected assign")
	}
	if m, err := projector.ReplayMission(s.List("mission", "m1")); err != nil || m.Assignee != "" {
		t.Fatalf("replay %+v err=%v", m, err)
	}
}

func TestAssignEmptyAssigneeRejectedFRRHZ111(t *testing.T) {
	s := &events.Store{}
	svc := fixtureMission080(t, s, "m1")
	before, _ := json.Marshal(s.All())
	for _, a := range []string{"", "   ", "\t\n"} {
		_, err := svc.Assign("m1", 1, a, "")
		if err == nil || !strings.Contains(err.Error(), "assignee required") {
			t.Fatalf("assignee %q: err = %v", a, err)
		}
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on rejected assign")
	}
}

func TestAssignRevisionConflictFRRHZ111(t *testing.T) {
	s := &events.Store{}
	svc := fixtureMission080(t, s, "m1")
	before, _ := json.Marshal(s.All())
	if _, err := svc.Assign("m1", 7, "agent-a", ""); !errors.Is(err, events.ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
	if _, err := svc.Assign("nope", 0, "agent-a", ""); err == nil {
		t.Fatal("unknown mission accepted")
	}
	after, _ := json.Marshal(s.All())
	if string(before) != string(after) {
		t.Fatal("journal changed on rejected assign")
	}
}

// K2 (FR-RHZ-111): 앞뒤 공백은 저장 시 제거되어 ?assignee= 정확 일치가 성립.
func TestAssignStoresTrimmedFRRHZ111(t *testing.T) {
	s := &events.Store{}
	ms := Service{Store: s}
	if _, err := ms.CreateGoal("goal-t", "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	m, err := ms.Create("mission-t", "goal-t", "d", "s")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ms.Assign("mission-t", m.Revision, "  agent-a  ", "")
	if err != nil || got.Assignee != "agent-a" {
		t.Fatalf("assignee=%q err=%v", got.Assignee, err)
	}
}
