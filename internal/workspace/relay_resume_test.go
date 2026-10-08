package workspace

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/approval"
	"rhizome/internal/decision"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
)

// RHZ-044: fixture를 포함해 모든 쓰기는 실제 events.Store에 기록한다.
func rhz044Store(t *testing.T, state domain.MissionState) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g044", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	m, err := ms.Create("m044", "g044", "mission", "done")
	if err != nil {
		t.Fatal(err)
	}
	step := func(to domain.MissionState) {
		t.Helper()
		if to == domain.MissionBlocked {
			m, err = ms.TransitionWithReason(m.ID, m.Revision, to, "dependency unavailable")
		} else {
			m, err = ms.Transition(m.ID, m.Revision, to)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if state != domain.MissionPlanned && state != domain.MissionCancelled {
		step(domain.MissionReady)
		if state != domain.MissionReady && state != domain.MissionBlocked {
			step(domain.MissionRunning)
		}
	}
	switch state {
	case domain.MissionPlanned, domain.MissionReady, domain.MissionRunning:
	case domain.MissionPaused, domain.MissionWaitingHuman, domain.MissionWaitingResult,
		domain.MissionBlocked, domain.MissionCancelled:
		step(state)
	case domain.MissionSucceeded, domain.MissionFailed:
		// Terminal fixture: 실제 Decision 생성 후, 그 ID를 가진 유효 전이 stream 구성.
		kind := decision.Complete
		if state == domain.MissionFailed {
			kind = decision.Fail
		}
		d, e := (decision.Service{Store: s}).Create(decision.Decision{
			ID: "terminal044", MissionID: m.ID, Kind: kind, Reason: "fixture outcome",
			Evidence: []decision.Evidence{{SourceType: "mission", SourceID: m.ID}},
		})
		if e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(struct {
			To         domain.MissionState
			DecisionID string
		}{state, d.ID})
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Append(m.Revision, events.Event{
			AggregateType: "mission", AggregateID: m.ID, Revision: m.Revision + 1,
			Type: "mission.transitioned", Payload: b, CorrelationID: "fixture044",
		}); e != nil {
			t.Fatal(e)
		}
	default:
		t.Fatalf("unsupported fixture state: %s", state)
	}
	got, err := projector.ReplayMission(s.List("mission", "m044"))
	if err != nil || got.State != state {
		t.Fatalf("fixture state=%s want=%s error=%v", got.State, state, err)
	}
	return s
}

func rhz044Resume(t *testing.T, s events.Port) RelayResult {
	t.Helper()
	r, err := RelayIntent(s, Intent{Kind: "task.resume", TaskID: "m044"}, "test-operator", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rhz044Unchanged(t *testing.T, before []events.Event, s events.Port) {
	t.Helper()
	// 全イベントの順序・Sequence・Revision・Payload bytes・時刻を比較する。
	if !reflect.DeepEqual(before, s.All()) {
		t.Fatal("rejected request changed event log")
	}
}

func rhz044Advance(t *testing.T, from domain.MissionState, want ...domain.MissionState) {
	t.Helper()
	s := rhz044Store(t, from)
	before := s.All()
	stream := s.List("mission", "m044")
	r := rhz044Resume(t, s)
	if !r.Accepted || r.Reason != "" {
		t.Fatalf("resume rejected: %+v", r)
	}
	after := s.All()
	if len(after) != len(before)+len(want) || !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatal("unexpected event count or rewritten prefix")
	}
	for i, to := range want {
		e := after[len(before)+i]
		var p struct{ To domain.MissionState }
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if e.AggregateType != "mission" || e.AggregateID != "m044" ||
			e.Type != "mission.transitioned" || e.Revision != uint64(len(stream)+i+1) || p.To != to {
			t.Fatalf("transition %d: %+v, to=%s want=%s", i, e, p.To, to)
		}
	}
	m, err := projector.ReplayMission(s.List("mission", "m044"))
	if err != nil || m.State != domain.MissionRunning || m.Revision != uint64(len(stream)+len(want)) {
		t.Fatalf("final mission=%+v error=%v", m, err)
	}
}

func rhz044Reject(t *testing.T, state domain.MissionState) {
	t.Helper()
	s := rhz044Store(t, state)
	before := s.All()
	r := rhz044Resume(t, s)
	if r.Accepted || r.Reason != domain.ErrInvalidState.Error() {
		t.Errorf("resume from %s: %+v", state, r)
	}
	rhz044Unchanged(t, before, s)
}

func TestRelayResumeFromPlanned(t *testing.T) {
	rhz044Advance(t, domain.MissionPlanned, domain.MissionReady, domain.MissionRunning)
}
func TestRelayResumeFromReady(t *testing.T) {
	rhz044Advance(t, domain.MissionReady, domain.MissionRunning)
}
func TestRelayResumeFromPaused(t *testing.T) {
	rhz044Advance(t, domain.MissionPaused, domain.MissionRunning)
}
func TestRelayResumeRunningRejected(t *testing.T) {
	rhz044Reject(t, domain.MissionRunning)
}
func TestRelayResumeWaitingHumanRejected(t *testing.T) {
	rhz044Reject(t, domain.MissionWaitingHuman)
}
func TestRelayResumeWaitingResultRejected(t *testing.T) {
	rhz044Reject(t, domain.MissionWaitingResult)
}
func TestRelayResumeTerminalRejected(t *testing.T) {
	for _, state := range []domain.MissionState{domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled} {
		t.Run(string(state), func(t *testing.T) { rhz044Reject(t, state) })
	}
}
func TestRelayResumeBlockedRejected(t *testing.T) {
	rhz044Reject(t, domain.MissionBlocked)
}

// Fake: relay 첫 조회에는 과거 ready stream, 도메인 재조회에는 현재 paused stream.
// 실제 Store는 변경하지 않아 revision conflict 거부의 로그 불변도 확인할 수 있다.
type rhz044StaleReadFake struct {
	events.Port
	stale []events.Event
	reads int
}

func (f *rhz044StaleReadFake) List(kind, id string) []events.Event {
	if kind == "mission" && id == "m044" {
		f.reads++
		if f.reads == 1 {
			return f.stale
		}
	}
	return f.Port.List(kind, id)
}

func rhz044CheckStale(t *testing.T) {
	t.Helper()
	s := rhz044Store(t, domain.MissionPaused)
	f := &rhz044StaleReadFake{Port: s, stale: s.List("mission", "m044")[:2]}
	before := f.All()
	r := rhz044Resume(t, f)
	if r.Accepted || r.Reason != events.ErrRevisionConflict.Error() || f.reads < 2 {
		t.Fatalf("revision conflict not preserved: %+v, reads=%d", r, f.reads)
	}
	rhz044Unchanged(t, before, f)
}

func TestRelayResumeStaleRevisionRejected(t *testing.T) {
	rhz044CheckStale(t)
}

// Fake: 제품 코드의 주입 지점 추가 없이 두 번째 Append만 실패시킨다.
type rhz044SecondAppendFake struct {
	events.Port
	calls   int
	failure error
}

func (f *rhz044SecondAppendFake) Append(expected uint64, e events.Event) error {
	f.calls++
	if f.calls == 2 {
		return f.failure
	}
	return f.Port.Append(expected, e)
}

func TestRelayResumePlannedIntermediateFailure(t *testing.T) {
	f := &rhz044SecondAppendFake{
		Port: rhz044Store(t, domain.MissionPlanned), failure: errors.New("second append failed"),
	}
	before := f.All()
	r := rhz044Resume(t, f)
	if r.Accepted || !strings.Contains(r.Reason, "ready에서 중단됨") || !strings.Contains(r.Reason, f.failure.Error()) {
		t.Fatalf("partial failure result=%+v", r)
	}
	if f.calls != 2 {
		t.Fatalf("append attempts=%d want=2", f.calls)
	}
	after := f.All()
	if len(after) != len(before)+1 || !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatal("expected exactly one persisted transition")
	}
	// 실패 대역을 통해 남은 stream을 읽는다. backing store를 직접 읽지 않는다.
	m, err := projector.ReplayMission(f.List("mission", "m044"))
	if err != nil || m.State != domain.MissionReady || m.Revision != 2 {
		t.Fatalf("residual mission=%+v error=%v", m, err)
	}
	var p struct{ To domain.MissionState }
	last := after[len(after)-1]
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if last.Type != "mission.transitioned" || p.To != domain.MissionReady {
		t.Fatal("persisted transition was not planned -> ready")
	}
	// 안전한 ready 중간 상태에서 같은 관문으로 재시도할 수 있다.
	r = rhz044Resume(t, f)
	m, err = projector.ReplayMission(f.List("mission", "m044"))
	if !r.Accepted || err != nil || m.State != domain.MissionRunning {
		t.Fatalf("retry result=%+v mission=%+v error=%v", r, m, err)
	}
}

func rhz044GateJSON(t *testing.T, linked bool, name string) map[string]json.RawMessage {
	t.Helper()
	s := rhz044Store(t, domain.MissionPlanned)
	did := ""
	if linked {
		d, err := (decision.Service{Store: s}).Create(decision.Decision{
			ID: "wait044", MissionID: "m044", Kind: decision.WaitHuman, Reason: "review requested",
		})
		if err != nil {
			t.Fatal(err)
		}
		did = d.ID
	}
	key := approval.RequestKey{
		TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "gate044",
	}
	as := approval.Service{Store: s}
	var a approval.Ref
	var err error
	if name == "" {
		a, err = as.RecordInput(key, approval.Allow, "", "response044", "digest044", "test-operator", "corr044", did, noAuthority())
	} else {
		a, err = as.RecordInputWithGate(key, approval.Allow, "", "response044", "digest044", "test-operator", "corr044", did, noAuthority(), approval.GateFields{
			GateName: name, GateType: "approval", RequestedAction: "review", RiskTier: "logged",
			Request: approval.Request{Target: "workspace", RequestedBy: "test-operator", RequestedAt: "2000-01-01T00:00:00Z"},
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Gates) != 1 || p.Gates[0].ID != a.ID {
		t.Fatalf("unexpected gate projection: %+v", p.Gates)
	}
	b, err := json.Marshal(toDTO(p))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Gates []map[string]json.RawMessage `json:"gates"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Gates) != 1 {
		t.Fatalf("gate count=%d", len(body.Gates))
	}
	g := body.Gates[0]
	for _, key := range []string{"MissionID", "Name"} {
		if _, exists := g[key]; exists {
			t.Fatalf("PascalCase field leaked: %s", key)
		}
	}
	return g
}

func rhz044String(t *testing.T, g map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(g[key], &got); err != nil || got != want {
		t.Fatalf("%s=%s want=%q error=%v", key, g[key], want, err)
	}
}

func TestGateDTOFieldsBothPresent(t *testing.T) {
	g := rhz044GateJSON(t, true, "Review deployment")
	rhz044String(t, g, "missionId", "m044")
	rhz044String(t, g, "name", "Review deployment")
}
func TestGateDTONameOmittedMissionIncluded(t *testing.T) {
	g := rhz044GateJSON(t, true, "")
	rhz044String(t, g, "missionId", "m044")
	if _, exists := g["name"]; exists {
		t.Fatal("missing gate name must be omitted")
	}
}
func TestGateDTOMissionAndNameOmitted(t *testing.T) {
	g := rhz044GateJSON(t, false, "")
	for _, key := range []string{"missionId", "name"} {
		if _, exists := g[key]; exists {
			t.Fatalf("%s must be omitted", key)
		}
	}
}
func TestEmptyProjectionCollections(t *testing.T) {
	p, err := Snapshot(&events.Store{})
	if err != nil || p.Revision != 0 {
		t.Fatalf("empty snapshot: %+v error=%v", p, err)
	}
	b, err := json.Marshal(toDTO(p))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"missions", "tasks", "gates", "deliverables", "edges", "attention"} {
		if string(body[key]) != "[]" {
			t.Errorf("%s=%s want=[]", key, body[key])
		}
	}
}
func TestResumeLogImmutableOnRejection(t *testing.T) {
	for _, state := range []domain.MissionState{
		domain.MissionRunning, domain.MissionWaitingHuman, domain.MissionWaitingResult,
		domain.MissionBlocked, domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled,
	} {
		t.Run(string(state), func(t *testing.T) { rhz044Reject(t, state) })
	}
	t.Run("stale_revision", rhz044CheckStale)
}
