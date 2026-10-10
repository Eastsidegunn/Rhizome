package workspace

// RHZ-107 G1 (FR-RHZ-135, SCP §2, §4.2): secret request intents over HTTP
// and RelayIntentHooks, the I0–I7 refusals X1–X15 with zero writes, I1 strict
// decoding, idempotency, the single writer under concurrency, GET
// /v1/secrets, SSE re-publication and the ZV evidence. X16 is not tested:
// approval Q2 makes it unreachable (attested refs may be rotated/revoked).

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/mission"
	"rhizome/internal/peercred"
	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
	"rhizome/internal/trust"
)

var secretT0 = time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)

func secretOp(n int) string    { return fmt.Sprintf("op-%032x", n) }
func secretPlane(n int) string { return fmt.Sprintf("pl-%032x", n) }

func secretSentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

// assertNoSentinelForms checks the §8.1 forms of s in data.
func assertNoSentinelForms(t *testing.T, where string, data []byte, s string) {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	for _, f := range []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]} {
		if bytes.Contains(data, []byte(f)) {
			t.Fatalf("%s contains a sentinel form", where)
		}
	}
}

// secretVerified returns a real same-user peercred.Verified (socketpair).
func secretVerified(t *testing.T) peercred.Verified {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	fa, fb := os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
	defer fa.Close()
	defer fb.Close()
	ca, err := net.FileConn(fa)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := net.FileConn(fb)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	v, err := peercred.Verify(ca.(*net.UnixConn))
	if err != nil || !v.SameUser() {
		t.Fatalf("peercred: %v", err)
	}
	return v
}

type secretEnv struct {
	path  string // "" for a memory store
	store events.Port
	svc   *secretref.Service
	h     *HTTPServer
	url   string
	plane string
	seq   uint64
	v     peercred.Verified
}

func newSecretEnv(t *testing.T, file bool) *secretEnv {
	t.Helper()
	e := &secretEnv{plane: secretPlane(1)}
	if file {
		e.path = filepath.Join(t.TempDir(), "journal.ndjson")
		j, err := journal.OpenGuarded(e.path, trust.NewAnchorless())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { j.Close() })
		e.store = j
	} else {
		e.store = &events.Store{}
	}
	e.svc = secretref.New(e.store, secretref.WithClock(func() time.Time { return secretT0 }))
	e.h = NewHTTP(e.store)
	e.h.Secrets = e.svc
	srv := httptest.NewServer(e.h.Handler())
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

