package policy

import (
	"sort"
	"strings"
)

type Policy struct {
	Capabilities    []string
	Domains         []string
	Budget, Timeout int64
	MaxDepth        int64
	Units           string
	Unlimited       bool
}
type Result struct {
	Policy  Policy
	Empty   bool
	Invalid bool
}

func Merge(a, b Policy) Result {
	if !Valid(a) || !Valid(b) {
		return Result{Invalid: true}
	}
	set := map[string]bool{}
	for _, x := range a.Capabilities {
		set[x] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, x := range b.Capabilities {
		if set[x] && !seen[x] {
			out = append(out, x)
			seen[x] = true
		}
	}
	sort.Strings(out)
	r := Policy{Capabilities: out, Domains: intersectDomains(a.Domains, b.Domains)}
	if a.Unlimited {
		r.Budget = b.Budget
	} else if b.Unlimited {
		r.Budget = a.Budget
	} else if a.Budget < b.Budget {
		r.Budget = a.Budget
	} else {
		r.Budget = b.Budget
	}
	if a.Timeout == 0 {
		r.Timeout = b.Timeout
	} else if b.Timeout == 0 {
		r.Timeout = a.Timeout
	} else if a.Timeout < b.Timeout {
		r.Timeout = a.Timeout
	} else {
		r.Timeout = b.Timeout
	}
	if a.MaxDepth == 0 {
		r.MaxDepth = b.MaxDepth
	} else if b.MaxDepth == 0 {
		r.MaxDepth = a.MaxDepth
	} else if a.MaxDepth < b.MaxDepth {
		r.MaxDepth = a.MaxDepth
	} else {
		r.MaxDepth = b.MaxDepth
	}
	if a.Units != "" && a.Units == b.Units {
		r.Units = a.Units
	}
	r.Unlimited = a.Unlimited && b.Unlimited
	return Result{Policy: r, Empty: len(out) == 0}
}

func Valid(p Policy) bool { return p.Budget >= 0 && p.Timeout >= 0 && p.MaxDepth >= 0 }

func intersectDomains(a, b []string) []string {
	set := map[string]bool{}
	for _, d := range a {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			set[d] = true
		}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, d := range b {
		d = strings.ToLower(strings.TrimSpace(d))
		if set[d] && !seen[d] {
			out = append(out, d)
			seen[d] = true
		}
	}
	sort.Strings(out)
	return out
}
