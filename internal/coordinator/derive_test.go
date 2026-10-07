package coordinator

// RHZ-048 tests (FR-RHZ-079): the completion record is derived
// deterministically by ApplyDecision itself — no human hand-recording path.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/decision"
	"rhizome/internal/deliverable"
	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/policy"
	"rhizome/internal/source"
	"rhizome/internal/workspace"
)

// deriveStore: mission "m" in running state (the existing apply_test idiom).
func deriveStore(t *testing.T) *events.Store {
	t.Helper()
	s := ms()
	if err := s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: []byte(`{"To":"running"}`)}); err != nil {
		t.Fatal(err)
	}
	return s
}

func secondMission(t *testing.T, s *events.Store, id string) {
	t.Helper()
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{id, "g", "x", "y"})
	if err := s.Append(0, events.Event{AggregateType: "mission", AggregateID: id, Revision: 1, Type: "mission.created", Payload: p}); err != nil {
		t.Fatal(err)
	}
}

func deriveExec(t *testing.T, s events.Port, missionID, key string) string {
	t.Helper()
	p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
	r, err := (execution.Service{Store: s}).IntentWithPolicy(missionID, key, p, p)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func completeDecision(t *testing.T, s events.Port, id, reason string, ev []decision.Evidence) decision.Decision {
	t.Helper()
	d, err := (decision.Service{Store: s}).Create(decision.Decision{ID: id, MissionID: "m", Kind: decision.Complete, Reason: reason, Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func countAggType(s events.Port, typ string) int {
	n := 0
	for _, e := range s.All() {
		if e.AggregateType == typ {
			n++
		}
	}
	return n
}

func reasonBlobID(reason string) string {
	sum := sha256.Sum256([]byte(reason))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// T1: applying a Complete decision makes the completion record exist by
// itself — machine-derived ID, kind, summary and execution source.
func TestCompleteDerivesDeliverableFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	execID := deriveExec(t, s, "m", "k")
	d := completeDecision(t, s, "d", "산출물 요지", []decision.Evidence{{SourceType: "execution", SourceID: execID}})
	m, err := (&Coordinator{Store: s}).ApplyDecision(d.ID)
	if err != nil || m.State != "succeeded" {
		t.Fatal(m, err)
	}
	dl, err := (deliverable.Service{Store: s}).Get("deliv-m")
	if err != nil {
		t.Fatal(err)
	}
	if dl.Kind != "completion" || dl.MissionID != "m" || dl.SourceRef != execID || dl.Summary != "산출물 요지" {
		t.Fatalf("derived: %+v", dl)
	}
}

// T2 (D27): the first execution evidence in document order wins.
func TestDeriveSourceRefFirstExecutionEvidenceFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	a := deriveExec(t, s, "m", "a")
	b := deriveExec(t, s, "m", "b")
	d := completeDecision(t, s, "d", "done", []decision.Evidence{
		{SourceType: "mission", SourceID: "m"},
		{SourceType: "execution", SourceID: a},
		{SourceType: "execution", SourceID: b},
	})
	if _, err := (&Coordinator{Store: s}).ApplyDecision(d.ID); err != nil {
		t.Fatal(err)
	}
	dl, err := (deliverable.Service{Store: s}).Get("deliv-m")
	if err != nil || dl.SourceRef != a {
		t.Fatalf("%+v %v (want %s)", dl, err, a)
	}
}

// T3 (D28): with no execution evidence the Reason bytes become a
// content-addressed source blob — fully deterministic, contract vocabulary
// unchanged. This is exactly the legacy shape (mission+goal evidence).
func TestDeriveWithoutExecutionEvidenceFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	d := completeDecision(t, s, "d", "정리 완료", []decision.Evidence{{SourceType: "mission", SourceID: "m"}})
	m, err := (&Coordinator{Store: s}).ApplyDecision(d.ID)
	if err != nil || m.State != "succeeded" {
		t.Fatal(m, err)
	}
	want := reasonBlobID("정리 완료")
	dl, err := (deliverable.Service{Store: s}).Get("deliv-m")
	if err != nil || dl.SourceRef != want || dl.Summary != "정리 완료" {
		t.Fatalf("%+v %v (want %s)", dl, err, want)
	}
	sr, err := (source.Service{Store: s}).Get(want)
	if err != nil || sr.MediaType != "text/markdown" || sr.SourceURI != "decision://d" || sr.SizeBytes != int64(len("정리 완료")) {
		t.Fatalf("source: %+v %v", sr, err)
	}
}

// T4: the derivation is a pure function — identical inputs, identical
// outputs, no clock or ordering influence.
func TestDeriveDeterministicFRRHZ079(t *testing.T) {
	d := decision.Decision{ID: "d", MissionID: "m", Kind: decision.Complete, Reason: "요지",
		Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}, {SourceType: "execution", SourceID: "exec-k"}}}
	a, ra := DeriveDeliverable(d)
	b, rb := DeriveDeliverable(d)
	if !reflect.DeepEqual(a, b) || ra != rb || ra {
		t.Fatalf("%+v/%v vs %+v/%v", a, ra, b, rb)
	}
	if a.ID != "deliv-m" || a.Kind != "completion" || a.SourceRef != "exec-k" || a.Summary != "요지" {
		t.Fatalf("derived: %+v", a)
	}
	d.Evidence = d.Evidence[:1] // no execution evidence -> register fallback
	c1, r1 := DeriveDeliverable(d)
	c2, r2 := DeriveDeliverable(d)
	if !reflect.DeepEqual(c1, c2) || !r1 || !r2 || c1.SourceRef != "" {
		t.Fatalf("fallback: %+v/%v vs %+v/%v", c1, r1, c2, r2)
	}
	if reasonBlobID("요지") != reasonBlobID("요지") {
		t.Fatal("content address not deterministic")
	}
}