func (e *secretEnv) journalBytes(t *testing.T) []byte {
	t.Helper()
	if e.path == "" {
		b, _ := json.Marshal(e.store.All())
		return b
	}
	b, err := os.ReadFile(e.path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return b
}

func (e *secretEnv) register(t *testing.T, op int, name string) string {
	t.Helper()
	r, err := e.svc.RequestRegister(secretref.RegisterRequest{OpID: secretOp(op), Name: name, RefKind: "provider-key", Actor: "tester"})
	if err != nil || !r.Accepted {
		t.Fatalf("register: %+v %v", r, err)
	}
	return r.Ref
}

func (e *secretEnv) rotate(t *testing.T, op int, ref string) {
	t.Helper()
	r, err := e.svc.RequestRotate(secretref.RotateRequest{OpID: secretOp(op), Ref: ref, Actor: "tester"})
	if err != nil || !r.Accepted {
		t.Fatalf("rotate: %+v %v", r, err)
	}
}

func (e *secretEnv) revoke(t *testing.T, op int, ref string) {
	t.Helper()
	r, err := e.svc.RequestRevoke(secretref.RevokeRequest{OpID: secretOp(op), Ref: ref, ReasonCode: "unused", Actor: "tester"})
	if err != nil || !r.Accepted {
		t.Fatalf("revoke: %+v %v", r, err)
	}
}

// fact delivers one plane entry with the next plane seq.
func (e *secretEnv) fact(t *testing.T, en secretwire.Entry) {
	t.Helper()
	if e.v == (peercred.Verified{}) {
		e.v = secretVerified(t)
	}
	e.seq++
	en.Seq = e.seq
	var ok bool
	var err error
	switch en.Fact {
	case secretwire.FactOutcome:
		ok, err = e.svc.RecordOutcome(e.v, e.plane, en)
	case secretwire.FactReconciled:
		ok, err = e.svc.RecordReconciled(e.v, e.plane, en)
	case secretwire.FactObserved:
		ok, err = e.svc.RecordObserved(e.v, e.plane, en)
	}
	if err != nil || !ok {
		t.Fatalf("fact %+v: %v %v", en, ok, err)
	}
}

func (e *secretEnv) outcome(t *testing.T, op int, ref string, version uint64, result, failure string) {
	t.Helper()
	e.fact(t, secretwire.Entry{Fact: secretwire.FactOutcome, OpID: secretOp(op), Ref: ref, Version: version, Result: result, FailureCode: failure})
}

func (e *secretEnv) post(t *testing.T, body string) (int, string) {
	t.Helper()
	return postRaw(t, e.url, body)
}

func secretRefused(reason string) string {
	b, _ := json.Marshal(RelayResult{Reason: reason})
	return string(b) + "\n"
}

func registerBody(op int, name string) string {
	return fmt.Sprintf(`{"kind":"secret.register.request","actor":"tester","opId":"%s","name":"%s","refKind":"provider-key"}`, secretOp(op), name)
}

func rotateBody(op int, ref string) string {
	return fmt.Sprintf(`{"kind":"secret.rotate.request","actor":"tester","opId":"%s","ref":"%s"}`, secretOp(op), ref)
}

// secretFixture: alpha active (op1), beta pending registration (op2), gamma
// rotating (op3 ok, op4 rotate pending).
func secretFixture(t *testing.T, file bool) (e *secretEnv, alpha, beta, gamma string) {
	e = newSecretEnv(t, file)
	alpha = e.register(t, 1, "alpha")
	e.outcome(t, 1, alpha, 1, "ok", "")
	beta = e.register(t, 2, "beta")
	gamma = e.register(t, 3, "gamma")
	e.outcome(t, 3, gamma, 1, "ok", "")
	e.rotate(t, 4, gamma)
	return e, alpha, beta, gamma
}

func TestSecretIntentRefusalsHTTPFRRHZ135(t *testing.T) {
	e, alpha, beta, gamma := secretFixture(t, true)
	unknown := secretwire.DeriveRef(secretOp(99))
	cases := []struct{ name, body, reason string }{
		{"X1 fact kind", `{"kind":"secret.op.outcome","actor":"tester"}`, secretref.ReasonChannelOnly},
		{"X1 requested kind", `{"kind":"secret.register.requested","actor":"tester","opId":"` + secretOp(50) + `"}`, secretref.ReasonChannelOnly},
		{"X1 observed kind", `{"kind":"secret.observed"}`, secretref.ReasonChannelOnly},
		{"X1 other", `{"kind":"secret.","value":1,"value":2}`, secretref.ReasonChannelOnly},
		{"X2 bad opId over HTTP", `{"kind":"secret.register.request","actor":"tester","opId":"op-1","name":"n","refKind":"other"}`, secretref.ReasonMalformed},
		{"X10 opId conflict", registerBody(1, "other-name"), secretref.ReasonOpIDConflict},
		{"X10 kind conflict", rotateBody(1, alpha), secretref.ReasonOpIDConflict},
		{"X11 unknown ref", rotateBody(60, unknown), secretref.ReasonUnknownRef},
		{"X12 pending registration", rotateBody(61, beta), secretref.ReasonRefNotActive},
		{"X13 rotating", rotateBody(62, gamma), secretref.ReasonRefBusy},
		{"X13 revoke while rotating", fmt.Sprintf(`{"kind":"secret.revoke.request","actor":"tester","opId":"%s","ref":"%s","reasonCode":"exposed"}`, secretOp(63), gamma), secretref.ReasonRefBusy},
		{"X14 name in use (active)", registerBody(64, "alpha"), secretref.ReasonNameInUse},
		{"X14 name in use (pending)", registerBody(65, "beta"), secretref.ReasonNameInUse},
	}
	for _, c := range cases {
		before := e.journalBytes(t)
		code, body := e.post(t, c.body)
		if code != 200 || body != secretRefused(c.reason) {
			t.Fatalf("%s: %d %q", c.name, code, body)
		}
		if !bytes.Equal(before, e.journalBytes(t)) {
			t.Fatalf("%s: journal bytes changed", c.name)
		}
	}
	// X12 also covers registration-failed and revoked refs.
	delta := e.register(t, 70, "delta")
	e.outcome(t, 70, delta, 0, "failed", "backend-error")
	e.revoke(t, 71, alpha)
	e.outcome(t, 71, alpha, 1, "ok", "")
	for _, ref := range []string{delta, alpha} {
		before := e.journalBytes(t)
		if code, body := e.post(t, rotateBody(72, ref)); code != 200 || body != secretRefused(secretref.ReasonRefNotActive) {
			t.Fatalf("X12: %d %q", code, body)
		}
		if !bytes.Equal(before, e.journalBytes(t)) {
			t.Fatal("X12: journal bytes changed")
		}
	}
}

// X3–X9 are the Go-internal path (no I1): the Service applies I2/I3. Over
// HTTP the same bodies are V-J 5 violations, answered X2 (checked too).
func TestSecretIntentFormatRefusalsFRRHZ135(t *testing.T) {
	e, alpha, _, _ := secretFixture(t, true)
	hooks := RelayHooks{Secrets: e.svc}
	okReg := Intent{Kind: secretref.KindRegisterRequest, OpID: secretOp(80), Name: "n", RefKind: "other"}
	cases := []struct {
		name   string
		in     Intent
		actor  string
		reason string
	}{
		{"X3", Intent{Kind: secretref.KindRegisterRequest, OpID: "op-xyz", Name: "n", RefKind: "other"}, "tester", secretref.ReasonInvalidOpID},
		{"X3 rotate", Intent{Kind: secretref.KindRotateRequest, OpID: strings.ToUpper(secretOp(1)), Ref: alpha}, "tester", secretref.ReasonInvalidOpID},
		{"X4", Intent{Kind: secretref.KindRegisterRequest, OpID: secretOp(80), Name: "Bad Name", RefKind: "other"}, "tester", secretref.ReasonInvalidName},
		{"X5", Intent{Kind: secretref.KindRegisterRequest, OpID: secretOp(80), Name: "n", RefKind: "password"}, "tester", secretref.ReasonInvalidRefKind},
		{"X6 unsorted", Intent{Kind: secretref.KindRegisterRequest, OpID: secretOp(80), Name: "n", RefKind: "other", ScopeTags: []string{"b", "a"}}, "tester", secretref.ReasonInvalidScopeTags},
		{"X6 empty", Intent{Kind: secretref.KindRegisterRequest, OpID: secretOp(80), Name: "n", RefKind: "other", ScopeTags: []string{}}, "tester", secretref.ReasonInvalidScopeTags},
		{"X7", Intent{Kind: secretref.KindRotateRequest, OpID: secretOp(80), Ref: "sref-xyz"}, "tester", secretref.ReasonInvalidRef},
		{"X8", Intent{Kind: secretref.KindRevokeRequest, OpID: secretOp(80), Ref: alpha, ReasonCode: "because"}, "tester", secretref.ReasonInvalidReasonCode},
		{"X9", okReg, "", secretref.ReasonInvalidActor},
		{"X9 prefixed", okReg, "unverified-local-operator:tester", secretref.ReasonInvalidActor},
	}
	for _, c := range cases {
		before := e.journalBytes(t)
		res, err := RelayIntentHooks(e.store, c.in, c.actor, trust.Authority{}, hooks)
		if err != nil || res != (RelayResult{Reason: c.reason}) {
			t.Fatalf("%s: %+v %v", c.name, res, err)
		}
		if !bytes.Equal(before, e.journalBytes(t)) {
			t.Fatalf("%s: journal bytes changed", c.name)
		}
	}
	for name, body := range map[string]string{
		"X3": `{"kind":"secret.register.request","actor":"tester","opId":"op-xyz","name":"n","refKind":"other"}`,
		"X4": `{"kind":"secret.register.request","actor":"tester","opId":"` + secretOp(80) + `","name":"Bad","refKind":"other"}`,
		"X5": `{"kind":"secret.register.request","actor":"tester","opId":"` + secretOp(80) + `","name":"n","refKind":"x"}`,
		"X6": `{"kind":"secret.register.request","actor":"tester","opId":"` + secretOp(80) + `","name":"n","refKind":"other","scopeTags":["b","a"]}`,
		"X7": `{"kind":"secret.rotate.request","actor":"tester","opId":"` + secretOp(80) + `","ref":"sref-x"}`,
		"X8": `{"kind":"secret.revoke.request","actor":"tester","opId":"` + secretOp(80) + `","ref":"` + alpha + `","reasonCode":"x"}`,
		"X9": `{"kind":"secret.register.request","actor":"","opId":"` + secretOp(80) + `","name":"n","refKind":"other"}`,
	} {
		before := e.journalBytes(t)
		if code, got := e.post(t, body); code != 200 || got != secretRefused(secretref.ReasonMalformed) {
			t.Fatalf("HTTP %s: %d %q", name, code, got)
		}
		if !bytes.Equal(before, e.journalBytes(t)) {
			t.Fatalf("HTTP %s: journal bytes changed", name)
		}
	}
	// I0 on the Go-internal path as well.
	if res, err := RelayIntentHooks(e.store, Intent{Kind: "secret.op.outcome"}, "tester", trust.Authority{}, hooks); err != nil || res.Reason != secretref.ReasonChannelOnly {
		t.Fatalf("X1 internal: %+v %v", res, err)
	}
}

func TestSecretIntentUnavailableX15FRRHZ135(t *testing.T) {
	// Not wired: the HTTP server and the plain relay have no writer.
	e := newSecretEnv(t, true)
	e.h.Secrets = nil
	before := e.journalBytes(t)
	if code, body := e.post(t, registerBody(1, "alpha")); code != 200 || body != secretRefused(secretref.ReasonUnavailable) {
		t.Fatalf("nil hook: %d %q", code, body)
	}
	if res, err := RelayIntent(e.store, Intent{Kind: secretref.KindRegisterRequest, OpID: secretOp(1), Name: "a", RefKind: "other"}, "tester", trust.Authority{}); err != nil || res.Reason != secretref.ReasonUnavailable {
		t.Fatalf("RelayIntent: %+v %v", res, err)
	}
	if code, _ := getRaw(t, e.url, "/v1/secrets"); code != http.StatusNotFound {
		t.Fatalf("unwired /v1/secrets = %d", code)
	}
	if !bytes.Equal(before, e.journalBytes(t)) {
		t.Fatal("journal bytes changed")
	}
	// Projection error (R8): a hand-made secret event the replay rejects.
	e2 := newSecretEnv(t, true)
	bad := events.Event{AggregateType: secretref.AggregateOp, AggregateID: secretOp(1), Revision: 1,
		Type: secretref.TypeRegisterRequested, Payload: json.RawMessage(`{"OpID":1}`), CreatedAt: secretT0}
	if err := e2.store.Append(0, bad); err != nil {
		t.Fatal(err)
	}
	before = e2.journalBytes(t)
	if code, body := e2.post(t, registerBody(2, "alpha")); code != 200 || body != secretRefused(secretref.ReasonUnavailable) {
		t.Fatalf("projection error: %d %q", code, body)
	}
	if !bytes.Equal(before, e2.journalBytes(t)) {
		t.Fatal("journal bytes changed")
	}
	code, body := getRaw(t, e2.url, "/v1/secrets")
	if code != 500 || body != "secret replay: V-J\n" {
		t.Fatalf("/v1/secrets on replay error: %d %q", code, body)
	}
}

func TestSecretIntentMalformedX2FRRHZ135(t *testing.T) {
	e, alpha, _, _ := secretFixture(t, true)
	op := secretOp(90)
	reg := `"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":"n","refKind":"other"`
	bodies := map[string]string{
		"duplicate key":     `{` + reg + `,"name":"m"}`,
		"duplicate kind":    `{"kind":"nope",` + reg + `}`,
		"null":              `{` + reg + `,"scopeTags":null}`,
		"null element":      `{` + reg + `,"scopeTags":[null]}`,
		"trailing data":     `{` + reg + `} {"x":1}`,
		"trailing garbage":  `{` + reg + `}x`,
		"verification key":  `{` + reg + `,"verification":{"status":"claimed"}}`,
		"verified key":      `{` + reg + `,"verified":true}`,
		"correlationId key": `{` + reg + `,"correlationId":"c"}`,
		"OpId case":         `{"kind":"secret.register.request","actor":"tester","OpId":"` + op + `","name":"n","refKind":"other"}`,
		"value key":         `{` + reg + `,"value":"v"}`,
		"missing actor":     `{"kind":"secret.register.request","opId":"` + op + `","name":"n","refKind":"other"}`,
		"number name":       `{"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":5,"refKind":"other"}`,
		"empty scopeTags":   `{` + reg + `,"scopeTags":[]}`,
		"refs key":          `{` + reg + `,"refs":[]}`,
		"rotate extra name": `{"kind":"secret.rotate.request","actor":"tester","opId":"` + op + `","ref":"` + alpha + `","name":"n"}`,
		"revoke missing rc": `{"kind":"secret.revoke.request","actor":"tester","opId":"` + op + `","ref":"` + alpha + `"}`,
		"escape":            `{"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":"` + "\\" + `u006e","refKind":"other"}`,
		"non-utf8":          "{" + reg + `,"x":"` + "\xff\xfe" + `"}`,
		"DEL byte":          "{" + reg + `,"x":"a` + "\x7f" + `"}`,
		"array":             `[{` + reg + `}]`,
	}
	for name, body := range bodies {
		before := e.journalBytes(t)
		code, got := e.post(t, body)
		if name == "array" {
			// Not an object: the lenient probe finds no kind, so it stays
			// on the pre-existing path ("invalid json" for an array).
			if code != 400 {
				t.Fatalf("%s: %d %q", name, code, got)
			}
		} else if code != 200 || got != secretRefused(secretref.ReasonMalformed) {
			t.Fatalf("%s: %d %q", name, code, got)
		}
		if !bytes.Equal(before, e.journalBytes(t)) {
			t.Fatalf("%s: journal bytes changed", name)
		}
	}
	// JSON syntax errors keep the existing 400 (§2.2 응답 의미).
	if code, got := e.post(t, `{"kind":"secret.register.request",`); code != 400 || got != "invalid json\n" {
		t.Fatalf("syntax: %d %q", code, got)
	}
}

func TestSecretIntentAcceptIdempotentFRRHZ135(t *testing.T) {
	e := newSecretEnv(t, true)
	op := secretOp(1)
	ref := secretwire.DeriveRef(op)
	body := `{"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":"alpha","refKind":"provider-key","scopeTags":["ci","prod"]}`
	want := `{"Accepted":true,"Reason":"","opId":"` + op + `","ref":"` + ref + `"}` + "\n"
	if code, got := e.post(t, body); code != 200 || got != want {
		t.Fatalf("register: %d %q", code, got)
	}
	n := len(e.store.All())
	if n != 1 || e.store.All()[0].Type != secretref.TypeRegisterRequested {
		t.Fatalf("events = %+v", e.store.All())
	}
	// I5: same content (actor not compared) → Accepted, zero writes, also
	// after the outcome.
	before := e.journalBytes(t)
	other := strings.Replace(body, `"actor":"tester"`, `"actor":"someone-else"`, 1)
	for _, b := range []string{body, other} {
		if code, got := e.post(t, b); code != 200 || got != want {
			t.Fatalf("replay: %d %q", code, got)
		}
	}
	if !bytes.Equal(before, e.journalBytes(t)) {
		t.Fatal("idempotent replay wrote")
	}
	e.outcome(t, 1, ref, 1, "ok", "")
	before = e.journalBytes(t)
	if code, got := e.post(t, body); code != 200 || got != want {
		t.Fatalf("replay after outcome: %d %q", code, got)
	}
	// X10: same opId, different content or kind.
	for _, b := range []string{
		strings.Replace(body, `["ci","prod"]`, `["ci"]`, 1),
		strings.Replace(body, `provider-key`, `other`, 1),
		rotateBody(1, ref),
	} {
		if code, got := e.post(t, b); code != 200 || got != secretRefused(secretref.ReasonOpIDConflict) {
			t.Fatalf("X10: %d %q", code, got)
		}
	}
	if !bytes.Equal(before, e.journalBytes(t)) {
		t.Fatal("replay / X10 wrote")
	}
	// rotate and revoke: accepted, then idempotent; a revoke reasonCode
	// change is X10.
	rot := rotateBody(2, ref)
	wantRot := `{"Accepted":true,"Reason":"","opId":"` + secretOp(2) + `","ref":"` + ref + `"}` + "\n"
	for i := 0; i < 2; i++ {
		if code, got := e.post(t, rot); code != 200 || got != wantRot {
			t.Fatalf("rotate %d: %d %q", i, code, got)
		}
	}
	e.outcome(t, 2, ref, 2, "ok", "")
	rev := fmt.Sprintf(`{"kind":"secret.revoke.request","actor":"tester","opId":"%s","ref":"%s","reasonCode":"rotated-out"}`, secretOp(3), ref)
	wantRev := `{"Accepted":true,"Reason":"","opId":"` + secretOp(3) + `","ref":"` + ref + `"}` + "\n"
	for i := 0; i < 2; i++ {
		if code, got := e.post(t, rev); code != 200 || got != wantRev {
			t.Fatalf("revoke %d: %d %q", i, code, got)
		}
	}
	if code, got := e.post(t, strings.Replace(rev, "rotated-out", "exposed", 1)); code != 200 || got != secretRefused(secretref.ReasonOpIDConflict) {
		t.Fatalf("revoke X10: %d %q", code, got)
	}
	var types []string
	for _, ev := range e.store.All() {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "secret.register.requested,secret.op.outcome,secret.rotate.requested,secret.op.outcome,secret.revoke.requested" {
		t.Fatalf("types = %v", types)
	}
}

func TestSecretConcurrentRotatesFRRHZ135(t *testing.T) {
	for round := 0; round < 5; round++ {
		e := newSecretEnv(t, true)
		ref := e.register(t, 1, "alpha")
		e.outcome(t, 1, ref, 1, "ok", "")
		var wg sync.WaitGroup
		got := make([]string, 2)
		for i := range got {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp, err := http.Post(e.url+"/v1/intent", "application/json", strings.NewReader(rotateBody(10+i, ref)))
				if err != nil {
					got[i] = err.Error()
					return
				}
				defer resp.Body.Close()
				var b bytes.Buffer
				b.ReadFrom(resp.Body)
				got[i] = b.String()
			}(i)
		}
		wg.Wait()
		accepted := 0
		for _, g := range got {
			if strings.HasPrefix(g, `{"Accepted":true`) {
				accepted++
			} else if g != secretRefused(secretref.ReasonRefBusy) {
				t.Fatalf("loser = %q", g)
			}
		}
		rotates := 0
		for _, ev := range e.store.All() {
			if ev.Type == secretref.TypeRotateRequested {
				rotates++
			}
		}
		if accepted != 1 || rotates != 1 {
			t.Fatalf("accepted %d, recorded %d: %q", accepted, rotates, got)
		}
	}
}

// secretLifecycle scripts every DTO field: alpha (register ok, rotate ok,
// rotate failed, observed, revoke pending), beta (attested E5 with tags),
// gamma (pending registration with tags), delta (revoked), epsilon
// (registration failed).
func secretLifecycle(t *testing.T, e *secretEnv) {
	alpha := e.register(t, 1, "alpha")
	e.outcome(t, 1, alpha, 1, "ok", "")
	e.rotate(t, 2, alpha)
	e.outcome(t, 2, alpha, 2, "ok", "")
	e.rotate(t, 3, alpha)
	e.outcome(t, 3, alpha, 2, "failed", "backend-error")
	e.fact(t, secretwire.Entry{Fact: secretwire.FactObserved, Ref: alpha, Version: 2, Present: true, PlaneObservedAt: "2026-10-10T05:06:07Z"})
	e.revoke(t, 4, alpha)
	e.fact(t, secretwire.Entry{Fact: secretwire.FactReconciled, OpID: secretOp(5), Ref: secretwire.DeriveRef(secretOp(5)),
		Name: "beta", RefKind: "gateway-credential", Version: 1, Origin: "attested", ScopeTags: []string{"ci", "prod"}})
	if r, err := e.svc.RequestRegister(secretref.RegisterRequest{OpID: secretOp(6), Name: "gamma", RefKind: "other", ScopeTags: []string{"dev"}, Actor: "tester"}); err != nil || !r.Accepted {
		t.Fatalf("%+v %v", r, err)
	}
	delta := e.register(t, 7, "delta")
	e.outcome(t, 7, delta, 1, "ok", "")
	e.revoke(t, 8, delta)
	e.outcome(t, 8, delta, 1, "ok", "")
	eps := e.register(t, 9, "epsilon")
	e.outcome(t, 9, eps, 0, "rejected", "")
}

func TestSecretsViewGoldenFRRHZ135(t *testing.T) {
	e := newSecretEnv(t, false)
	if code, got := getRaw(t, e.url, "/v1/secrets"); code != 200 || got != `{"revision":0,"body":{"secrets":[]}}`+"\n" {
		t.Fatalf("empty: %d %q", code, got)
	}
	secretLifecycle(t, e)
	code, got := getRaw(t, e.url, "/v1/secrets")
	if code != 200 || got != secretsViewGoldenFRRHZ135 {
		t.Fatalf("view: %d\n%s", code, got)
	}
	// The envelope revision pairs with /v1/workspace (same definition).
	var sv, wv struct{ Revision uint64 }
	json.Unmarshal([]byte(got), &sv)
	_, ws := getRaw(t, e.url, "/v1/workspace")
	json.Unmarshal([]byte(ws), &wv)
	if sv.Revision == 0 || sv.Revision != wv.Revision || sv.Revision != uint64(len(e.store.All())) {
		t.Fatalf("revision %d vs workspace %d", sv.Revision, wv.Revision)
	}
	// /v1/workspace carries no secrets field, and neither does /v1/context.
	if strings.Contains(ws, `"secrets"`) || strings.Contains(ws, "sref-") {
		t.Fatalf("workspace leaks the secrets view: %s", ws)
	}
}

// TestSecretEventsLeaveContextBytesFRRHZ135: secret-only events do not
// change /v1/context (§4.1: never carried there), byte for byte.
func TestSecretEventsLeaveContextBytesFRRHZ135(t *testing.T) {
	store := fixture064(t)
	e := &secretEnv{store: store, plane: secretPlane(1)}
	e.svc = secretref.New(store, secretref.WithClock(func() time.Time { return secretT0 }))
	e.h = NewHTTP(store)
	e.h.Secrets = e.svc
	srv := httptest.NewServer(e.h.Handler())
	t.Cleanup(srv.Close)
	e.url = srv.URL
	paths := []string{"/v1/context?task=mission-x", "/v1/context?goal=goal-g"}
	before := map[string]string{}
	for _, p := range paths {
		code, body := getRaw(t, e.url, p)
		before[p] = fmt.Sprint(code) + " " + body
	}
	n := len(store.All())
	secretLifecycle(t, e)
	if code, got := e.post(t, registerBody(40, "zeta")); code != 200 || !strings.HasPrefix(got, `{"Accepted":true`) {
		t.Fatalf("%d %q", code, got)
	}
	if len(store.All()) <= n {
		t.Fatal("no secret events appended")
	}
	for _, p := range paths {
		code, body := getRaw(t, e.url, p)
		if after := fmt.Sprint(code) + " " + body; after != before[p] {
			t.Fatalf("%s changed:\n%s\n%s", p, before[p], after)
		}
		for _, bad := range []string{"secrets", "sref-", "op-"} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s contains %q", p, bad)
			}
		}
	}
	if before[paths[0]][:4] != "200 " {
		t.Fatalf("context fixture not served: %s", before[paths[0]])
	}
}

