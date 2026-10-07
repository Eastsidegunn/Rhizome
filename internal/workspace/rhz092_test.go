package workspace

// RHZ-092 (FR-RHZ-123): mission.start relay — rejection order with zero
// writes, planned → ready → running chain before the start hook, idempotent
// lookup, handle resolution, HTTP wire (executionId, /v1/execution session),
// NDJSON round trip. The hook is a fake starter; the adapter is never
// imported here.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
	"rhizome/internal/projector"
)

// fakeStarter records requests and, on Start, writes the execution kernel's
// intent/claim/accepted triple itself (what the adapter starter does with a
// Fake Runner). reason/existing script the Prepare answer.
type fakeStarter struct {
	s        events.Port
	prepared []ExecStartRequest
	started  []ExecStartRequest
	reason   string
	existing string
	startErr string
}

func (f *fakeStarter) key(r ExecStartRequest) string { return "k-" + r.MissionID + "-" + r.Instruction }
func (f *fakeStarter) Prepare(r ExecStartRequest) (ExecStartPlan, string, error) {
	f.prepared = append(f.prepared, r)
	if len(f.s.List("mission", r.MissionID)) == 0 {
		panic("Prepare after an unknown mission")
	}
	return ExecStartPlan{ExecutionID: "exec-" + f.key(r), Existing: f.existing}, f.reason, nil
}
func (f *fakeStarter) Start(r ExecStartRequest) (string, string, error) {
	f.started = append(f.started, r)
	es := execution.Service{Store: f.s}
	p := policy.Policy{Capabilities: []string{"fs:workspace"}, Budget: 200000, Timeout: 600000, MaxDepth: 2, Units: "tokens-ms-v1"}
	ref, err := es.IntentWithPolicy(r.MissionID, f.key(r), p, p)
	if err != nil {
		return "", "", err
	}
	if ref, err = es.ClaimDispatch(ref.ID, "janus", "c"); err != nil {
		return "", "", err
	}
	if f.startErr != "" {
		return ref.ID, f.startErr, nil
	}
	if ref, err = es.Accept(ref.ID, injectTrace); err != nil {
		return "", "", err
	}
	return ref.ID, "", nil
}

func startRelay(t *testing.T, s events.Port, in Intent, st ExecStarter) RelayResult {
	t.Helper()
	r, e := RelayIntentHooks(s, in, "op", false, RelayHooks{Start: st})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func missionTypes(s events.Port) string {
	out := []string{}
	for _, e := range s.All() {
		if e.AggregateType == "execution" || e.Type == "mission.transitioned" {
			out = append(out, e.Type)
		}
	}
	return strings.Join(out, ",")
}

func runningMission(t *testing.T) *events.Store {
	t.Helper()
	s := ws(t)
	m := mission.Service{Store: s}
	if _, e := m.Transition("m", 1, domain.MissionReady); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Transition("m", 2, domain.MissionRunning); e != nil {
		t.Fatal(e)
	}
	return s
}

// S1: running mission — exactly the three execution events, instruction
// defaults to the mission description, no mission transition.
func TestRelayStartRunningMissionFRRHZ123(t *testing.T) {
	s := runningMission(t)
	f := &fakeStarter{s: s}
	r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f)
	if !r.Accepted || r.Reason != "" || r.ExecutionID != "exec-k-m-mission" {
		t.Fatalf("%+v", r)
	}
	// The two transitions come from the runningMission fixture; the start
	// itself adds only the three execution events (no mission transition).
	if got := missionTypes(s); got != "mission.transitioned,mission.transitioned,execution.intent,execution.dispatch_claimed,execution.accepted" {
		t.Fatal(got)
	}
	if len(f.started) != 1 || f.started[0].Instruction != "mission" || f.started[0].SessionMode != "" || f.started[0].Budget.Tokens != nil {
		t.Fatalf("%+v", f.started)
	}
	p, err := ExecutionSnapshot(s, "m", nil)
	if err != nil || len(p.Sessions) != 1 || p.Sessions[0].State != "running" || p.Sessions[0].ID != r.ExecutionID {
		t.Fatalf("%+v %v", p, err)
	}
	// Explicit instruction / budget / sessionMode ride through verbatim.
	r = startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m", Instruction: "  other  ", SessionMode: "oneshot", Budget: &BudgetOverride{Tokens: i64w(5)}}, f)
	if !r.Accepted || f.started[1].Instruction != "other" || f.started[1].SessionMode != "oneshot" || *f.started[1].Budget.Tokens != 5 {
		t.Fatalf("%+v %+v", r, f.started[1])
	}
}

