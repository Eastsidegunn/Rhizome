package janusadapter

// RHZ-107 G1 (FR-RHZ-140, SCP §5): mission.start refs and the ledger
// refCeiling are an approval record (not enforced) in the intent
// provenance — never part of the key, the fingerprint or the JANUS run
// request. Fake Runner only.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/execution"
)

// refsIntentBytesNoRefs is the execution.intent payload of a refs-free
// mission.start, captured at the commit before FR-RHZ-140 (pinned literal).
const refsIntentBytesNoRefs = `{"MissionID":"m","IdempotencyKey":"9e1329e2e8c823b2d34e7046299de0e7e12c953ebccfbe5a77e5a7373e48557f","ExternalID":"","Cursor":"","Summary":"","SourceRef":"","State":"intent","Policy":{"Capabilities":["fs:workspace"],"Domains":[],"Budget":200000,"Timeout":600000,"MaxDepth":2,"Units":"tokens-ms-v1","Unlimited":false},"provenance":{"ceiling":{"Capabilities":["fs:workspace"],"Domains":null,"Budget":200000,"Timeout":600000,"MaxDepth":2,"Units":"tokens-ms-v1","Unlimited":false},"requested":{"Capabilities":["fs:workspace"],"Domains":null,"Budget":200000,"Timeout":600000,"MaxDepth":2,"Units":"tokens-ms-v1","Unlimited":false},"effective":{"Capabilities":["fs:workspace"],"Domains":[],"Budget":200000,"Timeout":600000,"MaxDepth":2,"Units":"tokens-ms-v1","Unlimited":false},"profileId":"manual","profileHash":"abababababababababababababababababababababababababababababababab","execConfigDigest":"sha256:a27169f81474a32644a6772e8906a44892c816b73ea6ec8826396ec059f63667","actor":"unverified-local-operator:op"}}`

// refsLedgerDigest is the digest of execConfigJSON(nil) at the commit before
// FR-RHZ-140 (pinned literal): a ledger without refCeiling digests the same.
const refsLedgerDigest = "sha256:a27169f81474a32644a6772e8906a44892c816b73ea6ec8826396ec059f63667"

func testRef(c byte) string { return "sref-" + strings.Repeat(string(c), 24) }

// rawRunner records the exact run request bytes and accepts like echoRunner.
type rawRunner struct {
	echo echoRunner
	raw  [][]byte
}

func (r *rawRunner) run(cfg RunConfig, req []byte) (io.Reader, error) {
	r.raw = append(r.raw, append([]byte(nil), req...))
	return r.echo.run(cfg, req)
}