const secretsViewGoldenFRRHZ135 = `{"revision":16,"body":{"secrets":[` +
	`{"ref":"sref-02b733ce56a9cba83e1fe8c7","name":"alpha","kind":"provider-key","origin":"request","state":"revoking","version":2,"scopeTags":[],"pending":{"opId":"op-00000000000000000000000000000004","op":"revoke"},"lastOutcome":{"opId":"op-00000000000000000000000000000003","op":"rotate","result":"failed","failureCode":"backend-error"},"registeredAt":"2026-10-10T01:02:03Z","rotatedAt":"2026-10-10T01:02:03Z","observed":{"present":true,"version":2,"planeObservedAt":"2026-10-10T05:06:07Z"}},` +
	`{"ref":"sref-731b4fbdcfc45013e2f9dcd5","name":"beta","kind":"gateway-credential","origin":"attested","state":"active","version":1,"scopeTags":["ci","prod"],"registeredAt":"2026-10-10T01:02:03Z"},` +
	`{"ref":"sref-4ce715a2edfc8d544dee3344","name":"gamma","kind":"other","origin":"request","state":"pending-registration","version":0,"scopeTags":["dev"],"pending":{"opId":"op-00000000000000000000000000000006","op":"register"}},` +
	`{"ref":"sref-6575e7e25eba4c5bb608319c","name":"delta","kind":"provider-key","origin":"request","state":"revoked","version":1,"scopeTags":[],"lastOutcome":{"opId":"op-00000000000000000000000000000008","op":"revoke","result":"ok"},"registeredAt":"2026-10-10T01:02:03Z","revokedAt":"2026-10-10T01:02:03Z"},` +
	`{"ref":"sref-79da67e0ff4b3bd21329e57d","name":"epsilon","kind":"provider-key","origin":"request","state":"registration-failed","version":0,"scopeTags":[],"lastOutcome":{"opId":"op-00000000000000000000000000000009","op":"register","result":"rejected"}}` +
	`]}}` + "\n"

