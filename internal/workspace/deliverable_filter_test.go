package workspace

// RHZ-089 FR-RHZ-120: GET /v1/workspace deliverable filter query —
// deliverableKind (exact), deliverableGoal / deliverableMission (exact
// binding match, ID or RHZ-073 handle), deliverableOrder (asc = today's
// order, desc = newest registration first by journal seq), deliverableLimit
// (> 0, applied after ordering; 0/negative/non-number → 400). Only
// deliverables[] changes; no parameter → byte-identical to the unfiltered
// body; independent of ?assignee=; the SSE stream ignores the parameters;
// GET never writes. F1–F6/W1.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rhizome/internal/domain"
	"rhizome/internal/events"
)

// fixture089 registers five deliverables in a known order and returns the
// store plus their ids in REGISTRATION order:
//
//	d[0] mission-a   mmx-turn  (mission-bound, goal-mission-a's mission)
//	d[1] goal-mission-a mmx-turn (goal-bound)
//	d[2] mission-a   code
//	d[3] mission-b   mmx-turn
//	d[4] goal-mission-b record   (goal-bound)
func fixture089(t *testing.T) (*events.Store, []string) {
	t.Helper()
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady, domain.MissionRunning)
	missionIn062(t, s, "mission-b", domain.MissionReady, domain.MissionRunning)
	type reg struct{ mission, goal, kind, summary string }
	regs := []reg{
		{"mission-a", "", "mmx-turn", "turn 1"},
		{"", "goal-mission-a", "mmx-turn", "turn 2"},
		{"mission-a", "", "code", "patch"},
		{"mission-b", "", "mmx-turn", "turn 3"},
		{"", "goal-mission-b", "record", "rec"},
	}
	ids := []string{}
	for _, r := range regs {
		src := blob081(t, s, "blob:"+r.summary)
		res := register081(t, s, Intent{MissionID: r.mission, GoalID: r.goal, DeliverableKind: r.kind, Summary: r.summary, SourceRef: src.BlobID})
		if !res.Accepted {
			t.Fatalf("register %+v: %+v", r, res)
		}
		binding := "mission:" + r.mission
		if r.goal != "" {
			binding = "goal:" + r.goal
		}
		ids = append(ids, delivID081(binding, r.kind, r.summary, src.BlobID))
	}
	return s, ids
}

