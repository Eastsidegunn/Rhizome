package workspace

// RHZ-078 (FR-RHZ-109): gate.requestChanges on an internal gate (question).
// A third, non-terminal decision "requestChanges" is recorded as the existing
// question.answered event; the gate projects as "changes_requested" and still
// accepts approve/reject (no re-ask). R1 happy path, R2 reason/digest guards,
// R3 terminal guard, R4 sequencing, R5 capability ⇔ relay truth table,
// R6 NDJSON round trip + determinism incl. legacy journals.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/procedure"
	"rhizome/internal/question"
)

type wire078 struct {
	Body struct {
		Gates            []map[string]any             `json:"gates"`
		GateCapabilities map[string]map[string]string `json:"gateCapabilities"`
	} `json:"body"`
}

func decode078(t *testing.T, s events.Port) wire078 {
	t.Helper()
	raw := getWorkspace071(t, s)
	var w wire078
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	return w
}

// askBound078 asks a mission-bound question through the relay and returns id + digest.
func askBound078(t *testing.T, s events.Port, name string) (string, string) {
	t.Helper()
	res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: name, Body: "body " + name, Recommendation: "r", MissionID: "mission-078"}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("ask %s: %v %+v", name, err, res)
	}
	return mustQuestionID(name, "body "+name, "r"), question.Digest(name, "body "+name, "r")
}

func store078(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	missionIn062(t, s, "mission-078", domain.MissionReady, domain.MissionRunning)
	return s
}

func countAnswered078(s events.Port, id string) int {
	n := 0
	for _, e := range s.List("question", id) {
		if e.Type == "question.answered" {
			n++
		}
	}
	return n
}

// R1: pending → requestChanges(reason) accepted → state changes_requested,
// decisionReason/decidedBy projected, journal +1 question.answered (no new type).
func TestGateRequestChangesRecordsQuestionAnsweredFRRHZ109(t *testing.T) {
	s := store078(t)
	id, digest := askBound078(t, s, "r1")
	before := len(s.All())
	res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: id, Digest: digest, Reason: "  더 짧게  "}, "alice", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("requestChanges: %v %+v", err, res)
	}
	if len(s.All()) != before+1 || countAnswered078(s, id) != 1 {
		t.Fatalf("journal: %d events (before %d), answered=%d", len(s.All()), before, countAnswered078(s, id))
	}
	last := s.All()[len(s.All())-1]
	var p map[string]string
	_ = json.Unmarshal(last.Payload, &p)
	if last.Type != "question.answered" || p["Decision"] != "requestChanges" {
		t.Fatalf("event: %s %s", last.Type, last.Payload)
	}
	w := decode078(t, s)
	g := gateByID075(t, w.Body.Gates, id)
	if g["state"] != "changes_requested" || g["decisionReason"] != "  더 짧게  " || g["decidedBy"] != "unverified-local-operator:alice" {
		t.Fatalf("gate: %v", g)
	}
	// instruction fallback: the cockpit's decision text for gate.requestChanges travels as `instruction`
	id2, digest2 := askBound078(t, s, "r1b")
	if res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: id2, Digest: digest2, Instruction: "via instruction"}, "alice", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("instruction fallback: %v %+v", err, res)
	}
	if q, _ := (question.Service{Store: s}).Get(id2); q.Decision != question.RequestChanges || q.Reason != "via instruction" {
		t.Fatalf("fallback reason: %+v", q)
	}
}

