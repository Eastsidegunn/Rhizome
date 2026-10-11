package sessionlauncher

// RHZ-139 (FR-RHZ-183): with RecordSessions a launched session is registered
// (session.opened, mode background, linked by executionId) and closed when it
// ends; without it the journal is unchanged. terminalDriver is optional and
// leaves the digest of a ledger without it unchanged.

import (
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/session"
)

func TestLauncherRecordsSessionsFRRHZ183(t *testing.T) {
	for _, record := range []bool{false, true} {
		f := newFake(t, emit(`{"type":"result","subtype":"success","is_error":false,"result":"ok","num_turns":1}`))
		s := launchStore(t)
		l := newLauncher(s, f.config(t, nil), &outcomes{})
		l.RecordSessions = record
		id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x"})
		if err != nil || reason != "" {
			t.Fatalf("%q %v", reason, err)
		}
		l.Wait()
		var ids []string
		for _, e := range s.All() {
			if e.AggregateType == session.AggregateType && (len(ids) == 0 || ids[len(ids)-1] != e.AggregateID) {
				ids = append(ids, e.AggregateID)
			}
		}
		if !record {
			if len(ids) != 0 {
				t.Fatalf("unrecorded launcher wrote sessions %v", ids)
			}
			continue
		}
		if len(ids) != 1 || ids[0] != ref(t, s, id).ExternalID {
			t.Fatalf("session streams %v, want the accepted session id", ids)
		}
		x, err := (session.Service{Store: s}).Get(ids[0])
		if err != nil || x.State != session.Closed || x.Reason != "exited" || x.Mode != session.Background || x.ExecutionID != id || x.MissionID != "m1" || x.Cwd != f.work {
			t.Fatalf("session = %+v %v", x, err)
		}
	}
}

func TestLedgerTerminalDriverFRRHZ183(t *testing.T) {
	f := newFake(t, "")
	base := f.config(t, nil)
	drv := filepath.Join(t.TempDir(), "driver")
	_ = os.WriteFile(drv, []byte("#!/bin/sh\n"), 0o700)
	c := f.config(t, func(m map[string]any) { m["terminalDriver"] = drv })
	if c.TerminalDriver != drv || c.Digest() == base.Digest() {
		t.Fatalf("terminalDriver not loaded or not in digest: %+v", c)
	}
	noexec := filepath.Join(t.TempDir(), "driver")
	_ = os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o600)
	for _, bad := range []string{"driver", noexec, drv + "-absent", t.TempDir()} {
		if _, err := Parse(f.ledger(func(m map[string]any) { m["terminalDriver"] = bad })); err == nil {
			t.Errorf("terminalDriver %q accepted", bad)
		}
	}
}
