package edge

// RHZ-057 FR-RHZ-087 1단계: contains(goal→goal)·about(memory→goal/mission)
// 화이트리스트 — 발행·모양검사만, 비대칭/무순환/중복 거부 규칙 없음(2단계).
// 테스트 계획 RHZ-057 T1~T4.

import (
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/memory"
	"rhizome/internal/mission"
)

func rhz057Store(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g1", "goal one", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.CreateGoal("g2", "goal two", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("ms1", "g1", "mission one", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "note-m1", Kind: memory.Observation, Content: "obs", SourceType: "blob", SourceID: "sha256:deadbeef", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	return s
}

func mkEdge(id, fromT, fromID, toT, toID string, k Kind) Edge {
	return Edge{ID: id, From: Endpoint{fromT, fromID}, To: Endpoint{toT, toID}, Kind: k, Actor: "tester", Correlation: "test", Verified: true}
}

// T1: contains(goal→goal) 발행 — 저널 실재(이벤트 수 증가)와 재생 일치.
func TestContainsEdgeDeclaredFRRHZ087(t *testing.T) {
	s := rhz057Store(t)
	n := len(s.All())
	got, err := (Service{Store: s}).Create(mkEdge("e-contains-1", "goal", "g1", "goal", "g2", Contains))
	if err != nil {
		t.Fatal(err)
	}
	all := s.All()
	if len(all) != n+1 {
		t.Fatalf("journal grew %d, want 1", len(all)-n)
	}
	last := all[len(all)-1]
	if last.AggregateType != "edge" || last.Type != "edge.declared" {
		t.Fatalf("last event %s/%s", last.AggregateType, last.Type)
	}
	back, err := (Service{Store: s}).Get("e-contains-1")
	if err != nil || back != got {
		t.Fatalf("replay mismatch: %+v vs %+v err=%v", back, got, err)
	}
}

// T2: about(memory→goal)·about(memory→mission) 발행.
func TestAboutEdgeDeclaredFRRHZ087(t *testing.T) {
	s := rhz057Store(t)
	n := len(s.All())
	svc := Service{Store: s}
	if _, err := svc.Create(mkEdge("e-about-goal", "memory", "note-m1", "goal", "g1", About)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(mkEdge("e-about-mission", "memory", "note-m1", "mission", "ms1", About)); err != nil {
		t.Fatal(err)
	}
	if len(s.All()) != n+2 {
		t.Fatalf("journal grew %d, want 2", len(s.All())-n)
	}
}

// T3: 모양검사 테이블 — 전부 거부·저널 불변. 기존 Kind 양성 회귀 1건 포함.
func TestShapeWhitelistRejectionsFRRHZ087(t *testing.T) {
	s := rhz057Store(t)
	svc := Service{Store: s}
	n := len(s.All())
	bad := []Edge{
		mkEdge("b1", "goal", "g1", "mission", "ms1", Contains),
		mkEdge("b2", "memory", "note-m1", "goal", "g1", Contains),
		mkEdge("b3", "goal", "g1", "memory", "note-m1", About),
		mkEdge("b4", "memory", "note-m1", "memory", "note-m1", About),
		mkEdge("b5", "mission", "ms1", "goal", "g1", About),
		mkEdge("b6", "memory", "note-m1", "goal", "g1", Dependency),
		mkEdge("b7", "goal", "g1", "goal", "g2", Kind("unknown")),
	}
	for _, x := range bad {
		if _, err := svc.Create(x); err == nil {
			t.Fatalf("%s (%s %s→%s) accepted, want rejection", x.ID, x.Kind, x.From.Type, x.To.Type)
		}
	}
	if len(s.All()) != n {
		t.Fatal("journal changed by rejected edges")
	}
	// 기존 Kind 회귀: dependency(goal→mission)는 여전히 성립.
	if _, err := svc.Create(mkEdge("ok-dep", "goal", "g1", "mission", "ms1", Dependency)); err != nil {
		t.Fatalf("legacy dependency edge regressed: %v", err)
	}
}

// T4: 끝점 실재 검사 — 미등록 memory/goal은 거부, 저널 불변.
func TestAboutEndpointExistenceFRRHZ087(t *testing.T) {
	s := rhz057Store(t)
	svc := Service{Store: s}
	n := len(s.All())
	if _, err := svc.Create(mkEdge("x1", "memory", "note-ghost", "mission", "ms1", About)); err == nil {
		t.Fatal("nonexistent memory endpoint accepted")
	}
	if _, err := svc.Create(mkEdge("x2", "memory", "note-m1", "goal", "goal-ghost", About)); err == nil {
		t.Fatal("nonexistent goal endpoint accepted")
	}
	if len(s.All()) != n {
		t.Fatal("journal changed by rejected edges")
	}
}
