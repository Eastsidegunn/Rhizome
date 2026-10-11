package main

import (
	"strings"
	"testing"
)

// FR-RHZ-182: every command line of the top-level usage keeps the
// pre-RHZ-136 column — two spaces, the name padded to ten, then the text —
// so the help line reads exactly as it did at 7381442.
func TestUsageColumnsFRRHZ182(t *testing.T) {
	if !strings.Contains(rhizomeUsage, "\n  help      print this message (also -h, --help)\n") {
		t.Fatalf("help line changed:\n%s", rhizomeUsage)
	}
	_, cmds, _ := strings.Cut(rhizomeUsage, "commands:\n")
	for _, l := range strings.Split(strings.TrimSuffix(cmds, "\n"), "\n") {
		if len(l) < 13 || l[:2] != "  " || l[11] != ' ' || l[12] == ' ' || strings.TrimSpace(l[2:12]) != strings.TrimRight(l[2:12], " ") {
			t.Fatalf("misaligned usage line %q", l)
		}
	}
}
