package workspace

// RHZ-042 전송 계층 테스트 (FR-RHZ-072).

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
	"rhizome/internal/projector"
	"rhizome/internal/surface"
)

func httpFixture(t *testing.T) (*events.Store, *httptest.Server) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, e := ms.CreateGoal("g", "목표", "done", "p"); e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Create("m", "g", "미션", "done"); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func richHTTPFixture(t *testing.T) (*events.Store, *httptest.Server) {
	t.Helper()
	s, srv := httpFixture(t)
	ms := mission.Service{Store: s}
	if _, e := ms.Create("m2", "g", "블록미션", "done"); e != nil {
		t.Fatal(e)
	}
	m2, e := projector.ReplayMission(s.List("mission", "m2"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e := ms.Transition("m2", m2.Revision, domain.MissionReady); e != nil {
		t.Fatal(e)
	}
	m2, _ = projector.ReplayMission(s.List("mission", "m2"))
	if _, e := ms.TransitionWithReason("m2", m2.Revision, domain.MissionBlocked, "대기 사유"); e != nil {
		t.Fatal(e)
	}
	p := 0.5
	if _, e := (surface.Service{Store: s}).ReportProgress("m", "작업 중", &p, "src"); e != nil {
		t.Fatal(e)
	}
	return s, srv
}

func TestHTTPWorkspaceSchemaFRRHZ072(t *testing.T) {
	_, srv := richHTTPFixture(t)
	resp, e := http.Get(srv.URL + "/v1/workspace")
	if e != nil || resp.StatusCode != 200 {
		t.Fatal(resp.Status, e)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatal(ct)
	}
	var env map[string]json.RawMessage
	if e := json.NewDecoder(resp.Body).Decode(&env); e != nil {
		t.Fatal(e)
	}
	if _, ok := env["revision"]; !ok {
		t.Fatal("no revision")
	}
	var body map[string]json.RawMessage
	if e := json.Unmarshal(env["body"], &body); e != nil {
		t.Fatal(e)
	}
	for _, k := range []string{"missions", "tasks", "gates", "deliverables", "edges", "counts", "attention"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("body missing %q: %v", k, body)
		}
	}
	var counts map[string]int
	if e := json.Unmarshal(body["counts"], &counts); e != nil {
		t.Fatal(e)
	}
	for _, k := range []string{"running", "needsYou", "blocked"} {
		if _, ok := counts[k]; !ok {
			t.Fatalf("counts missing %q", k)
		}
	}
	var tasks []map[string]json.RawMessage
	if e := json.Unmarshal(body["tasks"], &tasks); e != nil || len(tasks) != 2 {
		t.Fatal(tasks, e)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, tk := range tasks {
		var id string
		_ = json.Unmarshal(tk["id"], &id)
		byID[id] = tk
	}
	for _, k := range []string{"missionId", "hasProgress", "state", "name"} {
		if _, ok := byID["m"][k]; !ok {
			t.Fatalf("task missing camelCase %q: %v", k, byID["m"])
		}
	}
	// 값이 있는 fixture에서만 존재해야 하는 필드 (미제공=omit은 승인 스키마)
	for _, k := range []string{"currentAction", "progress"} {
		if _, ok := byID["m"][k]; !ok {
			t.Fatalf("task m missing %q despite provided value: %v", k, byID["m"])
		}
	}
	if _, ok := byID["m2"]["blockedReason"]; !ok {
		t.Fatalf("blocked task missing blockedReason: %v", byID["m2"])
	}
	if _, ok := byID["m2"]["currentAction"]; ok {
		t.Fatalf("absent currentAction not omitted: %v", byID["m2"])
	}
}

type sseReader struct {
	events chan string
}

func openSSE(t *testing.T, url string) *sseReader {
	t.Helper()
	resp, e := http.Get(url + "/v1/workspace/stream")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { resp.Body.Close() })
	r := &sseReader{events: make(chan string, 8)}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				r.events <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()
	return r
}

func (r *sseReader) next(t *testing.T, within time.Duration) (string, bool) {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev, true
	case <-time.After(within):
		return "", false
	}
}

