package main

// RHZ-107 G1 (FR-RHZ-141, SCP §6): `rhizome secret …` goldens against a real
// serve and an httptest stub, fixed usage/error strings, and §8.1 ZV over
// the CLI's stdout and stderr on every path.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/secretwire"
)

// assertSecretZV is §8.1 target ②/④: no form of s in any CLI output.
func assertSecretZV(t *testing.T, s string, outs ...string) {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	forms := []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
	for i, o := range outs {
		for _, f := range forms {
			if strings.Contains(o, f) {
				t.Fatalf("output %d contains a sentinel form", i)
			}
		}
	}
}

func runSecret(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(append([]string{"secret"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

// secretStub is an httptest serve stand-in that records request bodies.
type secretStub struct {
	mu     sync.Mutex
	bodies []string
	hits   int
	srv    *httptest.Server
}

func newSecretStub(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) (*secretStub, string) {
	t.Helper()
	st := &secretStub{}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		st.mu.Lock()
		st.hits++
		st.bodies = append(st.bodies, string(b))
		st.mu.Unlock()
		h(w, r, b)
	}))
	t.Cleanup(st.srv.Close)
	return st, strings.TrimPrefix(st.srv.URL, "http://")
}

func (st *secretStub) last() (int, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.bodies) == 0 {
		return st.hits, ""
	}
	return st.hits, st.bodies[len(st.bodies)-1]
}

func opN(n int) string { return fmt.Sprintf("op-%032x", n) }

func TestSecretAgainstServeGoldenFRRHZ141(t *testing.T) {
	s := secretSentinel(t)
	jp := filepath.Join(t.TempDir(), "j.ndjson")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, done, eout := serveWithURL(t, ctx, jp, 0, nil)
	addr := strings.TrimPrefix(url, "http://")
	var all []string
	check := func(args []string, wantCode int, wantOut, wantErr string) {
		t.Helper()
		code, o, e := runSecret(append(args, "-addr", addr)...)
		all = append(all, o, e)
		if code != wantCode || o != wantOut || e != wantErr {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, o, e)
		}
	}
	// Empty list.
	check([]string{"list"}, 0, "ref\tname\tkind\tstate\tversion\torigin\tpending\n", "")
	check([]string{"list", "-json"}, 0, "[]\n", "")

	op := opN(1)
	ref := secretwire.DeriveRef(op)
	reg := []string{"register-request", "-name", "alpha", "-kind", "provider-key", "-scope-tag", "zeta", "-scope-tag", "beta", "-op-id", op}
	check(reg, 0, op+"\t"+ref+"\n", "")
	check(reg, 0, op+"\t"+ref+"\n", "") // opId idempotent: same answer, no write
	check([]string{"list"}, 0, "ref\tname\tkind\tstate\tversion\torigin\tpending\n"+
		ref+"\talpha\tprovider-key\tpending-registration\t0\trequest\tregister\n", "")

	// -json is the secrets array of GET /v1/secrets, byte for byte.
	res, err := http.Get(url + "/v1/secrets")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	i, j := bytes.Index(raw, []byte(`"secrets":`)), bytes.LastIndex(raw, []byte("}}"))
	if i < 0 || j < 0 {
		t.Fatalf("/v1/secrets %q", raw)
	}
	want := string(raw[i+len(`"secrets":`):j]) + "\n"
	if !strings.Contains(want, `"scopeTags":["beta","zeta"]`) {
		t.Fatalf("scope tags not sorted: %q", want)
	}
	check([]string{"list", "-json"}, 0, want, "")

	// Fixed §0.2 rejections from the real writer.
	check([]string{"register-request", "-name", "alpha", "-kind", "other", "-op-id", opN(2)}, 1, "",
		"secret: rejected: secret intent: name in use\n")
	check([]string{"register-request", "-name", "gamma", "-kind", "other", "-op-id", op}, 1, "",
		"secret: rejected: secret intent: opId conflict\n")
	check([]string{"rotate-request", "-ref", ref, "-op-id", opN(3)}, 1, "",
		"secret: rejected: secret intent: ref not active\n")
	check([]string{"revoke-request", "-ref", ref, "-reason", "exposed", "-op-id", opN(4)}, 1, "",
		"secret: rejected: secret intent: ref not active\n")
	check([]string{"rotate-request", "-ref", secretwire.DeriveRef(opN(99)), "-op-id", opN(5)}, 1, "",
		"secret: rejected: secret intent: unknown ref\n")
	// Sentinel in value positions: the server refuses it and the CLI only
	// prints the fixed §0.2 phrase (X2: I1 grammar is checked first).
	for _, args := range [][]string{
		{"rotate-request", "-ref", s},
		{"rotate-request", "-ref", ref, "-op-id", s},
		{"revoke-request", "-ref", ref, "-reason", s},
		{"register-request", "-name", "n", "-kind", s},
		{"register-request", "-name", "n", "-kind", "other", "-actor", s + "\xff"},
		{"register-request", "-name", "n", "-kind", "other", "-scope-tag", s + "\\"},
	} {
		check(args, 1, "", "secret: rejected: secret intent: malformed\n")
	}
	cancel()
	waitDone(t, done, eout, 10*time.Second)
	assertSecretZV(t, s, all...)
}

