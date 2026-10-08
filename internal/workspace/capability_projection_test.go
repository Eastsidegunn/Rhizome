package workspace

// RHZ-070 FR-RHZ-099: /v1/workspace에 capabilities · gateCapabilities 투영.
// 순수 읽기 투영(신규 이벤트 0). G1 gate, T1 task 상태표(relay 일치 고정),
// D1 결정론, A1 additive, W1 쓰기 0.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/question"
)

func getWorkspace070(t *testing.T, s *events.Store) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace", nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type wire070 struct {
	Body struct {
		Gates            []map[string]any             `json:"gates"`
		Capabilities     map[string]map[string]string `json:"capabilities"`
		GateCapabilities map[string]map[string]string `json:"gateCapabilities"`
	} `json:"body"`
}

func decode070(t *testing.T, b []byte) wire070 {
	t.Helper()
	var w wire070
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return w
}

func ask070(t *testing.T, s *events.Store, name, missionID string) string {
	t.Helper()
	if missionID == "" {
		// RHZ-071 (FR-RHZ-100): the relay now requires missionId, so a
		// mission-less gate (legacy journal shape) is seeded through the
		// kernel directly — the capability assertions stay as they were.
		if _, err := (question.Service{Store: s}).Ask(name, "body "+name, "approve", "", "", "tester", ""); err != nil {
			t.Fatalf("ask %s: %v", name, err)
		}
	} else {
		res, err := RelayIntent(s, Intent{Kind: "question.ask", Name: name, Body: "body " + name, Recommendation: "approve", MissionID: missionID}, "tester", noAuthority())
		if err != nil || !res.Accepted {
			t.Fatalf("ask %s: %v %+v", name, err, res)
		}
	}
	w := decode070(t, getWorkspace070(t, s))
	for _, g := range w.Body.Gates {
		if g["name"] == name {
			return g["id"].(string)
		}
	}
	t.Fatalf("gate %s not projected", name)
	return ""
}

// G1: pending question → approve/reject enabled; requestChanges enabled iff
// mission/goal-bound (RHZ-078, FR-RHZ-109 spec update: the relay now has a
// question branch for gate.requestChanges, but an unbound question has no
// worker to send changes to, so the relay rejects it and hidden stays
// honest; the same test pins that rejection). approve 후 → 셋 다 hidden.
func TestGateCapabilitiesPendingAndDecidedFRRHZ099(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-g1", domain.MissionReady, domain.MissionRunning)
	withMission := ask070(t, s, "G1 bound", "mission-g1")
	noMission := ask070(t, s, "G1 free", "")

	w := decode070(t, getWorkspace070(t, s))
	want := map[string]map[string]string{
		withMission: {"approve": "enabled", "reject": "enabled", "requestChanges": "enabled"},
		noMission:   {"approve": "enabled", "reject": "enabled", "requestChanges": "hidden"},
	}
	for id, exp := range want {
		got, ok := w.Body.GateCapabilities[id]
		if !ok {
			t.Fatalf("gateCapabilities missing %s: %v", id, w.Body.GateCapabilities)
		}
		for k, v := range exp {
			if got[k] != v {
				t.Errorf("%s.%s = %q want %q", id, k, got[k], v)
			}
		}
	}
	if len(w.Body.GateCapabilities) != 2 {
		t.Fatalf("gateCapabilities has %d entries, want 2 (internal gates only)", len(w.Body.GateCapabilities))
	}

	// requestChanges on an unbound internal gate is rejected by the relay — hidden is honest.
	if res, err := RelayIntent(s, Intent{Kind: "gate.requestChanges", GateID: noMission, Instruction: "please fix"}, "tester", noAuthority()); err != nil || res.Accepted {
		t.Fatalf("gate.requestChanges on an unbound question must be rejected: %v %+v", err, res)
	}
	// approve the bound one with the digest the human saw
	var digest string
	for _, g := range w.Body.Gates {
		if g["id"] == withMission {
			digest, _ = g["requestDigest"].(string)
		}
	}
	res, err := RelayIntent(s, Intent{Kind: "gate.approve", GateID: withMission, Digest: digest}, "tester", noAuthority())
	if err != nil || !res.Accepted {
		t.Fatalf("approve: %v %+v", err, res)
	}
	w = decode070(t, getWorkspace070(t, s))
	got := w.Body.GateCapabilities[withMission]
	for _, k := range []string{"approve", "reject", "requestChanges"} {
		if got[k] != "hidden" {
			t.Errorf("after approve %s = %q want hidden", k, got[k])
		}
	}
	// the untouched one is still pending
	if w.Body.GateCapabilities[noMission]["approve"] != "enabled" {
		t.Errorf("unrelated gate changed: %v", w.Body.GateCapabilities[noMission])
	}
}