// T5 (L-k): the exec-/mission bond is enforced on write
// (Create) and on read (Get) alike; the pure-format rules keep rejecting on
// Replay. A tampered stream appended directly never reaches consumers.
func TestDeliverableRuleSymmetryFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	secondMission(t, s, "m2")
	execID := deriveExec(t, s, "m", "k") // belongs to mission m
	// Write side: Create refuses a foreign-mission execution source.
	if _, err := (deliverable.Service{Store: s}).Create(deliverable.Deliverable{ID: "deliv-m2", Kind: "completion", MissionID: "m2", SourceRef: execID, Summary: "s"}); err == nil || !strings.Contains(err.Error(), "mission mismatch") {
		t.Fatal("create accepted foreign execution:", err)
	}
	// Read side: the same shape appended directly is rejected on Get.
	raw, _ := json.Marshal(struct{ ID, Kind, MissionID, SourceRef, Summary string }{"deliv-m2", "completion", "m2", execID, "s"})
	if err := s.Append(0, events.Event{AggregateType: "deliverable", AggregateID: "deliv-m2", Revision: 1, Type: "deliverable.declared", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := (deliverable.Service{Store: s}).Get("deliv-m2"); err == nil || !strings.Contains(err.Error(), "mission mismatch") {
		t.Fatal("tampered stream consumed:", err)
	}
	// Pure Replay format rules stay symmetric (regression).
	for _, bad := range []struct{ id, ref, summary string }{
		{"deliv-x", "unknown:ref", "s"},
		{"deliv-x", "exec-k", " "},
	} {
		p, _ := json.Marshal(struct{ ID, Kind, MissionID, SourceRef, Summary string }{bad.id, "completion", "m", bad.ref, bad.summary})
		if _, err := deliverable.Replay([]events.Event{{AggregateType: "deliverable", AggregateID: bad.id, Revision: 1, Type: "deliverable.declared", Payload: p, Sequence: 1}}); err == nil {
			t.Fatalf("replay accepted %+v", bad)
		}
	}
}

// U1: re-applying the same decision derives nothing twice.
func TestReapplyIdempotentNoDuplicateFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	execID := deriveExec(t, s, "m", "k")
	d := completeDecision(t, s, "d", "done", []decision.Evidence{{SourceType: "execution", SourceID: execID}})
	c := &Coordinator{Store: s}
	if _, err := c.ApplyDecision(d.ID); err != nil {
		t.Fatal(err)
	}
	n := len(s.All())
	for i := 0; i < 2; i++ {
		m, err := c.ApplyDecision(d.ID)
		if err != nil || m.State != "succeeded" {
			t.Fatal(m, err)
		}
	}
	if len(s.All()) != n || countAggType(s, "deliverable") != 1 {
		t.Fatalf("duplicated: events %d->%d deliverables %d", n, len(s.All()), countAggType(s, "deliverable"))
	}
}

