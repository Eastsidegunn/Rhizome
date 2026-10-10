package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"rhizome/internal/secretwire"
)

func mustContain(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("output lacks %q:\n%s", want, out)
	}
}

func refState(t *testing.T, u *unit, ref string) (version uint64, revoked, found bool) {
	t.Helper()
	refs, err := u.pl.Refs()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Ref == ref {
			return r.Version, r.Revoked, true
		}
	}
	return 0, false, false
}

// TestSessionRequestFlowsFRRHZ139: register / rotate / revoke / reject of
// Rhizome requests through the scripted terminal loop. The value goes only to
// the values file; ledger, quarantine and terminal output carry no §8.1 form.
func TestSessionRequestFlowsFRRHZ139(t *testing.T) {
	s1, s2, s3, bad1, bad2 := sentinel(t), sentinel(t), sentinel(t), sentinel(t), sentinel(t)
	u := newUnit(t)
	var transcript string

	// register: Begin, value twice (no echo), typed "yes", Commit
	p1 := regPending(t, "svc", "a", "b")
	u.pending = []secretwire.Pending{p1}
	u.pw.push(s1, s1)
	out := u.run("pending", "next", "accept", "yes")
	transcript += out
	mustContain(t, out, "register opId="+p1.OpID)
	mustContain(t, out, "outcome: ok version 1 ref "+p1.Ref)
	if !bytes.Contains(u.file("values.json"), []byte(base64.StdEncoding.EncodeToString([]byte(s1)))) {
		t.Fatal("values file lacks the registered value")
	}

	// rotate: mismatched entries leave the op in progress; handling it again
	// completes it
	p2 := rotPending(t, p1.Ref, "svc")
	u.pending = []secretwire.Pending{p1, p2}
	u.pw.push(bad1, bad2)
	out = u.run("next", "accept")
	transcript += out
	mustContain(t, out, "the two entries differ")
	mustContain(t, out, msgStaysPending)
	if q := u.queue(); len(q) != 1 || q[0].Pending.OpID != p2.OpID {
		t.Fatal("rotate not queued")
	}
	u.pw.push(s2, s2)
	out = u.run("pending", "handle "+p2.OpID, "accept", "yes")
	transcript += out
	mustContain(t, out, "in-progress")
	mustContain(t, out, "outcome: ok version 2 ref "+p1.Ref)
	vals := u.file("values.json")
	if !bytes.Contains(vals, []byte(base64.StdEncoding.EncodeToString([]byte(s2)))) ||
		bytes.Contains(vals, []byte(base64.StdEncoding.EncodeToString([]byte(s1)))) {
		t.Fatal("rotate did not replace the value")
	}

	// revoke: wrong phrase records nothing; exact phrase revokes
	p3 := revPending(t, p1.Ref, "svc", secretwire.ReasonExposed)
	u.pending = []secretwire.Pending{p3}
	led := u.file("ledger.ndjson")
	out = u.run("next", "accept", "yes")
	transcript += out
	mustContain(t, out, msgVendorNote)
	mustContain(t, out, msgNotConfirmed)
	if !bytes.Equal(led, u.file("ledger.ndjson")) {
		t.Fatal("unconfirmed revoke wrote")
	}
	out = u.run("next", "accept", "revoke")
	transcript += out
	mustContain(t, out, "outcome: ok version 2 ref "+p1.Ref)
	if _, revoked, _ := refState(t, u, p1.Ref); !revoked {
		t.Fatal("not revoked")
	}
	if bytes.Contains(u.file("values.json"), []byte(base64.StdEncoding.EncodeToString([]byte(s2)))) {
		t.Fatal("revoke left the value")
	}

	// reject: typed "yes", no value read
	p4 := regPending(t, "other")
	u.pending = []secretwire.Pending{p4}
	out = u.run("next", "reject", "yes")
	transcript += out
	mustContain(t, out, "outcome: rejected version 0 ref "+p4.Ref)

	// skip and unknown action record nothing
	p5 := regPending(t, "third")
	u.pending = []secretwire.Pending{p5}
	led = u.file("ledger.ndjson")
	out = u.run("next", "skip", "next", "maybe", "next")
	transcript += out
	mustContain(t, out, "skipped")
	mustContain(t, out, msgInvalidInput)
	if !bytes.Equal(led, u.file("ledger.ndjson")) {
		t.Fatal("skip wrote")
	}

	if u.fatals != 0 {
		t.Fatal("fatal")
	}
	assertNoSentinel(t, "terminal", []byte(transcript+u.out.String()), s1, s2, s3, bad1, bad2)
	assertNoSentinel(t, "ledger", u.file("ledger.ndjson"), s1, s2, s3, bad1, bad2)
	assertNoSentinel(t, "quarantine", u.file("quarantine.ndjson"), s1, s2, s3, bad1, bad2)
	assertWording(t, "terminal", u.out.String()+helpText)
}

