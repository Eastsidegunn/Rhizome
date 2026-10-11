package sessionlauncher

// RHZ-149 (FR-RHZ-193): the optional lifecycle keys autoCompleteOnDone and
// staleDays — absent keeps the digest, present changes it, out of range fails.

import "testing"

func TestLedgerLifecycleKeysFRRHZ193(t *testing.T) {
	f := newFake(t, "")
	c := f.config(t, nil)
	if c.AutoCompleteOnDone || c.StaleDays != 0 {
		t.Fatalf("defaults: %+v", c)
	}
	on := f.config(t, func(m map[string]any) { m["autoCompleteOnDone"] = true; m["staleDays"] = 14 })
	if !on.AutoCompleteOnDone || on.StaleDays != 14 || on.Digest() == c.Digest() {
		t.Fatalf("set: %+v", on)
	}
	if off := f.config(t, func(m map[string]any) { m["autoCompleteOnDone"] = false }); off.Digest() != c.Digest() {
		t.Fatal("explicit false changed the digest")
	}
	for _, bad := range []any{-1, MaxStaleDays + 1} {
		if _, err := Parse(f.ledger(func(m map[string]any) { m["staleDays"] = bad })); err == nil {
			t.Fatalf("staleDays %v accepted", bad)
		}
	}
}
