package policy

import "testing"

func TestMergeIntersectionAndNarrowing(t *testing.T) {
	cases := []struct {
		a, b  Policy
		caps  string
		empty bool
	}{{Policy{Capabilities: []string{"a", "b"}, Budget: 10, Timeout: 20}, Policy{Capabilities: []string{"b", "c"}, Budget: 5, Timeout: 30}, "b", false}, {Policy{Capabilities: []string{"a"}, Budget: 1, Timeout: 1, Unlimited: true}, Policy{Capabilities: []string{"b"}, Budget: 2, Timeout: 2, Unlimited: true}, "", true}}
	for _, c := range cases {
		r := Merge(c.a, c.b)
		if r.Empty != c.empty || (c.caps == "b" && (len(r.Policy.Capabilities) != 1 || r.Policy.Capabilities[0] != "b")) {
			if c.empty && r.Empty {
				continue
			}
			t.Fatalf("%+v", r)
		}
	}
}

func TestMergeDoesNotMutateOrDuplicate(t *testing.T) {
	a := Policy{Capabilities: []string{"x", "x"}, Budget: 10, Timeout: 20}
	b := Policy{Capabilities: []string{"x", "x"}, Budget: 5, Timeout: 5}
	r := Merge(a, b)
	if len(r.Policy.Capabilities) != 1 {
		t.Fatal(r)
	}
	if len(a.Capabilities) != 2 || len(b.Capabilities) != 2 {
		t.Fatal("inputs mutated")
	}
}
func TestValidRejectsNegativeLimits(t *testing.T) {
	if Valid(Policy{Budget: -1}) || Valid(Policy{Timeout: -1}) {
		t.Fatal("negative limit accepted")
	}
	if !Valid(Policy{Budget: 0, Timeout: 0}) {
		t.Fatal("zero rejected")
	}
}

func TestMergeMarksInvalidInput(t *testing.T) {
	r := Merge(Policy{Budget: -1}, Policy{})
	if !r.Invalid || r.Policy.Capabilities != nil {
		t.Fatalf("invalid result: %+v", r)
	}
}
