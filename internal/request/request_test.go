package request

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/mission"
)

const testActor = "unverified-local-operator:tester"

func requestFixture(t *testing.T) (*events.Store, Create) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-r", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-r", "goal-r", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	return s, Create{Name: "Deploy by hand", MissionID: "mission-r", Why: "needs hardware", Where: "rack 3", Commands: []string{"printf ' a  b '", "# exact bytes"}, After: "check health", Rollback: "restore image", RequestedBy: testActor, CorrelationID: "corr-create"}
}

// FR-RHZ-154: exact keys, replay invariants, id identity, actor shape and
// command byte rules all fail closed.
func TestRequestReplayRuleTableFRRHZ154(t *testing.T) {
	_, in := requestFixture(t)
	id := IDFor(in)
	validCreated, _ := json.Marshal(created{Name: in.Name, MissionID: in.MissionID, Why: in.Why, Where: in.Where, Commands: in.Commands, After: in.After, Rollback: in.Rollback, RequestedBy: in.RequestedBy})
	validClosed, _ := json.Marshal(closed{Decision: "done", Memo: "ok", ActorRef: testActor})
	base := []events.Event{
		{AggregateType: "request", AggregateID: id, Revision: 1, Type: "request.created", Payload: validCreated, CreatedAt: time.Unix(1, 2)},
		{AggregateType: "request", AggregateID: id, Revision: 2, Type: "request.closed", Payload: validClosed, CreatedAt: time.Unix(3, 4)},
	}
	if got, err := Replay(base); err != nil || got.Commands[0] != in.Commands[0] || got.CreatedAt != base[0].CreatedAt || got.ClosedAt != base[1].CreatedAt {
		t.Fatalf("round trip: %#v %v", got, err)
	}
	cases := []struct {
		name string
		edit func([]events.Event) []events.Event
	}{
		{"unknown created key", func(log []events.Event) []events.Event {
			log[0].Payload = append(log[0].Payload[:len(log[0].Payload)-1], []byte(`,"extra":1}`)...)
			return log
		}},
		{"wrong key case", func(log []events.Event) []events.Event {
			log[0].Payload = []byte(strings.Replace(string(log[0].Payload), `"Name"`, `"name"`, 1))
			return log
		}},
		{"id mismatch", func(log []events.Event) []events.Event {
			log[0].AggregateID, log[1].AggregateID = "r-000000000000000000000000", "r-000000000000000000000000"
			return log
		}},
		{"revision three", func(log []events.Event) []events.Event {
			return append(log, events.Event{AggregateType: "request", AggregateID: id, Revision: 3, Type: "request.closed", Payload: validClosed})
		}},
		{"decision pairing", func(log []events.Event) []events.Event {
			log[1].Payload = []byte(`{"Decision":"unable","Memo":"x","Reason":"why","ActorRef":"` + testActor + `"}`)
			return log
		}},
		{"command fence", func(log []events.Event) []events.Event {
			log[0].Payload = []byte(strings.Replace(string(log[0].Payload), `# exact bytes`, "```sh", 1))
			return log
		}},
		{"command newline", func(log []events.Event) []events.Event {
			log[0].Payload = []byte(strings.Replace(string(log[0].Payload), `# exact bytes`, `# exact bytes\n`, 1))
			return log
		}},
		{"command carriage", func(log []events.Event) []events.Event {
			log[0].Payload = []byte(strings.Replace(string(log[0].Payload), `# exact bytes`, `# exact bytes\rmore`, 1))
			return log
		}},
		{"command control", func(log []events.Event) []events.Event {
			log[0].Payload = []byte(strings.Replace(string(log[0].Payload), `# exact bytes`, `# exact bytes\u001b`, 1))
			return log
		}},
		{"actor prefix", func(log []events.Event) []events.Event {
			log[1].Payload = []byte(`{"Decision":"done","Memo":"ok","ActorRef":"tester"}`)
			return log
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := append([]events.Event(nil), base...)
			if _, err := Replay(tc.edit(log)); err == nil {
				t.Fatal("replay accepted invalid stream")
			}
		})
	}
}

func TestRequestIDDeterministicFRRHZ154(t *testing.T) {
	_, in := requestFixture(t)
	a, b := IDFor(in), IDFor(in)
	if a != b || len(a) != 26 || !strings.HasPrefix(a, "r-") {
		t.Fatalf("ids %q %q", a, b)
	}
	in.Commands = []string{"printf", " a"}
	if IDFor(in) == a {
		t.Fatal("length-prefixed command boundary did not affect id")
	}
}