// walkSecretKeys applies the provenance key rule (same regex as
// internal/janusadapter/provenance_test.go secretKeyRE, unchanged) to every
// key, exempting exactly the container path body.secrets.
var secretKeyREFRRHZ135 = regexp.MustCompile(`(?i)token|secret|password|passwd|apikey|api_key|credential|bearer|path|filename`)

func secretKeyViolations(v any, at string, seen *int) []string {
	var bad []string
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			p := at + "." + k
			*seen++
			if secretKeyREFRRHZ135.MatchString(k) && p != "$.body.secrets" {
				bad = append(bad, p)
			}
			bad = append(bad, secretKeyViolations(c, p, seen)...)
		}
	case []any:
		for _, c := range x {
			bad = append(bad, secretKeyViolations(c, at+"[]", seen)...)
		}
	}
	return bad
}

func TestSecretsViewKeyWalkFRRHZ135(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "janusadapter", "provenance_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src, []byte("var secretKeyRE = regexp.MustCompile(`"+secretKeyREFRRHZ135.String()+"`)")) {
		t.Fatal("secretKeyRE copy drifted from janusadapter provenance_test.go")
	}
	e := newSecretEnv(t, false)
	secretLifecycle(t, e)
	_, body := getRaw(t, e.url, "/v1/secrets")
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	seen := 0
	if bad := secretKeyViolations(v, "$", &seen); len(bad) != 0 || seen < 30 {
		t.Fatalf("violations %v (walked %d keys)", bad, seen)
	}
	// The exemption is exactly one path: the container key matches the
	// regex, and the same key anywhere else is still flagged.
	for _, other := range []any{
		map[string]any{"secrets": []any{}},
		map[string]any{"body": map[string]any{"secrets": []any{map[string]any{"secrets": 1}}}},
		map[string]any{"body": map[string]any{"secrets": []any{map[string]any{"apiKey": 1, "valuePath": 2}}}},
	} {
		if bad := secretKeyViolations(other, "$", new(int)); len(bad) != 1 && len(bad) != 2 {
			t.Fatalf("non-exempt keys not flagged: %v", other)
		}
	}
}

