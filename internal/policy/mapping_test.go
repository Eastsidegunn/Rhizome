package policy

import "testing"

func TestMergeNewAxesFRRHZ058(t *testing.T) {
	a := Policy{Capabilities: []string{"fs:workspace", "net:egress"}, Domains: []string{"Example.COM", "a.test"}, Budget: 100, Timeout: 200, MaxDepth: 4, Units: "tokens-ms-v1"}
	b := Policy{Capabilities: []string{"net:egress", "fs:workspace"}, Domains: []string{"example.com", "b.test"}, Budget: 50, Timeout: 300, MaxDepth: 2, Units: "tokens-ms-v1"}
	r := Merge(a, b)
	if r.Invalid || len(r.Policy.Domains) != 1 || r.Policy.Domains[0] != "example.com" || r.Policy.MaxDepth != 2 || r.Policy.Budget != 50 || r.Policy.Timeout != 200 || r.Policy.Units != "tokens-ms-v1" {
		t.Fatalf("merge=%+v", r)
	}
	if len(Merge(Policy{Capabilities: []string{"x"}}, Policy{Capabilities: []string{"x"}}).Policy.Domains) != 0 {
		t.Fatal("unexpected domains")
	}
	if Merge(Policy{Capabilities: []string{"x"}, MaxDepth: 3}, Policy{Capabilities: []string{"x"}}).Policy.MaxDepth != 3 {
		t.Fatal("one-sided max depth lost")
	}
	if Merge(Policy{Capabilities: []string{"x"}, MaxDepth: 3}, Policy{Capabilities: []string{"x"}, MaxDepth: 1}).Policy.MaxDepth != 1 {
		t.Fatal("max depth not narrowed")
	}
	if Merge(Policy{Capabilities: []string{"x"}, Units: "tokens-ms-v1"}, Policy{Capabilities: []string{"x"}}).Policy.Units != "" {
		t.Fatal("legacy units inferred")
	}
}

func TestMapToJanusFRRHZ059(t *testing.T) {
	p := Policy{Capabilities: []string{"net:egress", "fs:workspace", "ext:use"}, Domains: []string{"Example.COM"}, Budget: 10, Timeout: 20, MaxDepth: 2, Units: "tokens-ms-v1"}
	j, err := MapToJanus(p)
	if err != nil {
		t.Fatal(err)
	}
	if j.MappingVersion != MappingVersion || len(j.FSScope) != 1 || j.FSScope[0] != "workspace" || len(j.EgressDomains) != 1 || j.EgressDomains[0] != "example.com" || j.Budget.Tokens != 10 || j.Budget.TimeMS != 20 || j.Budget.MaxDepth != 2 {
		t.Fatalf("janus=%+v", j)
	}
	rebuilt, err := Reconstruct(j, p.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Budget != p.Budget || rebuilt.Timeout != p.Timeout || rebuilt.MaxDepth != p.MaxDepth || rebuilt.Units != p.Units || len(rebuilt.Domains) != 1 || rebuilt.Domains[0] != "example.com" {
		t.Fatalf("reconstructed=%+v", rebuilt)
	}
	for _, bad := range []Policy{
		{Capabilities: []string{"fs:workspace"}, Domains: []string{"x"}, Budget: 1, Timeout: 1, MaxDepth: 1},
		{Capabilities: []string{"fs:workspace"}, Domains: []string{"x"}, Budget: 1, Timeout: 1, MaxDepth: 1, Units: "bad"},
		{Capabilities: []string{"fs:workspace"}, Domains: []string{"x"}, Budget: 1, Timeout: 1, MaxDepth: 1, Units: "tokens-ms-v1", Unlimited: true},
		{Capabilities: []string{"fs:workspace"}, Domains: []string{"x"}, Budget: 1, Timeout: 0, MaxDepth: 1, Units: "tokens-ms-v1"},
		{Capabilities: []string{"fs:workspace"}, Domains: []string{"x"}, Budget: 1, Timeout: 1, MaxDepth: 0, Units: "tokens-ms-v1"},
		{Capabilities: []string{"unknown"}, Domains: []string{"x"}, Budget: 1, Timeout: 1, MaxDepth: 1, Units: "tokens-ms-v1"},
		{Capabilities: []string{"net:egress"}, Budget: 1, Timeout: 1, MaxDepth: 1, Units: "tokens-ms-v1"},
	} {
		if _, err := MapToJanus(bad); err == nil {
			t.Fatalf("invalid policy accepted: %+v", bad)
		}
	}
}

func TestMergeDoesNotMutateNewSlicesFRRHZ058(t *testing.T) {
	a := Policy{Capabilities: []string{"x"}, Domains: []string{"A.COM"}, Budget: 1, Timeout: 1}
	b := Policy{Capabilities: []string{"x"}, Domains: []string{"a.com"}, Budget: 1, Timeout: 1}
	_ = Merge(a, b)
	if a.Domains[0] != "A.COM" || b.Domains[0] != "a.com" {
		t.Fatal("domains mutated")
	}
}