// acceptStub answers every POST /v1/intent as Accepted for the body's opId.
func acceptStub(t *testing.T) (*secretStub, string) {
	return newSecretStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path != "/v1/intent" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			OpID string `json:"opId"`
		}
		if json.Unmarshal(body, &in) != nil {
			http.Error(w, "invalid json", 400)
			return
		}
		fmt.Fprintf(w, `{"Accepted":true,"Reason":"","opId":%q,"ref":%q}`+"\n", in.OpID, secretwire.DeriveRef(in.OpID))
	})
}

func TestSecretRequestGoldenStubFRRHZ141(t *testing.T) {
	s := secretSentinel(t)
	st, addr := acceptStub(t)
	var all []string
	ref := secretwire.DeriveRef(opN(7))
	for _, tc := range []struct {
		args []string
		op   string
		body string
	}{
		{[]string{"register-request", "-name", "n1", "-kind", "other", "-op-id", opN(1)}, opN(1),
			`{"kind":"secret.register.request","actor":"rhizome-cli","opId":"` + opN(1) + `","name":"n1","refKind":"other"}` + "\n"},
		{[]string{"register-request", "-scope-tag", "b", "-name", "n2", "-scope-tag", "a", "-kind", "gateway-credential", "-op-id", opN(2), "-actor", "ops"}, opN(2),
			`{"kind":"secret.register.request","actor":"ops","opId":"` + opN(2) + `","name":"n2","refKind":"gateway-credential","scopeTags":["a","b"]}` + "\n"},
		{[]string{"rotate-request", "-ref", ref, "-op-id", opN(3)}, opN(3),
			`{"kind":"secret.rotate.request","actor":"rhizome-cli","opId":"` + opN(3) + `","ref":"` + ref + `"}` + "\n"},
		{[]string{"revoke-request", "-ref", ref, "-reason", "rotated-out", "-op-id", opN(4), "-actor", "a"}, opN(4),
			`{"kind":"secret.revoke.request","actor":"a","opId":"` + opN(4) + `","ref":"` + ref + `","reasonCode":"rotated-out"}` + "\n"},
	} {
		code, o, e := runSecret(append(tc.args, "-addr", addr)...)
		all = append(all, o, e)
		_, body := st.last()
		if code != 0 || e != "" || body != tc.body || o != tc.op+"\t"+secretwire.DeriveRef(tc.op)+"\n" {
			t.Fatalf("%v: exit %d stdout %q stderr %q body %q", tc.args, code, o, e, body)
		}
	}
	// Default -op-id is crypto/rand G1, and stdout names it.
	code, o, e := runSecret("rotate-request", "-ref", ref, "-addr", addr)
	_, body := st.last()
	parts := strings.Split(strings.TrimSuffix(o, "\n"), "\t")
	if code != 0 || e != "" || len(parts) != 2 || !secretwire.ValidOpID(parts[0]) || parts[1] != secretwire.DeriveRef(parts[0]) ||
		body != `{"kind":"secret.rotate.request","actor":"rhizome-cli","opId":"`+parts[0]+`","ref":"`+ref+`"}`+"\n" {
		t.Fatalf("default op-id: exit %d stdout %q stderr %q body %q", code, o, e, body)
	}
	code2, o2, e2 := runSecret("rotate-request", "-ref", ref, "-addr", addr)
	if code2 != 0 || o2 == o {
		t.Fatal("default op-id repeated")
	}
	assertSecretZV(t, s, append(all, o, e, o2, e2)...)
}

