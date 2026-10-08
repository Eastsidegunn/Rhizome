package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/mission"
)

func requestWorkspaceFixture(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-request", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-request", "goal-request", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func createRequestIntent() Intent {
	return Intent{Kind: "request.create", Name: "Turn the key", MissionID: "mission-request", Why: "manual lock", Where: "rack 4", Commands: []string{}, After: "green light", Rollback: "turn it back", CorrelationID: "create-corr"}
}

func workspaceBody118(t *testing.T, s events.Port, target string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

// FR-RHZ-158: request-free GET and SSE bytes are the pre-RHZ-118 literals.
func TestWorkspaceGoldenLiteralWithoutRequestsFRRHZ158(t *testing.T) {
	s := &events.Store{}
	const getGolden = `{"revision":0,"body":{"missions":[],"tasks":[],"gates":[],"deliverables":[],"edges":[],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{},"gateCapabilities":{}}}` + "\n"
	if got := string(workspaceBody118(t, s, "/v1/workspace")); got != getGolden {
		t.Fatalf("GET bytes changed:\n%s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace/stream", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rr, req)
	const sseGolden = `event: snapshot
data: {"revision":0,"body":{"missions":[],"tasks":[],"gates":[],"deliverables":[],"edges":[],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{},"gateCapabilities":{}}}

`
	if rr.Body.String() != sseGolden {
		t.Fatalf("SSE bytes changed:\n%s", rr.Body.String())
	}
}

func TestRequestKeyOrderFRRHZ158(t *testing.T) {
	s := requestWorkspaceFixture(t)
	if res, err := RelayIntent(s, createRequestIntent(), "creator", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("create: %#v %v", res, err)
	}
	b := workspaceBody118(t, s, "/v1/workspace")
	var env struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	want := []string{"missions", "tasks", "gates", "deliverables", "edges", "counts", "attention", "requests", "requestCapabilities", "capabilities", "gateCapabilities"}
	if got := objectKeys070(t, env.Body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys=%v want=%v", got, want)
	}
	if !bytes.Contains(b, []byte(`"commands":[]`)) {
		t.Fatalf("zero commands not []: %s", b)
	}
	var decoded struct {
		Body struct {
			Counts struct {
				NeedsYou int `json:"needsYou"`
			} `json:"counts"`
			Requests []requestDTO `json:"requests"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Body.Counts.NeedsYou != 1 || len(decoded.Body.Requests) != 1 || !strings.HasPrefix(decoded.Body.Requests[0].Handle, "r-") {
		t.Fatalf("projection=%+v", decoded.Body)
	}
}

func TestRequestFilterPassThroughFRRHZ158(t *testing.T) {
	s := requestWorkspaceFixture(t)
	if res, _ := RelayIntent(s, createRequestIntent(), "creator", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	base := workspaceBody118(t, s, "/v1/workspace")
	for _, query := range []string{"?assignee=nobody", "?deliverableKind=report&deliverableLimit=1"} {
		filtered := workspaceBody118(t, s, "/v1/workspace"+query)
		var a, b struct {
			Body struct {
				Requests json.RawMessage `json:"requests"`
				Caps     json.RawMessage `json:"requestCapabilities"`
			} `json:"body"`
		}
		if json.Unmarshal(base, &a) != nil || json.Unmarshal(filtered, &b) != nil {
			t.Fatal("decode")
		}
		if !bytes.Equal(a.Body.Requests, b.Body.Requests) || !bytes.Equal(a.Body.Caps, b.Body.Caps) {
			t.Fatalf("request fields changed for %s", query)
		}
	}
	before := len(s.All())
	_ = workspaceBody118(t, s, "/v1/workspace")
	if len(s.All()) != before {
		t.Fatal("workspace read wrote events")
	}
}

func TestWorkspaceStreamRequestFrameFRRHZ158(t *testing.T) {
	s := requestWorkspaceFixture(t)
	h := NewHTTP(s)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/workspace/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := make(chan string, 2)
	go func() {
		buf := make([]byte, 4096)
		var pending string
		for {
			n, e := resp.Body.Read(buf)
			pending += string(buf[:n])
			for strings.Contains(pending, "\n\n") {
				parts := strings.SplitN(pending, "\n\n", 2)
				frames <- parts[0]
				pending = parts[1]
			}
			if e != nil {
				return
			}
		}
	}()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial SSE")
	}
	body, _ := json.Marshal(map[string]any{"kind": "request.create", "actor": "creator", "name": "Turn the key", "missionId": "mission-request", "why": "manual", "where": "rack", "commands": []string{}, "after": "verify"})
	r, err := http.Post(srv.URL+"/v1/intent", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	r.Body.Close()
	select {
	case frame := <-frames:
		if !strings.Contains(frame, "event: projection") || !strings.Contains(frame, `"requests"`) {
			t.Fatalf("frame=%s", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no request projection SSE")
	}
}

func TestRequestIntentExactKeysAndReplayDeterminismFRRHZ155(t *testing.T) {
	s := requestWorkspaceFixture(t)
	h := NewHTTP(s).Handler()
	bad := `{"kind":"request.create","actor":"creator","name":"x","missionId":"mission-request","why":"why","where":"where","commands":[],"after":"after","command":"typo"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(bad)))
	if !strings.Contains(rr.Body.String(), "unknown intent field") || len(s.All()) != 2 {
		t.Fatalf("response=%s writes=%d", rr.Body.String(), len(s.All()))
	}
	ordered := `{"kind":"request.create","actor":"creator","name":"","missionId":"mission-request","why":"why","where":"where","commands":[],"after":"after","command":"typo"}`
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/intent", strings.NewReader(ordered)))
	if !strings.Contains(rr.Body.String(), "request name required") || len(s.All()) != 2 {
		t.Fatalf("Q ordering response=%s writes=%d", rr.Body.String(), len(s.All()))
	}
	if res, _ := RelayIntent(s, createRequestIntent(), "creator", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	p1, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := &events.Store{}
	for _, e := range s.All() {
		if err := reloaded.Append(e.Revision-1, e); err != nil {
			t.Fatal(err)
		}
	}
	p2, err := Snapshot(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Requests[0].ID != p2.Requests[0].ID || toDTO(p1).Requests[0].Handle != toDTO(p2).Requests[0].Handle {
		t.Fatal("id/handle changed on replay")
	}
}

func TestRequestCloseIntentRelayFRRHZ156(t *testing.T) {
	s := requestWorkspaceFixture(t)
	if res, _ := RelayIntent(s, createRequestIntent(), "creator", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Requests[0]
	handle := toDTO(p).Requests[0].Handle
	before := len(s.All())
	if res, _ := RelayIntent(s, Intent{Kind: "request.unable", RequestID: handle}, "closer", noAuthority()); res.Accepted || res.Reason != "reason required" || len(s.All()) != before {
		t.Fatalf("unable=%#v writes=%d", res, len(s.All())-before)
	}
	if res, _ := RelayIntent(s, Intent{Kind: "request.complete", RequestID: handle, Memo: "finished"}, "closer", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	log := s.List("request", r.ID)
	if len(log) != 2 || log[1].CorrelationID != "relay:unverified-local-operator:closer" {
		t.Fatalf("close log=%#v", log)
	}
	p, err = Snapshot(s)
	if err != nil || p.Requests[0].State != "done" || p.RequestCapabilities[r.ID] != (RequestCapabilities{Complete: capHidden, Unable: capHidden}) {
		t.Fatalf("closed projection=%#v err=%v", p.Requests, err)
	}

	in := createRequestIntent()
	in.Name = "Turn the spare key"
	if res, _ := RelayIntent(s, in, "creator", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	p, _ = Snapshot(s)
	var open Request
	for _, candidate := range p.Requests {
		if candidate.State == "waiting" {
			open = candidate
		}
	}
	if res, _ := RelayIntent(s, Intent{Kind: "request.cancel", RequestID: open.ID, Reason: "ops withdrew it", CorrelationID: "cancel-corr"}, "unrelated-operator", noAuthority()); !res.Accepted {
		t.Fatal(res.Reason)
	}
	cancelLog := s.List("request", open.ID)
	if len(cancelLog) != 2 || cancelLog[1].CorrelationID != "cancel-corr" {
		t.Fatalf("cancel log=%#v", cancelLog)
	}
}
