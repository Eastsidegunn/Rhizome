package deliverable

// RHZ-081 FR-RHZ-112 (K1): goal-bound deliverable — Create/Replay/Get bind
// to a goal, exactly one of MissionID/GoalID is enforced on write and on
// replay, a legacy payload without a GoalID key replays unchanged, and a
// succeeded mission still accepts a deliverable (post-hoc record).

import (
	"bytes"
	"encoding/json"
	"rhizome/internal/events"
	"rhizome/internal/source"
	"testing"
)

// goalStore is missionStore plus the goal "g" the mission already names.
func goalStore() *events.Store {
	s := missionStore()
	p, _ := json.Marshal(struct{ ID, Description, Success, PolicyRef string }{"g", "d", "s", ""})
	_ = s.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: p})
	return s
}

func TestDeliverableGoalBoundReplayFRRHZ112(t *testing.T) {
	s := goalStore()
	src, err := source.Service{Store: s}.Register([]byte("body"), "text/plain", "note://x")
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{Store: s}
	before := len(s.All())
	// both / neither → rejected before append.
	if _, e := svc.Create(Deliverable{ID: "both", Kind: "file", MissionID: "m", GoalID: "g", SourceRef: src.BlobID, Summary: "x"}); e == nil {
		t.Fatal("both mission and goal accepted")
	}
	if _, e := svc.Create(Deliverable{ID: "none", Kind: "file", SourceRef: src.BlobID, Summary: "x"}); e == nil {
		t.Fatal("neither mission nor goal accepted")
	}
	if _, e := svc.Create(Deliverable{ID: "nogoal", Kind: "file", GoalID: "missing", SourceRef: src.BlobID, Summary: "x"}); e == nil {
		t.Fatal("unknown goal accepted")
	}
	// goal-bound + exec- source: no mission to belong to → rejected.
	if _, e := svc.Create(Deliverable{ID: "gx", Kind: "file", GoalID: "g", SourceRef: "exec-1", Summary: "x"}); e == nil {
		t.Fatal("goal-bound exec source accepted")
	}
	if len(s.All()) != before {
		t.Fatal("rejection wrote")
	}
	d, e := svc.Create(Deliverable{ID: "dg", Kind: "file", GoalID: "g", SourceRef: src.BlobID, Summary: "goal-bound"})
	if e != nil {
		t.Fatal(e)
	}
	if d.GoalID != "g" || d.MissionID != "" || d.Revision != 1 {
		t.Fatalf("create %+v", d)
	}
	log := s.List("deliverable", "dg")
	if !bytes.Contains(log[0].Payload, []byte(`"GoalID":"g"`)) {
		t.Fatalf("payload must carry GoalID: %s", log[0].Payload)
	}
	if r, e := Replay(log); e != nil || r != d {
		t.Fatalf("replay %+v err=%v", r, e)
	}
	if g, e := svc.Get("dg"); e != nil || g != d {
		t.Fatalf("get %+v err=%v", g, e)
	}
	// Get re-verifies the goal: a goal-less stream for the same payload fails.
	orphan := &events.Store{}
	_ = orphan.Append(0, log[0])
	if _, e := (Service{Store: orphan}).Get("dg"); e == nil {
		t.Fatal("get without the goal stream accepted")
	}
	// Replay rejects a tampered payload naming both or neither.
	var p payload
	_ = json.Unmarshal(log[0].Payload, &p)
	p.MissionID = "m"
	tampered := log[0]
	tampered.Payload, _ = json.Marshal(p)
	if _, e := Replay([]events.Event{tampered}); e == nil {
		t.Fatal("both on replay accepted")
	}
	p.MissionID, p.GoalID = "", ""
	tampered.Payload, _ = json.Marshal(p)
	if _, e := Replay([]events.Event{tampered}); e == nil {
		t.Fatal("neither on replay accepted")
	}
	// Mission-bound payloads omit GoalID on the wire (pre-081 bytes).
	dm, e := svc.Create(Deliverable{ID: "dm", Kind: "file", MissionID: "m", SourceRef: src.BlobID, Summary: "mission-bound"})
	if e != nil {
		t.Fatal(e)
	}
	if ml := s.List("deliverable", "dm"); bytes.Contains(ml[0].Payload, []byte("GoalID")) {
		t.Fatalf("mission-bound payload must omit GoalID: %s", ml[0].Payload)
	}
	if got, e := svc.ByMission("m"); e != nil || len(got) != 1 || got[0] != dm {
		t.Fatalf("by mission %v err=%v", got, e)
	}
}

// Legacy: a deliverable.declared payload exactly as pre-081 code wrote it
// (five keys, no GoalID) replays and Gets unchanged.
func TestDeliverableLegacyPayloadReplayFRRHZ112(t *testing.T) {
	s := missionStore()
	src, err := source.Service{Store: s}.Register([]byte("old"), "text/plain", "note://old")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"ID":"legacy","Kind":"record","MissionID":"m","SourceRef":"` + src.BlobID + `","Summary":"old"}`)
	ev := events.Event{AggregateType: "deliverable", AggregateID: "legacy", Revision: 1, Type: "deliverable.declared", Payload: raw}
	want := Deliverable{ID: "legacy", Kind: "record", MissionID: "m", SourceRef: src.BlobID, Summary: "old", Revision: 1}
	if d, e := Replay([]events.Event{ev}); e != nil || d != want {
		t.Fatalf("replay %+v err=%v", d, e)
	}
	if err := s.Append(0, ev); err != nil {
		t.Fatal(err)
	}
	if d, e := (Service{Store: s}).Get("legacy"); e != nil || d != want {
		t.Fatalf("get %+v err=%v", d, e)
	}
}

// A succeeded mission still accepts a deliverable (the coordinator declares
// after Complete): unchanged by RHZ-081.
func TestDeliverableSucceededMissionStillAcceptedFRRHZ112(t *testing.T) {
	s := missionStore()
	_ = s.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"ready"}`)})
	_ = s.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"running"}`)})
	_ = s.Append(3, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 4, Type: "mission.transitioned", Payload: json.RawMessage(`{"To":"succeeded","DecisionID":"d"}`)})
	src, _ := source.Service{Store: s}.Register([]byte("x"), "text/plain", "u")
	if _, e := (Service{Store: s}).Create(Deliverable{ID: "post", Kind: "f", MissionID: "m", SourceRef: src.BlobID, Summary: "sum"}); e != nil {
		t.Fatal(e)
	}
}