// R2: empty/whitespace reason, missing digest, digest mismatch → rejected, journal unchanged.
func TestGateRequestChangesGuardsFRRHZ109(t *testing.T) {
	s := store078(t)
	id, digest := askBound078(t, s, "r2")
	before := journalBytes075(t, s)
	cases := map[string]Intent{
		"empty reason":      {Kind: "gate.requestChanges", GateID: id, Digest: digest},
		"whitespace reason": {Kind: "gate.requestChanges", GateID: id, Digest: digest, Reason: " \t\n"},
		"missing digest":    {Kind: "gate.requestChanges", GateID: id, Reason: "fix"},
		"digest mismatch":   {Kind: "gate.requestChanges", GateID: id, Digest: question.Digest("r2", "other", "r"), Reason: "fix"},
	}
	wantReason := map[string]string{"empty reason": "reason required", "whitespace reason": "reason required", "missing digest": "digest required", "digest mismatch": question.ErrDigestMismatch.Error()}
	for name, in := range cases {
		res, err := RelayIntent(s, in, "alice", noAuthority())
		if err != nil || res.Accepted || res.Reason != wantReason[name] {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
	if !bytes.Equal(before, journalBytes075(t, s)) {
		t.Fatal("journal changed on rejected requestChanges")
	}
	if w := decode078(t, s); gateByID075(t, w.Body.Gates, id)["state"] != "pending" {
		t.Fatal("state changed")
	}
}

// R3: approved and rejected questions → requestChanges rejected, journal unchanged.
func TestGateRequestChangesAfterTerminalRejectedFRRHZ109(t *testing.T) {
	s := store078(t)
	ida, da := askBound078(t, s, "r3a")
	idr, dr := askBound078(t, s, "r3r")
	if res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: ida, Digest: da}, "alice", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("approve: %v %+v", err, res)
	}
	if res, err := RelayIntent(s, Intent{Kind: "gate.reject", GateID: idr, Digest: dr, Reason: "no"}, "alice", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("reject: %v %+v", err, res)
	}
	before := journalBytes075(t, s)
	for _, c := range [][2]string{{ida, da}, {idr, dr}} {
		res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: c[0], Digest: c[1], Reason: "too late"}, "alice", noAuthority())
		if err != nil || res.Accepted || res.Reason != "gate already has input" {
			t.Errorf("%s: %v %+v", c[0], err, res)
		}
	}
	if !bytes.Equal(before, journalBytes075(t, s)) {
		t.Fatal("journal changed")
	}
	w := decode078(t, s)
	if gateByID075(t, w.Body.Gates, ida)["state"] != "approved" || gateByID075(t, w.Body.Gates, idr)["state"] != "rejected" {
		t.Fatalf("states: %v", w.Body.Gates)
	}
}

// R4: changes_requested → approve OK (state approved; reason/decidedBy of the
// approve), → second requestChanges rejected; changes_requested → reject OK.
func TestGateRequestChangesThenDecideFRRHZ109(t *testing.T) {
	s := store078(t)
	ida, da := askBound078(t, s, "r4a")
	idr, dr := askBound078(t, s, "r4r")
	for _, c := range [][2]string{{ida, da}, {idr, dr}} {
		if res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: c[0], Digest: c[1], Reason: "first"}, "alice", noAuthority()); err != nil || !res.Accepted {
			t.Fatalf("requestChanges %s: %v %+v", c[0], err, res)
		}
	}
	before := journalBytes075(t, s)
	res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: ida, Digest: da, Reason: "second"}, "alice", noAuthority())
	if err != nil || res.Accepted || res.Reason != "changes already requested" {
		t.Fatalf("second requestChanges: %v %+v", err, res)
	}
	if !bytes.Equal(before, journalBytes075(t, s)) {
		t.Fatal("journal changed on second requestChanges")
	}
	// approve after changes_requested — same digest, no re-ask
	if res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: ida, Digest: da, Reason: "fine now"}, "bob", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("approve after changes_requested: %v %+v", err, res)
	}
	if res, err := RelayIntent(s, Intent{Kind: "gate.reject", GateID: idr, Digest: dr, Reason: "still wrong"}, "bob", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("reject after changes_requested: %v %+v", err, res)
	}
	w := decode078(t, s)
	ga := gateByID075(t, w.Body.Gates, ida)
	if ga["state"] != "approved" || ga["decisionReason"] != "fine now" || ga["decidedBy"] != "unverified-local-operator:bob" {
		t.Fatalf("approved gate: %v", ga)
	}
	gr := gateByID075(t, w.Body.Gates, idr)
	if gr["state"] != "rejected" || gr["decisionReason"] != "still wrong" || gr["decidedBy"] != "unverified-local-operator:bob" {
		t.Fatalf("rejected gate: %v", gr)
	}
	if countAnswered078(s, ida) != 2 || countAnswered078(s, idr) != 2 {
		t.Fatal("expected two question.answered per gate")
	}
	if q, _ := (question.Service{Store: s}).Get(ida); q.Revision != 3 {
		t.Fatalf("revision %d want 3", q.Revision)
	}
	// terminal now: nothing more
	for _, k := range []string{"gate.approve", "gate.reject", "gate.requestChanges"} {
		if res, err := RelayIntent(s, Intent{Kind: k, GateID: ida, Digest: da, Reason: "x"}, "bob", noAuthority()); err != nil || res.Accepted {
			t.Errorf("%s after approve accepted: %+v", k, res)
		}
	}
}