// T1: 상태별 기대 level + 같은 테스트에서 실제 relay(task.pause/task.resume)
// 호출 — enabled ⇔ Accepted (capability는 relay보다 넓게 말하지 않는다).
func TestTaskCapabilitiesMatchRelayFRRHZ099(t *testing.T) {
	type tc struct {
		path                    []domain.MissionState
		terminal                domain.MissionState
		pause, resume, instruct string
	}
	cases := map[string]tc{
		"planned":            {nil, "", "disabled", "enabled", "enabled"},
		"ready":              {[]domain.MissionState{domain.MissionReady}, "", "disabled", "enabled", "enabled"},
		"running":            {[]domain.MissionState{domain.MissionReady, domain.MissionRunning}, "", "enabled", "disabled", "enabled"},
		"paused":             {[]domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionPaused}, "", "disabled", "enabled", "enabled"},
		"waiting_for_human":  {[]domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingHuman}, "", "disabled", "disabled", "enabled"},
		"waiting_for_result": {[]domain.MissionState{domain.MissionReady, domain.MissionRunning, domain.MissionWaitingResult}, "", "disabled", "disabled", "enabled"},
		"blocked":            {[]domain.MissionState{domain.MissionReady, domain.MissionBlocked}, "", "disabled", "disabled", "enabled"},
		"succeeded":          {nil, domain.MissionSucceeded, "hidden", "hidden", "hidden"},
		"failed":             {nil, domain.MissionFailed, "hidden", "hidden", "hidden"},
		"cancelled":          {nil, domain.MissionCancelled, "hidden", "hidden", "hidden"},
	}
	build := func(t *testing.T, state string, c tc) (*events.Store, string) {
		id := "mission-" + state
		s := &events.Store{}
		switch c.terminal {
		case domain.MissionCancelled:
			missionIn062(t, s, id)
			if res := cancelMission062(t, s, id); !res.Accepted {
				t.Fatal(res)
			}
		case domain.MissionSucceeded, domain.MissionFailed:
			missionIn062(t, s, id)
			drive062Terminal(t, s, id, c.terminal)
		default:
			missionIn062(t, s, id, c.path...)
		}
		if got := missionState062(t, s, id); string(got) != state {
			t.Fatalf("fixture %s landed in %s", state, got)
		}
		return s, id
	}
	for state, c := range cases {
		s, id := build(t, state, c)
		w := decode070(t, getWorkspace070(t, s))
		got, ok := w.Body.Capabilities[id]
		if !ok {
			t.Fatalf("%s: capabilities missing %s: %v", state, id, w.Body.Capabilities)
		}
		if got["pause"] != c.pause || got["resume"] != c.resume || got["instruct"] != c.instruct {
			t.Errorf("%s: got %v want pause=%s resume=%s instruct=%s", state, got, c.pause, c.resume, c.instruct)
		}
		if _, extra := got["cancel"]; extra || len(got) != 3 {
			t.Errorf("%s: unexpected keys %v (only pause/resume/instruct are relayed)", state, got)
		}
		// relay agreement: fresh store per intent so one does not affect the other
		for _, kind := range []string{"task.pause", "task.resume"} {
			fs, fid := build(t, state, c)
			res, err := RelayIntent(fs, Intent{Kind: kind, TaskID: fid}, "tester", noAuthority())
			if err != nil {
				t.Fatal(err)
			}
			level := got["pause"]
			if kind == "task.resume" {
				level = got["resume"]
			}
			if res.Accepted != (level == "enabled") {
				t.Errorf("%s: %s capability=%q but relay Accepted=%v (%s)", state, kind, level, res.Accepted, res.Reason)
			}
		}
	}
}

