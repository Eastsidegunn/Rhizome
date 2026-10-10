package workspace

// RHZ-133 (FR-RHZ-173) reviewer pin: the event-envelope CreatedAt is a
// display-only fact. It never enters a derived id, request digest, trust
// journal id, signature message or attest manifest, and a journal replays to
// the same projection whatever its envelope times are — including legacy
// lines whose created_at is the zero time.
//
// Only these wire keys are allowed to differ, because they ARE renderings of
// the envelope time: lastActivityTs, decidedAt, createdAt, closedAt.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"rhizome/internal/attest"
	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

// envelopeTimeKeys173 is the complete list of wire keys derived from the
// envelope CreatedAt. Nothing else may differ between replays.
var envelopeTimeKeys173 = map[string]bool{"lastActivityTs": true, "decidedAt": true, "createdAt": true, "closedAt": true}

// createdAtField173 matches the envelope created_at, which events.Event
// marshals as the final field of every journal line.
var createdAtField173 = regexp.MustCompile(`"created_at":"[^"]*"}$`)

const zeroCreatedAt173 = "0001-01-01T00:00:00Z"

// rewriteCreatedAt173 copies a journal file, replacing only the envelope
// created_at of line i with at(i); every other byte is preserved.
func rewriteCreatedAt173(t *testing.T, src, dst string, at func(i int) string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	var out bytes.Buffer
	n := 0
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		body := bytes.TrimSuffix(line, []byte("\n"))
		if !createdAtField173.Match(body) {
			t.Fatalf("line %d has no trailing created_at: %s", n+1, body)
		}
		out.Write(createdAtField173.ReplaceAll(body, []byte(`"created_at":"`+at(n)+`"}`)))
		out.WriteByte('\n')
		n++
	}
	if n == 0 {
		t.Fatal("empty journal")
	}
	if err := os.WriteFile(dst, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func shiftedAt173(i int) string {
	return time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC).Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano)
}

func zeroAt173(int) string { return zeroCreatedAt173 }

