package question

// RHZ-134 (FR-RHZ-175): the structured "card" of a question — the
// decision-ready facts a human needs (risk, what happens if skipped, why it
// needs a human, the options, how to roll back, where it sits). The card is
// stored on question.asked as additive omitempty Go-name keys; it is NOT part
// of the digest or the id (Digest(title, body, recommendation) is unchanged),
// so a gate's identity is still its title/body/recommendation.
//
// Write path and replay share one rule (NormalizeCard): the writer stores
// only the normalized card, and replay rejects a stored card that is not
// already in normalized form, so replay accepts exactly what the writer can
// produce.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Card limits (FR-RHZ-175).
const (
	CardRiskLineMax   = 200
	CardTextMax       = 1000 // ifSkipped, whyHuman, rollback
	CardOptionsMax    = 8
	CardOptionKeyMax  = 32
	CardOptionLabel   = 64
	CardOptionTextMax = 500 // meaning, consequence
	CardContextMax    = 200 // each context field
)

// Risk is the one-line risk statement of a card.
type Risk struct{ Level, Line string }

// Option is one answer the human may give, with what it means and causes.
type Option struct{ Key, Label, Meaning, Consequence string }

// Context places the question: which project, what is being done, and the
// current state.
type Context struct{ Project, What, State string }

// Card is the structured part of a question. The zero Card is "no card".
// The JSON tags are the stored (journal) form: Go-name keys, omitempty, so a
// question without a card serializes byte-identically to pre-RHZ-134.
type Card struct {
	Risk      *Risk    `json:",omitempty"`
	IfSkipped string   `json:",omitempty"`
	WhyHuman  string   `json:",omitempty"`
	Options   []Option `json:",omitempty"`
	Rollback  string   `json:",omitempty"`
	Context   *Context `json:",omitempty"`
}

// Empty reports whether the card carries nothing.
func (c Card) Empty() bool { return reflect.DeepEqual(c, Card{}) }

var optionKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func singleLine(s string) bool { return !strings.ContainsAny(s, "\r\n") }

func runes(s string) int { return utf8.RuneCountInString(s) }

// optional maps a whitespace-only value to absent and checks the rune cap.
func optional(name, s string, max int) (string, error) {
	if blank(s) {
		return "", nil
	}
	if runes(s) > max {
		return "", fmt.Errorf("%s exceeds %d characters", name, max)
	}
	return s, nil
}

// NormalizeCard validates c and returns its canonical form: whitespace-only
// strings become absent, an empty options list and an all-empty context
// become absent. Non-blank values are kept byte-for-byte.
func NormalizeCard(c Card) (Card, error) {
	var out Card
	var err error
	if c.Risk != nil {
		switch c.Risk.Level {
		case "low", "medium", "high":
		default:
			return Card{}, fmt.Errorf("risk.level must be low, medium or high")
		}
		if blank(c.Risk.Line) {
			return Card{}, fmt.Errorf("risk.line required")
		}
		if !singleLine(c.Risk.Line) {
			return Card{}, fmt.Errorf("risk.line must be one line")
		}
		if runes(c.Risk.Line) > CardRiskLineMax {
			return Card{}, fmt.Errorf("risk.line exceeds %d characters", CardRiskLineMax)
		}
		out.Risk = &Risk{Level: c.Risk.Level, Line: c.Risk.Line}
	}
	if out.IfSkipped, err = optional("ifSkipped", c.IfSkipped, CardTextMax); err != nil {
		return Card{}, err
	}
	if out.WhyHuman, err = optional("whyHuman", c.WhyHuman, CardTextMax); err != nil {
		return Card{}, err
	}
	if out.Rollback, err = optional("rollback", c.Rollback, CardTextMax); err != nil {
		return Card{}, err
	}
	if len(c.Options) > CardOptionsMax {
		return Card{}, fmt.Errorf("at most %d options", CardOptionsMax)
	}
	seen := map[string]bool{}
	for i, o := range c.Options {
		if o.Key == "" || runes(o.Key) > CardOptionKeyMax || !optionKeyPattern.MatchString(o.Key) {
			return Card{}, fmt.Errorf("options[%d].key must match %s (max %d)", i, optionKeyPattern.String(), CardOptionKeyMax)
		}
		if seen[o.Key] {
			return Card{}, fmt.Errorf("options[%d].key %q is duplicated", i, o.Key)
		}
		seen[o.Key] = true
		if blank(o.Label) {
			return Card{}, fmt.Errorf("options[%d].label required", i)
		}
		if !singleLine(o.Label) {
			return Card{}, fmt.Errorf("options[%d].label must be one line", i)
		}
		if runes(o.Label) > CardOptionLabel {
			return Card{}, fmt.Errorf("options[%d].label exceeds %d characters", i, CardOptionLabel)
		}
		meaning, err := optional(fmt.Sprintf("options[%d].meaning", i), o.Meaning, CardOptionTextMax)
		if err != nil {
			return Card{}, err
		}
		consequence, err := optional(fmt.Sprintf("options[%d].consequence", i), o.Consequence, CardOptionTextMax)
		if err != nil {
			return Card{}, err
		}
		out.Options = append(out.Options, Option{Key: o.Key, Label: o.Label, Meaning: meaning, Consequence: consequence})
	}
	if c.Context != nil {
		var ctx Context
		for _, f := range []struct {
			name string
			in   string
			out  *string
		}{{"context.project", c.Context.Project, &ctx.Project}, {"context.what", c.Context.What, &ctx.What}, {"context.state", c.Context.State, &ctx.State}} {
			if blank(f.in) {
				continue
			}
			if !singleLine(f.in) {
				return Card{}, fmt.Errorf("%s must be one line", f.name)
			}
			if runes(f.in) > CardContextMax {
				return Card{}, fmt.Errorf("%s exceeds %d characters", f.name, CardContextMax)
			}
			*f.out = f.in
		}
		if ctx != (Context{}) {
			out.Context = &ctx
		}
	}
	return out, nil
}