// U2: a legacy hand-recorded deliverable in its real
// shape (own ID scheme, sha256: source) makes derivation skip — the mission
// already has its record, byte-for-byte untouched.
func TestLegacyHandRecordedSkipFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	sr, err := (source.Service{Store: s}).Register([]byte("legacy report body"), "text/markdown", "note://legacy.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (deliverable.Service{Store: s}).Create(deliverable.Deliverable{ID: "deliv-rhz043", Kind: "report", MissionID: "m", SourceRef: sr.BlobID, Summary: "손 기록 요지"}); err != nil {
		t.Fatal(err)
	}
	var legacyPayload []byte
	for _, e := range s.All() {
		if e.AggregateType == "deliverable" {
			legacyPayload = append([]byte(nil), e.Payload...)
		}
	}
	d := completeDecision(t, s, "d", "done", []decision.Evidence{{SourceType: "mission", SourceID: "m"}})
	m, err := (&Coordinator{Store: s}).ApplyDecision(d.ID)
	if err != nil || m.State != "succeeded" {
		t.Fatal(m, err)
	}
	if countAggType(s, "deliverable") != 1 {
		t.Fatal("derived beside legacy record")
	}
	for _, e := range s.All() {
		if e.AggregateType == "deliverable" && string(e.Payload) != string(legacyPayload) {
			t.Fatal("legacy record mutated")
		}
	}
}

type failOnce struct {
	events.Port
	match   func(events.Event) bool
	tripped bool
}

func (f *failOnce) Append(expected uint64, e events.Event) error {
	if !f.tripped && f.match(e) {
		f.tripped = true
		return errors.New("injected append failure")
	}
	return f.Port.Append(expected, e)
}

// U3 (D32): the record is durable before the transition, and a failure
// on either side leaves no poisoned state — a retry converges completely.
func TestDeliverableDurableBeforeTransitionFRRHZ079(t *testing.T) {
	t.Run("transition_fails_after_record", func(t *testing.T) {
		underlying := deriveStore(t)
		execID := deriveExec(t, underlying, "m", "k")
		d := completeDecision(t, underlying, "d", "done", []decision.Evidence{{SourceType: "execution", SourceID: execID}})
		s := &failOnce{Port: underlying, match: func(e events.Event) bool {
			return e.AggregateType == "mission" && e.Type == "mission.transitioned"
		}}
		c := &Coordinator{Store: s}
		if _, err := c.ApplyDecision(d.ID); err == nil {
			t.Fatal("injected failure swallowed")
		}
		if _, err := (deliverable.Service{Store: underlying}).Get("deliv-m"); err != nil {
			t.Fatal("record not durable before transition:", err)
		}
		if m, err := workspaceMissionState(underlying); err != nil || m != "running" {
			t.Fatal("mission poisoned:", m, err)
		}
		m, err := c.ApplyDecision(d.ID)
		if err != nil || m.State != "succeeded" || countAggType(underlying, "deliverable") != 1 {
			t.Fatal("retry did not converge:", m, err)
		}
	})
	t.Run("record_fails_before_transition", func(t *testing.T) {
		underlying := deriveStore(t)
		execID := deriveExec(t, underlying, "m", "k")
		d := completeDecision(t, underlying, "d", "done", []decision.Evidence{{SourceType: "execution", SourceID: execID}})
		s := &failOnce{Port: underlying, match: func(e events.Event) bool {
			return e.AggregateType == "deliverable"
		}}
		c := &Coordinator{Store: s}
		if _, err := c.ApplyDecision(d.ID); err == nil {
			t.Fatal("injected failure swallowed")
		}
		if m, err := workspaceMissionState(underlying); err != nil || m != "running" {
			t.Fatal("mission transitioned despite derivation failure:", m, err)
		}
		if countAggType(underlying, "deliverable") != 0 {
			t.Fatal("partial record")
		}
		m, err := c.ApplyDecision(d.ID)
		if err != nil || m.State != "succeeded" || countAggType(underlying, "deliverable") != 1 {
			t.Fatal("retry did not converge:", m, err)
		}
	})
}

