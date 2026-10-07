package mission

import (
	"errors"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

type FakeFailingPort struct {
	events.Port
	err error
}

func (f FakeFailingPort) Append(uint64, events.Event) error { return f.err }

func TestPortAppendError(t *testing.T) { // FR-RHZ-014
	store := &events.Store{}
	setup := Service{Store: store}
	if _, err := setup.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Create("m", "g", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("append failed")
	svc := Service{Store: FakeFailingPort{Port: store, err: sentinel}}
	calls := []func() error{
		func() error { _, err := svc.CreateGoal("g2", "goal", "done", ""); return err },
		func() error { _, err := svc.TransitionGoal("g", 1, domain.GoalPaused); return err },
		func() error { _, err := svc.Create("m2", "g", "mission", "done"); return err },
		func() error { _, err := svc.Transition("m", 1, domain.MissionReady); return err },
	}
	for i, call := range calls {
		if err := call(); !errors.Is(err, sentinel) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(store.All()) != 2 {
		t.Fatal("failed append changed store")
	}
}
func TestNilPort(t *testing.T) { // FR-RHZ-014
	var svc Service
	calls := []func() error{
		func() error { _, err := svc.CreateGoal("g", "goal", "done", ""); return err },
		func() error { _, err := svc.TransitionGoal("g", 1, domain.GoalPaused); return err },
		func() error { _, err := svc.Create("m", "g", "mission", "done"); return err },
		func() error { _, err := svc.Transition("m", 1, domain.MissionReady); return err },
	}
	for i, call := range calls {
		if err := call(); err == nil {
			t.Fatalf("call %d accepted nil", i)
		}
	}
}