func TestSecretOnlyAppendRepublishesWorkspaceSSEFRRHZ135(t *testing.T) {
	e := newSecretEnv(t, true)
	sse := openSSEFrames(t, e.url)
	snap := sse.next(t)
	_, ws0 := getRaw(t, e.url, "/v1/workspace")
	if code, got := e.post(t, registerBody(1, "alpha")); code != 200 || !strings.HasPrefix(got, `{"Accepted":true`) {
		t.Fatalf("%d %q", code, got)
	}
	frame := sse.next(t)
	if !strings.HasPrefix(frame, "event: projection\ndata: ") {
		t.Fatalf("frame = %q", frame)
	}
	var a, b struct {
		Revision uint64
		Body     json.RawMessage
	}
	json.Unmarshal([]byte(strings.TrimPrefix(snap, "event: snapshot\ndata: ")), &a)
	json.Unmarshal([]byte(strings.TrimPrefix(frame, "event: projection\ndata: ")), &b)
	if b.Revision != a.Revision+1 || !bytes.Equal(a.Body, b.Body) {
		t.Fatalf("revision %d → %d; body changed: %v", a.Revision, b.Revision, !bytes.Equal(a.Body, b.Body))
	}
	_, ws1 := getRaw(t, e.url, "/v1/workspace")
	if strings.Replace(ws0, `{"revision":0,`, `{"revision":1,`, 1) != ws1 {
		t.Fatalf("workspace body bytes changed:\n%s\n%s", ws0, ws1)
	}
	// A refusal writes nothing and publishes nothing.
	e.post(t, registerBody(2, "alpha"))
	sse.none(t, 200*time.Millisecond)
}

