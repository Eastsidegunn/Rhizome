package workspace

// RHZ-081 FR-RHZ-112: deliverable.register relay — mission- or goal-bound
// (exactly one), deliverable.declared + produces edge.declared through the
// kernel writers only, deterministic content-derived ID, empty sourceRef →
// summary registered as the source, rejections with a byte-unchanged
// journal, idempotent re-register (zero writes) with edge repair, legacy
// payload (no GoalID) round trip over a real NDJSON journal, and the
// single-writer pin on the relay file. D1/D1'/D1''/D2/D3/D4/R1/W1.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"rhizome/internal/deliverable"
	"rhizome/internal/domain"
	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/source"
)

// delivID081 recomputes the FR-RHZ-112 ID scheme independently of the relay:
// deliv-<sha256("mission:"+id | "goal:"+id ‖ NUL ‖ kind ‖ NUL ‖ summary ‖ NUL ‖ sourceRef)[:12]>.
func delivID081(binding, kind, summary, ref string) string {
	h := sha256.Sum256([]byte(binding + "\x00" + kind + "\x00" + summary + "\x00" + ref))
	return "deliv-" + hex.EncodeToString(h[:])[:12]
}

func register081(t *testing.T, s events.Port, in Intent) RelayResult {
	t.Helper()
	in.Kind = "deliverable.register"
	res, err := RelayIntent(s, in, "tester", true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func blob081(t *testing.T, s events.Port, content string) source.SourceRef {
	t.Helper()
	src, err := source.Service{Store: s}.Register([]byte(content), "text/plain", "test://"+content)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

type section081 struct {
	Body struct {
		Deliverables []map[string]any `json:"deliverables"`
		Edges        []map[string]any `json:"edges"`
	} `json:"body"`
}

func sections081(t *testing.T, body []byte) (map[string]map[string]any, map[string]map[string]any) {
	t.Helper()
	var env section081
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	ds, es := map[string]map[string]any{}, map[string]map[string]any{}
	for _, d := range env.Body.Deliverables {
		ds[d["id"].(string)] = d
	}
	for _, e := range env.Body.Edges {
		es[e["id"].(string)] = e
	}
	return ds, es
}

type edgePayload081 struct {
	ID          string
	From, To    struct{ Type, ID string }
	Kind        string
	Correlation string
}

func edgeEvent081(t *testing.T, e events.Event) edgePayload081 {
	t.Helper()
	var p edgePayload081
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// D1: mission-bound register → exactly deliverable.declared + edge.declared
// (produces mission→deliverable, relay correlation), ID per scheme, payload
// without a GoalID key, /v1/workspace deliverables[] (pre-081 key set, no
// goalId) and edges[]; missionId may be an RHZ-073 handle; same summary with
// a different blob is a distinct deliverable (ID salts sourceRef).
func TestDeliverableRegisterMissionBoundFRRHZ112(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady, domain.MissionRunning)
	src := blob081(t, s, "artifact-1")
	n := len(s.All())
	res := register081(t, s, Intent{MissionID: "mission-a", DeliverableKind: "code", Summary: "patch v1", SourceRef: src.BlobID, CorrelationID: "corr-1"})
	if !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	all := s.All()
	if len(all) != n+2 {
		t.Fatalf("journal grew %d, want 2", len(all)-n)
	}
	id := delivID081("mission:mission-a", "code", "patch v1", src.BlobID)
	dev, eev := all[n], all[n+1]
	if dev.Type != "deliverable.declared" || dev.AggregateID != id || dev.Revision != 1 {
		t.Fatalf("deliverable event %+v", dev)
	}
	if bytes.Contains(dev.Payload, []byte(`"GoalID"`)) {
		t.Fatalf("mission-bound payload must omit GoalID: %s", dev.Payload)
	}
	wantEdge := "edge-produces-mission-a-" + id
	if eev.Type != "edge.declared" || eev.AggregateID != wantEdge {
		t.Fatalf("edge event %+v", eev)
	}
	ep := edgeEvent081(t, eev)
	if ep.From.Type != "mission" || ep.From.ID != "mission-a" || ep.To.Type != "deliverable" || ep.To.ID != id || ep.Kind != "produces" || ep.Correlation != "corr-1" {
		t.Fatalf("edge payload %+v", ep)
	}
	d, err := (deliverable.Service{Store: s}).Get(id)
	if err != nil || d.MissionID != "mission-a" || d.GoalID != "" || d.Kind != "code" || d.SourceRef != src.BlobID || d.Summary != "patch v1" {
		t.Fatalf("get %+v err=%v", d, err)
	}
	body := workspaceBody080(t, s, "")
	ds, es := sections081(t, body)
	got := ds[id]
	if got == nil {
		t.Fatalf("deliverable missing from workspace: %s", body)
	}
	if ks := keys080(got); strings.Join(ks, ",") != "handle,id,kind,missionId,sourceRef,summary" {
		t.Fatalf("key set %v", ks)
	}
	if got["kind"] != "code" || got["missionId"] != "mission-a" || got["sourceRef"] != src.BlobID || got["summary"] != "patch v1" || got["handle"] != handleFor("d", id, handlePrefixLen) {
		t.Fatalf("deliverable dto %v", got)
	}
	e := es[wantEdge]
	if e == nil || e["edgeKind"] != "produces" || e["from"].(map[string]any)["type"] != "mission" || e["from"].(map[string]any)["id"] != "mission-a" || e["to"].(map[string]any)["id"] != id {
		t.Fatalf("edge dto %v", e)
	}
	// handle input (RHZ-073): the stored binding is the ID.
	src2 := blob081(t, s, "artifact-2")
	res = register081(t, s, Intent{MissionID: handleFor("m", "mission-a", handlePrefixLen), DeliverableKind: "code", Summary: "patch v1", SourceRef: src2.BlobID})
	if !res.Accepted {
		t.Fatalf("handle: %+v", res)
	}
	id2 := delivID081("mission:mission-a", "code", "patch v1", src2.BlobID)
	if d2, err := (deliverable.Service{Store: s}).Get(id2); err != nil || d2.MissionID != "mission-a" {
		t.Fatalf("handle-registered %+v err=%v", d2, err)
	}
	if ds, _ = sections081(t, workspaceBody080(t, s, "")); len(ds) != 2 {
		t.Fatalf("want 2 deliverables, got %d", len(ds))
	}
}

// D1': goal-bound register → payload carries GoalID and an empty MissionID,
// produces edge from the goal, DTO goalId trails handle (additive suffix),
// missionId "", kernel Get replays the goal; a mission-bound-only body has
// no goalId key anywhere.
func TestDeliverableRegisterGoalBoundFRRHZ112(t *testing.T) {
	s := goals066(t, "goal-x")
	src := blob081(t, s, "artifact-g")
	n := len(s.All())
	res := register081(t, s, Intent{GoalID: "goal-x", DeliverableKind: "record", Summary: "goal record", SourceRef: src.BlobID})
	if !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	all := s.All()
	if len(all) != n+2 {
		t.Fatalf("journal grew %d, want 2", len(all)-n)
	}
	id := delivID081("goal:goal-x", "record", "goal record", src.BlobID)
	var p struct{ ID, MissionID, GoalID string }
	if err := json.Unmarshal(all[n].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if all[n].Type != "deliverable.declared" || p.ID != id || p.GoalID != "goal-x" || p.MissionID != "" || !bytes.Contains(all[n].Payload, []byte(`"GoalID":"goal-x"`)) {
		t.Fatalf("payload %s", all[n].Payload)
	}
	ep := edgeEvent081(t, all[n+1])
	if all[n+1].AggregateID != "edge-produces-goal-x-"+id || ep.From.Type != "goal" || ep.From.ID != "goal-x" || ep.To.ID != id || ep.Kind != "produces" {
		t.Fatalf("edge %+v %+v", all[n+1], ep)
	}
	if d, err := (deliverable.Service{Store: s}).Get(id); err != nil || d.GoalID != "goal-x" || d.MissionID != "" {
		t.Fatalf("get %+v err=%v", d, err)
	}
	body := workspaceBody080(t, s, "")
	ds, es := sections081(t, body)
	got := ds[id]
	if got == nil || got["goalId"] != "goal-x" || got["missionId"] != "" {
		t.Fatalf("dto %v", got)
	}
	if !bytes.Contains(body, []byte(`,"handle":"`+handleFor("d", id, handlePrefixLen)+`","goalId":"goal-x"}`)) {
		t.Fatalf("goalId must trail handle: %s", body)
	}
	if e := es["edge-produces-goal-x-"+id]; e == nil || e["from"].(map[string]any)["type"] != "goal" {
		t.Fatalf("edge dto %v", e)
	}
	// A1 pin: a mission-bound-only journal never mentions goalId.
	s2 := &events.Store{}
	missionIn062(t, s2, "mission-m")
	src2 := blob081(t, s2, "artifact-m")
	if res := register081(t, s2, Intent{MissionID: "mission-m", DeliverableKind: "code", Summary: "m", SourceRef: src2.BlobID}); !res.Accepted {
		t.Fatalf("%+v", res)
	}
	if b := workspaceBody080(t, s2, ""); bytes.Contains(b, []byte(`"goalId"`)) {
		t.Fatalf("mission-bound body must not carry goalId: %s", b)
	}
}

// D1”: empty sourceRef → the summary bytes are registered as a text/markdown
// source (intent://deliverable) before the deliverable; the deliverable's
// sourceRef (and its ID salt) is that blob id.
func TestDeliverableRegisterEmptySourceRefFRRHZ112(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady)
	n := len(s.All())
	res := register081(t, s, Intent{MissionID: "mission-a", DeliverableKind: "note", Summary: "  summary only  "})
	if !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	all := s.All()
	if len(all) != n+3 {
		t.Fatalf("journal grew %d, want 3", len(all)-n)
	}
	sum := sha256.Sum256([]byte("summary only"))
	blob := "sha256:" + hex.EncodeToString(sum[:])
	if all[n].Type != "source.registered" || all[n].AggregateID != blob {
		t.Fatalf("source event %+v", all[n])
	}
	src, err := source.Service{Store: s}.Get(blob)
	if err != nil || src.MediaType != "text/markdown" || src.SourceURI != "intent://deliverable" || src.SizeBytes != int64(len("summary only")) {
		t.Fatalf("source %+v err=%v", src, err)
	}
	id := delivID081("mission:mission-a", "note", "summary only", blob)
	if all[n+1].Type != "deliverable.declared" || all[n+1].AggregateID != id || all[n+2].Type != "edge.declared" {
		t.Fatalf("events %+v %+v", all[n+1], all[n+2])
	}
	ds, _ := sections081(t, workspaceBody080(t, s, ""))
	if got := ds[id]; got == nil || got["sourceRef"] != blob || got["summary"] != "summary only" {
		t.Fatalf("dto %v", got)
	}
}

// D2: a sha256 sourceRef registered through source.Service is carried
// verbatim; the blob stays resolvable by that id.
func TestDeliverableRegisterBlobSourceRefFRRHZ112(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady)
	src, err := source.Service{Store: s}.Register([]byte("# report\nbody"), "text/markdown", "upload://report.md")
	if err != nil {
		t.Fatal(err)
	}
	n := len(s.All())
	if res := register081(t, s, Intent{MissionID: "mission-a", DeliverableKind: "document", Summary: "report", SourceRef: src.BlobID, State: "completed"}); !res.Accepted {
		t.Fatalf("rejected: %+v", res)
	}
	if len(s.All()) != n+2 {
		t.Fatalf("journal grew %d, want 2 (no second source)", len(s.All())-n)
	}
	id := delivID081("mission:mission-a", "document", "report", src.BlobID)
	d, err := (deliverable.Service{Store: s}).Get(id)
	if err != nil || d.SourceRef != src.BlobID {
		t.Fatalf("get %+v err=%v", d, err)
	}
	if got, err := (source.Service{Store: s}).Get(d.SourceRef); err != nil || got.MediaType != "text/markdown" || got.ContentHash != strings.TrimPrefix(src.BlobID, "sha256:") {
		t.Fatalf("source by deliverable ref %+v err=%v", got, err)
	}
	// state is accepted but never stored: no "state" key on the wire.
	if b := workspaceBody080(t, s, ""); bytes.Contains(b, []byte(`"state":"completed"`)) {
		t.Fatalf("state must not be projected: %s", b)
	}
}

// D3: every rejection leaves the journal byte-identical and nothing in
// deliverables[]/edges[].
func TestDeliverableRegisterRejectedNoWriteFRRHZ112(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-live", domain.MissionReady, domain.MissionRunning)
	missionIn062(t, s, "mission-gone")
	if res := cancelMission062(t, s, "mission-gone"); !res.Accepted {
		t.Fatalf("cancel fixture: %+v", res)
	}
	missionIn062(t, s, "mission-failed", domain.MissionReady, domain.MissionRunning)
	if res, err := RelayIntent(s, Intent{Kind: "mission.fail", MissionID: "mission-failed", Reason: "x"}, "tester", true); err != nil || !res.Accepted {
		t.Fatalf("fail fixture: %+v err=%v", res, err)
	}
	ms := mission.Service{Store: s}
	for _, g := range []string{"goal-ok", "goal-failed", "goal-cancelled", "goal-achieved"} {
		if _, err := ms.CreateGoal(g, g, "done", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []Intent{{Kind: "goal.fail", GoalID: "goal-failed"}, {Kind: "goal.cancel", GoalID: "goal-cancelled"}, {Kind: "goal.resolve", GoalID: "goal-achieved"}} {
		if res, err := RelayIntent(s, step, "tester", true); err != nil || !res.Accepted {
			t.Fatalf("%+v: %+v err=%v", step, res, err)
		}
	}
	src := blob081(t, s, "ok")
	before := journalBytes069(t, s)
	ok := Intent{MissionID: "mission-live", DeliverableKind: "code", Summary: "s", SourceRef: src.BlobID}
	with := func(f func(*Intent)) Intent { in := ok; f(&in); return in }
	cases := []struct {
		name string
		in   Intent
		want string
	}{
		{"both", with(func(i *Intent) { i.GoalID = "goal-ok" }), "both missionId and goalId given"},
		{"neither", with(func(i *Intent) { i.MissionID = "" }), "missionId or goalId required"},
		{"unknown mission", with(func(i *Intent) { i.MissionID = "mission-nope" }), "mission not found"},
		{"unknown goal", with(func(i *Intent) { i.MissionID = ""; i.GoalID = "goal-nope" }), "goal not found"},
		{"cancelled mission", with(func(i *Intent) { i.MissionID = "mission-gone" }), "mission is terminal"},
		{"failed mission", with(func(i *Intent) { i.MissionID = "mission-failed" }), "mission is terminal"},
		{"failed goal", with(func(i *Intent) { i.MissionID = ""; i.GoalID = "goal-failed" }), "goal is terminal"},
		{"cancelled goal", with(func(i *Intent) { i.MissionID = ""; i.GoalID = "goal-cancelled" }), "goal is terminal"},
		{"empty kind", with(func(i *Intent) { i.DeliverableKind = " " }), "deliverableKind required"},
		{"empty summary", with(func(i *Intent) { i.Summary = "\t\n" }), "summary required"},
		{"url sourceRef", with(func(i *Intent) { i.SourceRef = "https://example.com/a.md" }), "invalid source ref"},
		{"unknown sha256", with(func(i *Intent) { i.SourceRef = "sha256:" + strings.Repeat("0", 64) }), "source not found"},
		{"unknown exec", with(func(i *Intent) { i.SourceRef = "exec-nope" }), "execution not found"},
		{"goal + exec", with(func(i *Intent) { i.MissionID = ""; i.GoalID = "goal-ok"; i.SourceRef = "exec-nope" }), "execution source needs missionId"},
		{"state bogus", with(func(i *Intent) { i.State = "bogus" }), "state not supported"},
		{"state draft", with(func(i *Intent) { i.State = "draft" }), "state not supported"},
	}
	for _, c := range cases {
		c.in.Kind = "deliverable.register"
		res, err := RelayIntent(s, c.in, "tester", true)
		if err != nil || res.Accepted || res.Reason != c.want {
			t.Fatalf("%s: %+v err=%v (want reason %q)", c.name, res, err, c.want)
		}
	}
	if after := journalBytes069(t, s); after != before {
		t.Fatal("journal changed on rejected deliverable.register")
	}
	ds, es := sections081(t, workspaceBody080(t, s, ""))
	if len(ds) != 0 || len(es) != 0 {
		t.Fatalf("deliverables %v edges %v", ds, es)
	}
	// succeeded mission / achieved goal: still accepted (post-hoc record).
	missionIn062(t, s, "mission-done", domain.MissionReady, domain.MissionRunning)
	if res, err := RelayIntent(s, Intent{Kind: "mission.complete", MissionID: "mission-done"}, "tester", true); err != nil || !res.Accepted {
		t.Fatalf("complete fixture: %+v err=%v", res, err)
	}
	if res := register081(t, s, with(func(i *Intent) { i.MissionID = "mission-done" })); !res.Accepted {
		t.Fatalf("succeeded mission: %+v", res)
	}
	if res := register081(t, s, with(func(i *Intent) { i.MissionID = ""; i.GoalID = "goal-achieved" })); !res.Accepted {
		t.Fatalf("achieved goal: %+v", res)
	}
}

// D4: identical re-register → Accepted with zero writes; a deliverable that
// exists without its produces edge (crash between the two appends, simulated
// through the kernel writer) → re-register appends exactly the edge.
func TestDeliverableRegisterIdempotentFRRHZ112(t *testing.T) {
	s := &events.Store{}
	missionIn062(t, s, "mission-a", domain.MissionReady)
	src := blob081(t, s, "a")
	in := Intent{MissionID: "mission-a", DeliverableKind: "code", Summary: "same", SourceRef: src.BlobID}
	if res := register081(t, s, in); !res.Accepted {
		t.Fatalf("first: %+v", res)
	}
	before := journalBytes069(t, s)
	if res := register081(t, s, in); !res.Accepted {
		t.Fatalf("second: %+v", res)
	}
	if journalBytes069(t, s) != before {
		t.Fatal("re-register wrote")
	}
	// empty-sourceRef variant is idempotent too (source Register is content-addressed).
	noRef := Intent{MissionID: "mission-a", DeliverableKind: "note", Summary: "n"}
	if res := register081(t, s, noRef); !res.Accepted {
		t.Fatalf("noRef first: %+v", res)
	}
	before = journalBytes069(t, s)
	if res := register081(t, s, noRef); !res.Accepted || journalBytes069(t, s) != before {
		t.Fatalf("noRef re-register wrote or rejected: %+v", res)
	}
	// edge repair.
	goalSt := goals066(t, "goal-r")
	srcR := blob081(t, goalSt, "r")
	id := delivID081("goal:goal-r", "record", "half", srcR.BlobID)
	if _, err := (deliverable.Service{Store: goalSt}).Create(deliverable.Deliverable{ID: id, Kind: "record", GoalID: "goal-r", SourceRef: srcR.BlobID, Summary: "half"}); err != nil {
		t.Fatal(err)
	}
	n := len(goalSt.All())
	if res := register081(t, goalSt, Intent{GoalID: "goal-r", DeliverableKind: "record", Summary: "half", SourceRef: srcR.BlobID}); !res.Accepted {
		t.Fatalf("repair: %+v", res)
	}
	all := goalSt.All()
	if len(all) != n+1 || all[n].Type != "edge.declared" || all[n].AggregateID != "edge-produces-goal-r-"+id {
		t.Fatalf("repair appended %d: %+v", len(all)-n, all[len(all)-1])
	}
	if _, err := (edge.Service{Store: goalSt}).Get("edge-produces-goal-r-" + id); err != nil {
		t.Fatal(err)
	}
	before = journalBytes069(t, goalSt)
	if res := register081(t, goalSt, Intent{GoalID: "goal-r", DeliverableKind: "record", Summary: "half", SourceRef: srcR.BlobID}); !res.Accepted || journalBytes069(t, goalSt) != before {
		t.Fatalf("after repair must be zero writes: %+v", res)
	}
}

// R1: real NDJSON journal — mission-bound, goal-bound, empty-sourceRef and a
// LEGACY deliverable.declared payload (no GoalID key) survive close/reopen;
// /v1/workspace byte-identical; the legacy record replays with GoalID "".
func TestDeliverableRegisterJournalRoundTripFRRHZ112(t *testing.T) {
	path := t.TempDir() + "/j.ndjson"
	j, err := journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	missionIn069(t, j, "mission-1", domain.MissionReady, domain.MissionRunning)
	if _, err := (mission.Service{Store: j}).CreateGoal("goal-r", "r", "done", ""); err != nil {
		t.Fatal(err)
	}
	src := blob081(t, j, "round")
	// legacy fixture: the pre-081 payload shape, appended as a journal fixture.
	legacy, _ := json.Marshal(struct{ ID, Kind, MissionID, SourceRef, Summary string }{"deliv-legacy", "record", "mission-1", src.BlobID, "old"})
	if err := j.Append(0, events.Event{AggregateType: "deliverable", AggregateID: "deliv-legacy", Revision: 1, Type: "deliverable.declared", Payload: legacy}); err != nil {
		t.Fatal(err)
	}
	steps := []Intent{
		{MissionID: "mission-1", DeliverableKind: "code", Summary: "m", SourceRef: src.BlobID},
		{GoalID: "goal-r", DeliverableKind: "record", Summary: "g", SourceRef: src.BlobID},
		{GoalID: "goal-r", DeliverableKind: "note", Summary: "inline"},
	}
	for _, st := range steps {
		if res := register081(t, j, st); !res.Accepted {
			t.Fatalf("%+v: %+v", st, res)
		}
	}
	types := map[string]int{}
	for _, e := range j.All() {
		types[e.Type]++
	}
	if types["deliverable.declared"] != 4 || types["edge.declared"] != 3 || types["source.registered"] != 2 {
		t.Fatalf("event mix %v", types)
	}
	body1 := workspaceBody080(t, j, "")
	ds1, es1 := sections081(t, body1)
	if len(ds1) != 4 || len(es1) != 3 {
		t.Fatalf("before restart: %d deliverables %d edges", len(ds1), len(es1))
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = journal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if body2 := workspaceBody080(t, j, ""); !bytes.Equal(body1, body2) {
		t.Fatalf("workspace diverged after restart:\n%s\n%s", body1, body2)
	}
	ds := deliverable.Service{Store: j}
	if d, err := ds.Get("deliv-legacy"); err != nil || d.GoalID != "" || d.MissionID != "mission-1" {
		t.Fatalf("legacy %+v err=%v", d, err)
	}
	if d, err := ds.Get(delivID081("goal:goal-r", "record", "g", src.BlobID)); err != nil || d.GoalID != "goal-r" || d.MissionID != "" {
		t.Fatalf("goal-bound %+v err=%v", d, err)
	}
	if ms, err := ds.ByMission("mission-1"); err != nil || len(ms) != 2 {
		t.Fatalf("by mission %v err=%v", ms, err)
	}
	// the legacy line is on disk without a GoalID key.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"deliv-legacy"`) && strings.Contains(line, "GoalID") {
			t.Fatalf("legacy line must not carry GoalID: %s", line)
		}
	}
}

// W1: single-writer pin — the relay file (workspace.go) issues no .Append(
// call; every write crosses a kernel service. The only non-test file in the
// package with a direct Append stays codeindexhttp.go (pre-existing repo/main
// writer), pinned exactly so a new direct writer cannot slip in.
func TestDeliverableRegisterSingleWriterFRRHZ112(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	withAppend := []string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Append" {
					found = true
				}
			}
			return !found
		})
		if found {
			withAppend = append(withAppend, f)
		}
	}
	sort.Strings(withAppend)
	if strings.Join(withAppend, ",") != "codeindexhttp.go" {
		t.Fatalf("direct Append callers in package workspace: %v (want only codeindexhttp.go)", withAppend)
	}
}