func i64w(v int64) *int64 { return &v }

// S2: planned mission — the three execution events, THEN ready, running
// (the mission becomes running only once an execution is live);
// paused resumes to running; ready goes straight to running.
func TestRelayStartPlannedChainFRRHZ123(t *testing.T) {
	s := ws(t)
	f := &fakeStarter{s: s}
	r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f)
	if !r.Accepted {
		t.Fatalf("%+v", r)
	}
	if got := missionTypes(s); got != "execution.intent,execution.dispatch_claimed,execution.accepted,mission.transitioned,mission.transitioned" {
		t.Fatal(got)
	}
	log := s.List("mission", "m")
	for i, want := range []domain.MissionState{domain.MissionReady, domain.MissionRunning} {
		if ms, e := projector.ReplayMission(log[:i+2]); e != nil || ms.State != want {
			t.Fatalf("transition %d: %s %v", i, ms.State, e)
		}
	}
	// Prepare ran before the transition (zero-write guard order).
	if len(f.prepared) != 1 {
		t.Fatal("Prepare not called once")
	}
	s = ws(t)
	if _, e := (mission.Service{Store: s}).Transition("m", 1, domain.MissionReady); e != nil {
		t.Fatal(e)
	}
	if r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, &fakeStarter{s: s}); !r.Accepted {
		t.Fatalf("%+v", r)
	}
	if cur, _ := Snapshot(s); cur.Tasks[0].State != "running" {
		t.Fatal(cur.Tasks[0].State)
	}
	s = runningMission(t)
	if _, e := (mission.Service{Store: s}).Transition("m", 3, domain.MissionPaused); e != nil {
		t.Fatal(e)
	}
	if r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, &fakeStarter{s: s}); !r.Accepted {
		t.Fatalf("%+v", r)
	}
	if cur, _ := Snapshot(s); cur.Tasks[0].State != "running" {
		t.Fatal(cur.Tasks[0].State)
	}
}

// S3/S5: every rejection is zero writes and happens in order — missionId,
// unknown mission, terminal (ErrInvalidState), hook nil, Prepare reason
// (ledger not loaded / budget) — and Start is never reached.
func TestRelayStartRejectionsZeroWritesFRRHZ123(t *testing.T) {
	s := runningMission(t)
	f := &fakeStarter{s: s}
	before := len(s.All())
	check := func(in Intent, st ExecStarter, want string) {
		t.Helper()
		r := startRelay(t, s, in, st)
		if r.Accepted || !strings.HasPrefix(r.Reason, want) {
			t.Fatalf("%+v want %q", r, want)
		}
		if len(s.All()) != before {
			t.Fatalf("%q wrote %d events", want, len(s.All())-before)
		}
	}
	check(Intent{Kind: "mission.start"}, f, "missionId required")
	check(Intent{Kind: "mission.start", MissionID: "nope"}, f, `unknown mission "nope"`)
	check(Intent{Kind: "mission.start", MissionID: "m"}, nil, "execution start unavailable")
	f.reason = "execution config not loaded"
	check(Intent{Kind: "mission.start", MissionID: "m"}, f, "execution config not loaded")
	f.reason = "POLICY_DENIED: budget.tokens 300000 exceeds ceiling 200000"
	check(Intent{Kind: "mission.start", MissionID: "m", Budget: &BudgetOverride{Tokens: i64w(300000)}}, f, "POLICY_DENIED")
	f.reason = "BUDGET_INVALID: budget.maxDepth must be > 0"
	check(Intent{Kind: "mission.start", MissionID: "m"}, f, "BUDGET_INVALID")
	if len(f.started) != 0 {
		t.Fatal("Start reached on rejection")
	}
	// Terminal mission: ErrInvalidState before the hook (hook nil is fine).
	if r, e := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "m"}, "op", true); e != nil || !r.Accepted {
		t.Fatal(r, e)
	}
	before = len(s.All())
	f.reason = ""
	check(Intent{Kind: "mission.start", MissionID: "m"}, f, domain.ErrInvalidState.Error())
	check(Intent{Kind: "mission.start", MissionID: "m"}, nil, domain.ErrInvalidState.Error())
	if len(f.prepared) != 3 {
		t.Fatalf("Prepare calls %d (terminal must not reach the hook)", len(f.prepared))
	}
	// Planned mission: a Prepare rejection must not have moved the mission
	// either (validation strictly before the resume chain).
	s = ws(t)
	f = &fakeStarter{s: s, reason: "POLICY_DENIED: budget.tokens 300000 exceeds ceiling 200000"}
	before = len(s.All())
	check(Intent{Kind: "mission.start", MissionID: "m", Budget: &BudgetOverride{Tokens: i64w(300000)}}, f, "POLICY_DENIED")
	f.reason = "execution config not loaded"
	check(Intent{Kind: "mission.start", MissionID: "m"}, f, "execution config not loaded")
	if p, _ := Snapshot(s); p.Tasks[0].State != "queued" {
		t.Fatalf("planned mission moved to %s on rejection", p.Tasks[0].State)
	}
}