// get089 performs the GET and returns status and body (no fatal on 4xx).
func get089(t *testing.T, s events.Port, query string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace"+query, nil)
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// deliverableIDs089 returns deliverables[].id in wire order.
func deliverableIDs089(t *testing.T, body []byte) []string {
	t.Helper()
	var env struct {
		Body struct {
			Deliverables []struct {
				ID string `json:"id"`
			} `json:"deliverables"`
		} `json:"body"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, d := range env.Body.Deliverables {
		out = append(out, d.ID)
	}
	return out
}

// sections089 splits the body into its top-level raw sections plus revision.
func sections089(t *testing.T, body []byte) (json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	var env struct {
		Revision json.RawMessage            `json:"revision"`
		Body     map[string]json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	return env.Revision, env.Body
}

// sameExceptDeliverables089 asserts every top-level body section but
// deliverables (and the revision) is byte-identical between two bodies.
func sameExceptDeliverables089(t *testing.T, full, filtered []byte, query string) {
	t.Helper()
	fr, fs := sections089(t, full)
	gr, gs := sections089(t, filtered)
	if !bytes.Equal(fr, gr) {
		t.Fatalf("%s: revision changed %s → %s", query, fr, gr)
	}
	if len(fs) != len(gs) {
		t.Fatalf("%s: section key set changed", query)
	}
	for k, v := range fs {
		if k == "deliverables" {
			continue
		}
		if !bytes.Equal(v, gs[k]) {
			t.Fatalf("%s: section %q changed under deliverable filter:\n%s\n%s", query, k, v, gs[k])
		}
	}
	if _, ok := gs["deliverables"]; !ok {
		t.Fatalf("%s: deliverables section missing", query)
	}
}

func eq089(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// F1: deliverableKind narrows by exact kind, keeps today's (asc) order;
// unknown kind → [] (never null); other sections untouched.
func TestWorkspaceDeliverableKindFilterFRRHZ120(t *testing.T) {
	s, d := fixture089(t)
	full := workspaceBody080(t, s, "")
	all := deliverableIDs089(t, full)
	if len(all) != 5 {
		t.Fatalf("fixture: want 5 deliverables, got %v", all)
	}
	want := []string{}
	for _, id := range all { // asc order of the subset
		if id == d[0] || id == d[1] || id == d[3] {
			want = append(want, id)
		}
	}
	code, body := get089(t, s, "?deliverableKind=mmx-turn")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if got := deliverableIDs089(t, body); !eq089(got, want) {
		t.Fatalf("kind=mmx-turn got %v want %v", got, want)
	}
	sameExceptDeliverables089(t, full, body, "kind=mmx-turn")
	code, body = get089(t, s, "?deliverableKind=code")
	if code != 200 || !eq089(deliverableIDs089(t, body), []string{d[2]}) {
		t.Fatalf("kind=code %d %s", code, body)
	}
	code, body = get089(t, s, "?deliverableKind=nope")
	if code != 200 || !bytes.Contains(body, []byte(`"deliverables":[]`)) {
		t.Fatalf("unknown kind must give []: %d %s", code, body)
	}
	sameExceptDeliverables089(t, full, body, "kind=nope")
}

// F2: binding filters are exact — deliverableGoal matches goal-bound only
// (NOT the mission-bound deliverables of that goal's missions),
// deliverableMission matches mission-bound only; an RHZ-073 handle (g-…/
// m-…) yields the byte-identical body of the ID; both given → [] (filters
// intersect, no deliverable has two bindings); unknown binding → [].
func TestWorkspaceDeliverableBindingFilterFRRHZ120(t *testing.T) {
	s, d := fixture089(t)
	full := workspaceBody080(t, s, "")
	cases := []struct {
		query string
		want  []string // registration ids; order checked as asc subset
	}{
		{"?deliverableGoal=goal-mission-a", []string{d[1]}},
		{"?deliverableGoal=goal-mission-b", []string{d[4]}},
		{"?deliverableMission=mission-a", []string{d[0], d[2]}},
		{"?deliverableMission=mission-b", []string{d[3]}},
		{"?deliverableGoal=goal-mission-a&deliverableMission=mission-a", []string{}},
		{"?deliverableGoal=goal-nope", []string{}},
		{"?deliverableMission=mission-nope", []string{}},
		{"?deliverableGoal=mission-a", []string{}},         // a mission id is not a goal binding
		{"?deliverableMission=goal-mission-a", []string{}}, // and vice versa
	}
	all := deliverableIDs089(t, full)
	for _, c := range cases {
		want := []string{}
		for _, id := range all {
			for _, w := range c.want {
				if id == w {
					want = append(want, id)
				}
			}
		}
		code, body := get089(t, s, c.query)
		if code != 200 {
			t.Fatalf("%s: %d %s", c.query, code, body)
		}
		if got := deliverableIDs089(t, body); !eq089(got, want) {
			t.Fatalf("%s: got %v want %v", c.query, got, want)
		}
		if len(want) == 0 && !bytes.Contains(body, []byte(`"deliverables":[]`)) {
			t.Fatalf("%s: empty must be [] not null: %s", c.query, body)
		}
		sameExceptDeliverables089(t, full, body, c.query)
	}
	// handle resolution through the RHZ-073 index.
	gh, mh := handleFor("g", "goal-mission-a", handlePrefixLen), handleFor("m", "mission-a", handlePrefixLen)
	if !bytes.Contains(full, []byte(`"handle":"`+gh+`"`)) || !bytes.Contains(full, []byte(`"handle":"`+mh+`"`)) {
		t.Fatalf("handle fixture missing from body")
	}
	_, byID := get089(t, s, "?deliverableGoal=goal-mission-a")
	_, byHandle := get089(t, s, "?deliverableGoal="+gh)
	if !bytes.Equal(byID, byHandle) || !eq089(deliverableIDs089(t, byHandle), []string{d[1]}) {
		t.Fatalf("goal handle must resolve to the id body:\n%s\n%s", byID, byHandle)
	}
	_, byID = get089(t, s, "?deliverableMission=mission-a")
	_, byHandle = get089(t, s, "?deliverableMission="+mh)
	if !bytes.Equal(byID, byHandle) || len(deliverableIDs089(t, byHandle)) != 2 {
		t.Fatalf("mission handle must resolve to the id body:\n%s\n%s", byID, byHandle)
	}
}

// F3: deliverableOrder=desc is newest registration first (journal seq of
// the first deliverable event), deterministic across calls; asc (explicit)
// is byte-identical to no parameter, i.e. today's order.
func TestWorkspaceDeliverableOrderFRRHZ120(t *testing.T) {
	s, d := fixture089(t)
	full := workspaceBody080(t, s, "")
	code, body := get089(t, s, "?deliverableOrder=asc")
	if code != 200 || !bytes.Equal(body, full) {
		t.Fatalf("asc must equal the unfiltered body (%d)", code)
	}
	code, body = get089(t, s, "?deliverableOrder=desc")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := []string{d[4], d[3], d[2], d[1], d[0]}
	if got := deliverableIDs089(t, body); !eq089(got, want) {
		t.Fatalf("desc got %v want %v", got, want)
	}
	sameExceptDeliverables089(t, full, body, "order=desc")
	if _, again := get089(t, s, "?deliverableOrder=desc"); !bytes.Equal(again, body) {
		t.Fatal("desc body not deterministic")
	}
	// Sanity: the fixture's asc (by id) order is not already reverse
	// registration order, so desc is a real reordering and not a coincidence.
	if eq089(deliverableIDs089(t, full), want) {
		t.Fatal("fixture degenerate: asc order equals reverse registration order; change a summary")
	}
	// desc composes with a filter (filter → order): kind=mmx-turn newest first.
	_, body = get089(t, s, "?deliverableKind=mmx-turn&deliverableOrder=desc")
	if got := deliverableIDs089(t, body); !eq089(got, []string{d[3], d[1], d[0]}) {
		t.Fatalf("kind+desc got %v", got)
	}
}

// F4: deliverableLimit keeps the first n AFTER ordering (desc+limit = the
// newest n; asc+limit = the first n of today's order); larger than the
// list is a no-op; 0, negative, non-number and a bad order → 400 with the
// journal unchanged.
func TestWorkspaceDeliverableLimitFRRHZ120(t *testing.T) {
	s, d := fixture089(t)
	full := workspaceBody080(t, s, "")
	asc := deliverableIDs089(t, full)
	before := journalBytes069(t, s)
	code, body := get089(t, s, "?deliverableOrder=desc&deliverableLimit=2")
	if code != 200 || !eq089(deliverableIDs089(t, body), []string{d[4], d[3]}) {
		t.Fatalf("desc limit=2 %d %v", code, deliverableIDs089(t, body))
	}
	sameExceptDeliverables089(t, full, body, "desc limit=2")
	// Sanity: the two smallest ids are not the two newest registrations, so a
	// limit-before-order implementation cannot pass by coincidence.
	if (asc[0] == d[4] || asc[0] == d[3]) && (asc[1] == d[4] || asc[1] == d[3]) {
		t.Fatal("fixture degenerate: first two asc ids are the newest two; change a summary")
	}
	_, body = get089(t, s, "?deliverableLimit=2")
	if !eq089(deliverableIDs089(t, body), asc[:2]) {
		t.Fatalf("asc limit=2 %v", deliverableIDs089(t, body))
	}
	_, body = get089(t, s, "?deliverableLimit=1&deliverableOrder=desc&deliverableKind=mmx-turn")
	if !eq089(deliverableIDs089(t, body), []string{d[3]}) {
		t.Fatalf("filter→order→limit %v", deliverableIDs089(t, body))
	}
	_, body = get089(t, s, "?deliverableLimit=99")
	if !bytes.Equal(body, full) {
		t.Fatal("limit beyond the list must be a no-op")
	}
	for _, bad := range []string{"?deliverableLimit=0", "?deliverableLimit=-1", "?deliverableLimit=abc", "?deliverableLimit=1.5", "?deliverableLimit=2&deliverableOrder=sideways", "?deliverableOrder=DESC"} {
		code, body := get089(t, s, bad)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", bad, code, body)
		}
		if bytes.Contains(body, []byte(`"deliverables"`)) {
			t.Fatalf("%s: 400 must not carry a body projection: %s", bad, body)
		}
	}
	if journalBytes069(t, s) != before {
		t.Fatal("GET changed the journal")
	}
}

// F5: no parameter → byte-identical to the DTO path without any filter
// (golden); under any filter combination, every section but deliverables
// (missions, tasks, gates, edges, counts, attention, capabilities,
// gateCapabilities) and the revision are byte-identical to the unfiltered
// body — counts stay global facts.
func TestWorkspaceDeliverableFilterAdditiveFRRHZ120(t *testing.T) {
	s, _ := fixture089(t)
	p, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := json.Marshal(envelope{p.Revision, toDTO(p)})
	if err != nil {
		t.Fatal(err)
	}
	full := workspaceBody080(t, s, "")
	if !bytes.Equal(bytes.TrimSpace(full), golden) {
		t.Fatalf("no-parameter body must equal the unfiltered DTO:\n%s\n%s", full, golden)
	}
	_, keys := sections089(t, full)
	for _, k := range []string{"missions", "tasks", "gates", "deliverables", "edges", "counts", "attention", "capabilities", "gateCapabilities"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("section %q missing", k)
		}
	}
	for _, q := range []string{
		"?deliverableKind=mmx-turn",
		"?deliverableKind=mmx-turn&deliverableOrder=desc&deliverableLimit=1",
		"?deliverableGoal=goal-mission-a&deliverableOrder=desc",
		"?deliverableMission=mission-b&deliverableLimit=1",
		"?deliverableKind=nope",
		"?deliverableOrder=desc",
		"?deliverableLimit=1",
	} {
		code, body := get089(t, s, q)
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, body)
		}
		sameExceptDeliverables089(t, full, body, q)
		if bytes.Equal(body, full) {
			// every query above changes deliverables[] (subset, reorder or cut)
			t.Fatalf("%s: body unexpectedly unchanged", q)
		}
	}
}

// F6: ?assignee= and deliverable* are independent — combined, tasks[]
// equals the assignee-only body's tasks and deliverables[] equals the
// deliverable-only body's deliverables; everything else is the full body.
// The SSE stream ignores the deliverable parameters (snapshot = full body).
func TestWorkspaceDeliverableFilterWithAssigneeFRRHZ120(t *testing.T) {
	s, d := fixture089(t)
	if res := assign080(t, s, "mission-a", "alice"); !res.Accepted {
		t.Fatalf("assign: %+v", res)
	}
	full := workspaceBody080(t, s, "")
	_, onlyA := get089(t, s, "?assignee=alice")
	_, onlyD := get089(t, s, "?deliverableKind=code&deliverableOrder=desc")
	code, both := get089(t, s, "?assignee=alice&deliverableKind=code&deliverableOrder=desc")
	if code != 200 {
		t.Fatalf("%d %s", code, both)
	}
	_, fs := sections089(t, full)
	_, as := sections089(t, onlyA)
	_, ds := sections089(t, onlyD)
	_, bs := sections089(t, both)
	if !bytes.Equal(bs["tasks"], as["tasks"]) || bytes.Equal(bs["tasks"], fs["tasks"]) || len(tasks080(t, both)) != 1 {
		t.Fatalf("tasks must be the assignee-filtered list:\n%s\n%s", bs["tasks"], as["tasks"])
	}
	if !bytes.Equal(bs["deliverables"], ds["deliverables"]) || !eq089(deliverableIDs089(t, both), []string{d[2]}) {
		t.Fatalf("deliverables must be the deliverable-filtered list:\n%s\n%s", bs["deliverables"], ds["deliverables"])
	}
	if !bytes.Equal(as["deliverables"], fs["deliverables"]) {
		t.Fatal("assignee alone must not touch deliverables")
	}
	if !bytes.Equal(ds["tasks"], fs["tasks"]) {
		t.Fatal("deliverable filter alone must not touch tasks")
	}
	for k, v := range fs {
		if k == "tasks" || k == "deliverables" {
			continue
		}
		if !bytes.Equal(v, bs[k]) {
			t.Fatalf("section %q changed under combined filter", k)
		}
	}
	// SSE: the snapshot frame is the full body regardless of the parameters.
	req := httptest.NewRequest(http.MethodGet, "/v1/workspace/stream?deliverableKind=code&deliverableOrder=desc&deliverableLimit=1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	cancel()
	NewHTTP(s).Handler().ServeHTTP(rec, req)
	frame := rec.Body.String()
	if !strings.HasPrefix(frame, "event: snapshot\ndata: ") || !strings.Contains(frame, strings.TrimSpace(string(full))) {
		t.Fatalf("stream frame must be the unfiltered snapshot:\n%s", frame)
	}
}

// W1: no GET /v1/workspace variant writes — the journal is byte-identical
// after every accepted and rejected query, and a replayed copy of the same
// journal yields the same filtered bytes (determinism from events alone).
func TestWorkspaceDeliverableFilterNoWritesFRRHZ120(t *testing.T) {
	s, _ := fixture089(t)
	before := journalBytes069(t, s)
	n := len(s.All())
	queries := []string{"", "?deliverableKind=mmx-turn", "?deliverableGoal=goal-mission-a", "?deliverableMission=" + handleFor("m", "mission-a", handlePrefixLen), "?deliverableOrder=desc&deliverableLimit=2", "?deliverableLimit=0", "?deliverableOrder=x"}
	bodies := map[string][]byte{}
	for _, q := range queries {
		_, bodies[q] = get089(t, s, q)
	}
	if journalBytes069(t, s) != before || len(s.All()) != n {
		t.Fatal("GET /v1/workspace wrote to the journal")
	}
	copyStore := &events.Store{}
	for _, e := range s.All() {
		if err := copyStore.AppendRevision(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range queries {
		if _, b := get089(t, copyStore, q); !bytes.Equal(b, bodies[q]) {
			t.Fatalf("%s: replayed journal gives different bytes", q)
		}
	}
}
