package memory

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"rhizome/internal/events"
)

func sample(id string) Memory {
	return Memory{ID: id, Kind: Fact, Content: "observed result", SourceType: "mission", SourceID: "source-1", Confidence: 0.75, GoalID: "g1", MissionID: "m1", Tags: []string{"verified", "work"}}
}
func creation(t *testing.T, id string) events.Event {
	t.Helper()
	m := sample(id)
	p, err := json.Marshal(payload{m.ID, m.Kind, m.Content, m.SourceType, m.SourceID, m.Confidence, m.GoalID, m.MissionID, m.Tags, ""})
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{AggregateType: "memory", AggregateID: id, Revision: 1, Type: "memory.created", Payload: p}
}
func create(t *testing.T, s Service, m Memory) Memory {
	t.Helper()
	got, err := s.Create(m)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRoundTripAllFields(t *testing.T) { // FR-RHZ-009
	s := Service{Store: &events.Store{}}
	input := sample("one")
	got := create(t, s, input)
	input.Revision = 1
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("got %#v want %#v", got, input)
	}
	log := s.Store.All()
	if len(log) != 1 || log[0].Type != "memory.created" {
		t.Fatalf("unexpected events: %#v", log)
	}
	replayed, err := Replay(log)
	if err != nil || !reflect.DeepEqual(replayed, input) {
		t.Fatalf("replay %#v: %v", replayed, err)
	}
	// Returned state and caller slices must not expose persisted event bytes.
	got.Tags[0] = "changed"
	input.Tags[1] = "changed"
	replayed, err = Replay(s.Store.All())
	if err != nil || !reflect.DeepEqual(replayed.Tags, []string{"verified", "work"}) {
		t.Fatalf("aliased tags: %#v %v", replayed, err)
	}
	for _, kind := range []Kind{Fact, Decision, Preference, Observation, Hypothesis, Reference} {
		m := sample(string(kind))
		m.Kind = kind
		create(t, s, m)
	}
}

func TestSupersedePreservesOriginalAndLink(t *testing.T) { // FR-RHZ-010
	s := Service{Store: &events.Store{}}
	old := create(t, s, sample("old"))
	before, err := json.Marshal(s.Store.List("memory", "old"))
	if err != nil {
		t.Fatal(err)
	}
	next := sample("next")
	next.Content = "corrected observation"
	got, err := s.Supersede(old, next)
	if err != nil {
		t.Fatal(err)
	}
	next.Revision = 1
	next.Supersedes = old.ID
	if !reflect.DeepEqual(got, next) {
		t.Fatalf("got %#v want %#v", got, next)
	}
	replayed, err := Replay(s.Store.List("memory", "next"))
	if err != nil || !reflect.DeepEqual(replayed, next) {
		t.Fatalf("replayed %#v: %v", replayed, err)
	}
	after, err := json.Marshal(s.Store.List("memory", "old"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("original event bytes changed")
	}
	if len(s.Store.All()) != 2 || s.Store.All()[1].Type != "memory.superseded" {
		t.Fatal("replacement did not append exactly one event")
	}
	if _, err = s.Supersede(old, sample("another")); err != nil {
		t.Fatalf("multiple replacements must be allowed: %v", err)
	}
}

func TestInvalidInputsNeverAppend(t *testing.T) { // FR-RHZ-009 FR-RHZ-010
	cases := map[string]func(*Memory){
		"blank ID": func(m *Memory) { m.ID = "  " }, "blank content": func(m *Memory) { m.Content = "\t" },
		"blank source type": func(m *Memory) { m.SourceType = " " }, "blank source ID": func(m *Memory) { m.SourceID = "" },
		"unknown kind": func(m *Memory) { m.Kind = "bogus" }, "negative confidence": func(m *Memory) { m.Confidence = -0.1 },
		"excess confidence": func(m *Memory) { m.Confidence = 1.1 }, "NaN": func(m *Memory) { m.Confidence = math.NaN() },
		"positive infinity": func(m *Memory) { m.Confidence = math.Inf(1) }, "negative infinity": func(m *Memory) { m.Confidence = math.Inf(-1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := Service{Store: &events.Store{}}
			old := create(t, s, sample("old"))
			m := sample("next")
			mutate(&m)
			if _, err := s.Create(m); err == nil {
				t.Fatal("invalid creation accepted")
			}
			if _, err := s.Supersede(old, m); err == nil {
				t.Fatal("invalid replacement accepted")
			}
			if len(s.Store.All()) != 1 {
				t.Fatal("invalid command appended event")
			}
		})
	}
	for _, confidence := range []float64{0, 1} {
		s := Service{Store: &events.Store{}}
		m := sample("boundary")
		m.Confidence = confidence
		create(t, s, m)
	}
}

func TestInvalidSupersessionNeverAppends(t *testing.T) { // FR-RHZ-010
	s := Service{Store: &events.Store{}}
	old := create(t, s, sample("old"))
	create(t, s, sample("existing"))
	tests := []struct {
		name      string
		old, next Memory
	}{
		{"missing", sample("absent"), sample("next")}, {"same ID", old, sample("old")},
		{"existing ID", old, sample("existing")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Supersede(tt.old, tt.next); err == nil {
				t.Fatal("invalid supersession accepted")
			}
			if len(s.Store.All()) != 2 {
				t.Fatal("invalid supersession appended")
			}
		})
	}
	m := sample("linked")
	m.Supersedes = "absent"
	if _, err := s.Create(m); err == nil {
		t.Fatal("Create allowed forged link")
	}
	if _, err := s.Supersede(old, m); err == nil {
		t.Fatal("mismatched link accepted")
	}
	if len(s.Store.All()) != 2 {
		t.Fatal("invalid link appended")
	}
}

