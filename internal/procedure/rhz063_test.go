package procedure

// RHZ-063 FR-RHZ-092 A3: 사이클·불량 DAG는 생성 시점에 거부 — runner에 도달
// 자체가 불가능하다(command는 소비자가 거부할 구조를 append하지 않는다).

import "testing"

func TestCreateRejectsBadDAGFRRHZ092(t *testing.T) {
	s, _, k, _ := procedureFixtures(t)
	bad := map[string][]Step{
		"cycle":        {{ID: "a", Action: "a", After: []string{"b"}}, {ID: "b", Action: "b", After: []string{"a"}}},
		"self-cycle":   {{ID: "a", Action: "a", After: []string{"a"}}},
		"unknown-ref":  {{ID: "a", Action: "a", After: []string{"ghost"}}},
		"duplicate-id": {{ID: "a", Action: "a"}, {ID: "a", Action: "again"}},
	}
	before := len(s.All())
	for name, steps := range bad {
		p := validProcedure(k.ID, "p-"+name)
		p.Steps = steps
		if _, err := (Service{Store: s}).Create(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if len(s.All()) != before {
		t.Fatal("rejected DAGs changed the journal")
	}
}

func TestTopoOrderDeterministicFRRHZ092(t *testing.T) {
	steps := []Step{
		{ID: "z", Action: "z"},
		{ID: "m", Action: "m", After: []string{"z"}},
		{ID: "a", Action: "a", After: []string{"z"}},
	}
	for i := 0; i < 5; i++ {
		order, err := TopoOrder(steps)
		if err != nil || order[0].ID != "z" || order[1].ID != "a" || order[2].ID != "m" {
			t.Fatalf("order %+v err=%v", order, err)
		}
	}
}