var errInvalidCard = errors.New("invalid card")

// strictObject decodes a JSON object whose keys must be a subset of allowed
// (exact case) and whose values must all be JSON strings.
func strictObject(name string, raw json.RawMessage, allowed ...string) (map[string]string, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	out := map[string]string{}
	for k, v := range object {
		if !ok[k] {
			return nil, fmt.Errorf("%s: unknown key %q", name, k)
		}
		var s string
		if isNull(v) || json.Unmarshal(v, &s) != nil {
			return nil, fmt.Errorf("%s.%s must be a string", name, k)
		}
		out[k] = s
	}
	return out, nil
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// DecodeIntentCard decodes the lowerCamel intent wire of a card (FR-RHZ-175)
// strictly — unknown nested keys and wrong types are rejected — and returns
// the normalized card. Absent or null nested values are "not given".
func DecodeIntentCard(risk, options, context json.RawMessage, ifSkipped, whyHuman, rollback string) (Card, error) {
	c := Card{IfSkipped: ifSkipped, WhyHuman: whyHuman, Rollback: rollback}
	if !isNull(risk) {
		o, err := strictObject("risk", risk, "level", "line")
		if err != nil {
			return Card{}, err
		}
		c.Risk = &Risk{Level: o["level"], Line: o["line"]}
	}
	if !isNull(options) {
		var items []json.RawMessage
		if json.Unmarshal(options, &items) != nil {
			return Card{}, fmt.Errorf("options must be an array")
		}
		for i, item := range items {
			o, err := strictObject(fmt.Sprintf("options[%d]", i), item, "key", "label", "meaning", "consequence")
			if err != nil {
				return Card{}, err
			}
			c.Options = append(c.Options, Option{Key: o["key"], Label: o["label"], Meaning: o["meaning"], Consequence: o["consequence"]})
		}
	}
	if !isNull(context) {
		o, err := strictObject("context", context, "project", "what", "state")
		if err != nil {
			return Card{}, err
		}
		c.Context = &Context{Project: o["project"], What: o["what"], State: o["state"]}
	}
	return NormalizeCard(c)
}

// replayCard enforces "replay accepts exactly what the writer produces": the
// stored card must validate and already be in normalized form.
func replayCard(c Card) error {
	n, err := NormalizeCard(c)
	if err != nil || !reflect.DeepEqual(n, c) {
		return errInvalidCard
	}
	return nil
}
