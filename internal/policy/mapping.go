package policy

import (
	"fmt"
	"sort"
	"strings"
)

const MappingVersion = "janus-map-v1"

type JanusBudget struct{ Tokens, TimeMS, MaxDepth int64 }
type JanusPolicy struct {
	FSScope, EgressDomains, AllowExtensions []string
	Budget                                  JanusBudget
	MappingVersion                          string
}

type capabilityMapping struct{ fs, egress, extension string }

var capabilityTable = map[string]capabilityMapping{
	"fs:workspace": {fs: "workspace"},
	"net:egress":   {egress: "egress"},
	"ext:use":      {extension: "extensions"},
}

func MapToJanus(p Policy) (JanusPolicy, error) {
	if p.Unlimited {
		return JanusPolicy{}, fmt.Errorf("unlimited policy cannot be mapped")
	}
	if p.Units != "tokens-ms-v1" {
		return JanusPolicy{}, fmt.Errorf("unsupported or missing units")
	}
	if p.Timeout == 0 {
		return JanusPolicy{}, fmt.Errorf("timeout must be finite")
	}
	if p.MaxDepth == 0 {
		return JanusPolicy{}, fmt.Errorf("max depth must be specified")
	}
	if !Valid(p) {
		return JanusPolicy{}, fmt.Errorf("invalid policy limits")
	}
	result := JanusPolicy{MappingVersion: MappingVersion, Budget: JanusBudget{Tokens: p.Budget, TimeMS: p.Timeout, MaxDepth: p.MaxDepth}}
	for _, raw := range p.Capabilities {
		cap := strings.ToLower(strings.TrimSpace(raw))
		m, ok := capabilityTable[cap]
		if !ok {
			return JanusPolicy{}, fmt.Errorf("unmapped capability %q", raw)
		}
		if m.fs != "" {
			result.FSScope = append(result.FSScope, m.fs)
		}
		if m.egress != "" {
			if len(p.Domains) == 0 {
				return JanusPolicy{}, fmt.Errorf("egress domains required")
			}
			result.EgressDomains = normalize(p.Domains)
		}
		if m.extension != "" {
			result.AllowExtensions = append(result.AllowExtensions, m.extension)
		}
	}
	result.FSScope = uniqueSorted(result.FSScope)
	result.EgressDomains = uniqueSorted(result.EgressDomains)
	result.AllowExtensions = uniqueSorted(result.AllowExtensions)
	return result, nil
}

// Reconstruct restores the policy axes represented by JanusPolicy. The caller
// supplies the original capability vocabulary because JANUS stores axes, not
// Rhizome capability labels.
func Reconstruct(j JanusPolicy, capabilities []string) (Policy, error) {
	if j.MappingVersion != MappingVersion {
		return Policy{}, fmt.Errorf("unsupported mapping version")
	}
	if len(capabilities) == 0 {
		return Policy{}, fmt.Errorf("capabilities required")
	}
	for _, c := range capabilities {
		if _, ok := capabilityTable[strings.ToLower(strings.TrimSpace(c))]; !ok {
			return Policy{}, fmt.Errorf("unmapped capability %q", c)
		}
	}
	p := Policy{Capabilities: uniqueSorted(normalize(capabilities)), Domains: uniqueSorted(j.EgressDomains), Budget: j.Budget.Tokens, Timeout: j.Budget.TimeMS, MaxDepth: j.Budget.MaxDepth, Units: "tokens-ms-v1"}
	return p, nil
}

func normalize(in []string) []string {
	out := []string{}
	for _, x := range in {
		x = strings.ToLower(strings.TrimSpace(x))
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}
func uniqueSorted(in []string) []string {
	out := normalize(in)
	sort.Strings(out)
	r := []string{}
	for _, x := range out {
		if len(r) == 0 || r[len(r)-1] != x {
			r = append(r, x)
		}
	}
	return r
}