func get173(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// stripEnvelopeTimes173 decodes a wire body and recursively drops only the
// envelope-time keys, returning a canonical re-encoding.
func stripEnvelopeTimes173(t *testing.T, raw []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	var walk func(any) any
	walk = func(x any) any {
		switch y := x.(type) {
		case map[string]any:
			for k, val := range y {
				if envelopeTimeKeys173[k] {
					delete(y, k)
					continue
				}
				y[k] = walk(val)
			}
		case []any:
			for i := range y {
				y[i] = walk(y[i])
			}
		}
		return x
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

type views173 struct{ workspace, knowledge []byte }

func views173Of(t *testing.T, s events.Port, verifier *trust.Verifier) views173 {
	t.Helper()
	h := NewHTTP(s)
	if verifier != nil {
		h.Trust = verifier
	}
	return views173{workspace: get173(t, h.Handler(), "/v1/workspace"), knowledge: get173(t, h.Handler(), "/v1/knowledge")}
}

func assertSameModuloEnvelopeTime173(t *testing.T, label string, want, got views173) {
	t.Helper()
	if a, b := stripEnvelopeTimes173(t, want.workspace), stripEnvelopeTimes173(t, got.workspace); a != b {
		t.Fatalf("%s: /v1/workspace differs beyond envelope-time keys\nwant %s\n got %s", label, a, b)
	}
	if a, b := stripEnvelopeTimes173(t, want.knowledge), stripEnvelopeTimes173(t, got.knowledge); a != b {
		t.Fatalf("%s: /v1/knowledge differs beyond envelope-time keys\nwant %s\n got %s", label, a, b)
	}
}

// derivedIDs173 lists every intent-derived identifier the scenario creates,
// read back from the projection.
type derivedIDs173 struct {
	Notes, Knowledge, Relations, Requests, Deliverables, Gates, Digests []string
}

func derived173(t *testing.T, v views173) derivedIDs173 {
	t.Helper()
	var ws struct {
		Body struct {
			Gates []struct {
				ID            string `json:"id"`
				RequestDigest string `json:"requestDigest"`
			} `json:"gates"`
			Requests     []struct{ ID string } `json:"requests"`
			Deliverables []struct{ ID string } `json:"deliverables"`
		} `json:"body"`
	}
	if err := json.Unmarshal(v.workspace, &ws); err != nil {
		t.Fatal(err)
	}
	var kn struct {
		Body struct {
			Notes     []struct{ ID string } `json:"notes"`
			Items     []struct{ ID string } `json:"items"`
			Relations []struct{ ID string } `json:"relations"`
		} `json:"body"`
	}
	if err := json.Unmarshal(v.knowledge, &kn); err != nil {
		t.Fatal(err)
	}
	var d derivedIDs173
	for _, g := range ws.Body.Gates {
		d.Gates, d.Digests = append(d.Gates, g.ID), append(d.Digests, g.RequestDigest)
	}
	for _, r := range ws.Body.Requests {
		d.Requests = append(d.Requests, r.ID)
	}
	for _, x := range ws.Body.Deliverables {
		d.Deliverables = append(d.Deliverables, x.ID)
	}
	for _, x := range kn.Body.Notes {
		d.Notes = append(d.Notes, x.ID)
	}
	for _, x := range kn.Body.Items {
		d.Knowledge = append(d.Knowledge, x.ID)
	}
	for _, x := range kn.Body.Relations {
		d.Relations = append(d.Relations, x.ID)
	}
	return d
}

// scenario173 drives one fixed sequence of intents through RelayIntent (the
// /v1/intent path) on a fresh on-disk journal and returns its path and the
// live views (taken before close).
func scenario173(t *testing.T, dir string) (string, views173) {
	t.Helper()
	path := filepath.Join(dir, "journal.ndjson")
	j, err := journal.OpenGuarded(path, trust.NewAnchorless())
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Service{Store: j}
	if _, err := ms.CreateGoal("goal-173", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("mission-173", "goal-173", "mission", "done"); err != nil {
		t.Fatal(err)
	}
	relay := func(in Intent) {
		t.Helper()
		res, err := RelayIntent(j, in, "tester", noAuthority())
		if err != nil || !res.Accepted {
			t.Fatalf("%s: %+v %v", in.Kind, res, err)
		}
	}
	relay(Intent{Kind: "note.create", Content: "envelope time is display only", MemoryKind: "fact", MissionID: "mission-173", Tags: []string{"t173"}})
	note := "note-" + sha256Hex173("envelope time is display only")
	relay(Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "ids ignore created_at", KnowledgeKind: "claim"})
	relay(Intent{Kind: "knowledge.create", SourceMemoryID: note, Content: "replay is deterministic", KnowledgeKind: "claim"})
	kA := "know-" + sha256Hex173(note + "\x00" + "ids ignore created_at")[:12]
	kB := "know-" + sha256Hex173(note + "\x00" + "replay is deterministic")[:12]
	relay(Intent{Kind: "relation.create", From: kA, To: kB, RelationType: "supports", SourceMemoryIDs: []string{note}})
	relay(Intent{Kind: "question.ask", Name: "q173", Body: "b173", Recommendation: "r173", MissionID: "mission-173"})
	relay(Intent{Kind: "question.ask", Name: "q173-open", Body: "b173", Recommendation: "r173", MissionID: "mission-173"})
	q := mustQuestionID("q173", "b173", "r173")
	ref, err := (question.Service{Store: j}).Get(q)
	if err != nil {
		t.Fatal(err)
	}
	relay(Intent{Kind: "gate.approve", GateID: q, Digest: ref.Digest})
	relay(createRequestIntent173())
	relay(Intent{Kind: "deliverable.register", MissionID: "mission-173", DeliverableKind: "code", Summary: "patch 173"})
	live := views173Of(t, j, nil)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return path, live
}

func createRequestIntent173() Intent {
	in := createRequestIntent()
	in.MissionID = "mission-173"
	return in
}

func sha256Hex173(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func reopen173(t *testing.T, path string, guard events.Guard) *journal.Journal {
	t.Helper()
	j, err := journal.OpenGuarded(path, guard)
	if err != nil {
		t.Fatalf("reopen %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

// Why the claim holds (verbatim from the RHZ-133 review brief):
// "envelope CreatedAt enters no digest/ID/signature input — question
// digestV1/V2 (title·body·recommendation), request IDFor (creation fields),
// note/knowledge/relation ids (content), deliverableRegisterID
// (binding·kind·summary·sourceRef), trust AttestManifestBytes/ManifestDigest
// and Decision/Add/Revoke/AttestMessage (time = signature signedAt only);
// trust CheckAppend/CheckReplay do not read CreatedAt."
//
// C1: the same intents applied at different wall times, and the same journal
// replayed with rewritten or zeroed (legacy) envelope times, yield identical
// ids, digests and projections except for the envelope-time keys.
func TestCreatedAtNeverEntersDerivedIdsOrProjectionFRRHZ173(t *testing.T) {
	pathA, liveA := scenario173(t, t.TempDir())
	time.Sleep(20 * time.Millisecond) // a later wall clock for the second build
	pathD, liveD := scenario173(t, t.TempDir())

	// The durable line and the in-memory event agree: reopening A reproduces
	// the live views byte-for-byte, envelope times included.
	reA := views173Of(t, reopen173(t, pathA, trust.NewAnchorless()), nil)
	if !bytes.Equal(reA.workspace, liveA.workspace) || !bytes.Equal(reA.knowledge, liveA.knowledge) {
		t.Fatalf("reopen changed the projection:\nlive %s\nre   %s", liveA.workspace, reA.workspace)
	}

	dir := t.TempDir()
	pathB, pathC := filepath.Join(dir, "shifted.ndjson"), filepath.Join(dir, "legacy.ndjson")
	rewriteCreatedAt173(t, pathA, pathB, shiftedAt173)
	rewriteCreatedAt173(t, pathA, pathC, zeroAt173)
	b := views173Of(t, reopen173(t, pathB, trust.NewAnchorless()), nil)
	c := views173Of(t, reopen173(t, pathC, trust.NewAnchorless()), nil)

	// The rewrite really moved the rendered times (the test is not vacuous).
	if stripEnvelopeTimes173(t, liveA.workspace) == string(liveA.workspace) || bytes.Equal(b.workspace, liveA.workspace) || bytes.Equal(c.workspace, liveA.workspace) {
		t.Fatal("envelope times did not reach the wire; rewrite exercised nothing")
	}
	for _, key := range []string{`"lastActivityTs":`, `"decidedAt":`, `"createdAt":`} {
		if !bytes.Contains(liveA.workspace, []byte(key)) {
			t.Fatalf("scenario does not render %s", key)
		}
	}
	// Legacy zero lines read as unknown: no lastActivityTs at all.
	if bytes.Contains(c.workspace, []byte(`"lastActivityTs":`)) {
		t.Fatalf("legacy zero created_at rendered an activity time: %s", c.workspace)
	}

	assertSameModuloEnvelopeTime173(t, "built later (D)", liveA, liveD)
	assertSameModuloEnvelopeTime173(t, "shifted created_at (B)", liveA, b)
	assertSameModuloEnvelopeTime173(t, "zero created_at (C)", liveA, c)

	want := derived173(t, liveA)
	if len(want.Notes) != 1 || len(want.Knowledge) != 2 || len(want.Relations) != 1 || len(want.Requests) != 1 || len(want.Deliverables) != 1 || len(want.Gates) != 2 {
		t.Fatalf("scenario ids incomplete: %+v", want)
	}
	for _, d := range want.Digests {
		if !strings.HasPrefix(d, "rhz-question-") {
			t.Fatalf("gate digest %q is not an rhz-question digest", d)
		}
	}
	wantJSON, _ := json.Marshal(want)
	for label, v := range map[string]views173{"D": liveD, "B": b, "C": c} {
		gotJSON, _ := json.Marshal(derived173(t, v))
		if !bytes.Equal(wantJSON, gotJSON) {
			t.Fatalf("%s derived ids differ:\nwant %s\n got %s", label, wantJSON, gotJSON)
		}
	}
	_ = pathD
}

// trustScenario173 builds an anchored on-disk journal: genesis, a signed gate
// decision, a signed trust.key.add and revoke, and a signed attest manifest
// over an unsigned decision.
func trustScenario173(t *testing.T) (string, trust.Anchor, string, string) {
	t.Helper()
	anchor, err := trust.ParseAnchor([]byte(anchorJSON148))
	if err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewAnchored(anchor)
	path := filepath.Join(t.TempDir(), "trust.ndjson")
	j, err := journal.OpenGuarded(path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.EnsureGenesis(j, anchor); err != nil {
		t.Fatal(err)
	}
	root := rootKey149(t)
	qs := question.Service{Store: j}

	signed, err := qs.Ask("signed 173", "body", "", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	v := decisionVerification149(t, root, domain149(t, verifier, j), "question", signed.ID, signed.Digest, "approve", "", signedAt149(), "73737373737373737373737373737301")
	if res, err := RelayIntent(j, Intent{Kind: "gate.approve", GateID: signed.ID, Digest: signed.Digest, Verification: intentVerification149(t, v)}, "signer", trust.Authority{}); err != nil || !res.Accepted {
		t.Fatalf("signed approve: %+v %v", res, err)
	}

	added := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	addIn, addedID := addIntent148(t, j, verifier, root, added, signedAt149(), "73737373737373737373737373737302")
	if res, err := RelayIntent(j, addIn, "signer", trust.Authority{}); err != nil || !res.Accepted {
		t.Fatalf("key add: %+v %v", res, err)
	}
	if res, err := RelayIntent(j, revokeIntent148(t, j, verifier, root, addedID, signedAt149(), "73737373737373737373737373737303"), "signer", trust.Authority{}); err != nil || !res.Accepted {
		t.Fatalf("key revoke: %+v %v", res, err)
	}

	attested, item := unsignedQuestion164(t, j, "attested 173", question.Approve, "")
	items := []attest.Item{item}
	if err := (attest.Service{Store: j}).Create(trust.ManifestDigest(items), items, attestSignature164(t, j, verifier, root, items, "73737373737373737373737373737304")); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return path, anchor, signed.ID, attested.ID
}

func trustViews173(t *testing.T, path string, anchor trust.Anchor) (views173, Projection) {
	t.Helper()
	verifier := trust.NewAnchored(anchor)
	j := reopen173(t, path, verifier)
	p, err := Snapshot(j, verifier)
	if err != nil {
		t.Fatalf("snapshot %s: %v", filepath.Base(path), err)
	}
	return views173Of(t, j, verifier), p
}

func gateStatus173(p Projection, id string) string {
	for _, g := range p.Gates {
		if g.ID == id && g.Verification != nil {
			return g.Verification.Status
		}
	}
	return ""
}

// C2: a trust genesis, signed decision, signed key add/revoke and a signed
// attest manifest replay and verify identically when every line's envelope
// time is rewritten or zeroed. Signature messages are built from payload
// fields only — see the compile-level pin below.
func TestTrustReplayIndependentOfCreatedAtFRRHZ173(t *testing.T) {
	path, anchor, signedID, attestedID := trustScenario173(t)
	base, baseP := trustViews173(t, path, anchor)
	if baseP.Trust == nil || gateStatus173(baseP, signedID) != "verified" || gateStatus173(baseP, attestedID) != "attested" {
		t.Fatalf("baseline trust projection: trust=%+v gates=%+v", baseP.Trust, baseP.Gates)
	}
	dir := t.TempDir()
	for name, at := range map[string]func(int) string{"shifted": shiftedAt173, "legacy-zero": zeroAt173} {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(dir, name+".ndjson")
			rewriteCreatedAt173(t, path, dst, at)
			got, p := trustViews173(t, dst, anchor)
			if p.Trust == nil || *p.Trust != *baseP.Trust {
				t.Fatalf("trust summary changed: %+v vs %+v", p.Trust, baseP.Trust)
			}
			if s := gateStatus173(p, signedID); s != "verified" {
				t.Fatalf("signed gate status %q", s)
			}
			if s := gateStatus173(p, attestedID); s != "attested" {
				t.Fatalf("attested gate status %q", s)
			}
			assertSameModuloEnvelopeTime173(t, name, base, got)
		})
	}
}

// C2 (structural): every trust message builder and the manifest digest take
// only payload strings/bytes — no time.Time and no events.Event — so the
// envelope CreatedAt cannot reach a signed message. A signature change here
// fails compilation.
var (
	_ func(journalID, consumer, gateID, requestDigest, decision, reason, keyID, signedAt, nonce string) []byte                         = trust.DecisionMessage
	_ func(journalID, manifestDigest, count, keyID, signedAt, nonce string) []byte                                                     = trust.AttestMessage
	_ func(journalID, keyID, algorithm string, publicKeyDER []byte, principal, assurance, signingKeyID, signedAt, nonce string) []byte = trust.AddMessage
	_ func(journalID, keyID, reason, signingKeyID, signedAt, nonce string) []byte                                                      = trust.RevokeMessage
	_ func(items []trust.AttestItem) string                                                                                            = trust.ManifestDigest
)
