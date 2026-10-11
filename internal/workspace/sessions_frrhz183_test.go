package workspace

// RHZ-139 (FR-RHZ-183): sessions[] on /v1/workspace, session_stale attention
// for an open session of a terminal mission, and the session.close intent.

import (
	"bytes"
	"encoding/json"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/session"
)

func TestSessionsProjectionAndStaleFRRHZ183(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-183", domain.MissionReady)
	if bytes.Contains(getWorkspace071(t, s), []byte(`"sessions"`)) {
		t.Fatal("sessions key present without any session")
	}
	reg := session.Service{Store: s}
	if _, err := reg.Open(session.Opened{SessionID: "s-1", Name: "tab", MissionID: "mission-183", Mode: session.Interactive, Cwd: "/tmp", DriverRef: "d-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open(session.Opened{SessionID: "s-2", Name: "bg", MissionID: "mission-183", Mode: session.Background, Cwd: "/tmp", ExecutionID: "exec-x"}); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Body struct {
			Sessions []map[string]any
		}
	}
	if err := json.Unmarshal(getWorkspace071(t, s), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Body.Sessions) != 2 || env.Body.Sessions[0]["sessionId"] != "s-1" || env.Body.Sessions[0]["mode"] != "interactive" || env.Body.Sessions[0]["state"] != "open" || env.Body.Sessions[0]["driverRef"] != "d-1" || env.Body.Sessions[1]["missionId"] != "mission-183" {
		t.Fatalf("sessions = %+v", env.Body.Sessions)
	}
	if ny, items := wire132(t, s); ny != 0 || len(ofKind132(items, "session_stale")) != 0 {
		t.Fatalf("live mission: needsYou=%d items=%+v", ny, items)
	}
	relay132(t, s, Intent{Kind: "mission.cancel", MissionID: "mission-183", Reason: "done"})
	if ny, items := wire132(t, s); ny != 2 || len(ofKind132(items, "session_stale")) != 2 {
		t.Fatalf("terminal mission: needsYou=%d items=%+v", ny, items)
	}
	relay132(t, s, Intent{Kind: "session.close", SessionID: "s-1"})
	if ny, items := wire132(t, s); ny != 1 || len(ofKind132(items, "session_stale")) != 1 {
		t.Fatalf("after close: needsYou=%d items=%+v", ny, items)
	}
	x, err := reg.Get("s-1")
	if err != nil || x.State != session.Closed || x.Reason != "closed by tester" {
		t.Fatalf("s-1 = %+v %v", x, err)
	}
	// A second close and an unknown session are rejected with zero writes.
	n := s.Len()
	for _, in := range []Intent{{Kind: "session.close", SessionID: "s-1"}, {Kind: "session.close", SessionID: "nope"}} {
		res, err := RelayIntent(s, in, "tester", noAuthority())
		if err != nil || res.Accepted {
			t.Fatalf("%s accepted: %+v %v", in.SessionID, res, err)
		}
	}
	if s.Len() != n {
		t.Fatalf("rejected close wrote %d events", s.Len()-n)
	}
}