func TestRequestCreateRuleTableZeroWriteFRRHZ155(t *testing.T) {
	s, valid := requestFixture(t)
	ms := mission.Service{Store: s}
	if _, err := ms.Create("mission-terminal", "goal-r", "terminal mission", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Transition("mission-terminal", 1, domain.MissionReady); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Transition("mission-terminal", 2, domain.MissionRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.ApplyMissionDecision("mission-terminal", 3, domain.MissionSucceeded, "decision-mission-terminal", "corr-mission-terminal", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.CreateGoal("goal-terminal", "terminal goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.ApplyGoalDecision("goal-terminal", "decision-terminal", "corr-terminal", domain.GoalAchieved); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, want   string
		edit         func(*Create)
		unknownField bool
	}{
		{"Q1 name required", "request name required", func(in *Create) { in.Name = " \t" }, false},
		{"Q1 name invalid", "request name invalid", func(in *Create) { in.Name = "bad\nname" }, false},
		{"Q2 target required", "missionId or goalId required", func(in *Create) { in.MissionID = "" }, false},
		{"Q2 both ids given", "both missionId and goalId given", func(in *Create) { in.GoalID = "goal-r" }, false},
		{"Q2 mission not found", "mission not found", func(in *Create) { in.MissionID = "mission-missing" }, false},
		{"Q2 goal not found", "goal not found", func(in *Create) {
			in.MissionID, in.GoalID = "", "goal-missing"
		}, false},
		{"Q2 mission is terminal", "mission is terminal", func(in *Create) { in.MissionID = "mission-terminal" }, false},
		{"Q2 goal is terminal", "goal is terminal", func(in *Create) {
			in.MissionID, in.GoalID = "", "goal-terminal"
		}, false},
		{"Q3 why required", "request why required", func(in *Create) { in.Why = " " }, false},
		{"Q3 where required", "request where required", func(in *Create) { in.Where = " " }, false},
		{"Q3 after required", "request after required", func(in *Create) { in.After = " " }, false},
		{"Q3 why too long", "request why too long", func(in *Create) { in.Why = strings.Repeat("x", 2001) }, false},
		{"Q3 where too long", "request where too long", func(in *Create) { in.Where = strings.Repeat("x", 2001) }, false},
		{"Q3 after too long", "request after too long", func(in *Create) { in.After = strings.Repeat("x", 2001) }, false},
		{"Q3 rollback too long", "request rollback too long", func(in *Create) { in.Rollback = strings.Repeat("x", 2001) }, false},
		{"Q4 too many commands", "too many commands", func(in *Create) {
			in.Commands = make([]string, 33)
			for i := range in.Commands {
				in.Commands[i] = "x"
			}
		}, false},
		{"Q4 command too long", "invalid command", func(in *Create) { in.Commands = []string{" x " + strings.Repeat("x", 2000)} }, false},
		{"Q4 command fence", "invalid command", func(in *Create) { in.Commands = []string{"```sh"} }, false},
		{"Q4 command trailing newline", "invalid command", func(in *Create) { in.Commands = []string{"echo ok\n"} }, false},
		{"Q4 command whitespace only", "invalid command", func(in *Create) { in.Commands = []string{" \t "} }, false},
		{"Q5 control character in name", "invalid control character in name", func(in *Create) { in.Name = "bad\x1bname" }, false},
		{"Q5 control character in where", "invalid control character in where", func(in *Create) { in.Where = "rack\x1b" }, false},
		{"Q5 control character in command", "invalid control character in command", func(in *Create) { in.Commands = []string{"echo\x1bok"} }, false},
		{"Q6 actor", "actor required", func(in *Create) { in.RequestedBy = " " }, false},
		{"Q11 unknown intent field", "unknown intent field", func(*Create) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			in.Commands = append([]string(nil), valid.Commands...)
			tc.edit(&in)
			before := len(s.All())
			_, err := (Service{Store: s}).CreateRequest(in, tc.unknownField)
			if err == nil || err.Error() != tc.want || len(s.All()) != before {
				t.Fatalf("err=%v writes=%d", err, len(s.All())-before)
			}
		})
	}
	r, err := (Service{Store: s}).CreateRequest(valid)
	if err != nil || r.Commands[0] != valid.Commands[0] {
		t.Fatalf("valid create: %#v %v", r, err)
	}
	before := len(s.All())
	if _, err := (Service{Store: s}).CreateRequest(valid); err == nil || err.Error() != "request already exists: "+r.ID || len(s.All()) != before {
		t.Fatalf("duplicate: %v", err)
	}
}

type barrierStore struct {
	base    *events.Store
	arrived chan struct{}
	release chan struct{}
	rev     uint64
}

func (s *barrierStore) Append(expected uint64, event events.Event) error {
	if event.AggregateType == "request" && event.Revision == s.rev {
		s.arrived <- struct{}{}
		<-s.release
	}
	return s.base.Append(expected, event)
}
func (s *barrierStore) List(kind, id string) []events.Event { return s.base.List(kind, id) }
func (s *barrierStore) All() []events.Event                 { return s.base.All() }

func TestRequestConcurrentDuplicateCreateFRRHZ155(t *testing.T) {
	base, in := requestFixture(t)
	bs := &barrierStore{base: base, arrived: make(chan struct{}, 2), release: make(chan struct{}), rev: 1}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := (Service{Store: bs}).CreateRequest(in); errs <- err }()
	}
	<-bs.arrived
	<-bs.arrived
	close(bs.release)
	var ok, conflict int
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil {
			ok++
		} else if err.Error() == "request already exists: "+IDFor(in) {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 || len(base.List("request", IDFor(in))) != 1 {
		t.Fatalf("ok=%d conflict=%d log=%d", ok, conflict, len(base.List("request", IDFor(in))))
	}
}

func TestRequestCloseStateMachineFRRHZ156(t *testing.T) {
	t.Run("complete without memo succeeds", func(t *testing.T) {
		s, in := requestFixture(t)
		r, err := (Service{Store: s}).CreateRequest(in)
		if err != nil {
			t.Fatal(err)
		}
		closed, err := (Service{Store: s}).Close(r.ID, Done, "", "", "closer", "")
		if err != nil || closed.Decision != Done || closed.Memo != "" {
			t.Fatalf("close without memo: %#v %v", closed, err)
		}
		if strings.Contains(string(s.List("request", r.ID)[1].Payload), `"Memo"`) {
			t.Fatalf("empty memo key was serialized: %s", s.List("request", r.ID)[1].Payload)
		}
	})

	s, in := requestFixture(t)
	r, err := (Service{Store: s}).CreateRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	if _, err := (Service{Store: s}).Close(r.ID, Unable, "", " ", "other", ""); err == nil || err.Error() != "reason required" || len(s.All()) != before {
		t.Fatalf("unable rejection: %v", err)
	}
	for _, tc := range []struct {
		name         string
		decision     Decision
		memo, reason string
	}{
		{"complete with reason", Done, "", "extra"},
		{"unable with memo", Unable, "extra", "cannot"},
		{"cancel with memo", Cancelled, "extra", "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.All())
			if _, err := (Service{Store: s}).Close(r.ID, tc.decision, tc.memo, tc.reason, "other", ""); err == nil || err.Error() != "unknown intent field" || len(s.All()) != before {
				t.Fatalf("pairing rejection: %v writes=%d", err, len(s.All())-before)
			}
		})
	}
	for _, tc := range []struct {
		name         string
		decision     Decision
		memo, reason string
		want         string
	}{
		{"memo too long", Done, strings.Repeat("x", 2001), "", "request memo too long"},
		{"reason too long", Unable, "", strings.Repeat("x", 2001), "request reason too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.All())
			if _, err := (Service{Store: s}).Close(r.ID, tc.decision, tc.memo, tc.reason, "other", ""); err == nil || err.Error() != tc.want || len(s.All()) != before {
				t.Fatalf("length rejection: %v writes=%d", err, len(s.All())-before)
			}
		})
	}
	closed, err := (Service{Store: s}).Close(r.ID, Done, "memo", "", "other", "corr-close")
	if err != nil || closed.Decision != Done || closed.Memo != "memo" || s.List("request", r.ID)[1].CorrelationID != "corr-close" {
		t.Fatalf("close: %#v %v", closed, err)
	}
	before = len(s.All())
	if _, err := (Service{Store: s}).Close(r.ID, Cancelled, "", "ops reason", "unrelated", ""); err == nil || err.Error() != "request already closed" || len(s.All()) != before {
		t.Fatalf("reclose: %v", err)
	}
}

func TestRequestConcurrentCloseFRRHZ156(t *testing.T) {
	base, in := requestFixture(t)
	r, err := (Service{Store: base}).CreateRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	bs := &barrierStore{base: base, arrived: make(chan struct{}, 2), release: make(chan struct{}), rev: 2}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, decision := range []Decision{Done, Unable} {
		wg.Add(1)
		go func(d Decision) {
			defer wg.Done()
			reason := ""
			if d == Unable {
				reason = "cannot"
			}
			_, e := (Service{Store: bs}).Close(r.ID, d, "", reason, "closer", "")
			errs <- e
		}(decision)
	}
	<-bs.arrived
	<-bs.arrived
	close(bs.release)
	wg.Wait()
	close(errs)
	var ok, conflict int
	for err := range errs {
		if err == nil {
			ok++
		} else if err.Error() == "request already closed" {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 || len(base.List("request", r.ID)) != 2 {
		t.Fatalf("ok=%d conflict=%d log=%d", ok, conflict, len(base.List("request", r.ID)))
	}
}
