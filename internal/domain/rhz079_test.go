package domain

// RHZ-079 FR-RHZ-110: mission 전이표 확장 핀 — 새 표 == 구 표 ∪ {
// (waiting_for_human,succeeded),(waiting_for_human,failed),
// (blocked,succeeded),(blocked,failed) }. 구 표를 테스트에 명시해 "확장만"을
// (from,to) 전수 비교로 고정한다(제거·추가 어느 쪽도 FAIL).

import "testing"

var allMissionStates079 = []MissionState{
	MissionPlanned, MissionReady, MissionRunning, MissionPaused, MissionWaitingResult,
	MissionWaitingHuman, MissionBlocked, MissionSucceeded, MissionFailed, MissionCancelled,
}

// oldMissionTable079: RHZ-079 이전 validMissionTransition 그대로.
func oldMissionTable079(from, to MissionState) bool {
	switch from {
	case MissionPlanned:
		return to == MissionReady || to == MissionCancelled
	case MissionReady:
		return to == MissionRunning || to == MissionBlocked || to == MissionCancelled
	case MissionRunning:
		return to == MissionPaused || to == MissionWaitingResult || to == MissionWaitingHuman || to == MissionSucceeded || to == MissionFailed || to == MissionBlocked
	case MissionWaitingResult:
		return to == MissionRunning || to == MissionSucceeded || to == MissionFailed || to == MissionBlocked
	case MissionPaused:
		return to == MissionRunning || to == MissionCancelled
	case MissionWaitingHuman:
		return to == MissionRunning || to == MissionCancelled
	case MissionBlocked:
		return to == MissionReady || to == MissionCancelled
	default:
		return false
	}
}

type pair079 struct{ from, to MissionState }

// D1: 전수 (from,to) 비교.
func TestMissionTransitionTableExtensionOnlyFRRHZ110(t *testing.T) {
	added := map[pair079]bool{
		{MissionWaitingHuman, MissionSucceeded}: true,
		{MissionWaitingHuman, MissionFailed}:    true,
		{MissionBlocked, MissionSucceeded}:      true,
		{MissionBlocked, MissionFailed}:         true,
	}
	for _, from := range allMissionStates079 {
		for _, to := range allMissionStates079 {
			want := oldMissionTable079(from, to) || added[pair079{from, to}]
			if got := validMissionTransition(from, to); got != want {
				t.Errorf("(%s -> %s): got %v want %v", from, to, got, want)
			}
			// Transition()이 같은 표를 쓰고 Revision+1만 하는지.
			m := Mission{ID: "m", State: from, Revision: 7}
			n, err := m.Transition(to)
			if want && (err != nil || n.State != to || n.Revision != 8) {
				t.Errorf("(%s -> %s): Transition %+v err=%v", from, to, n, err)
			}
			if !want && err != ErrInvalidState {
				t.Errorf("(%s -> %s): Transition err=%v want ErrInvalidState", from, to, err)
			}
		}
	}
	// 추가분 4건은 구 표에서 실제로 무효였다(확장이 의미 있음을 핀).
	for p := range added {
		if oldMissionTable079(p.from, p.to) {
			t.Errorf("%+v already valid in old table", p)
		}
	}
}