func postIntent(t *testing.T, url string, body string) map[string]any {
	t.Helper()
	resp, e := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
	if e != nil || resp.StatusCode != 200 {
		t.Fatal(resp, e)
	}
	var out map[string]any
	if e := json.NewDecoder(resp.Body).Decode(&out); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestHTTPSSESnapshotThenAcceptedPushFRRHZ072(t *testing.T) {
	_, srv := httpFixture(t)
	sse := openSSE(t, srv.URL)
	if ev, ok := sse.next(t, 2*time.Second); !ok || ev != "snapshot" {
		t.Fatal(ev, ok)
	}
	res := postIntent(t, srv.URL, `{"kind":"mission.create","name":"n1","prompt":"p1","actor":"op"}`)
	if res["Accepted"] != true && res["accepted"] != true {
		t.Fatalf("intent rejected: %v", res)
	}
	if ev, ok := sse.next(t, 2*time.Second); !ok || ev != "projection" {
		t.Fatalf("no projection push: %q %v", ev, ok)
	}
}

func TestHTTPSSERejectedNoPushFRRHZ072(t *testing.T) {
	_, srv := httpFixture(t)
	sse := openSSE(t, srv.URL)
	if ev, ok := sse.next(t, 2*time.Second); !ok || ev != "snapshot" {
		t.Fatal(ev, ok)
	}
	res := postIntent(t, srv.URL, `{"kind":"mission.create","name":"","actor":"op"}`)
	if res["Accepted"] == true || res["accepted"] == true {
		t.Fatal("empty name accepted")
	}
	if ev, ok := sse.next(t, 700*time.Millisecond); ok {
		t.Fatalf("rejected intent pushed %q", ev)
	}
}

func TestHTTPIntentReasonPreservedAndAppendThroughFRRHZ072(t *testing.T) {
	s, srv := httpFixture(t)
	before := len(s.All())
	res := postIntent(t, srv.URL, `{"kind":"task.pause","taskId":"m","actor":"op"}`)
	reason, _ := res["Reason"].(string)
	if r2, ok := res["reason"].(string); ok && reason == "" {
		reason = r2
	}
	if reason == "" || len(s.All()) != before {
		t.Fatalf("rejection: %v log=%d->%d", res, before, len(s.All()))
	}
	res = postIntent(t, srv.URL, `{"kind":"task.instruct","taskId":"m","instruction":"do","actor":"alice","verified":true}`)
	if res["Accepted"] != true && res["accepted"] != true {
		t.Fatalf("instruct rejected: %v", res)
	}
	if len(s.All()) != before+1 {
		t.Fatal("accepted intent did not append")
	}
	found := false
	for _, e := range s.All() {
		if e.AggregateType == "surface" && strings.Contains(string(e.Payload), "unverified-local-operator:alice") {
			found = true
		}
	}
	if !found {
		t.Fatal("verified=true body did not force unverified marking")
	}
}

func TestHTTPNotImplementedAndUnknownRoutesFRRHZ072(t *testing.T) {
	_, srv := httpFixture(t)
	// RHZ-046 2부(D15): /v1/execution/*는 계약 표면 v1으로 구현되어
	// 501 목록에서 제거 — 미지 taskId의 404는 FRRHZ077 K7/M4가 커버한다.
	for _, p := range []string{"/v1/terminal/y/stream"} {
		resp, e := http.Get(srv.URL + p)
		if e != nil || resp.StatusCode != http.StatusNotImplemented {
			t.Fatal(p, resp.Status, e)
		}
		var body map[string]string
		if json.NewDecoder(resp.Body).Decode(&body) != nil || !strings.Contains(body["reason"], "JANUS") {
			t.Fatal(body)
		}
	}
	if resp, _ := http.Get(srv.URL + "/nope"); resp.StatusCode != 404 {
		t.Fatal(resp.Status)
	}
	if resp, _ := http.Post(srv.URL+"/v1/workspace", "application/json", strings.NewReader("{}")); resp.StatusCode != 404 {
		t.Fatal("POST workspace not rejected:", resp.Status)
	}
}
