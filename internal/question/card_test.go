package question_test

// RHZ-134 (FR-RHZ-175): the structured question card — validation, storage,
// replay, digest/id independence and re-ask conflict.

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/mission"
	question "rhizome/internal/question"
)

func fullCard174() question.Card {
	return question.Card{
		Risk:      &question.Risk{Level: "high", Line: "prod data may be lost"},
		IfSkipped: "the release slips a day",
		WhyHuman:  "it touches billing",
		Options: []question.Option{
			{Key: "go", Label: "Ship it", Meaning: "deploy now", Consequence: "billing migrates"},
			{Key: "wait_1", Label: "Wait"},
		},
		Rollback: "revert the migration",
		Context:  &question.Context{Project: "rhizome", What: "migration", State: "staged"},
	}
}

func store174(t *testing.T) *events.Store {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("goal-174", "g", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-174", "goal-174", "m", "done"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAskCardStoresAndReplaysFRRHZ175(t *testing.T) {
	s := store174(t)
	card := fullCard174()
	q, err := (question.Service{Store: s}).AskCard("t", "b", "r", "mission-174", "", "op", "", card)
	if err != nil {
		t.Fatal(err)
	}
	if q.Card == nil || !reflect.DeepEqual(*q.Card, card) {
		t.Fatalf("ref card: %+v", q.Card)
	}
	log := s.List("question", q.ID)
	var p map[string]json.RawMessage
	if err := json.Unmarshal(log[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Risk", "IfSkipped", "WhyHuman", "Options", "Rollback", "Context"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("payload lacks %s: %s", k, log[0].Payload)
		}
	}
	got, err := question.Replay(log)
	if err != nil || got.Card == nil || !reflect.DeepEqual(*got.Card, card) {
		t.Fatalf("replay: %+v %v", got, err)
	}
}

// A card-less question stores exactly the pre-RHZ-134 payload keys.
func TestAskWithoutCardPayloadUnchangedFRRHZ175(t *testing.T) {
	s := store174(t)
	q, err := (question.Service{Store: s}).Ask("t", "b", "r", "mission-174", "", "op", "c")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Title":"t","Body":"b","Recommendation":"r","MissionID":"mission-174","RequestedBy":"unverified-local-operator:op","CorrelationID":"c","Digest":"` + q.Digest + `"}`
	if got := string(s.List("question", q.ID)[0].Payload); got != want {
		t.Fatalf("payload\n got %s\nwant %s", got, want)
	}
	if q.Card != nil {
		t.Fatalf("card on card-less question: %+v", q.Card)
	}
}

func TestCardDoesNotChangeDigestOrIDFRRHZ175(t *testing.T) {
	s := store174(t)
	want, _ := question.IDFor("t", "b", "r")
	q, err := (question.Service{Store: s}).AskCard("t", "b", "r", "mission-174", "", "op", "", fullCard174())
	if err != nil {
		t.Fatal(err)
	}
	if q.ID != want || q.Digest != question.Digest("t", "b", "r") || !strings.HasPrefix(q.Digest, "rhz-question-v2:") {
		t.Fatalf("id/digest moved: %s %s", q.ID, q.Digest)
	}
}

func TestCardValidationRejectsWithZeroWritesFRRHZ175(t *testing.T) {
	long := func(n int) string { return strings.Repeat("가", n) }
	nine := make([]question.Option, 9)
	for i := range nine {
		nine[i] = question.Option{Key: string(rune('a' + i)), Label: "x"}
	}
	cases := map[string]question.Card{
		"risk level":          {Risk: &question.Risk{Level: "extreme", Line: "x"}},
		"risk line missing":   {Risk: &question.Risk{Level: "low", Line: "  "}},
		"risk line multiline": {Risk: &question.Risk{Level: "low", Line: "a\nb"}},
		"risk line long":      {Risk: &question.Risk{Level: "low", Line: long(201)}},
		"ifSkipped long":      {IfSkipped: long(1001)},
		"whyHuman long":       {WhyHuman: long(1001)},
		"rollback long":       {Rollback: long(1001)},
		"too many options":    {Options: nine},
		"option key empty":    {Options: []question.Option{{Key: "", Label: "x"}}},
		"option key pattern":  {Options: []question.Option{{Key: "Go", Label: "x"}}},
		"option key leading":  {Options: []question.Option{{Key: "-go", Label: "x"}}},
		"option key long":     {Options: []question.Option{{Key: strings.Repeat("a", 33), Label: "x"}}},
		"option key dup":      {Options: []question.Option{{Key: "a", Label: "x"}, {Key: "a", Label: "y"}}},
		"option label empty":  {Options: []question.Option{{Key: "a", Label: " "}}},
		"option label lines":  {Options: []question.Option{{Key: "a", Label: "x\ry"}}},
		"option label long":   {Options: []question.Option{{Key: "a", Label: long(65)}}},
		"option meaning long": {Options: []question.Option{{Key: "a", Label: "x", Meaning: long(501)}}},
		"option conseq long":  {Options: []question.Option{{Key: "a", Label: "x", Consequence: long(501)}}},
		"context long":        {Context: &question.Context{Project: long(201)}},
		"context multiline":   {Context: &question.Context{State: "a\nb"}},
	}
	for name, card := range cases {
		s := store174(t)
		before := len(s.All())
		if _, err := (question.Service{Store: s}).AskCard("t", "b", "r", "mission-174", "", "op", "", card); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if len(s.All()) != before {
			t.Fatalf("%s: wrote events", name)
		}
	}
}

func TestCardNormalizationFRRHZ175(t *testing.T) {
	ok := strings.Repeat("가", 200)
	got, err := question.NormalizeCard(question.Card{
		Risk:      &question.Risk{Level: "low", Line: ok},
		IfSkipped: "   ",
		Options:   []question.Option{},
		Context:   &question.Context{Project: " ", What: "", State: "\t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.IfSkipped != "" || got.Options != nil || got.Context != nil || got.Risk.Line != ok {
		t.Fatalf("normalized: %+v", got)
	}
}

func TestReAskCardConflictFRRHZ175(t *testing.T) {
	s := store174(t)
	qs := question.Service{Store: s}
	first, err := qs.AskCard("t", "b", "r", "mission-174", "", "op", "", fullCard174())
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	again, err := qs.AskCard("t", "b", "r", "mission-174", "", "op", "", fullCard174())
	if err != nil || again.ID != first.ID || len(s.All()) != before {
		t.Fatalf("same card re-ask: %v", err)
	}
	other := fullCard174()
	other.WhyHuman = "different"
	for name, c := range map[string]question.Card{"different": other, "missing": {}} {
		if _, err := qs.AskCard("t", "b", "r", "mission-174", "", "op", "", c); !errors.Is(err, question.ErrCardConflict) {
			t.Fatalf("%s: err=%v", name, err)
		}
		if len(s.All()) != before {
			t.Fatalf("%s: wrote", name)
		}
	}
	if _, err := qs.Ask("t", "b", "r", "mission-174", "", "op", ""); !errors.Is(err, question.ErrCardConflict) {
		t.Fatalf("Ask without card: %v", err)
	}
	// A card-less gate re-asked with a card is a conflict too; without one,
	// idempotent as before.
	plain, err := qs.Ask("t2", "b", "r", "mission-174", "", "op", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qs.AskCard("t2", "b", "r", "mission-174", "", "op", "", fullCard174()); !errors.Is(err, question.ErrCardConflict) {
		t.Fatalf("card on card-less: %v", err)
	}
	if again, err := qs.Ask("t2", "b", "r", "mission-174", "", "op", ""); err != nil || again != plain {
		t.Fatalf("card-less re-ask: %v", err)
	}
	// Equal after normalization counts as equal.
	norm := fullCard174()
	norm.Context = &question.Context{Project: "rhizome", What: "migration", State: "staged"}
	norm.IfSkipped = fullCard174().IfSkipped
	if _, err := qs.AskCard("t", "b", "r", "mission-174", "", "op", "", norm); err != nil {
		t.Fatalf("normalized-equal re-ask: %v", err)
	}
}

func askedPayload174(t *testing.T, extra map[string]any) []byte {
	t.Helper()
	d := question.Digest("t", "b", "r")
	p := map[string]any{"Title": "t", "Body": "b", "Recommendation": "r", "MissionID": "m", "RequestedBy": "unverified-local-operator:op", "CorrelationID": "", "Digest": d}
	for k, v := range extra {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReplayRejectsInvalidStoredCardFRRHZ175(t *testing.T) {
	id, _ := question.IDFor("t", "b", "r")
	bad := map[string]map[string]any{
		"risk level":       {"Risk": map[string]string{"Level": "extreme", "Line": "x"}},
		"blank ifSkipped":  {"IfSkipped": "   "},
		"empty options":    {"Options": []any{}},
		"empty context":    {"Context": map[string]string{"Project": "", "What": "", "State": ""}},
		"dup option key":   {"Options": []map[string]string{{"Key": "a", "Label": "x"}, {"Key": "a", "Label": "y"}}},
		"multiline label":  {"Options": []map[string]string{{"Key": "a", "Label": "x\ny"}}},
		"whyHuman too big": {"WhyHuman": strings.Repeat("x", 1001)},
	}
	for name, extra := range bad {
		e := events.Event{AggregateType: "question", AggregateID: id, Revision: 1, Type: "question.asked", Payload: askedPayload174(t, extra), CreatedAt: time.Now().UTC()}
		if _, err := question.Replay([]events.Event{e}); err == nil {
			t.Fatalf("%s: replay accepted", name)
		}
	}
	good := map[string]any{"Risk": map[string]string{"Level": "low", "Line": "x"}, "Options": []map[string]string{{"Key": "a", "Label": "x", "Meaning": "", "Consequence": ""}}}
	e := events.Event{AggregateType: "question", AggregateID: id, Revision: 1, Type: "question.asked", Payload: askedPayload174(t, good), CreatedAt: time.Now().UTC()}
	if r, err := question.Replay([]events.Event{e}); err != nil || r.Card == nil || r.Card.Risk.Level != "low" {
		t.Fatalf("valid stored card: %+v %v", r, err)
	}
}

func TestDecodeIntentCardStrictFRRHZ175(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	bad := []struct {
		name                  string
		risk, options, contxt json.RawMessage
	}{
		{"risk unknown key", raw(`{"level":"low","line":"x","extra":1}`), nil, nil},
		{"risk wrong case", raw(`{"Level":"low","line":"x"}`), nil, nil},
		{"risk not object", raw(`"low"`), nil, nil},
		{"risk level number", raw(`{"level":1,"line":"x"}`), nil, nil},
		{"options not array", nil, raw(`{"key":"a"}`), nil},
		{"option unknown key", nil, raw(`[{"key":"a","label":"x","why":"y"}]`), nil},
		{"option label null", nil, raw(`[{"key":"a","label":null}]`), nil},
		{"context unknown key", nil, nil, raw(`{"project":"p","owner":"x"}`)},
		{"context wrong type", nil, nil, raw(`{"project":["p"]}`)},
	}
	for _, c := range bad {
		if _, err := question.DecodeIntentCard(c.risk, c.options, c.contxt, "", "", ""); err == nil {
			t.Fatalf("%s: accepted", c.name)
		}
	}
	card, err := question.DecodeIntentCard(raw(`{"level":"medium","line":"l"}`), raw(`[{"key":"a","label":"A","meaning":"m"}]`), raw(`{"what":"w"}`), "skip", " ", "rb")
	if err != nil {
		t.Fatal(err)
	}
	want := question.Card{Risk: &question.Risk{Level: "medium", Line: "l"}, IfSkipped: "skip", Options: []question.Option{{Key: "a", Label: "A", Meaning: "m"}}, Rollback: "rb", Context: &question.Context{What: "w"}}
	if !reflect.DeepEqual(card, want) {
		t.Fatalf("decoded %+v", card)
	}
	if c, err := question.DecodeIntentCard(raw(`null`), nil, raw(`null`), "", "", ""); err != nil || !c.Empty() {
		t.Fatalf("null = absent: %+v %v", c, err)
	}
}
