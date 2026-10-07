package memory

import (
	"errors"
	"testing"

	"rhizome/internal/events"
)

type FakeFailingPort struct {
	events.Port
	err error
}

func (f FakeFailingPort) Append(uint64, events.Event) error { return f.err }

func TestPortAppendError(t *testing.T) { // FR-RHZ-014
	store := &events.Store{}
	original := create(t, Service{Store: store}, sample("original"))
	sentinel := errors.New("append failed")
	svc := Service{Store: FakeFailingPort{Port: store, err: sentinel}}
	if _, err := svc.Create(sample("next")); !errors.Is(err, sentinel) {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Supersede(original, sample("replacement")); !errors.Is(err, sentinel) {
		t.Fatalf("Supersede: %v", err)
	}
	if len(store.All()) != 1 {
		t.Fatal("failed append changed store")
	}
}
