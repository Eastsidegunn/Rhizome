package sessionlauncher

// RHZ-107 G1 (FR-RHZ-140): the session-launcher backend of mission.start
// records refs as provenance requestedRefs — an approval record (not
// enforced). Its ledger has no refCeiling, so authorizedRefs stays empty;
// refs never feed the key or the child's argv, and malformed refs get the
// fixed X7 reason with zero writes and no spawn.

import (
	"strings"
	"testing"
)

func TestLauncherRefsRecordFRRHZ140(t *testing.T) {
	a, b := "sref-"+strings.Repeat("a", 24), "sref-"+strings.Repeat("b", 24)
	f := newFake(t, blockBody)
	s := launchStore(t)
	l := newLauncher(s, f.config(t, func(m map[string]any) { m["maxConcurrent"] = 3 }), &outcomes{})
	before := len(s.All())
	for name, refs := range map[string][]string{"format": {"sref-x"}, "duplicate": {a, a}, "upper": {strings.ToUpper(a)}} {
		req := StartRequest{MissionID: "m1", Instruction: "x", Refs: refs}
		if _, reason, err := l.Prepare(req); err != nil || reason != "secret intent: invalid ref" {
			t.Errorf("%s prepare: %q %v", name, reason, err)
		}
		if _, reason, err := l.Start(req); err != nil || reason != "secret intent: invalid ref" {
			t.Errorf("%s start: %q %v", name, reason, err)
		}
	}
	if len(s.All()) != before || runs(f) != 0 {
		t.Fatalf("writes=%d runs=%d", len(s.All())-before, runs(f))
	}
	plain, _, _ := l.Prepare(StartRequest{MissionID: "m1", Instruction: "x"})
	id, reason, err := l.Start(StartRequest{MissionID: "m1", Instruction: "x", Refs: []string{b, a}})
	if err != nil || reason != "" || id != plain.ExecutionID {
		t.Fatalf("%s %q %v (refs must not feed the key)", id, reason, err)
	}
	p := ref(t, s, id).Provenance
	if p == nil || strings.Join(p.RequestedRefs, ",") != a+","+b || p.AuthorizedRefs != nil {
		t.Fatalf("%+v", p)
	}
	waitRuns(t, f, 1)
	if args := strings.Join(readArgs(t, f), " "); strings.Contains(args, "sref-") {
		t.Fatalf("refs reached the child argv: %q", args)
	}
	f.release(t)
	l.Wait()
}
