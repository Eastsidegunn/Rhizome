package janusadapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"rhizome/internal/approval"
	"testing"
)

func fake(script func(map[string]any) map[string]any) Dialer {
	return func() (net.Conn, error) {
		srv, cli := net.Pipe()
		go func() {
			defer srv.Close()
			r, _ := bufio.NewReader(srv).ReadBytes('\n')
			var q map[string]any
			_ = json.Unmarshal(r, &q)
			b, _ := json.Marshal(script(q))
			srv.Write(append(b, '\n'))
		}()
		return cli, nil
	}
}
func key() approval.RequestKey {
	return approval.RequestKey{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", RequestID: "r"}
}
func TestSubmitWireExactFRRHZ076(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		want := map[string]bool{"op": true, "trace_id": true, "span_id": true, "request_id": true, "response_id": true, "decision": true, "reason": true}
		for k := range want {
			if _, ok := q[k]; !ok {
				t.Fatalf("missing %s", k)
			}
		}
		for k := range q {
			if !want[k] {
				t.Fatalf("unexpected %s", k)
			}
		}
		return map[string]any{"status": "pending"}
	})}
	if _, e := c.Submit(key(), "resp", approval.Allow, ""); e != nil {
		t.Fatal(e)
	}
}
func TestDenyReasonRequiredFRRHZ076(t *testing.T) {
	called := false
	c := Client{Dial: func() (net.Conn, error) { called = true; return nil, nil }}
	if _, e := c.Submit(key(), "r", approval.Deny, ""); e == nil || called {
		t.Fatal()
	}
}
func TestQueryDecidedNoObserveFRRHZ076(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "decided", "decision": "allow", "response_seq": 0}
	})}
	r, e := c.Query(key())
	if e != nil || r.Status != "decided" {
		t.Fatal(e)
	}
}

func TestIdempotentAndConflictFRRHZ076(t *testing.T) {
	calls := 0
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		calls++
		if calls == 2 {
			return map[string]any{"status": "error", "reason": "RESPONSE_CONFLICT"}
		}
		return map[string]any{"status": "decided", "decision": "allow", "response_seq": 0}
	})}
	if _, e := c.Submit(key(), "r", approval.Allow, ""); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Submit(key(), "r", approval.Allow, ""); e == nil || e.Error() != "RESPONSE_CONFLICT" {
		t.Fatalf("conflict not surfaced: %v", e)
	}
}
func TestPendingMetaFRRHZ076(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "pending", "request_digest": "hx-args-digest-v1:x", "policy_hash": "p", "display_summary": "s", "expires_at": 3}
	})}
	r, e := c.Query(key())
	if e != nil || r.Status != "pending" || r.RequestDigest == "" || r.ExpiresAt != 3 {
		t.Fatal(e, r)
	}
}
func TestExpiredDenyFRRHZ076(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "expired", "decision": "deny", "reason": "EXPIRED"}
	})}
	r, e := c.Query(key())
	if e != nil || r.Decision != approval.Deny || r.Reason != "EXPIRED" {
		t.Fatal(e)
	}
}
func TestForwardedUnknownRejectedFRRHZ076(t *testing.T) {
	for _, st := range []string{"forwarded", "weird"} {
		c := Client{Dial: fake(func(q map[string]any) map[string]any { return map[string]any{"status": st} })}
		if _, e := c.Query(key()); e == nil {
			t.Fatal(st)
		}
	}
}
func TestRestartQueryOnlyFRRHZ076(t *testing.T) {
	submit := false
	c := Client{Dial: func() (net.Conn, error) {
		srv, cli := net.Pipe()
		go func() {
			defer srv.Close()
			r, _ := bufio.NewReader(srv).ReadBytes('\n')
			var q map[string]any
			_ = json.Unmarshal(r, &q)
			if q["op"] == "submit" {
				submit = true
			}
			b, _ := json.Marshal(map[string]any{"status": "decided", "decision": "allow", "response_seq": 0})
			srv.Write(append(b, '\n'))
		}()
		return cli, nil
	}}
	if _, e := c.Query(key()); e != nil || submit {
		t.Fatal(e)
	}
}
func TestUnavailableFRRHZ076(t *testing.T) {
	c := Client{Dial: func() (net.Conn, error) { return nil, errors.New("down") }}
	if _, e := c.Query(key()); !errors.Is(e, ErrUnavailable) {
		t.Fatalf("want unavailable: %v", e)
	}
}
func TestUnavailableWriteReadFRRHZ076(t *testing.T) {
	c := Client{Dial: func() (net.Conn, error) { srv, cli := net.Pipe(); srv.Close(); return cli, nil }}
	if _, e := c.Query(key()); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestMalformedResponseFRRHZ076(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any { return map[string]any{} })}
	if _, e := c.Query(key()); e == nil || errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestDigestOpaqueMismatchFRRHZ076(t *testing.T) {
	if VerifyDigest("hx-args-digest-v1:a", "hx-args-digest-v1:a") != nil {
		t.Fatal("match must pass")
	}
	if !errors.Is(VerifyDigest("hx-args-digest-v1:a", "hx-args-digest-v1:b"), ErrDigestMismatch) {
		t.Fatal("mismatch")
	}
	if !errors.Is(VerifyDigest("a", "hx-args-digest-v1:a"), ErrDigestMismatch) {
		t.Fatal("local prefix")
	}
	if !errors.Is(VerifyDigest("hx-args-digest-v1:a", "a"), ErrDigestMismatch) {
		t.Fatal("remote prefix")
	}
}
func TestObservedContradictionNoPromotionFRRHZ076(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "decided", "decision": "deny", "response_seq": 0}
	})}
	r, e := c.Query(key())
	if e != nil || r.Decision != approval.Deny {
		t.Fatal(e)
	}
}