func TestSecretIntentPoisonFRRHZ135(t *testing.T) {
	p := &poisonPortFRRHZ144{store: &events.Store{}}
	svc := secretref.New(p)
	h := NewHTTP(p)
	h.Secrets = svc
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	ref := secretwire.DeriveRef(secretOp(1))
	if r, err := svc.RequestRegister(secretref.RegisterRequest{OpID: secretOp(1), Name: "alpha", RefKind: "other", Actor: "tester"}); err != nil || !r.Accepted {
		t.Fatal(r, err)
	}
	p.failNext.Store(true)
	check := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		if resp.StatusCode != 503 || b.String() != events.ErrPoisoned.Error() || resp.Header.Get("Content-Type") != "text/plain" {
			t.Fatalf("%d %q", resp.StatusCode, b.String())
		}
	}
	check(registerBody(2, "beta")) // this request's own append poisons
	if !p.Poisoned() {
		t.Fatal("not poisoned")
	}
	n := len(p.All())
	check(registerBody(3, "gamma"))
	check(`{"kind":"secret.op.outcome"}`)
	check(`{"kind":"secret.register.request","value":1}`)
	if res, err := RelayIntentHooks(p, Intent{Kind: secretref.KindRotateRequest, OpID: secretOp(4), Ref: ref}, "tester", trust.Authority{}, RelayHooks{Secrets: svc}); err != events.ErrPoisoned || res != (RelayResult{}) {
		t.Fatalf("internal: %+v %v", res, err)
	}
	if len(p.All()) != n {
		t.Fatal("wrote after poison")
	}
	code, body := getRaw(t, srv.URL, "/v1/secrets")
	if code != 200 || !strings.Contains(body, `"name":"alpha"`) {
		t.Fatalf("/v1/secrets after poison: %d %q", code, body)
	}
}

