package journal

import (
	"encoding/json"
	"path/filepath"
	"rhizome/internal/audit"
	"rhizome/internal/events"
	"rhizome/internal/projector"
	"testing"
)

func TestEndToEndReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j")
	j, e := openTestJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	g, _ := json.Marshal(struct{ ID, Description, Success, PolicyRef string }{"g", "goal", "done", ""})
	if e = j.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: g}); e != nil {
		t.Fatal(e)
	}
	m, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "work", "done"})
	j.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: m})
	d, _ := json.Marshal(struct {
		ID, MissionID, Reason string
		Kind                  string
		Evidence              []struct{ SourceType, SourceID string }
	}{"d", "m", "done", "complete", []struct{ SourceType, SourceID string }{{"mission", "m"}}})
	j.Append(0, events.Event{AggregateType: "decision", AggregateID: "d", Revision: 1, Type: "decision.created", Payload: d, CorrelationID: "c"})
	j.Append(1, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 2, Type: "mission.transitioned", Payload: []byte(`{"To":"ready"}`)})
	j.Append(2, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 3, Type: "mission.transitioned", Payload: []byte(`{"To":"running"}`)})
	j.Append(3, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 4, Type: "mission.transitioned", Payload: []byte(`{"To":"succeeded","DecisionID":"d"}`), CorrelationID: "c"})
	j.Close()
	j, e = openTestJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = projector.ReplayGoal(j.List("goal", "g")); e != nil {
		t.Fatal(e)
	}
	if _, e = projector.ReplayMission(j.List("mission", "m")); e != nil {
		t.Fatal(e)
	}
	if e = audit.TerminalLink(j, "d"); e != nil {
		t.Fatal(e)
	}
}