func workspaceMissionState(s events.Port) (string, error) {
	p, err := workspace.Snapshot(s)
	if err != nil {
		return "", err
	}
	for _, task := range p.Tasks {
		if task.ID == "m" {
			return task.State, nil
		}
	}
	return "", errors.New("mission missing")
}

// U4 (D33): a mission made terminal in the pre-derivation era backfills
// its record on re-apply — self-healing through the idempotent path.
func TestReapplyTerminalBackfillsFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	d := completeDecision(t, s, "d", "과거 완료", []decision.Evidence{{SourceType: "mission", SourceID: "m"}})
	// The old code path: transition recorded directly, no deliverable.
	raw, _ := json.Marshal(struct {
		To         string
		DecisionID string
	}{"succeeded", d.ID})
	if err := s.Append(3, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 4, Type: "mission.transitioned", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	missionEvents := countAggType(s, "mission")
	m, err := (&Coordinator{Store: s}).ApplyDecision(d.ID)
	if err != nil || m.State != "succeeded" {
		t.Fatal(m, err)
	}
	dl, err := (deliverable.Service{Store: s}).Get("deliv-m")
	if err != nil || dl.SourceRef != reasonBlobID("과거 완료") {
		t.Fatal("backfill missing:", dl, err)
	}
	if countAggType(s, "mission") != missionEvents {
		t.Fatal("backfill touched the mission stream")
	}
	n := len(s.All())
	if _, err = (&Coordinator{Store: s}).ApplyDecision(d.ID); err != nil || len(s.All()) != n {
		t.Fatal("backfill not idempotent")
	}
}

// U5 (D34): the derived identity already taken by another mission's
// record surfaces as an error — no silent skip, no transition.
func TestSkipConflictingIdentityFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	secondMission(t, s, "m2")
	sr, err := (source.Service{Store: s}).Register([]byte("other"), "text/markdown", "note://other.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (deliverable.Service{Store: s}).Create(deliverable.Deliverable{ID: "deliv-m", Kind: "report", MissionID: "m2", SourceRef: sr.BlobID, Summary: "충돌"}); err != nil {
		t.Fatal(err)
	}
	d := completeDecision(t, s, "d", "done", []decision.Evidence{{SourceType: "mission", SourceID: "m"}})
	if _, err = (&Coordinator{Store: s}).ApplyDecision(d.ID); err == nil {
		t.Fatal("identity collision silently skipped")
	}
	if m, e := workspaceMissionState(s); e != nil || m != "running" {
		t.Fatal("mission transitioned despite collision:", m, e)
	}
}