// S4: idempotent re-submission — Prepare finds the running execution, the
// relay answers Accepted with the same executionId and writes nothing (no
// transition either); a run-level refusal returns the id with the reason.
func TestRelayStartIdempotentAndFailureFRRHZ123(t *testing.T) {
	s := ws(t)
	f := &fakeStarter{s: s}
	r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f)
	before := len(s.All())
	f.existing = "accepted"
	for _, ex := range []string{"accepted", "observing"} {
		f.existing = ex
		r2 := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f)
		if !r2.Accepted || r2.ExecutionID != r.ExecutionID || len(s.All()) != before || len(f.started) != 1 {
			t.Fatalf("%s: %+v writes=%d starts=%d", ex, r2, len(s.All())-before, len(f.started))
		}
	}
	s = ws(t)
	f = &fakeStarter{s: s, startErr: "POLICY_DENIED: fs_scope"}
	r = startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f)
	if r.Accepted || r.Reason != "POLICY_DENIED: fs_scope" || r.ExecutionID == "" {
		t.Fatalf("%+v", r)
	}
	// F2: a run-level refusal leaves the mission where it was (planned) —
	// no phantom running mission; only the durable intent + claim remain.
	if got := missionTypes(s); got != "execution.intent,execution.dispatch_claimed" {
		t.Fatal(got)
	}
	if ms, e := projector.ReplayMission(s.List("mission", "m")); e != nil || ms.State != domain.MissionPlanned {
		t.Fatalf("mission must stay planned after a failed start: %s %v", ms.State, e)
	}
}

// F4: blocked and the waiting_* states are refused with zero
// writes; Prepare is never reached.
func TestRelayStartRefusedStatesFRRHZ123(t *testing.T) {
	for _, path := range [][]domain.MissionState{
		{domain.MissionReady, domain.MissionBlocked},
		{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman},
		{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult},
	} {
		s := ws(t)
		m := mission.Service{Store: s}
		for i, st := range path {
			var e error
			if st == domain.MissionBlocked {
				_, e = m.TransitionWithReason("m", uint64(i+1), st, "external")
			} else {
				_, e = m.Transition("m", uint64(i+1), st)
			}
			if e != nil {
				t.Fatal(e)
			}
		}
		before := len(s.All())
		f := &fakeStarter{s: s}
		r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m"}, f)
		want := string(path[len(path)-1]) + "에서 실행 시작 불가"
		if r.Accepted || r.Reason != want || len(s.All()) != before || len(f.prepared) != 0 {
			t.Fatalf("%v: %+v writes=%d prepared=%d", path, r, len(s.All())-before, len(f.prepared))
		}
	}
}

// F5: two identical concurrent starts — the loser's duplicate-key
// / revision-conflict error is answered idempotently from the store: both
// Accepted with the same executionId, exactly one intent and one claim.
func TestRelayStartConcurrentIdenticalFRRHZ123(t *testing.T) {
	s := runningMission(t)
	f := &raceStarter{fakeStarter: fakeStarter{s: s}}
	var wg sync.WaitGroup
	res := make([]RelayResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = RelayIntentHooks(s, Intent{Kind: "mission.start", MissionID: "m"}, "op", false, RelayHooks{Start: f})
		}(i)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		if errs[i] != nil || !res[i].Accepted || res[i].ExecutionID == "" {
			t.Fatalf("racer %d: %+v %v", i, res[i], errs[i])
		}
	}
	if res[0].ExecutionID != res[1].ExecutionID {
		t.Fatalf("different ids: %q %q", res[0].ExecutionID, res[1].ExecutionID)
	}
	n := map[string]int{}
	for _, e := range s.All() {
		n[e.Type]++
	}
	if n["execution.intent"] != 1 || n["execution.dispatch_claimed"] != 1 {
		t.Fatalf("want one intent and one claim: %v", n)
	}
}

// raceStarter widens the race window: both racers pass Prepare before either
// appends, so the loser hits the duplicate idempotency key on IntentWithPolicy.
type raceStarter struct {
	fakeStarter
	mu      sync.Mutex
	gate    sync.WaitGroup
	armed   bool
	entered int
}