func refsLedger(t *testing.T, ceiling []string) *ExecConfig {
	t.Helper()
	c, err := ParseExecConfig(execConfigJSON(func(m map[string]any) {
		if ceiling != nil {
			m["refCeiling"] = ceiling
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

func refsStart(t *testing.T, s *events.Store, cfg *ExecConfig, refs []string) (string, *rawRunner) {
	t.Helper()
	r := &rawRunner{}
	st := Starter{Store: s, Run: r.run, Cfg: runConfigFixture(), Exec: cfg}
	id, reason, err := st.Start(StartRequest{MissionID: "m", Instruction: "write the thing", Actor: "unverified-local-operator:op", Refs: refs})
	if err != nil || reason != "" {
		t.Fatal(reason, err)
	}
	return id, r
}

func refsProv(t *testing.T, s *events.Store, id string) *execution.Provenance {
	t.Helper()
	ref, err := execution.Replay(s.List("execution", id))
	if err != nil || ref.Provenance == nil {
		t.Fatalf("%+v %v", ref, err)
	}
	return ref.Provenance
}

func eqStrings(a, b []string) bool {
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

func TestStartRefsIntersectionFRRHZ140(t *testing.T) {
	a, b, c, d := testRef('a'), testRef('b'), testRef('c'), testRef('d')
	s := startStore(t)
	in := []string{d, b, a}
	id, _ := refsStart(t, s, refsLedger(t, []string{a, b, c}), in)
	p := refsProv(t, s, id)
	if !eqStrings(p.RequestedRefs, []string{a, b, d}) || !eqStrings(p.AuthorizedRefs, []string{a, b}) {
		t.Fatalf("requested %v authorized %v", p.RequestedRefs, p.AuthorizedRefs)
	}
	if !eqStrings(in, []string{d, b, a}) {
		t.Fatal("input refs mutated")
	}
	raw := string(s.List("execution", id)[0].Payload)
	if !strings.Contains(raw, `"requestedRefs":["`+a+`","`+b+`","`+d+`"],"authorizedRefs":["`+a+`","`+b+`"]`) {
		t.Fatalf("payload %s", raw)
	}
	// No ceiling in the ledger: everything requested, nothing authorized.
	s2 := startStore(t)
	id2, _ := refsStart(t, s2, refsLedger(t, nil), []string{a})
	if p := refsProv(t, s2, id2); !eqStrings(p.RequestedRefs, []string{a}) || p.AuthorizedRefs != nil {
		t.Fatalf("%+v", p)
	}
}

// An empty request authorizes nothing (the ceiling is never a default), and
// the refs-free payload is byte-identical to the pre-FR-RHZ-140 one.
func TestStartRefsEmptyRequestFRRHZ140(t *testing.T) {
	for name, refs := range map[string][]string{"nil": nil, "empty": {}} {
		s := startStore(t)
		cfg := refsLedger(t, []string{testRef('a'), testRef('b')})
		id, _ := refsStart(t, s, cfg, refs)
		p := refsProv(t, s, id)
		if p.RequestedRefs != nil || p.AuthorizedRefs != nil {
			t.Fatalf("%s: %+v", name, p)
		}
		if raw := string(s.List("execution", id)[0].Payload); strings.Contains(raw, "Refs") {
			t.Fatalf("%s: %s", name, raw)
		}
	}
	s := startStore(t)
	id, _ := refsStart(t, s, execConfigFixture(t), nil)
	if got := string(s.List("execution", id)[0].Payload); got != refsIntentBytesNoRefs {
		t.Fatalf("refs-free intent bytes changed:\n got %s\nwant %s", got, refsIntentBytesNoRefs)
	}
}

func TestRunRequestBytesIndependentOfRefsFRRHZ140(t *testing.T) {
	a, b := testRef('a'), testRef('b')
	cfg := refsLedger(t, []string{a})
	s1, s2 := startStore(t), startStore(t)
	id1, r1 := refsStart(t, s1, cfg, nil)
	id2, r2 := refsStart(t, s2, cfg, []string{b, a})
	if id1 != id2 {
		t.Fatalf("refs changed the execution key: %s vs %s", id1, id2)
	}
	if len(r1.raw) != 1 || len(r2.raw) != 1 || !bytes.Equal(r1.raw[0], r2.raw[0]) {
		t.Fatalf("run request bytes differ:\n%s\n%s", r1.raw, r2.raw)
	}
	if bytes.Contains(r2.raw[0], []byte("sref-")) {
		t.Fatalf("refs leaked into the run request: %s", r2.raw[0])
	}
}

func TestExecConfigDigestUnchangedFRRHZ140(t *testing.T) {
	if got := refsLedger(t, nil).Digest(); got != refsLedgerDigest {
		t.Fatalf("digest of a ledger without refCeiling changed: %s", got)
	}
	if got := refsLedger(t, []string{}).Digest(); got != refsLedgerDigest {
		t.Fatalf("empty refCeiling changed the digest: %s", got)
	}
	if got := refsLedger(t, []string{testRef('a')}).Digest(); got == refsLedgerDigest {
		t.Fatal("refCeiling does not feed the digest")
	}
	c := *refsLedger(t, nil)
	if c.RefCeiling != nil {
		t.Fatal("refCeiling normalized")
	}
}

func TestExecConfigRefCeilingValidationFRRHZ140(t *testing.T) {
	sn := refsSentinel(t)
	many := make([]string, MaxRefCeiling+1)
	for i := range many {
		many[i] = "sref-" + strings.Repeat("0", 18) + hex.EncodeToString([]byte{0, 0, byte(i)})
	}
	bad := map[string]any{
		"format":    []string{sn},
		"unsorted":  []string{testRef('b'), testRef('a')},
		"duplicate": []string{testRef('a'), testRef('a')},
		"too many":  many,
		"not list":  sn,
		"non-str":   []any{1},
	}
	for name, v := range bad {
		_, err := ParseExecConfig(execConfigJSON(func(m map[string]any) { m["refCeiling"] = v }))
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		if strings.Contains(err.Error(), sn) || strings.Contains(err.Error(), sn[8:16]) {
			t.Errorf("%s: error echoes input", name)
		}
	}
	if _, err := ParseExecConfig(execConfigJSON(func(m map[string]any) { m["refCeiling"] = many[:MaxRefCeiling] })); err != nil {
		t.Fatal(err)
	}
}

// Same content, different refs: the existing execution is found and the
// first provenance stays (refs are not keyed).
func TestStartIdempotentKeepsFirstProvenanceFRRHZ140(t *testing.T) {
	a, b := testRef('a'), testRef('b')
	cfg := refsLedger(t, []string{a, b})
	s := startStore(t)
	id, _ := refsStart(t, s, cfg, []string{a})
	n := len(s.All())
	r := &rawRunner{}
	st := Starter{Store: s, Run: r.run, Cfg: runConfigFixture(), Exec: cfg}
	plan, reason, err := st.Prepare(StartRequest{MissionID: "m", Instruction: "write the thing", Actor: "x", Refs: []string{b}})
	if err != nil || reason != "" || plan.ExecutionID != id || plan.Existing != "observing" && plan.Existing != "accepted" {
		t.Fatalf("%+v %q %v", plan, reason, err)
	}
	id2, reason, err := st.Start(StartRequest{MissionID: "m", Instruction: "write the thing", Actor: "x", Refs: []string{b}})
	if err != nil || reason != "" || id2 != id {
		t.Fatal(id2, reason, err)
	}
	if len(s.All()) != n {
		t.Fatalf("re-request wrote %d events", len(s.All())-n)
	}
	if p := refsProv(t, s, id); !eqStrings(p.RequestedRefs, []string{a}) || !eqStrings(p.AuthorizedRefs, []string{a}) {
		t.Fatalf("provenance replaced: %+v", p)
	}
}

func TestStarterInvalidRefsFRRHZ140(t *testing.T) {
	sn := refsSentinel(t)
	many := make([]string, execution.MaxStartRefs+1)
	for i := range many {
		many[i] = "sref-" + strings.Repeat("0", 18) + hex.EncodeToString([]byte{0, 0, byte(i)})
	}
	for name, refs := range map[string][]string{
		"sentinel":  {sn},
		"mixed":     {testRef('a'), "sref-" + sn[8:]},
		"duplicate": {testRef('a'), testRef('a')},
		"too many":  many,
		"empty str": {""},
	} {
		s := startStore(t)
		before, _ := json.Marshal(s.All())
		r := &rawRunner{}
		st := Starter{Store: s, Run: r.run, Cfg: runConfigFixture(), Exec: refsLedger(t, nil)}
		req := StartRequest{MissionID: "m", Instruction: "write the thing", Refs: refs}
		if _, reason, err := st.Prepare(req); err != nil || reason != "secret intent: invalid ref" {
			t.Errorf("%s prepare: %q %v", name, reason, err)
		}
		if _, reason, err := st.Start(req); err != nil || reason != "secret intent: invalid ref" {
			t.Errorf("%s start: %q %v", name, reason, err)
		}
		after, _ := json.Marshal(s.All())
		if !bytes.Equal(before, after) || len(r.raw) != 0 {
			t.Errorf("%s: wrote or ran", name)
		}
	}
	if _, ok := execution.StartRefs(many[:execution.MaxStartRefs]); !ok {
		t.Fatal("16 refs rejected")
	}
}

func refsSentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

// The comment and doc blocks that describe refs/refCeiling say "approval
// record (not enforced)" and never call a ref available, allowed or usable.
func TestRefsWordingFRRHZ140(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)availab|allowed|usable|사용 ?가능|허용됨`)
	topic := regexp.MustCompile(`FR-RHZ-140|(?i)\brefs\b|refceiling|requestedRefs|authorizedRefs|AuthorizeRefs|StartRefs|ValidRef`)
	files := []string{"start.go", "../execution/execution.go", "../workspace/workspace.go", "../sessionlauncher/launcher.go", "../sessionlauncher/gate.go", "../../cmd/rhizome/main.go"}
	seen := 0
	for _, f := range files {
		b, err := os.ReadFile(filepath.FromSlash(f))
		if err != nil {
			t.Fatal(err)
		}
		// A block is a run of consecutive comment lines plus the next line.
		lines := strings.Split(string(b), "\n")
		for i := 0; i < len(lines); {
			j := i
			for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "//") {
				j++
			}
			end := j
			if end < len(lines) {
				end++
			}
			block := strings.Join(lines[i:end], "\n")
			if topic.MatchString(block) {
				seen++
				if m := forbidden.FindString(block); m != "" {
					t.Errorf("%s:%d: %q in a refs block", f, i+1, m)
				}
			}
			if j == i {
				j++
			}
			i = j
		}
	}
	if seen < 10 {
		t.Fatalf("scanned only %d refs blocks", seen)
	}
}

// An explicit null (any key casing the decoder matches) or non-array
// refCeiling is malformed, never "no ceiling"; the error does not echo.
func TestExecConfigRefCeilingNullFRRHZ140(t *testing.T) {
	sn := refsSentinel(t)
	base := string(execConfigJSON(nil))
	for name, frag := range map[string]string{
		"null":       `"refCeiling":null`,
		"null cased": `"RefCeiling":null`,
		"string":     `"refCeiling":"` + sn + `"`,
		"object":     `"refCeiling":{"` + sn + `":1}`,
		"number":     `"refCeiling":7`,
	} {
		doc := "{" + frag + "," + base[1:]
		_, err := ParseExecConfig([]byte(doc))
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		if strings.Contains(err.Error(), sn[8:16]) {
			t.Errorf("%s: error echoes input", name)
		}
	}
}
