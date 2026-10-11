package session

// RHZ-143 (FR-RHZ-187): claude's real interactive entries ({name, kind,
// status, cwd, pid}, no ids) are kept by the observer in memory by pid and
// never journaled; an interactive stream journaled by RHZ-142 closes on the
// first tick; a ceiling-time close is never reopened; Retire writes once.

import (
	"context"
	"errors"
	"testing"

	"rhizome/internal/events"
)

func TestObserverInteractiveMemoryFRRHZ187(t *testing.T) {
	s := &events.Store{}
	svc := Service{Store: s}
	if _, err := svc.Open(Opened{SessionID: "old-it", Name: "it", Mode: Interactive, Cwd: "/tmp", Kind: KindInteractive}); err != nil {
		t.Fatal(err)
	}
	bin := fakeClaude(t, `echo '[{"name":"b","kind":"interactive","status":"busy","cwd":"/tmp","pid":20},{"name":"a","kind":"interactive","status":"idle","cwd":"/tmp","pid":10},{"name":"x","kind":"interactive","cwd":"/tmp"},{"name":"r","kind":"interactive","cwd":"rel","pid":30},{"id":"k1","sessionId":"K","kind":"background","state":"idle","cwd":"/tmp"}]'`)
	o := &Observer{Store: s, ClaudePath: bin}
	ctx := context.Background()
	n, err := o.Tick(ctx)
	if err != nil || n != 2 || s.Len() != 3 {
		t.Fatalf("tick %d %v (%d events)", n, err, s.Len())
	}
	if x, _ := svc.Get("old-it"); x.State != Closed || x.Reason != ReasonGone {
		t.Fatalf("legacy interactive %+v", x)
	}
	it := o.Interactive()
	if len(it) != 2 || it[0].Pid != 10 || it[0].Name != "a" || it[0].StatusText() != "idle" || it[1].Pid != 20 || it[1].Kind != KindInteractive {
		t.Fatalf("interactive %+v", it)
	}
	if n, _ := o.Tick(ctx); n != 0 || s.Len() != 3 {
		t.Fatalf("second tick wrote %d", n)
	}
	// a cancelled serve ctx ends the agents run: nothing written, view kept
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := o.Tick(cancelled); err == nil || len(o.Interactive()) != 2 {
		t.Fatalf("cancelled tick %v", err)
	}
	var nilObs *Observer
	if nilObs.Interactive() != nil {
		t.Fatal("nil observer interactive")
	}
	// ceiling-time: Retire closes once, the observation never reopens
	failed := errors.New("stop failed")
	if did, err := svc.Retire("K", ReasonCeilingTime, func() error { return failed }); did || err != failed || s.Len() != 3 {
		t.Fatalf("failed retire %v %v", did, err)
	}
	if did, err := svc.Retire("K", ReasonCeilingTime, nil); !did || err != nil {
		t.Fatalf("retire %v %v", did, err)
	}
	if did, err := svc.Retire("K", ReasonCeilingTime, func() error { t.Fatal("stop of a closed session"); return nil }); did || err != nil || s.Len() != 4 {
		t.Fatalf("second retire %v %v", did, err)
	}
	if n, _ := o.Tick(ctx); n != 0 || s.Len() != 4 {
		t.Fatalf("tick reopened a ceiling-time session: %d", n)
	}
}
