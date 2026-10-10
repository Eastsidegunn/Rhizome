package execution

// RHZ-107 G1 (FR-RHZ-140): the provenance ref record is format-checked at
// write time and on replay (G2, sorted, unique, authorized ⊆ requested).

import (
	"encoding/json"
	"strings"
	"testing"

	"rhizome/internal/events"
)

func ref140(c byte) string { return "sref-" + strings.Repeat(string(c), 24) }

func TestProvenanceRefRecordFRRHZ140(t *testing.T) {
	a, b, c := ref140('a'), ref140('b'), ref140('c')
	ceil := smokePolicy()
	s := Service{Store: missionStore()}
	good := provFixture(ceil, ceil)
	good.RequestedRefs, good.AuthorizedRefs = []string{a, b}, []string{b}
	ref, err := s.IntentWithProvenance("m1", "k", ceil, ceil, good)
	if err != nil || ref.Provenance == nil || len(ref.Provenance.AuthorizedRefs) != 1 {
		t.Fatalf("%+v %v", ref, err)
	}
	log := s.Store.List("execution", ref.ID)
	if r, err := Replay(log); err != nil || r.Provenance.RequestedRefs[1] != b {
		t.Fatalf("%+v %v", r, err)
	}
	bad := map[string][2][]string{
		"bad format":     {{"sref-x"}, nil},
		"empty string":   {{""}, nil},
		"unsorted":       {{b, a}, nil},
		"duplicate":      {{a, a}, nil},
		"auth unsorted":  {{a, b}, {b, a}},
		"auth duplicate": {{a, b}, {a, a}},
		"not subset":     {{a, b}, {c}},
		"auth only":      {nil, {a}},
	}
	for name, v := range bad {
		p := provFixture(ceil, ceil)
		p.RequestedRefs, p.AuthorizedRefs = v[0], v[1]
		if _, err := s.IntentWithProvenance("m1", "k-"+name, ceil, ceil, p); err == nil {
			t.Errorf("write: %s accepted", name)
		}
		var m map[string]any
		if err := json.Unmarshal(log[0].Payload, &m); err != nil {
			t.Fatal(err)
		}
		pv := m["provenance"].(map[string]any)
		pv["requestedRefs"], pv["authorizedRefs"] = v[0], v[1]
		forged, _ := json.Marshal(m)
		ev := []events.Event{{AggregateType: "execution", AggregateID: ref.ID, Revision: 1, Type: "execution.intent", Payload: forged}}
		if _, err := Replay(ev); err == nil {
			t.Errorf("replay: %s accepted", name)
		}
	}
	// A non-string element fails decoding.
	raw := strings.Replace(string(log[0].Payload), `"requestedRefs":["`+a+`"`, `"requestedRefs":[1`, 1)
	if raw == string(log[0].Payload) {
		t.Fatal("fixture did not change")
	}
	if _, err := Replay([]events.Event{{AggregateType: "execution", AggregateID: ref.ID, Revision: 1, Type: "execution.intent", Payload: json.RawMessage(raw)}}); err == nil {
		t.Fatal("non-string ref accepted")
	}
	if n := len(s.Store.All()); n != 2 {
		t.Fatalf("rejections wrote: %d events", n)
	}
}

func TestStartRefsHelpersFRRHZ140(t *testing.T) {
	a, b, c := ref140('a'), ref140('b'), ref140('c')
	in := []string{c, a}
	got, ok := StartRefs(in)
	if !ok || len(got) != 2 || got[0] != a || got[1] != c || in[0] != c {
		t.Fatal(got, ok, in)
	}
	if got, ok := StartRefs(nil); !ok || got != nil {
		t.Fatal(got, ok)
	}
	if auth := AuthorizeRefs([]string{a, c}, []string{b, c}); len(auth) != 1 || auth[0] != c {
		t.Fatal(auth)
	}
	if auth := AuthorizeRefs(nil, []string{a, b}); auth != nil {
		t.Fatal("empty request authorized", auth)
	}
}

// Explicit null or [] for either ref field was never written by the writer
// (omitempty): replay rejects it.
func TestReplayRejectsNullOrEmptyRefsFRRHZ140(t *testing.T) {
	ceil := smokePolicy()
	s := Service{Store: missionStore()}
	ref, err := s.IntentWithProvenance("m1", "k", ceil, ceil, provFixture(ceil, ceil))
	if err != nil {
		t.Fatal(err)
	}
	base := string(s.Store.List("execution", ref.ID)[0].Payload)
	if _, err := Replay([]events.Event{{AggregateType: "execution", AggregateID: ref.ID, Revision: 1, Type: "execution.intent", Payload: json.RawMessage(base)}}); err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"requestedRefs":null`, `"requestedRefs":[]`, `"authorizedRefs":null`, `"authorizedRefs":[]`, `"RequestedRefs":null`, `"requestedRefs":"x"`} {
		forged := strings.Replace(base, `"provenance":{`, `"provenance":{`+frag+`,`, 1)
		if forged == base {
			t.Fatal("fixture did not change")
		}
		if _, err := Replay([]events.Event{{AggregateType: "execution", AggregateID: ref.ID, Revision: 1, Type: "execution.intent", Payload: json.RawMessage(forged)}}); err == nil {
			t.Errorf("%s accepted", frag)
		}
	}
}
