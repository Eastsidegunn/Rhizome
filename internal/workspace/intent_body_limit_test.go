package workspace

// RHZ-107 independent review (FR-RHZ-135): POST /v1/intent buffers the body
// for the secret I1 check, so it is capped (413). Non-secret kinds still
// read only the first JSON value: a read error after it changes nothing.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failAfter struct{ r io.Reader }

func (f failAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func postIntentBody(t *testing.T, h http.Handler, body io.Reader) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/intent", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestIntentBodyLimitFRRHZ135(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	n := len(s.All())
	pad := strings.Repeat(" ", MaxIntentBodyBytes)
	for _, body := range []string{
		`{"kind":"goal.create","actor":"op","name":"big","success":"s"}` + pad,
		`{"kind":"secret.register.request","actor":"op","opId":"op-00000000000000000000000000000001","name":"a","refKind":"other"}` + pad,
		`{"kind":"goal.create","actor":"op","name":"` + strings.Repeat("x", MaxIntentBodyBytes) + `","success":"s"}`,
	} {
		code, resp := postIntentBody(t, h, strings.NewReader(body))
		// A body whose first value fits is decoded as before; only the
		// secret path reads past it (and hits the cap).
		if strings.HasPrefix(body, `{"kind":"goal.create","actor":"op","name":"big"`) {
			if code != 200 {
				t.Fatalf("non-secret first value under the cap: %d %q", code, resp)
			}
			continue
		}
		if code != http.StatusRequestEntityTooLarge || resp != "intent too large\n" {
			t.Fatalf("over cap: %d %q", code, resp)
		}
	}
	if got := len(s.All()); got != n+1 { // only the accepted goal.create
		t.Fatalf("writes: %d want %d", got-n, 1)
	}
}

func TestIntentReadErrorAfterFirstValueFRRHZ135(t *testing.T) {
	s := fixture064(t)
	h := NewHTTP(s).Handler()
	n := len(s.All())
	// Non-secret: the read error after the first value is never observed.
	code, resp := postIntentBody(t, h, failAfter{strings.NewReader(`{"kind":"goal.create","actor":"op","name":"g9","success":"s"}`)})
	if code != 200 || !strings.Contains(resp, `"Accepted":true`) {
		t.Fatalf("non-secret: %d %q", code, resp)
	}
	// A read error inside the first value stays 400 invalid json.
	code, resp = postIntentBody(t, h, failAfter{strings.NewReader(`{"kind":"goal.cr`)})
	if code != 400 || resp != "invalid json\n" {
		t.Fatalf("truncated: %d %q", code, resp)
	}
	// Secret: the rest must be read for I1, so the error is 400, zero writes.
	before := len(s.All())
	code, resp = postIntentBody(t, h, failAfter{strings.NewReader(`{"kind":"secret.register.request","actor":"op","opId":"op-00000000000000000000000000000002","name":"b","refKind":"other"}`)})
	if code != 400 || resp != "invalid json\n" || len(s.All()) != before {
		t.Fatalf("secret: %d %q writes %d", code, resp, len(s.All())-before)
	}
	if len(s.All()) != n+1 {
		t.Fatalf("writes: %d", len(s.All())-n)
	}
}