// V1 (D35): Fail derives nothing — the failure is already durable as
// decision + mission.failed.
func TestFailDecisionNoDeliverableFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	d, err := (decision.Service{Store: s}).Create(decision.Decision{ID: "f", MissionID: "m", Kind: decision.Fail, Reason: "bad", Evidence: []decision.Evidence{{SourceType: "mission", SourceID: "m"}}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := (&Coordinator{Store: s}).ApplyDecision(d.ID)
	if err != nil || m.State != "failed" {
		t.Fatal(m, err)
	}
	if countAggType(s, "deliverable") != 0 || countAggType(s, "source") != 0 {
		t.Fatal("fail derived a record")
	}
}

// V2: the derived record reaches the workspace surface
// with no hand-recording anywhere in the path.
func TestCompleteEndToEndSurfaceFRRHZ079(t *testing.T) {
	s := deriveStore(t)
	d := completeDecision(t, s, "d", "최종 보고", []decision.Evidence{{SourceType: "mission", SourceID: "m"}})
	if _, err := (&Coordinator{Store: s}).ApplyDecision(d.ID); err != nil {
		t.Fatal(err)
	}
	p, err := workspace.Snapshot(s)
	if err != nil || len(p.Deliverables) != 1 {
		t.Fatal(p.Deliverables, err)
	}
	if p.Deliverables[0].ID != "deliv-m" || p.Deliverables[0].Summary != "최종 보고" || p.Deliverables[0].SourceRef != reasonBlobID("최종 보고") {
		t.Fatalf("surface: %+v", p.Deliverables[0])
	}
	srv := httptest.NewServer(workspace.NewHTTP(s).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/workspace")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	defer resp.Body.Close()
	var env struct {
		Body struct {
			Deliverables []map[string]any `json:"deliverables"`
		} `json:"body"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&env); err != nil || len(env.Body.Deliverables) != 1 {
		t.Fatal(env, err)
	}
	if env.Body.Deliverables[0]["id"] != "deliv-m" || env.Body.Deliverables[0]["summary"] != "최종 보고" {
		t.Fatalf("dto: %v", env.Body.Deliverables[0])
	}
}

// V3: the existing evidence validation is untouched, and a refused
// apply derives nothing (no partial writes).
func TestEvidenceValidationUnchangedFRRHZ079(t *testing.T) {
	t.Run("missing_evidence", func(t *testing.T) {
		s := deriveStore(t)
		d := completeDecision(t, s, "d", "done", []decision.Evidence{{SourceType: "execution", SourceID: "exec-ghost"}})
		n := len(s.All())
		if _, err := (&Coordinator{Store: s}).ApplyDecision(d.ID); err == nil || len(s.All()) != n {
			t.Fatal("missing evidence accepted or wrote")
		}
	})
	t.Run("future_evidence", func(t *testing.T) {
		s := deriveStore(t)
		d := completeDecision(t, s, "d", "done", []decision.Evidence{{SourceType: "execution", SourceID: "exec-late"}})
		deriveExec(t, s, "m", "late") // created after the decision
		n := len(s.All())
		if _, err := (&Coordinator{Store: s}).ApplyDecision(d.ID); err == nil || len(s.All()) != n {
			t.Fatal("future evidence accepted or wrote")
		}
	})
	t.Run("foreign_mission_execution", func(t *testing.T) {
		s := deriveStore(t)
		secondMission(t, s, "m2")
		// An execution belonging to m2, offered as evidence for m's decision.
		p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
		r, err := (execution.Service{Store: s}).IntentWithPolicy("m2", "foreign", p, p)
		if err != nil {
			t.Fatal(err)
		}
		d := completeDecision(t, s, "d", "done", []decision.Evidence{{SourceType: "execution", SourceID: r.ID}})
		n := len(s.All())
		if _, err := (&Coordinator{Store: s}).ApplyDecision(d.ID); err == nil || len(s.All()) != n {
			t.Fatal("foreign execution evidence accepted or wrote")
		}
	})
}
