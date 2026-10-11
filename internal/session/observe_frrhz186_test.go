package session

// RHZ-142 (FR-RHZ-186): `claude agents --json` observation reconciled with
// the registry through opened/closed only, zero writes on an unchanged
// observation, launcher -p sessions untouched, Adopt over an unlinked
// observation. The fake claude is written into t.TempDir.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/events"
)

func TestReconcileAgentsFRRHZ186(t *testing.T) {
	s := &events.Store{}
	svc := Service{Store: s}
	launched := Opened{SessionID: "L", Name: "m-1", MissionID: "m-1", Mode: Background, Cwd: "/tmp", ExecutionID: "exec-1"}
	if _, err := svc.Open(launched); err != nil {
		t.Fatal(err)
	}
	agents := []Agent{
		{ID: "a1", SessionID: "A", Name: "alpha", Kind: KindBackground, Status: "idle", Cwd: "/tmp"},
		{Name: "b", Kind: KindInteractive, Status: "busy", Cwd: "/tmp", Pid: 7}, // claude's real interactive shape: no ids
		{ID: "c1", SessionID: "C", Kind: KindBackground, Cwd: "rel"},
		{ID: "d1", SessionID: "D", Kind: "weird", Cwd: "/tmp"},
		{ID: "l1", SessionID: "L", Kind: KindBackground, Cwd: "/tmp"},
	}
	// RHZ-143: interactive entries are never journaled
	n, err := svc.Reconcile(agents)
	if err != nil || n != 1 || s.Len() != 2 {
		t.Fatalf("first reconcile wrote %d (%d events) %v", n, s.Len(), err)
	}
	a, _ := svc.Get("A")
	if a.State != Open || a.Mode != Interactive || a.Kind != KindBackground || a.AttachID != "a1" || a.Name != "alpha" || a.MissionID != "" {
		t.Fatalf("A = %+v", a)
	}
	for _, id := range []string{"", "C", "D"} {
		if len(s.List(AggregateType, id)) != 0 {
			t.Fatalf("%s registered", id)
		}
	}
	if l, _ := svc.Get("L"); l.Opened != launched || l.Revision != 1 {
		t.Fatalf("launcher session touched: %+v", l)
	}
	// unchanged observation: zero writes
	if n, err := svc.Reconcile(agents); err != nil || n != 0 || s.Len() != 2 {
		t.Fatalf("unchanged reconcile wrote %d %v", n, err)
	}
	// A loses its short id → closed and reopened
	n, err = svc.Reconcile([]Agent{{SessionID: "A", Kind: KindBackground, Cwd: "/tmp"}})
	if err != nil || n != 2 {
		t.Fatalf("change reconcile wrote %d %v", n, err)
	}
	if a, _ := svc.Get("A"); a.State != Open || a.AttachID != "" || a.Name != "alpha" || a.Revision != 3 {
		t.Fatalf("A = %+v", a)
	}
	if l, _ := svc.Get("L"); l.State != Open || l.Revision != 1 {
		t.Fatalf("launcher session closed by absence: %+v", l)
	}
	// A gone → closed; a gone session that returns is reopened
	if n, _ := svc.Reconcile(nil); n != 1 {
		t.Fatalf("gone reconcile wrote %d", n)
	}
	if a, _ := svc.Get("A"); a.State != Closed || a.Reason != ReasonGone {
		t.Fatalf("A = %+v", a)
	}
	if n, _ := svc.Reconcile([]Agent{{ID: "a1", SessionID: "A", Kind: KindBackground, Cwd: "/tmp"}}); n != 1 {
		t.Fatalf("return reconcile wrote %d", n)
	}
}

func TestAdoptFRRHZ186(t *testing.T) {
	s := &events.Store{}
	svc := Service{Store: s}
	if _, err := svc.Reconcile([]Agent{{ID: "a1", SessionID: "A", Kind: KindBackground, Cwd: "/tmp"}}); err != nil {
		t.Fatal(err)
	}
	p := Opened{SessionID: "A", Name: "m-1", MissionID: "m-1", Mode: Interactive, Cwd: "/tmp", ExecutionID: "exec-1", Kind: KindBackground, AttachID: "a1"}
	x, err := svc.Adopt(p)
	if err != nil || x.Opened != p || x.Revision != 3 {
		t.Fatalf("adopt = %+v %v", x, err)
	}
	if _, err := svc.Adopt(p); err != nil || s.Len() != 3 {
		t.Fatalf("identical adopt wrote: %d %v", s.Len(), err)
	}
	// the observation keeps the launcher's links
	if n, _ := svc.Reconcile([]Agent{{ID: "a1", SessionID: "A", Name: "other", Kind: KindBackground, Cwd: "/tmp"}}); n != 0 {
		t.Fatalf("reconcile over adopted wrote %d", n)
	}
	if _, err := svc.Open(Opened{SessionID: "X", Name: "x", Mode: Interactive, Cwd: "/tmp", Kind: "pty"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n[ \"$1 $2\" = 'agents --json' ] || exit 3\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListAgentsAndObserverFRRHZ186(t *testing.T) {
	ok := fakeClaude(t, `echo '[{"id":"a1","sessionId":"A","name":"n","kind":"background","state":"idle","cwd":"/tmp","pid":42,"startedAt":"2026-10-11T00:00:00Z"},{"id":"x","kind":"background","cwd":"/tmp"}]'`)
	got, err := ListAgents(context.Background(), ok, 0)
	if err != nil || len(got) != 1 || got[0].ID != "a1" || got[0].StatusText() != "idle" || !got[0].Attachable() {
		t.Fatalf("agents = %+v %v", got, err)
	}
	for name, path := range map[string]string{
		"exit 1":   fakeClaude(t, "exit 1"),
		"not json": fakeClaude(t, "echo nope"),
		"object":   fakeClaude(t, `echo '{"agents":[]}'`),
		"none":     "",
	} {
		if _, err := ListAgents(context.Background(), path, 0); !errors.Is(err, ErrAgents) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	s := &events.Store{}
	writes := 0
	o := &Observer{Store: s, ClaudePath: ok, OnWrite: func() { writes++ }}
	ctx := context.Background()
	if n, err := o.Tick(ctx); err != nil || n != 1 || writes != 1 || o.Status("A") != "idle" {
		t.Fatalf("tick %d %v writes=%d status=%q", n, err, writes, o.Status("A"))
	}
	if n, _ := o.Tick(ctx); n != 0 || writes != 1 || s.Len() != 1 {
		t.Fatalf("second tick wrote %d", n)
	}
	// a failed run writes nothing (the open session is not closed)
	o.ClaudePath = fakeClaude(t, "exit 1")
	if n, err := o.Tick(ctx); !errors.Is(err, ErrAgents) || n != 0 || s.Len() != 1 {
		t.Fatalf("failed tick %d %v", n, err)
	}
	var nilObs *Observer
	if nilObs.Status("A") != "" {
		t.Fatal("nil observer status")
	}
}