// TestSessionAttestedFRRHZ139: attested declaration and record-only rotate /
// revoke (Q2) never read a value and never touch the values file.
func TestSessionAttestedFRRHZ139(t *testing.T) {
	s := sentinel(t)
	u := newUnit(t)
	u.pw.push(s, s)
	out := u.run("register", "stored", "other", "", "yes")
	mustContain(t, out, "outcome: ok version 1")
	vals := u.file("values.json")

	out = u.run("attest", "broker", "provider-key", "z, a", "attest")
	mustContain(t, out, "interactive caller attestation")
	mustContain(t, out, "outcome: ok version 1 ref sref-")
	var ref string
	refs, _ := u.pl.Refs()
	for _, r := range refs {
		if r.Name == "broker" {
			ref = r.Ref
			if !r.Attested() || strings.Join(r.ScopeTags, ",") != "a,z" {
				t.Fatalf("%+v", r)
			}
		}
	}
	if ref == "" {
		t.Fatal("attested ref missing")
	}

	rot := rotPending(t, ref, "broker")
	u.pending = []secretwire.Pending{rot}
	out = u.run("next", "accept", "rotated externally")
	mustContain(t, out, "this ref is attested")
	mustContain(t, out, "outcome: ok version 2 ref "+ref)
	rev := revPending(t, ref, "broker", secretwire.ReasonRotatedOut)
	u.pending = []secretwire.Pending{rev}
	out = u.run("next", "accept", "revoke")
	mustContain(t, out, "outcome: ok version 2 ref "+ref)
	if v, revoked, _ := refState(t, u, ref); !revoked || v != 2 {
		t.Fatal("attested revoke")
	}
	if !bytes.Equal(vals, u.file("values.json")) {
		t.Fatal("attested operations touched the values file")
	}
	// observe refuses attested refs (no stored value)
	out = u.run("observe " + ref)
	mustContain(t, out, "error: unknown-ref")

	assertNoSentinel(t, "terminal", []byte(u.out.String()), s)
	assertNoSentinel(t, "ledger", u.file("ledger.ndjson"), s)
	assertWording(t, "terminal", u.out.String())
}

// TestSessionEpochReplaceFRRHZ139: epoch-replace needs the exact phrase,
// freezes the plane (requests are then ignored), and the next poll completes
// cutoff, quarantine and split; the quarantine command is value-free.
func TestSessionEpochReplaceFRRHZ139(t *testing.T) {
	s := sentinel(t)
	u := newUnit(t)
	u.pw.push(s, s)
	u.run("register", "late", "gateway-credential", "t1", "yes")
	ref := ""
	refs, _ := u.pl.Refs()
	if len(refs) == 1 {
		ref = refs[0].Ref
	}
	out := u.run("observe "+ref, "status", "refs")
	mustContain(t, out, "present=true version 1 at 2026-10-10T01:02:03Z")
	mustContain(t, out, "phase normal ack 0 outbox 2")

	out = u.run("epoch-replace", "replace")
	mustContain(t, out, msgNotConfirmed)
	if st, _ := u.pl.Status(); st.Phase != "normal" {
		t.Fatal("frozen without the phrase")
	}
	out = u.run("epoch-replace", "replace epoch", "status")
	mustContain(t, out, "phase frozen")

	// frozen: requests are shown but not actionable
	p := regPending(t, "after")
	u.pending = []secretwire.Pending{p}
	led := u.file("ledger.ndjson")
	out = u.run("pending", "next", "handle "+p.OpID)
	mustContain(t, out, "ignored while the epoch is frozen")
	mustContain(t, out, "no actionable request")
	if !bytes.Equal(led, u.file("ledger.ndjson")) {
		t.Fatal("frozen request wrote")
	}

	// the server's next poll (ack 0) records the cutoff and splits
	pid, _ := u.pl.HelloPlaneID()
	if err := u.pl.OnPoll(pid, secretwire.Ack{PlaneID: pid}, nil); err != nil {
		t.Fatal(err)
	}
	out = u.run("status", "quarantine", "unreconciled")
	mustContain(t, out, "phase split-recorded")
	if strings.Contains(out, pid+" phase") {
		t.Fatal("planeId did not change")
	}
	mustContain(t, out, pid+" 1 reconciled")
	mustContain(t, out, pid+" 2 observed - "+ref+" after-cutoff")
	mustContain(t, out, "no unreconciled refs")

	assertNoSentinel(t, "terminal", []byte(u.out.String()), s)
	assertNoSentinel(t, "ledger", u.file("ledger.ndjson"), s)
	assertNoSentinel(t, "quarantine", u.file("quarantine.ndjson"), s)
	if !bytes.Contains(u.file("values.json"), []byte(base64.StdEncoding.EncodeToString([]byte(s)))) {
		t.Fatal("values file lacks the value")
	}
}

