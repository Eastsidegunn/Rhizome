package main

// RHZ-135 (FR-RHZ-177): rhizome and rhizome agent print their usage. Asked-for
// help (-h, --help, help) goes to stdout with exit 0; no command keeps exit 2
// with the usage on stderr.

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpPrintsUsageFRRHZ177(t *testing.T) {
	top := []string{"serve", "agent", "ingest", "memories", "gate", "index", "graph", "secret"}
	verbs := []string{"context", "start", "progress", "blocked", "done", "usage", "note", "ask"}
	for _, c := range []struct {
		args  []string
		words []string
	}{
		{[]string{"-h"}, top}, {[]string{"--help"}, top}, {[]string{"help"}, top},
		{[]string{"agent", "-h"}, verbs}, {[]string{"agent", "--help"}, verbs}, {[]string{"agent", "help"}, verbs},
	} {
		var out, errOut bytes.Buffer
		if code := run(c.args, &out, &errOut); code != 0 || errOut.Len() != 0 {
			t.Fatalf("%v: exit=%d stderr=%q", c.args, code, errOut.String())
		}
		for _, w := range c.words {
			if !strings.Contains(out.String(), "  "+w+" ") {
				t.Fatalf("%v: usage lacks %q:\n%s", c.args, w, out.String())
			}
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, rhizomeUsage}, {[]string{"agent"}, agentUsage},
		{[]string{"nope"}, "unknown command \"nope\"\n\n" + rhizomeUsage},
	} {
		var out, errOut bytes.Buffer
		if code := run(c.args, &out, &errOut); code != 2 || out.Len() != 0 || errOut.String() != c.want {
			t.Fatalf("%v: exit=%d stdout=%q stderr=%q", c.args, code, out.String(), errOut.String())
		}
	}
}