// D1: 같은 저널 2회 조회 → 바이트 동일; 두 맵의 키는 ID 사전순(삽입 역순 fixture).
func TestCapabilitiesDeterministicSortedFRRHZ099(t *testing.T) {
	s := &events.Store{}
	for _, id := range []string{"mission-z", "mission-a", "mission-m"} {
		missionIn062(t, s, id)
	}
	ask070(t, s, "D1 zz", "mission-z")
	ask070(t, s, "D1 aa", "mission-a")

	b1 := getWorkspace070(t, s)
	b2 := getWorkspace070(t, s)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("not byte-identical:\n%s\n%s", b1, b2)
	}
	var body struct {
		Body struct {
			Capabilities     json.RawMessage `json:"capabilities"`
			GateCapabilities json.RawMessage `json:"gateCapabilities"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b1, &body); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{"capabilities": body.Body.Capabilities, "gateCapabilities": body.Body.GateCapabilities} {
		keys := objectKeys070(t, raw)
		if len(keys) < 2 {
			t.Fatalf("%s: want ≥2 keys, got %v", name, keys)
		}
		for i := 1; i < len(keys); i++ {
			if keys[i-1] >= keys[i] {
				t.Fatalf("%s keys not sorted: %v", name, keys)
			}
		}
	}
}

// objectKeys070 returns an object's keys in wire order (Decoder token walk).
func objectKeys070(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s (%v)", raw, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// A1: additive — 빈 저널 골든 바이트(기존 7키 + capabilities + gateCapabilities
// 가 마지막 두 키, 빈 경우 {}), 그리고 fixture 저널에서 top-level 키 집합 정확.
func TestCapabilitiesAdditiveFRRHZ099(t *testing.T) {
	empty := getWorkspace070(t, &events.Store{})
	const golden = `{"revision":0,"body":{"missions":[],"tasks":[],"gates":[],"deliverables":[],"edges":[],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{},"gateCapabilities":{}}}` + "\n"
	if string(empty) != golden {
		t.Fatalf("empty golden mismatch:\n got %s\nwant %s", empty, golden)
	}

	s := &events.Store{}
	missionIn062(t, s, "mission-a1", domain.MissionReady, domain.MissionRunning)
	ask070(t, s, "A1 q", "mission-a1")
	b := getWorkspace070(t, s)
	var env struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	keys := objectKeys070(t, env.Body)
	want := []string{"missions", "tasks", "gates", "deliverables", "edges", "counts", "attention", "capabilities", "gateCapabilities"}
	if len(keys) != len(want) {
		t.Fatalf("keys %v want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("key[%d]=%s want %s (full %v)", i, keys[i], want[i], keys)
		}
	}
	w := decode070(t, b)
	if len(w.Body.Capabilities) != 1 || len(w.Body.GateCapabilities) != 1 {
		t.Fatalf("maps: %v %v", w.Body.Capabilities, w.Body.GateCapabilities)
	}
}

// W1: 조회는 쓰지 않는다 — 전후 저널 길이 동일.
func TestCapabilitiesNoWriteFRRHZ099(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-w1", domain.MissionReady, domain.MissionRunning)
	ask070(t, s, "W1 q", "mission-w1")
	before := len(s.All())
	for i := 0; i < 3; i++ {
		getWorkspace070(t, s)
	}
	if after := len(s.All()); after != before {
		t.Fatalf("journal grew %d → %d on read", before, after)
	}
}