func (r *raceStarter) Prepare(q ExecStartRequest) (ExecStartPlan, string, error) {
	r.mu.Lock()
	if !r.armed {
		r.armed = true
		r.gate.Add(2)
	}
	r.mu.Unlock()
	key := "exec-" + r.key(q)
	existing := ""
	if refs := r.s.List("execution", key); len(refs) > 0 {
		existing = "accepted"
	}
	return ExecStartPlan{ExecutionID: key, Existing: existing}, "", nil
}

func (r *raceStarter) Start(q ExecStartRequest) (string, string, error) {
	r.mu.Lock()
	r.entered++
	first := r.entered <= 2
	r.mu.Unlock()
	if first {
		r.gate.Done()
		r.gate.Wait() // both racers reach Start before either appends
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fakeStarter.Start(q)
}

// S7: missionId may be an RHZ-073 handle.
func TestRelayStartHandleFRRHZ123(t *testing.T) {
	s := runningMission(t)
	hi := buildHandleIndex(s.All())
	handle := ""
	for _, tag := range handleTags {
		if h := hi.of(tag, "m"); h != "" && hi.resolve(h) == "m" {
			handle = h
		}
	}
	if handle == "" {
		t.Fatal("no handle for mission m")
	}
	f := &fakeStarter{s: s}
	if r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: handle}, f); !r.Accepted || f.started[0].MissionID != "m" {
		t.Fatalf("%+v %+v", r, f.started)
	}
}

// HTTP wire: POST /v1/intent {kind:"mission.start", budget:{tokens:0}} keeps
// a present-but-zero axis (pointer), the response carries executionId, and
// GET /v1/execution/m shows the running session.
func TestRelayStartHTTPFRRHZ123(t *testing.T) {
	s := runningMission(t)
	h := NewHTTP(s)
	f := &fakeStarter{s: s}
	h.ExecStart = f
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	post := func(body string) map[string]any {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != 200 {
			t.Fatal(resp, err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return out
	}
	out := post(`{"kind":"mission.start","missionId":"m","budget":{"tokens":0},"sessionMode":"multiturn"}`)
	if len(f.prepared) != 1 || f.prepared[0].Budget.Tokens == nil || *f.prepared[0].Budget.Tokens != 0 || f.prepared[0].Budget.TimeMs != nil || f.prepared[0].SessionMode != "multiturn" {
		t.Fatalf("%+v", f.prepared)
	}
	if out["Accepted"] != true || out["executionId"] != "exec-k-m-mission" {
		t.Fatalf("%v", out)
	}
	resp, err := http.Get(srv.URL + "/v1/execution/m")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Body struct {
			Sessions []struct{ ID, State string }
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if len(env.Body.Sessions) != 1 || env.Body.Sessions[0].State != "running" {
		t.Fatalf("%+v", env)
	}
	h.ExecStart = nil
	if out := post(`{"kind":"mission.start","missionId":"m","instruction":"again"}`); out["Accepted"] != false || out["Reason"] != "execution start unavailable" || out["executionId"] != nil {
		t.Fatalf("%v", out)
	}
}

// R1: NDJSON journal round trip — the started execution and the mission
// transitions replay identically after reopen, with only existing event
// types in the file.
func TestRelayStartJournalRoundTripFRRHZ123(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := mission.Service{Store: j}
	if _, e := m.CreateGoal("g", "goal", "done", "p"); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Create("m", "g", "mission", "ok"); e != nil {
		t.Fatal(e)
	}
	r := startRelay(t, j, Intent{Kind: "mission.start", MissionID: "m"}, &fakeStarter{s: j})
	if !r.Accepted {
		t.Fatalf("%+v", r)
	}
	before, err := ExecutionSnapshot(j, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	wsBefore, _ := Snapshot(j)
	j.Close()
	j2, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	for _, e := range j2.All() {
		switch e.Type {
		case "goal.created", "mission.created", "mission.transitioned", "execution.intent", "execution.dispatch_claimed", "execution.accepted":
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
	}
	after, err := ExecutionSnapshot(j2, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatalf("execution projection changed:\n%s\n%s", a, b)
	}
	wsAfter, _ := Snapshot(j2)
	a, _ = json.Marshal(wsBefore)
	b, _ = json.Marshal(wsAfter)
	if !bytes.Equal(a, b) || wsAfter.Tasks[0].State != "running" {
		t.Fatalf("workspace projection changed:\n%s\n%s", a, b)
	}
}