// TestSessionInvalidInputFRRHZ139: invalid metadata is refused with a fixed
// message and never echoed; nothing is recorded.
func TestSessionInvalidInputFRRHZ139(t *testing.T) {
	s := sentinel(t)
	u := newUnit(t)
	led := u.file("ledger.ndjson")
	out := u.run("register", "Bad Name "+s, "attest", "ok", "nokind-"+s, "attest", "ok", "other", "b,b", "observe "+s, s)
	if strings.Count(out, msgInvalidInput) != 4 {
		t.Fatalf("output:\n%s", out)
	}
	mustContain(t, out, "unknown command")
	assertNoSentinel(t, "terminal", []byte(out), s)
	if !bytes.Equal(led, u.file("ledger.ndjson")) {
		t.Fatal("invalid input wrote")
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestPasswordBuffersZeroedFRRHZ139: every buffer returned by the non-echo
// reader is wiped by the session on success, mismatch and read-error paths.
func TestPasswordBuffersZeroedFRRHZ139(t *testing.T) {
	u := newUnit(t)
	// success (plane-direct register) and request register
	u.pw.push(sentinel(t), "")
	u.pw.vals[1] = append([]byte(nil), u.pw.vals[0]...)
	out := u.run("register", "zero", "other", "", "yes")
	mustContain(t, out, "outcome: ok version 1")
	p := regPending(t, "req")
	u.pending = []secretwire.Pending{p}
	s := sentinel(t)
	u.pw.push(s, s)
	out = u.run("next", "accept", "yes")
	mustContain(t, out, "outcome: ok version 1 ref "+p.Ref)
	// mismatch
	u.pw.push(sentinel(t), sentinel(t))
	out = u.run("register", "mis", "other", "")
	mustContain(t, out, "the two entries differ")
	// declined after entry
	u.pw.push(s, s)
	out = u.run("register", "dec", "other", "", "no")
	mustContain(t, out, msgNotConfirmed)
	if len(u.pw.given) != 8 {
		t.Fatalf("reads %d", len(u.pw.given))
	}
	for i, b := range u.pw.given {
		if len(b) == 0 || !allZero(b) {
			t.Fatalf("buffer %d not wiped", i)
		}
	}

	// read errors: a partial first or second read is wiped too
	for _, failAt := range []int{1, 2} {
		var given [][]byte
		n := 0
		pw := func() ([]byte, error) {
			n++
			b := []byte(sentinel(t))
			given = append(given, b)
			if n == failAt {
				return b, errors.New("read")
			}
			return b, nil
		}
		in := strings.NewReader("register\nerr\nother\n\n")
		var buf syncBuf
		newSession(u.pl, u.queue, in, &buf, pw, func() { t.Fatal("fatal") }).loop()
		if len(given) != failAt {
			t.Fatalf("reads %d", len(given))
		}
		for i, b := range given {
			if !allZero(b) {
				t.Fatalf("failAt %d: buffer %d not wiped", failAt, i)
			}
		}
	}
}