// secretsStub serves GET /v1/secrets with body and 404 elsewhere.
func secretsStub(t *testing.T, status int, body string) (*secretStub, string) {
	return newSecretStub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/v1/secrets" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func TestSecretListGoldenStubFRRHZ141(t *testing.T) {
	r1, r2, r3 := secretwire.DeriveRef(opN(1)), secretwire.DeriveRef(opN(2)), secretwire.DeriveRef(opN(3))
	rows := `[{"ref":"` + r1 + `","name":"alpha","kind":"provider-key","origin":"request","state":"rotating","version":2,"scopeTags":["a","b"],` +
		`"pending":{"opId":"` + opN(9) + `","op":"rotate"},"lastOutcome":{"opId":"` + opN(8) + `","op":"rotate","result":"ok"},` +
		`"registeredAt":"2026-10-01T00:00:00Z","rotatedAt":"2026-10-02T00:00:00Z","observed":{"present":true,"version":2,"planeObservedAt":"2026-10-03T00:00:00Z"}},` +
		`{"ref":"` + r2 + `","name":"beta","kind":"other","origin":"plane-direct","state":"revoked","version":1,"scopeTags":[],` +
		`"lastOutcome":{"opId":"` + opN(7) + `","op":"revoke","result":"failed","failureCode":"backend-error"},"registeredAt":"2026-10-01T00:00:00Z","revokedAt":"2026-10-04T00:00:00Z"},` +
		`{"ref":"` + r3 + `","name":"gamma","kind":"gateway-credential","origin":"attested","state":"active","version":1,"scopeTags":[]}]`
	_, addr := secretsStub(t, 200, `{"revision":12,"body":{"secrets":`+rows+`}}`+"\n")
	code, o, e := runSecret("list", "-addr", addr)
	want := "ref\tname\tkind\tstate\tversion\torigin\tpending\n" +
		r1 + "\talpha\tprovider-key\trotating\t2\trequest\trotate\n" +
		r2 + "\tbeta\tother\trevoked\t1\tplane-direct\t-\n" +
		r3 + "\tgamma\tgateway-credential\tactive\t1\tattested\t-\n"
	if code != 0 || o != want || e != "" {
		t.Fatalf("list: exit %d stdout %q stderr %q", code, o, e)
	}
	code, o, e = runSecret("list", "-json", "-addr", addr)
	if code != 0 || o != rows+"\n" || e != "" {
		t.Fatalf("list -json: exit %d stdout %q stderr %q", code, o, e)
	}
	// The empty projection is the literal [] from serve; -json echoes it.
	_, addr = secretsStub(t, 200, `{"revision":0,"body":{"secrets":[]}}`+"\n")
	if code, o, e := runSecret("list", "-json", "-addr", addr); code != 0 || o != "[]\n" || e != "" {
		t.Fatalf("list -json empty: exit %d stdout %q stderr %q", code, o, e)
	}
	// An absent or null array is not the §4.2 envelope (serve always sends
	// []). An earlier draft printed [] for these; Codex review: they are
	// malformed answers and must be a serve error, not an empty registry.
	for _, b := range []string{`{"revision":0,"body":{"secrets":null}}`, `{"revision":0,"body":{}}`, `{}`, `null`, `[]`} {
		_, addr := secretsStub(t, 200, b)
		for _, args := range [][]string{{"list", "-addr", addr}, {"list", "-json", "-addr", addr}} {
			if code, o, e := runSecret(args...); code != 1 || o != "" || e != "secret: serve error\n" {
				t.Fatalf("%s %v: exit %d stdout %q stderr %q", b, args, code, o, e)
			}
		}
	}
}