// TestSecretZVFRRHZ135: a crypto/rand sentinel in every rejected value
// position never reaches the journal file or any HTTP response.
func TestSecretZVFRRHZ135(t *testing.T) {
	s := secretSentinel(t)
	e := newSecretEnv(t, true)
	ms := mission.Service{Store: e.store}
	if _, err := ms.CreateGoal("goal-zv", "zv goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("zv-mission", "goal-zv", "zv mission", "done"); err != nil {
		t.Fatal(err)
	}
	sse := openSSEFrames(t, e.url)
	var responses bytes.Buffer
	ok := e.register(t, 1, "alpha")
	e.outcome(t, 1, ok, 1, "ok", "")
	op := secretOp(20)
	reg := `"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":"n","refKind":"other"`
	esc := ""
	for _, c := range s {
		esc += fmt.Sprintf(`\u%04x`, c)
	}
	bodies := []string{
		`{` + reg + `,"value":"` + s + `"}`,
		`{` + reg + `,"secretValue":"` + s + `"}`,
		`{"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":"` + s + `","name":"n","refKind":"other"}`,
		`{"kind":"secret.rotate.request","actor":"tester","opId":"` + op + `","ref":"` + s + `"}`,
		`{"kind":"secret.rotate.request","actor":"tester","opId":"` + op + `","ref":"sref-` + s + `"}`,
		"{" + reg + `,"value":"` + s + "\xff\xfe" + `"}`,
		`{` + reg + `,"value":"` + esc + `"}`,
		`{"kind":"secret.register.request","actor":"tester","opId":"` + op + `","name":"` + esc + `","refKind":"other"}`,
		`{"kind":"secret.op.outcome","value":"` + s + `"}`,
	}
	for _, b := range bodies {
		_, got := e.post(t, b)
		responses.WriteString(got)
	}
	// Go-internal malformed ref (no I1): fixed X7, nothing stored.
	res, _ := RelayIntentHooks(e.store, Intent{Kind: secretref.KindRotateRequest, OpID: op, Ref: s}, "tester", trust.Authority{}, RelayHooks{Secrets: e.svc})
	rb, _ := json.Marshal(res)
	responses.Write(rb)
	for _, path := range []string{"/v1/workspace", "/v1/secrets", "/v1/context?task=zv-mission", "/v1/context"} {
		_, got := getRaw(t, e.url, path)
		responses.WriteString(got)
	}
	responses.WriteString(sse.next(t)) // snapshot
	// The accepted register/outcome above were Service calls (no push); the
	// stream must not carry anything else either.
	for {
		select {
		case f := <-sse.ch:
			responses.WriteString(f)
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	assertNoSentinelForms(t, "HTTP responses", responses.Bytes(), s)
	assertNoSentinelForms(t, "journal file", e.journalBytes(t), s)
}