func TestReplayRejectsInvalidStreams(t *testing.T) { // FR-RHZ-009 FR-RHZ-010
	base := creation(t, "one")
	changed := func(f func(*events.Event)) events.Event { e := base; f(&e); return e }
	tests := map[string][]events.Event{
		"empty":                nil,
		"malformed":            {changed(func(e *events.Event) { e.Payload = []byte("{") })},
		"null":                 {changed(func(e *events.Event) { e.Payload = []byte("null") })},
		"wrong aggregate type": {changed(func(e *events.Event) { e.AggregateType = "goal" })},
		"ID mismatch":          {changed(func(e *events.Event) { e.AggregateID = "other" })},
		"mixed IDs":            {base, changed(func(e *events.Event) { e.AggregateID = "other"; e.Revision = 2 })},
		"initial gap":          {changed(func(e *events.Event) { e.Revision = 2 })},
		"later gap":            {base, changed(func(e *events.Event) { e.Revision = 3 })},
		"unknown event":        {changed(func(e *events.Event) { e.Type = "memory.modified" })},
		"duplicate creation":   {base, changed(func(e *events.Event) { e.Revision = 2 })},
		"missing supersedes":   {changed(func(e *events.Event) { e.Type = "memory.superseded" })},
	}
	for name, log := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Replay(log); err == nil {
				t.Fatal("invalid stream accepted")
			}
		})
	}
}

func TestSearchFiltersStableOrderAndErrors(t *testing.T) { // FR-RHZ-009
	s := Service{Store: &events.Store{}}
	c := sample("c")
	c.Kind = Observation
	c.SourceID = "other"
	c.Tags = []string{"other"}
	create(t, s, c)
	create(t, s, sample("b"))
	create(t, s, sample("a"))
	if err := s.Store.Append(0, events.Event{AggregateType: "goal", AggregateID: "a", Revision: 1, Payload: []byte("invalid")}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		kind        Kind
		tag, source string
		ids         []string
	}{
		{"all", "", "", "", []string{"a", "b", "c"}},
		{"kind", Fact, "", "", []string{"a", "b"}},
		{"tag", "", "verified", "", []string{"a", "b"}},
		{"source", "", "", "other", []string{"c"}},
		{"intersection", Fact, "verified", "source-1", []string{"a", "b"}},
		{"no match", Fact, "verified", "other", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Search(tt.kind, tt.tag, tt.source)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ID)
			}
			if !reflect.DeepEqual(ids, tt.ids) {
				t.Fatalf("got %v want %v", ids, tt.ids)
			}
		})
	}
	bad := creation(t, "bad")
	bad.Payload = []byte("{")
	if err := s.Store.Append(0, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(Reference, "nonmatching", "nonmatching"); err == nil {
		t.Fatal("search hid invalid replay")
	}
}

func TestNilStoreReturnsErrors(t *testing.T) { // FR-RHZ-009 FR-RHZ-010
	var s Service
	if _, err := s.Create(sample("one")); err == nil {
		t.Fatal("nil Create succeeded")
	}
	if _, err := s.Supersede(sample("one"), sample("two")); err == nil {
		t.Fatal("nil Supersede succeeded")
	}
	if _, err := s.Search("", "", ""); err == nil {
		t.Fatal("nil Search succeeded")
	}
}