// TestSecretUsageFRRHZ141: extra positional args, unknown flags, missing or
// unknown subcommands, missing required flags and duplicate -scope-tag all
// exit 2 with only "secret: usage", send nothing, and echo nothing.
func TestSecretUsageFRRHZ141(t *testing.T) {
	s := secretSentinel(t)
	st, addr := acceptStub(t)
	ref := secretwire.DeriveRef(opN(1))
	reg := []string{"register-request", "-addr", addr, "-name", "n", "-kind", "other"}
	var all []string
	for _, args := range [][]string{
		{},
		{s},
		{"-" + s},
		{"list", s, "-addr", addr},
		{"list", "-addr", addr, s},
		{"list", "-addr", addr, "-" + s},
		{"list", "-addr", addr, "--" + s + "=x"},
		{"list", "-addr", addr, "-json=" + s},
		{"list", "-h"},
		{"list", "-addr"},
		append(append([]string{}, reg...), s),
		append(append([]string{}, reg...), "-"+s),
		append(append([]string{}, reg...), "-"+s+"="+s),
		append(append([]string{}, reg...), "-scope-tag", s, "-scope-tag", s),
		append(append([]string{}, reg...), "-scope-tag", "a", "-scope-tag", "b", "-scope-tag", "a"),
		{"register-request", "-addr", addr, "-name", "n"},
		{"register-request", "-addr", addr, "-kind", "other"},
		{"register-request", "-addr", addr, "-name", "n", "-kind", "other", "-ref", ref},
		{"rotate-request", "-addr", addr},
		{"rotate-request", "-addr", addr, "-ref", ref, s},
		{"rotate-request", "-addr", addr, "-ref", ref, "-reason", "unused"},
		{"rotate-request", "-addr", addr, "-ref", ref, "-value", s},
		{"revoke-request", "-addr", addr, "-ref", ref},
		{"revoke-request", "-addr", addr, "-reason", "unused"},
		{"revoke-request", "-addr", addr, "-ref", ref, "-reason", "unused", "--", s},
		{"revoke-request", "-addr", addr, "-ref", ref, "-reason", "unused", "-secret", s},
	} {
		code, o, e := runSecret(args...)
		all = append(all, o, e)
		if code != 2 || o != "" || e != "secret: usage\n" {
			t.Fatalf("%q: exit %d stdout %q stderr %q", args, code, o, e)
		}
	}
	if hits, _ := st.last(); hits != 0 {
		t.Fatalf("usage errors sent %d requests", hits)
	}
	assertSecretZV(t, s, all...)
}

// TestSecretRejectedReasonFRRHZ141: a §0.2 Reason is printed after the fixed
// prefix; any other Reason is never echoed.
func TestSecretRejectedReasonFRRHZ141(t *testing.T) {
	s := secretSentinel(t)
	var reason string
	_, addr := newSecretStub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		fmt.Fprintf(w, `{"Accepted":false,"Reason":%q}`, reason)
	})
	ref := secretwire.DeriveRef(opN(1))
	var all []string
	for _, tc := range []struct{ reason, want string }{
		{"secret intent: ref busy", "secret: rejected: secret intent: ref busy\n"},
		{"secret registry unavailable", "secret: rejected: secret registry unavailable\n"},
		{"secret facts are channel-only", "secret: rejected: secret facts are channel-only\n"},
		{"secret intent: " + s, "secret: rejected\n"},
		{s, "secret: rejected\n"},
		{"secret intent: ref busy " + s, "secret: rejected\n"},
		{"Secret intent: ref busy", "secret: rejected\n"},
		{"", "secret: rejected\n"},
	} {
		reason = tc.reason
		code, o, e := runSecret("rotate-request", "-ref", ref, "-addr", addr)
		all = append(all, o, e)
		if code != 1 || o != "" || e != tc.want {
			t.Fatalf("reason %q: exit %d stdout %q stderr %q", tc.reason, code, o, e)
		}
	}
	assertSecretZV(t, s, all...)
}

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

