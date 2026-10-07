package coordinator

import (
	"encoding/json"
	"errors"
	"rhizome/internal/decision"
	"rhizome/internal/events"
	"rhizome/internal/wake"
	"sync"
	"testing"
	"time"
)

type fp struct {
	d   decision.Decision
	err error
	got Input
}

func TestHandleWakePersistsAndRejectsDuplicate(t *testing.T) {
	s := ms()
	if _, e := (wake.Service{Store: s}).Create(wake.Wake{ID: "w", Source: wake.Manual, RequestedAt: time.Now(), TargetType: "mission", TargetID: "m", CorrelationID: "corr", TickKey: "wk"}); e != nil {
		t.Fatal(e)
	}
	p := &fp{d: decision.Decision{ID: "d1", MissionID: "m", Kind: decision.StartExecution, Reason: "r"}}
	got, e := (&Coordinator{Store: s}).HandleWake("w", p)
	if e != nil || got.CorrelationID != "corr" || got.TickKey != "wk" {
		t.Fatalf("%+v %v", got, e)
	}
	if _, e = (&Coordinator{Store: s}).HandleWake("w", &fp{d: decision.Decision{ID: "d2", MissionID: "m", Kind: decision.StartExecution, Reason: "r"}}); e == nil {
		t.Fatal("duplicate")
	}
	ts, _ := (decision.Service{Store: s}).Timeline()
	if ts[0].CorrelationID != "corr" {
		t.Fatal("not persisted")
	}
}

func TestHandleWakeRejectsGoalNilAndPlannerError(t *testing.T) {
	s := ms()
	p, _ := json.Marshal(struct{ ID, Description, Success, PolicyRef string }{"g", "g", "s", ""})
	s.Append(0, events.Event{AggregateType: "goal", AggregateID: "g", Revision: 1, Type: "goal.created", Payload: p})
	if _, e := (wake.Service{Store: s}).Create(wake.Wake{ID: "wg", Source: wake.Manual, RequestedAt: time.Now(), TargetType: "goal", TargetID: "g", CorrelationID: "c", TickKey: "kg"}); e != nil {
		t.Fatal(e)
	}
	c := &Coordinator{Store: s}
	if _, e := c.HandleWake("wg", nil); e == nil {
		t.Fatal("nil accepted")
	}
	if _, e := c.HandleWake("wg", &fp{err: errors.New("planner")}); e == nil {
		t.Fatal("planner error accepted")
	}
	if len(s.All()) != 3 {
		t.Fatal("partial append")
	}
}

func (f *fp) Plan(i Input) (decision.Decision, error) { f.got = i; return f.d, f.err }
func ms() *events.Store {
	s := &events.Store{}
	p, _ := json.Marshal(struct{ ID, GoalID, Description, Success string }{"m", "g", "x", "y"})
	s.Append(0, events.Event{AggregateType: "mission", AggregateID: "m", Revision: 1, Type: "mission.created", Payload: p})
	return s
}
func TestTickHappyAndRestartDuplicate(t *testing.T) {
	s := ms()
	p := &fp{d: decision.Decision{ID: "d", MissionID: "m", Kind: decision.StartExecution, Reason: "r"}}
	c := &Coordinator{Store: s}
	got, e := c.Tick("k", "m", p)
	if e != nil || got.TickKey != "k" {
		t.Fatal(e)
	}
	if _, e = (&Coordinator{Store: s}).Tick("k", "m", p); e == nil {
		t.Fatal("duplicate")
	}
}
func TestTickRejectsInvalidInputs(t *testing.T) {
	s := ms()
	c := &Coordinator{Store: s}
	for _, k := range []string{"", "k"} {
		if _, e := c.Tick(k, "missing", nil); e == nil {
			t.Fatal("accepted")
		}
	}
	p := &fp{err: errors.New("planner")}
	if _, e := c.Tick("x", "m", p); e == nil {
		t.Fatal("planner")
	}
	if len(s.All()) != 1 {
		t.Fatal("append")
	}
}
func TestConcurrentOneSuccess(t *testing.T) {
	s := ms()
	p := &fp{d: decision.Decision{ID: "d", MissionID: "m", Kind: decision.StartExecution, Reason: "r"}}
	c := &Coordinator{Store: s}
	var w sync.WaitGroup
	ok := 0
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		w.Add(1)
		go func() {
			defer w.Done()
			if _, e := c.Tick("k", "m", p); e == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	w.Wait()
	if ok != 1 {
		t.Fatalf("successes=%d", ok)
	}
}
