//go:build darwin || linux

package terminal

// RHZ-143 (FR-RHZ-187): observed claude interactive sessions (no ids, keyed
// by pid in memory) are listed attachable:false with kind·status·cwd·name·
// pid; a launcher -p session, listed attachable:false, is 409 on attach by
// its full uuid too, with zero writes.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/session"
)

func TestInteractiveListAndAttachConsistencyFRRHZ187(t *testing.T) {
	store := &events.Store{}
	cwd := t.TempDir()
	if _, err := (session.Service{Store: store}).Open(session.Opened{SessionID: "uuid-p", Name: "p", MissionID: "m-1", Mode: session.Background, Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\necho \"$@\"\nread x\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h, srv := newHost(t, store, claude)
	h.Interactive = func() []session.Agent {
		return []session.Agent{{Name: "it", Kind: session.KindInteractive, Status: "busy", Cwd: cwd, Pid: 77}}
	}
	code, out := do(t, http.MethodGet, srv.URL+"/v1/terminals", "")
	var l listDTO
	if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil || len(l.Sessions) != 2 {
		t.Fatalf("list: %d %s", code, out)
	}
	if want := (sessionTargetDTO{SessionID: "uuid-p", Name: "p", MissionID: "m-1", Mode: session.Background, Cwd: cwd}); l.Sessions[0] != want {
		t.Fatalf("-p session %+v", l.Sessions[0])
	}
	if want := (sessionTargetDTO{Name: "it", Mode: session.Interactive, Cwd: cwd, Kind: session.KindInteractive, Status: "busy", Pid: 77}); l.Sessions[1] != want {
		t.Fatalf("interactive %+v", l.Sessions[1])
	}
	var raw struct {
		Sessions []map[string]any `json:"sessions"`
	}
	json.Unmarshal([]byte(out), &raw)
	if _, ok := raw.Sessions[1]["sessionId"]; ok || raw.Sessions[1]["attachable"] != false {
		t.Fatalf("interactive wire %v", raw.Sessions[1])
	}
	before := store.Len()
	if code, _ := do(t, http.MethodPost, srv.URL+"/v1/terminals", `{"target":{"session":{"sessionId":"uuid-p"}},"cols":80,"rows":24}`); code != http.StatusConflict || store.Len() != before || len(h.List()) != 0 {
		t.Fatalf("-p attach: %d", code)
	}
}