// TestSecretTransportErrorsFRRHZ141: unreachable, non-200 and a missing
// /v1/secrets route print fixed strings; bodies and -addr are not echoed.
func TestSecretTransportErrorsFRRHZ141(t *testing.T) {
	s := secretSentinel(t)
	ref := secretwire.DeriveRef(opN(1))
	var all []string
	check := func(args []string, want string) {
		t.Helper()
		code, o, e := runSecret(args...)
		all = append(all, o, e)
		if code != 1 || o != "" || e != want {
			t.Fatalf("%q: exit %d stdout %q stderr %q", args, code, o, e)
		}
	}
	gone := closedAddr(t)
	for _, a := range []string{gone, gone + "/" + s, "127.0.0.1:" + s} {
		check([]string{"list", "-addr", a}, "secret: serve unreachable\n")
		check([]string{"rotate-request", "-ref", ref, "-addr", a}, "secret: serve unreachable\n")
	}
	for _, status := range []int{400, 403, 500, 503} {
		_, addr := newSecretStub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			http.Error(w, s, status)
		})
		check([]string{"list", "-addr", addr}, "secret: serve error\n")
		check([]string{"rotate-request", "-ref", ref, "-addr", addr}, "secret: serve error\n")
	}
	// 404: the projection route is missing → unavailable; for an intent a
	// 404 is just a serve error.
	_, addr := newSecretStub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		http.Error(w, s, http.StatusNotFound)
	})
	check([]string{"list", "-addr", addr}, "secret: unavailable\n")
	check([]string{"list", "-json", "-addr", addr}, "secret: unavailable\n")
	check([]string{"register-request", "-name", "n", "-kind", "other", "-addr", addr}, "secret: serve error\n")
	// 200 with a body that is not the expected shape, or carries fields
	// outside the grammar, is a serve error and nothing of it is printed.
	r1 := secretwire.DeriveRef(opN(1))
	row := func(name string) string {
		return `{"revision":1,"body":{"secrets":[{"ref":"` + r1 + `","name":"` + name + `","kind":"other","origin":"request","state":"active","version":1,"scopeTags":[]}]}}`
	}
	for _, b := range []string{s, `{"body":{"secrets":"` + s + `"}}`, row("UPPER" + s), row(s + " x"),
		strings.Replace(row("n"), `"state":"active"`, `"state":"`+s+`"`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":["`+s+`","a"]`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":[],"pending":{"opId":"`+s+`","op":"rotate"}`, 1),
		strings.Replace(row("n"), `"version":1`, `"version":-1`, 1),
		// Exact §4.2 shape (Codex review): missing required, unknown,
		// duplicate or null keys, and G8 failureCode-iff-failed.
		strings.Replace(row("n"), `"name":"n",`, ``, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":[],"value":"`+s+`"`, 1),
		strings.Replace(row("n"), `"name":"n"`, `"name":"`+s+`","name":"n"`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":null`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":[],"pending":null`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":[],"lastOutcome":{"opId":"`+opN(2)+`","op":"rotate","result":"ok","failureCode":"backend-error"}`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":[],"lastOutcome":{"opId":"`+opN(2)+`","op":"rotate","result":"failed"}`, 1),
		strings.Replace(row("n"), `"scopeTags":[]`, `"scopeTags":[],"observed":{"present":"`+s+`","version":1,"planeObservedAt":"2026-10-03T00:00:00Z"}`, 1),
		strings.Replace(row("n"), `"revision":1,`, ``, 1),
		strings.Replace(row("n"), `"revision":1,`, `"revision":1,"`+s+`":1,`, 1),
		row("n") + s,
		row("n") + row("n")} {
		_, addr := secretsStub(t, 200, b)
		check([]string{"list", "-addr", addr}, "secret: serve error\n")
		check([]string{"list", "-json", "-addr", addr}, "secret: serve error\n")
	}
	// An accepted answer that does not match the sent opId / derived ref.
	for _, b := range []string{s,
		`{"Accepted":true,"Reason":"","opId":"` + s + `","ref":"` + ref + `"}`,
		`{"Accepted":true,"Reason":"","opId":"` + opN(1) + `","ref":"` + s + `"}`,
		`{"Accepted":true,"Reason":"","opId":"` + opN(1) + `","ref":"` + secretwire.DeriveRef(opN(2)) + `"}`,
		`{"Accepted":true,"Reason":"` + s + `"}`,
		// Exact RelayResult shape (Codex review): opId/ref only when
		// accepted, Reason empty when accepted, no unknown/dup/null keys.
		`{"Accepted":true,"Reason":"secret intent: ref busy","opId":"` + opN(1) + `","ref":"` + ref + `"}`,
		`{"Accepted":false,"Reason":"secret intent: ref busy","opId":"` + opN(1) + `","ref":"` + ref + `"}`,
		`{"Accepted":true,"opId":"` + opN(1) + `","ref":"` + ref + `"}`,
		`{"Accepted":true,"Reason":"","opId":"` + opN(1) + `","ref":"` + ref + `","` + s + `":1}`,
		`{"Accepted":false,"Reason":"secret intent: ref busy","Reason":"` + s + `"}`,
		`{"Accepted":false,"Reason":null}`,
		`{"Accepted":"` + s + `","Reason":""}`,
		`{"Accepted":false,"Reason":"` + s + `\u0041"}`,
		`{"Accepted":false,"Reason":""}` + s,
		`{}`, `null`} {
		_, addr := newSecretStub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { io.WriteString(w, b) })
		check([]string{"rotate-request", "-ref", ref, "-op-id", opN(1), "-addr", addr}, "secret: serve error\n")
	}
	assertSecretZV(t, s, all...)
}

