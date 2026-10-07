package coordinator

// RHZ-079 FR-RHZ-110 X1 (B-0 기록): 도메인 표 확장의 부수효과 — 실행측
// decision(Complete/Fail)이 waiting_for_human·blocked mission도 종결한다.
// 가드 없음이 현재 의도된 동작이므로 그 사실을 핀한다.

import (
	"testing"

	"rhizome/internal/decision"
	"rhizome/internal/events"
)

func TestApplyDecisionClosesWaitingHumanBlockedFRRHZ110(t *testing.T) {
	cases := []struct {
		name string
		hops []string
		kind decision.Kind
		want string
	}{
		{"human-complete", []string{"ready", "running", "waiting_for_human"}, decision.Complete, "succeeded"},
		{"human-fail", []string{"ready", "running", "waiting_for_human"}, decision.Fail, "failed"},
		{"blocked-complete", []string{"ready", "blocked"}, decision.Complete, "succeeded"},
		{"blocked-fail", []string{"ready", "blocked"}, decision.Fail, "failed"},
	}
	for _, c := range cases {
		s := ms()
		rev := uint64(1)
		for _, to := range c.hops {
			payload := `{"To":"` + to + `"}`
			if to == "blocked" {
				payload = `{"To":"blocked","Reason":"dep"}`
			}
			if err := s.Append(rev, events.Event{AggregateType: "mission", AggregateID: "m", Revision: rev + 1, Type: "mission.transitioned", Payload: []byte(payload)}); err != nil {
				t.Fatal(err)
			}
			rev++
		}
		d, e := (decision.Service{Store: s}).Create(decision.Decision{ID: "d-" + c.name, MissionID: "m", Kind: c.kind, Reason: "x", Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}}})
		if e != nil {
			t.Fatal(e)
		}
		m, e := (&Coordinator{Store: s}).ApplyDecision(d.ID)
		if e != nil || string(m.State) != c.want || m.TerminalDecisionID != d.ID || m.BlockedReason != "" {
			t.Fatalf("%s: %+v %v", c.name, m, e)
		}
	}
}