// R5: gateCapabilities table for pending(bound/unbound)/changes_requested/
// approved, and for every cell the relay is called (fresh store each time):
// enabled ⇔ Accepted — requestChanges enabled is now really accepted.
func TestGateCapabilitiesMatchRelayFRRHZ109(t *testing.T) {
	type row struct {
		approve, reject, requestChanges string
	}
	cases := map[string]row{
		"pending-bound":     {"enabled", "enabled", "enabled"},
		"pending-goal":      {"enabled", "enabled", "enabled"}, // goal-bound (RHZ-075) counts as bound
		"pending-unbound":   {"enabled", "enabled", "hidden"},
		"changes_requested": {"enabled", "enabled", "hidden"},
		"approved":          {"hidden", "hidden", "hidden"},
	}
	build := func(t *testing.T, state string) (*events.Store, string, string) {
		s := store078(t)
		var id, digest string
		switch state {
		case "pending-unbound":
			q, err := (question.Service{Store: s}).Ask("u", "body u", "r", "", "", "tester", "")
			if err != nil {
				t.Fatal(err)
			}
			id, digest = q.ID, q.Digest
		case "pending-goal":
			if _, err := (mission.Service{Store: s}).CreateGoal("goal-r5", "g", "ok", ""); err != nil {
				t.Fatal(err)
			}
			if res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: "pg", Body: "body pg", Recommendation: "r", GoalID: "goal-r5"}, "tester", noAuthority()); err != nil || !res.Accepted {
				t.Fatalf("goal-bound ask: %v %+v", err, res)
			}
			id, digest = mustQuestionID("pg", "body pg", "r"), question.Digest("pg", "body pg", "r")
		default:
			id, digest = askBound078(t, s, state)
		}
		switch state {
		case "changes_requested":
			if res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: id, Digest: digest, Reason: "r"}, "tester", noAuthority()); err != nil || !res.Accepted {
				t.Fatalf("fixture: %v %+v", err, res)
			}
		case "approved":
			if res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: id, Digest: digest}, "tester", noAuthority()); err != nil || !res.Accepted {
				t.Fatalf("fixture: %v %+v", err, res)
			}
		}
		return s, id, digest
	}
	for state, want := range cases {
		s, id, _ := build(t, state)
		w := decode078(t, s)
		got, ok := w.Body.GateCapabilities[id]
		if !ok {
			t.Fatalf("%s: capabilities missing", state)
		}
		if got["approve"] != want.approve || got["reject"] != want.reject || got["requestChanges"] != want.requestChanges {
			t.Errorf("%s: got %v want %+v", state, got, want)
		}
		if ps := gateByID075(t, w.Body.Gates, id)["state"]; (state == "changes_requested" && ps != "changes_requested") || (state == "approved" && ps != "approved") || (strings.HasPrefix(state, "pending") && ps != "pending") {
			t.Errorf("%s: projected state %v", state, ps)
		}
		for kind, level := range map[string]string{"gate.approve": got["approve"], "gate.reject": got["reject"], "gate.requestChanges": got["requestChanges"]} {
			fs, fid, fdigest := build(t, state)
			res, err := RelayIntent(fs, Intent{Kind: kind, GateID: fid, Digest: fdigest, Reason: "because"}, "tester", noAuthority())
			if err != nil {
				t.Fatal(err)
			}
			if res.Accepted != (level == "enabled") {
				t.Errorf("%s: %s capability=%q but relay Accepted=%v (%s)", state, kind, level, res.Accepted, res.Reason)
			}
		}
	}
}