// TestSecretOpIDSourceFailureFRRHZ141: if crypto/rand fails, nothing is sent.
func TestSecretOpIDSourceFailureFRRHZ141(t *testing.T) {
	st, addr := acceptStub(t)
	old := secretNewOpID
	secretNewOpID = func() (string, error) { return "", errors.New("no entropy") }
	t.Cleanup(func() { secretNewOpID = old })
	code, o, e := runSecret("rotate-request", "-ref", secretwire.DeriveRef(opN(1)), "-addr", addr)
	if hits, _ := st.last(); code != 1 || o != "" || e != "secret: unavailable\n" || hits != 0 {
		t.Fatalf("exit %d stdout %q stderr %q hits %d", code, o, e, hits)
	}
}

// TestSecretDefaultAddrFRRHZ141: the CLI's -addr default is serve's.
func TestSecretDefaultAddrFRRHZ141(t *testing.T) {
	if secretDefaultAddr != "127.0.0.1:8080" {
		t.Fatalf("default addr %q", secretDefaultAddr)
	}
}

// TestSecretNoRedirectFRRHZ141 (Codex review): a 3xx is a non-200 answer;
// the CLI never follows it, so a POST is never replayed to another target.
func TestSecretNoRedirectFRRHZ141(t *testing.T) {
	s := secretSentinel(t)
	target, taddr := acceptStub(t)
	ref := secretwire.DeriveRef(opN(1))
	var all []string
	for _, status := range []int{301, 302, 303, 307, 308} {
		_, addr := newSecretStub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			http.Redirect(w, r, "http://"+taddr+r.URL.Path+"?"+s, status)
		})
		for _, args := range [][]string{
			{"list", "-addr", addr},
			{"rotate-request", "-ref", ref, "-op-id", opN(1), "-addr", addr},
		} {
			code, o, e := runSecret(args...)
			all = append(all, o, e)
			if code != 1 || o != "" || e != "secret: serve error\n" {
				t.Fatalf("%d %v: exit %d stdout %q stderr %q", status, args, code, o, e)
			}
		}
	}
	if hits, _ := target.last(); hits != 0 {
		t.Fatalf("redirect followed %d times", hits)
	}
	assertSecretZV(t, s, all...)
}
