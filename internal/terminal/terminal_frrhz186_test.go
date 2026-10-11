//go:build darwin || linux

package terminal

// RHZ-142 (FR-RHZ-186): the list exposes attachable·kind·status; an
// observed background session attaches by its short id; a claude
// interactive session (another terminal) is 409 with zero writes.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/session"
)

func TestObservedSessionTargetFRRHZ186(t *testing.T) {
	store := &events.Store{}
	cwd := t.TempDir()
	svc := session.Service{Store: store}
	if _, err := svc.Reconcile([]session.Agent{
		{ID: "ab12", SessionID: "uuid-bg", Name: "bg", Kind: session.KindBackground, Status: "idle", Cwd: cwd},
		{ID: "cd34", SessionID: "uuid-it", Name: "it", Kind: session.KindInteractive, Status: "busy", Cwd: cwd},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open(session.Opened{SessionID: "uuid-p", Name: "p", Mode: session.Background, Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\necho \"$@\"\nread x\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h, srv := newHost(t, store, claude)
	h.Status = statusMap{"uuid-bg": "idle", "uuid-it": "busy"}.get
	code, out := do(t, http.MethodGet, srv.URL+"/v1/terminals", "")
	var l listDTO
	if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil || len(l.Sessions) != 3 {
		t.Fatalf("list: %d %s", code, out)
	}
	want := map[string]sessionTargetDTO{
		"uuid-bg": {SessionID: "uuid-bg", Name: "bg", Mode: session.Interactive, Cwd: cwd, Attachable: true, Kind: session.KindBackground, Status: "idle"},
		"uuid-it": {SessionID: "uuid-it", Name: "it", Mode: session.Interactive, Cwd: cwd, Kind: session.KindInteractive, Status: "busy"},
		"uuid-p":  {SessionID: "uuid-p", Name: "p", Mode: session.Background, Cwd: cwd},
	}
	for _, s := range l.Sessions {
		if s != want[s.SessionID] {
			t.Fatalf("session %+v, want %+v", s, want[s.SessionID])
		}
	}
	before := store.Len()
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"uuid-it"}},"cols":80,"rows":24}`); code != http.StatusConflict || store.Len() != before {
		t.Fatalf("interactive attach: %d", code)
	}
	code, out = do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"uuid-bg"}},"cols":80,"rows":24}`)
	if code != http.StatusCreated {
		t.Fatalf("open: %d %s", code, out)
	}
	var r struct{ TerminalID string }
	json.Unmarshal([]byte(out), &r)
	s := subscribe(t, srv, r.TerminalID)
	s.waitFor(t, "attach ab12")
	if code, _ := do(t, http.MethodDelete, srv.URL+"/v1/terminals/"+r.TerminalID, ""); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	s.waitExit(t)
}

type statusMap map[string]string

func (m statusMap) get(id string) string { return m[id] }