// R6: NDJSON round trip — a changes_requested gate and a changes_requested→
// approved gate survive Close/Open with byte-identical /v1/workspace and
// Snapshot; a legacy journal (approve/reject only) replays unchanged.
func TestJournalRoundTripChangesRequestedFRRHZ109(t *testing.T) {
	path := t.TempDir() + "/events.ndjson"
	j, err := openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("goal-078", "g", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-078", "goal-078", "m", "ok"); err != nil {
		t.Fatal(err)
	}
	idc, dc := askBound078(t, j, "rc")
	ida, da := askBound078(t, j, "ra")
	for _, c := range [][2]string{{idc, dc}, {ida, da}} {
		if res, err := RelayIntent(j, Intent{Kind: "gate.requestChanges", GateID: c[0], Digest: c[1], Reason: "tighten"}, "alice", noAuthority()); err != nil || !res.Accepted {
			t.Fatalf("requestChanges: %v %+v", err, res)
		}
	}
	if res, err := RelayIntent(j, Intent{Kind: "gate.approve", GateID: ida, Digest: da}, "bob", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("approve: %v %+v", err, res)
	}
	// legacy: question.answered exactly as a pre-RHZ-078 kernel wrote it (reject)
	dl := question.Digest("ql", "b", "r")
	legacyID := mustQuestionIDForDigest(dl)
	asked, _ := json.Marshal(map[string]string{"Title": "ql", "Body": "b", "Recommendation": "r", "MissionID": "mission-078", "RequestedBy": "unverified-local-operator:legacy", "CorrelationID": "", "Digest": dl})
	if err := j.Append(0, events.Event{AggregateType: "question", AggregateID: legacyID, Revision: 1, Type: "question.asked", Payload: asked, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	answered, _ := json.Marshal(map[string]string{"Decision": "reject", "Reason": "old", "ActorRef": "unverified-local-operator:legacy", "Digest": dl})
	if err := j.Append(1, events.Event{AggregateType: "question", AggregateID: legacyID, Revision: 2, Type: "question.answered", Payload: answered, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	wantEvents := len(j.All())
	want := getWorkspace071(t, j)
	want2 := getWorkspace071(t, j)
	if !bytes.Equal(want, want2) {
		t.Fatal("projection not deterministic")
	}
	wantSnap, err := Snapshot(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = openTestJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if got := len(j.All()); got != wantEvents {
		t.Fatalf("events %d want %d", got, wantEvents)
	}
	if got := getWorkspace071(t, j); !bytes.Equal(got, want) {
		t.Fatalf("projection after reopen differs:\n got %s\nwant %s", got, want)
	}
	gotSnap, err := Snapshot(j)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(wantSnap)
	b, _ := json.Marshal(gotSnap)
	if !bytes.Equal(a, b) {
		t.Fatalf("Snapshot after reopen differs:\n got %s\nwant %s", b, a)
	}
	w := decode078(t, j)
	if g := gateByID075(t, w.Body.Gates, idc); g["state"] != "changes_requested" || g["decisionReason"] != "tighten" {
		t.Fatalf("changes_requested gate: %v", g)
	}
	if g := gateByID075(t, w.Body.Gates, ida); g["state"] != "approved" || g["decidedBy"] != "unverified-local-operator:bob" {
		t.Fatalf("approved-after-changes gate: %v", g)
	}
	if g := gateByID075(t, w.Body.Gates, legacyID); g["state"] != "rejected" || g["decisionReason"] != "old" {
		t.Fatalf("legacy gate: %v", g)
	}
	if caps := w.Body.GateCapabilities[idc]; caps["approve"] != "enabled" || caps["requestChanges"] != "hidden" {
		t.Fatalf("caps after reopen: %v", caps)
	}
}

// R7: a procedure.run step's pending question → requestChanges(reason) →
// GET /v1/context?task=<run> shows that step's gate with state
// changes_requested, decisionReason == reason, decidedBy == actor (the worker
// reads the change request from its step). Pending gates carry neither key.
func TestContextStepGateShowsChangeRequestFRRHZ109(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-078", procedure.Step{ID: "a", Action: "a"}, procedure.Step{ID: "b", Action: "b", After: []string{"a"}})
	srv := httptest.NewServer(NewHTTP(s).Handler())
	defer srv.Close()
	runProc(t, srv, "proc-078", "r7")
	for _, c := range [][2]string{{"q-a", "mission-r7-a"}, {"q-b", "mission-r7-b"}} {
		if out := postIntent057(t, srv, map[string]any{"kind": "question.ask", "name": c[0], "body": "body " + c[0], "recommendation": "approve", "missionId": c[1], "actor": "tester"}); out["Accepted"] != true {
			t.Fatalf("ask %s: %v", c[0], out)
		}
	}
	h := NewHTTP(s).Handler()
	code, body, _ := contextSteps(t, h, "mission-r7")
	if code != 200 || strings.Contains(string(body), "decisionReason") || strings.Contains(string(body), "decidedBy") {
		t.Fatalf("pending gates must not carry decision keys: %d %s", code, body)
	}
	qa := mustQuestionID("q-a", "body q-a", "approve")
	if out := postIntent057(t, srv, map[string]any{"kind": "gate.requestChanges", "gateId": qa, "digest": question.Digest("q-a", "body q-a", "approve"), "reason": "add tests first", "actor": "test-operator"}); out["Accepted"] != true {
		t.Fatalf("requestChanges: %v", out)
	}
	_, body, _ = contextSteps(t, h, "mission-r7")
	var bundle struct {
		Steps []struct {
			ID    string `json:"id"`
			Gates []struct {
				ID, State, DecisionReason, DecidedBy string
			} `json:"gates"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Steps) != 2 || bundle.Steps[0].ID != "mission-r7-a" || len(bundle.Steps[0].Gates) != 1 {
		t.Fatalf("steps: %s", body)
	}
	g := bundle.Steps[0].Gates[0]
	if g.ID != qa || g.State != "changes_requested" || g.DecisionReason != "add tests first" || g.DecidedBy != "unverified-local-operator:test-operator" {
		t.Fatalf("step a gate: %+v", g)
	}
	gb := bundle.Steps[1].Gates[0]
	if gb.State != "pending" || gb.DecisionReason != "" || gb.DecidedBy != "" {
		t.Fatalf("step b gate must stay pending without decision keys: %+v", gb)
	}
}
