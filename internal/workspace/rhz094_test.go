package workspace

// RHZ-094 FR-RHZ-121: /v1/execution/<id> must reach missions whose id contains
// "/" (sent percent-encoded) and accept a RHZ-073 handle in place of the id.
// Probe: route on r.URL.Path instead of EscapedPath → slash case FAIL.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

func TestExecutionRouteSlashIDAndHandleFRRHZ121(t *testing.T) {
	s := &events.Store{}
	id := "mission-x — a/b intent"
	missionIn062(t, s, id, domain.MissionReady, domain.MissionRunning)
	h := NewHTTP(s).Handler()
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	enc := url.PathEscape(id)
	if c := get("/v1/execution/" + enc); c != http.StatusOK {
		t.Fatalf("encoded slash id: %d", c)
	}
	// stream: same routing; cancel the request context so the SSE loop ends
	// after the snapshot frame.
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/execution/"+enc+"/stream", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: snapshot") {
		t.Fatalf("encoded slash id stream: %d %q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
	hi := buildHandleIndex(s.All())
	if c := get("/v1/execution/" + hi.of("m", id)); c != http.StatusOK {
		t.Fatalf("handle: %d", c)
	}
	if c := get("/v1/execution/" + url.PathEscape("mission-nope")); c != http.StatusNotFound {
		t.Fatalf("unknown: %d", c)
	}
}
