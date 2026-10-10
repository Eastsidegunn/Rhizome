package secretwire

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

var (
	testRef   = DeriveRef(testOpID)
	testOpID2 = "op-ffeeddccbbaa99887766554433221100"
	testAck   = Ack{PlaneID: testPlaneID, PlaneEntrySeq: 10, JournalSeq: 42}
)

func testEntries() []Entry {
	return []Entry{
		{Seq: 11, Fact: FactReconciled, OpID: testOpID, Ref: testRef, Name: "openai", RefKind: RefKindProviderKey, Version: 1, Origin: OriginPlaneDirect, ScopeTags: []string{"dev", "prod"}},
		{Seq: 12, Fact: FactOutcome, OpID: testOpID, Ref: testRef, Version: 1, Result: ResultOK},
		{Seq: 13, Fact: FactOutcome, OpID: testOpID2, Ref: testRef, Version: 2, Result: ResultFailed, FailureCode: FailureNoTTY},
		{Seq: 14, Fact: FactObserved, Ref: testRef, Version: 1, Present: false, PlaneObservedAt: "2026-10-10T00:00:00Z"},
		{Seq: 15, Fact: FactReconciled, OpID: testOpID2, Ref: DeriveRef(testOpID2), Name: "gw", RefKind: RefKindOther, Version: 1, Origin: OriginAttested},
		{Seq: 16, Fact: FactOutcome, OpID: testOpID2, Ref: testRef, Version: 2, Result: ResultRejected},
	}
}

func testPending() []Pending {
	return []Pending{
		{OpID: testOpID, Op: OpRegister, Ref: testRef, Name: "openai", RefKind: RefKindProviderKey, ScopeTags: []string{"a", "b"}, JournalSeq: 7},
		{OpID: testOpID2, Op: OpRotate, Ref: testRef, Name: "openai", RefKind: RefKindProviderKey, JournalSeq: 8},
		{OpID: "op-" + strings.Repeat("1", 32), Op: OpRevoke, Ref: testRef, Name: "openai", RefKind: RefKindProviderKey, ReasonCode: ReasonExposed, JournalSeq: 9},
	}
}

func mustEncode(t *testing.T) func([]byte, error) []byte {
	return func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	}
}

// FR-RHZ-136: every §3.3 frame type round-trips encode → decode unchanged,
// and re-encoding is byte-identical (deterministic).
func TestFrameRoundTripFRRHZ136(t *testing.T) {
	must := mustEncode(t)
	hello := must(EncodeRhizomeHello())
	if string(hello) != `{"v":1,"type":"hello","role":"rhizome","versions":[1]}`+"\n" {
		t.Fatalf("rhizome hello golden: %s", hello)
	}
	f, err := DecodeFromRhizome(hello, TypeHello, 0)
	if err != nil || f.Type != TypeHello || !reflect.DeepEqual(f.RhizomeHello.Versions, []uint64{1}) {
		t.Fatalf("rhizome hello: %+v %v", f, err)
	}
	if v, ok := Negotiate(f.RhizomeHello.Versions); !ok || v != 1 {
		t.Fatal("negotiate")
	}
	if _, ok := Negotiate([]uint64{2, 3}); ok {
		t.Fatal("negotiated an unsupported version")
	}

	ph := PlaneHello{Version: 1, PlaneID: testPlaneID}
	b := must(EncodePlaneHello(ph))
	if string(b) != `{"v":1,"type":"hello","role":"plane","version":1,"planeId":"`+testPlaneID+`"}`+"\n" {
		t.Fatalf("plane hello golden: %s", b)
	}
	if f, err := DecodeFromPlane(b, TypeHello, 0); err != nil || f.PlaneHello != ph {
		t.Fatalf("plane hello: %+v %v", f, err)
	}

	polls := []Poll{
		{V: 1, Pending: testPending(), Ack: testAck, LastRejected: &Rejected{Seq: 11, Reason: EntrySeqGap}},
		{V: 1, Pending: []Pending{}, Ack: Ack{PlaneID: testPlaneID}},
	}
	for _, p := range polls {
		b := must(EncodePoll(p))
		f, err := DecodeFromRhizome(b, TypePoll, 1)
		if err != nil || !reflect.DeepEqual(f.Poll, p) {
			t.Fatalf("poll: %+v %v", f.Poll, err)
		}
		if again := must(EncodePoll(f.Poll)); !bytes.Equal(again, b) {
			t.Fatal("poll re-encode differs")
		}
	}
	if b := must(EncodePoll(Poll{V: 1, Ack: Ack{PlaneID: testPlaneID}})); !strings.Contains(string(b), `"pending":[]`) || strings.Contains(string(b), "lastRejected") {
		t.Fatalf("nil pending must encode as []: %s", b)
	}

	batches := []Batch{
		{V: 1, PlaneID: testPlaneID, Entries: testEntries()},
		{V: 1, PlaneID: testPlaneID, Entries: []Entry{}},
	}
	for _, bt := range batches {
		b := must(EncodeBatch(bt))
		f, err := DecodeFromPlane(b, TypeBatch, 1)
		if err != nil || !reflect.DeepEqual(f.Batch, bt) {
			t.Fatalf("batch: %+v %v", f.Batch, err)
		}
		if again := must(EncodeBatch(f.Batch)); !bytes.Equal(again, b) {
			t.Fatal("batch re-encode differs")
		}
		if CheckBatchPlane(f.Batch, testPlaneID) != nil {
			t.Fatal("matching planeId rejected")
		}
	}
	// Conditional keys appear only where §3.3 allows them.
	b = must(EncodeBatch(Batch{V: 1, PlaneID: testPlaneID, Entries: testEntries()[1:2]}))
	for _, k := range []string{"failureCode", "present", "scopeTags", "origin"} {
		if strings.Contains(string(b), k) {
			t.Fatalf("ok outcome carries %s", k)
		}
	}

	errs := []ErrorFrame{
		{Code: CodeUnsupportedVersion}, {Code: CodeMalformed}, {Code: CodePeerRejected}, {Code: CodeBusy},
		{Code: CodeRetiredPlane}, {Code: CodeJournalPoisoned},
		{Code: CodeEntryRejected, Reason: EntrySeqConflict, Ack: &testAck, RejectedSeq: 9},
	}
	for _, e := range errs {
		b := must(EncodeError(e))
		f, err := DecodeFromRhizome(b, TypePoll, 1) // an error frame is admissible at any step
		if err != nil || f.Type != TypeError || !reflect.DeepEqual(f.Error, e) {
			t.Fatalf("error %s: %+v %v", e.Code, f.Error, err)
		}
		_, err = DecodeFromPlane(b, TypeBatch, 1)
		shared := e.Code == CodeUnsupportedVersion || e.Code == CodeMalformed || e.Code == CodePeerRejected || e.Code == CodeBusy
		if shared != (err == nil) {
			t.Fatalf("plane error code %s: %v", e.Code, err)
		}
	}
}

// FR-RHZ-136: encoders refuse to emit frames their peer would reject.
func TestEncodeRejectsInvalidFRRHZ136(t *testing.T) {
	gap := []Entry{testEntries()[1], testEntries()[3]}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"seq gap", second(EncodeBatch(Batch{V: 1, PlaneID: testPlaneID, Entries: gap})), ErrFrameSeqOrder},
		{"unknown fact", second(EncodeBatch(Batch{V: 1, PlaneID: testPlaneID, Entries: []Entry{{Seq: 1, Fact: "value"}}})), ErrFrameMalformed},
		{"failed without code", second(EncodeBatch(Batch{V: 1, PlaneID: testPlaneID, Entries: []Entry{{Seq: 1, Fact: FactOutcome, OpID: testOpID, Ref: testRef, Result: ResultFailed}}})), ErrFrameMalformed},
		{"unsupported batch version", second(EncodeBatch(Batch{V: 2, PlaneID: testPlaneID})), ErrFrameBadVersion},
		{"unsupported plane version", second(EncodePlaneHello(PlaneHello{Version: 2, PlaneID: testPlaneID})), ErrFrameBadVersion},
		{"bad planeId", second(EncodePlaneHello(PlaneHello{Version: 1, PlaneID: "pl-x"})), ErrFrameMalformed},
		{"entry-rejected without ack", second(EncodeError(ErrorFrame{Code: CodeEntryRejected, Reason: EntrySeqGap, RejectedSeq: 3})), ErrFrameMalformed},
		{"reason on plain error", second(EncodeError(ErrorFrame{Code: CodeBusy, Reason: EntrySeqGap})), ErrFrameUnknownField},
		{"unknown code", second(EncodeError(ErrorFrame{Code: "value"})), ErrFrameMalformed},
		{"too many pending", second(EncodePoll(Poll{V: 1, Ack: testAck, Pending: make([]Pending, MaxPending+1)})), ErrFrameMalformed},
	}
	for _, c := range cases {
		if c.err != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.err, c.want)
		}
	}
}

func second(_ []byte, err error) error { return err }

// batchLine builds a raw batch frame around entry JSON.
func batchLine(v int, entries ...string) string {
	return fmt.Sprintf(`{"v":%d,"type":"batch","planeId":"%s","entries":[%s]}`+"\n", v, testPlaneID, strings.Join(entries, ","))
}

func outcomeJSON(seq int, extra string) string {
	return fmt.Sprintf(`{"seq":%d,"fact":"outcome","opId":"%s","ref":"%s","version":1,"result":"ok"%s}`, seq, testOpID, testRef, extra)
}

func pollLine(pending, extra string) string {
	return fmt.Sprintf(`{"v":1,"type":"poll","pending":[%s],"ack":{"planeId":"%s","planeEntrySeq":0,"journalSeq":0}%s}`+"\n", pending, testPlaneID, extra)
}

func pendingJSON(op, extra string) string {
	return fmt.Sprintf(`{"opId":"%s","op":"%s","ref":"%s","name":"n","refKind":"other","journalSeq":1%s}`, testOpID, op, testRef, extra)
}

// newSentinel returns s = "rhzsntl-" + 32 hex from crypto/rand (§8.1 ZV).
func newSentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "rhzsntl-" + hex.EncodeToString(b)
}

// zvForms are the §8.1 derived forms that must never appear on a surface.
func zvForms(s string) []string {
	sum := sha256.Sum256([]byte(s))
	return []string{s, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)), s[8:16], s[len(s)-8:]}
}

var errorEnum = map[string]bool{
	ErrMalformed.Error(): true, ErrUnknownField.Error(): true,
	ErrFrameTooLarge.Error(): true, ErrFrameMalformed.Error(): true, ErrFrameUnknownField.Error(): true,
	ErrFrameBadVersion.Error(): true, ErrFrameBadType.Error(): true, ErrFramePlaneIDMismatch.Error(): true,
	ErrFrameSeqOrder.Error(): true, ErrFrameTooManyEntries.Error(): true,
}

// FR-RHZ-136: each §3.5 frame defect is rejected with its §3.6 code; ZV
// (§8.1): defects carry a fresh sentinel and no error string contains it or
// a derived form, and every error string is an enum code.
func TestFrameDefectsFRRHZ136(t *testing.T) {
	s := newSentinel(t)
	escaped := ""
	for _, c := range s {
		escaped += fmt.Sprintf(`\u%04x`, c)
	}
	many := make([]string, MaxEntries+1)
	for i := range many {
		many[i] = outcomeJSON(i+1, "")
	}
	recon := func(extra string) string {
		return fmt.Sprintf(`{"seq":1,"fact":"reconciled","opId":"%s","ref":"%s","name":"n","refKind":"other","version":1,"origin":"plane-direct"%s}`, testOpID, testRef, extra)
	}
	failed := fmt.Sprintf(`{"seq":1,"fact":"outcome","opId":"%s","ref":"%s","version":1,"result":"failed"}`, testOpID, testRef)
	big := batchLine(1, outcomeJSON(1, `,"value":"`+s+strings.Repeat("x", MaxFrameBytes)+`"`))

	type tc struct {
		name  string
		frame string
		want  error
	}
	fromPlane := []tc{
		{"valid 64 entries", batchLine(1, many[:MaxEntries]...), nil},
		{"broken json", `{"v":1,"type":"batch","planeId":"` + s + "\n", ErrFrameMalformed},
		{"not an object", `["` + s + `"]` + "\n", ErrFrameMalformed},
		{"no newline", strings.TrimSuffix(batchLine(1), "\n"), ErrFrameMalformed},
		{"embedded newline", strings.Replace(batchLine(1), ",", ",\n", 1), ErrFrameMalformed},
		{"trailing data", strings.TrimSuffix(batchLine(1), "\n") + ` {"value":"` + s + `"}` + "\n", ErrFrameMalformed},
		{"duplicate key", `{"v":1,"type":"batch","planeId":"` + s + `","planeId":"` + testPlaneID + `","entries":[]}` + "\n", ErrFrameMalformed},
		{"duplicate nested key", batchLine(1, outcomeJSON(1, `,"opId":"`+s+`"`)), ErrFrameMalformed},
		{"case-variant key", `{"v":1,"type":"batch","planeId":"` + testPlaneID + `","PlaneId":"` + s + `","entries":[]}` + "\n", ErrFrameUnknownField},
		{"null", `{"v":1,"type":"batch","planeId":null,"entries":[]}` + "\n", ErrFrameMalformed},
		{"unknown key value", strings.Replace(batchLine(1), `"entries"`, `"value":"`+s+`","entries"`, 1), ErrFrameUnknownField},
		{"unknown nested key value", batchLine(1, outcomeJSON(1, `,"value":"`+s+`"`)), ErrFrameUnknownField},
		{"too large", big, ErrFrameTooLarge},
		{"non-utf8", batchLine(1, outcomeJSON(1, `,"value":"`+"\xff\xfe"+s+`"`)), ErrFrameMalformed},
		{"backslash escape", batchLine(1, outcomeJSON(1, `,"value":"`+escaped+`"`)), ErrFrameMalformed},
		{"backslash escape a", strings.Replace(batchLine(1), `"batch"`, `"b\u0061tch"`, 1), ErrFrameMalformed},
		{"exponent seq", batchLine(1, strings.Replace(outcomeJSON(1, ""), `"seq":1`, `"seq":1e0`, 1)), ErrFrameMalformed},
		{"negative seq", batchLine(1, strings.Replace(outcomeJSON(1, ""), `"seq":1`, `"seq":-1`, 1)), ErrFrameMalformed},
		{"leading-zero seq", batchLine(1, strings.Replace(outcomeJSON(1, ""), `"seq":1`, `"seq":01`, 1)), ErrFrameMalformed},
		{"zero seq", batchLine(1, outcomeJSON(0, "")), ErrFrameMalformed},
		{"seq above 2^53-1", batchLine(1, outcomeJSON(1<<53, "")), ErrFrameMalformed},
		{"string version", strings.Replace(batchLine(1), `"v":1`, `"v":"1"`, 1), ErrFrameMalformed},
		{"version mismatch", batchLine(2), ErrFrameBadVersion},
		{"version mismatch beats unknown key", strings.Replace(batchLine(2), `"entries"`, `"value":"`+s+`","entries"`, 1), ErrFrameBadVersion},
		{"wrong type", pollLine("", ""), ErrFrameBadType},
		{"unknown type", strings.Replace(batchLine(1), `"batch"`, `"`+s+`"`, 1), ErrFrameBadType},
		{"missing type", strings.Replace(batchLine(1), `"type":"batch",`, ``, 1), ErrFrameBadType},
		{"non-string type", strings.Replace(batchLine(1), `"type":"batch"`, `"type":1`, 1), ErrFrameBadType},
		{"bad planeId", strings.Replace(batchLine(1), testPlaneID, "pl-"+s, 1), ErrFrameMalformed},
		{"seq gap", batchLine(1, outcomeJSON(1, ""), outcomeJSON(3, "")), ErrFrameSeqOrder},
		{"seq repeated", batchLine(1, outcomeJSON(2, ""), outcomeJSON(2, "")), ErrFrameSeqOrder},
		{"seq decreasing", batchLine(1, outcomeJSON(2, ""), outcomeJSON(1, "")), ErrFrameSeqOrder},
		{"too many entries", batchLine(1, many...), ErrFrameTooManyEntries},
		{"failed without failureCode", batchLine(1, failed), ErrFrameMalformed},
		{"failed with failureCode", batchLine(1, strings.TrimSuffix(failed, "}")+`,"failureCode":"backend-error"}`), nil},
		{"ok with failureCode", batchLine(1, outcomeJSON(1, `,"failureCode":"backend-error"`)), ErrFrameUnknownField},
		{"bad failureCode", batchLine(1, strings.TrimSuffix(failed, "}")+`,"failureCode":"`+s+`"}`), ErrFrameMalformed},
		{"reconciled version 2", batchLine(1, strings.Replace(recon(""), `"version":1`, `"version":2`, 1)), ErrFrameMalformed},
		{"scopeTags sorted", batchLine(1, recon(`,"scopeTags":["a","b"]`)), nil},
		{"scopeTags unsorted", batchLine(1, recon(`,"scopeTags":["b","a"]`)), ErrFrameMalformed},
		{"scopeTags duplicate", batchLine(1, recon(`,"scopeTags":["a","a"]`)), ErrFrameMalformed},
		{"scopeTags empty", batchLine(1, recon(`,"scopeTags":[]`)), ErrFrameMalformed},
		{"scopeTags on outcome", batchLine(1, outcomeJSON(1, `,"scopeTags":["a"]`)), ErrFrameUnknownField},
		{"observed present as string", batchLine(1, fmt.Sprintf(`{"seq":1,"fact":"observed","ref":"%s","version":1,"present":"true","planeObservedAt":"2026-10-10T00:00:00Z"}`, testRef)), ErrFrameMalformed},
		{"unknown fact", batchLine(1, strings.Replace(outcomeJSON(1, ""), `"outcome"`, `"value"`, 1)), ErrFrameMalformed},
		{"plane sends entry-rejected", fmt.Sprintf(`{"v":1,"type":"error","code":"entry-rejected","reason":"seq-gap","ack":{"planeId":"%s","planeEntrySeq":0,"journalSeq":0},"rejectedSeq":1}`+"\n", testPlaneID), ErrFrameMalformed},
		{"plane sends retired-plane", `{"v":1,"type":"error","code":"retired-plane"}` + "\n", ErrFrameMalformed},
		{"error version 2", `{"v":2,"type":"error","code":"busy"}` + "\n", ErrFrameBadVersion},
		{"error unknown key", `{"v":1,"type":"error","code":"busy","value":"` + s + `"}` + "\n", ErrFrameUnknownField},
	}
	fromRhizome := []tc{
		{"poll unknown ack key", strings.Replace(pollLine("", ""), `"journalSeq":0}`, `"journalSeq":0,"value":"`+s+`"}`, 1), ErrFrameUnknownField},
		{"poll unknown pending key", pollLine(pendingJSON(OpRotate, `,"value":"`+s+`"`), ""), ErrFrameUnknownField},
		{"poll missing ack", `{"v":1,"type":"poll","pending":[]}` + "\n", ErrFrameMalformed},
		{"poll register scopeTags", pollLine(pendingJSON(OpRegister, `,"scopeTags":["a"]`), ""), nil},
		{"poll rotate scopeTags", pollLine(pendingJSON(OpRotate, `,"scopeTags":["a"]`), ""), ErrFrameUnknownField},
		{"poll register scopeTags unsorted", pollLine(pendingJSON(OpRegister, `,"scopeTags":["b","a"]`), ""), ErrFrameMalformed},
		{"poll revoke without reasonCode", pollLine(pendingJSON(OpRevoke, ""), ""), ErrFrameMalformed},
		{"poll register with reasonCode", pollLine(pendingJSON(OpRegister, `,"reasonCode":"other"`), ""), ErrFrameUnknownField},
		{"poll lastRejected bad reason", pollLine("", `,"lastRejected":{"seq":1,"reason":"`+s+`"}`), ErrFrameMalformed},
		{"poll lastRejected seq 0", pollLine("", `,"lastRejected":{"seq":0,"reason":"seq-gap"}`), ErrFrameMalformed},
		{"poll negotiated mismatch", strings.Replace(pollLine("", ""), `"v":1`, `"v":2`, 1), ErrFrameBadVersion},
		{"plane-only type", batchLine(1), ErrFrameBadType},
	}
	rhizomeHello := []tc{
		{"hello version 2", `{"v":2,"type":"hello","role":"rhizome","versions":[1]}` + "\n", ErrFrameBadVersion},
		{"hello wrong role", `{"v":1,"type":"hello","role":"plane","versions":[1]}` + "\n", ErrFrameMalformed},
		{"hello no versions", `{"v":1,"type":"hello","role":"rhizome","versions":[]}` + "\n", ErrFrameMalformed},
	}
	var errStrings []string
	run := func(dir string, cases []tc, decode func([]byte) error) {
		for _, c := range cases {
			err := decode([]byte(c.frame))
			if err != c.want {
				t.Errorf("%s %s: got %v want %v", dir, c.name, err, c.want)
			}
			if err != nil {
				errStrings = append(errStrings, err.Error())
			}
		}
	}
	run("plane", fromPlane, func(b []byte) error { _, err := DecodeFromPlane(b, TypeBatch, 1); return err })
	run("rhizome", fromRhizome, func(b []byte) error { _, err := DecodeFromRhizome(b, TypePoll, 1); return err })
	run("rhizome hello", rhizomeHello, func(b []byte) error { _, err := DecodeFromRhizome(b, TypeHello, 0); return err })

	if _, err := DecodeFromPlane([]byte(`{"v":1,"type":"hello","role":"plane","version":2,"planeId":"`+testPlaneID+`"}`+"\n"), TypeHello, 0); err != ErrFrameBadVersion {
		t.Errorf("plane hello outside Rhizome versions: %v", err)
	}
	f, err := DecodeFromPlane([]byte(batchLine(1)), TypeBatch, 1)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckBatchPlane(f.Batch, "pl-"+strings.Repeat("0", 32))
	if err != ErrFramePlaneIDMismatch {
		t.Errorf("planeId mismatch: %v", err)
	}
	errStrings = append(errStrings, err.Error())
	_, err = ReadFrame(bufio.NewReader(strings.NewReader(big)))
	if err != ErrFrameTooLarge {
		t.Errorf("ReadFrame too large: %v", err)
	}
	errStrings = append(errStrings, err.Error())
	err = StrictDecode([]byte(`{"name":"a","n":3,"value":"`+s+`"}`), testSpec)
	errStrings = append(errStrings, err.Error(), StrictDecode([]byte(`{"name":"`+s+`","name":"a","n":3}`), testSpec).Error())

	for _, e := range errStrings {
		if !errorEnum[e] {
			t.Errorf("error string outside the §3.6 enum")
		}
		for _, form := range zvForms(s) {
			if strings.Contains(e, form) {
				t.Fatalf("ZV: sentinel form leaked into an error string")
			}
		}
	}
}

// FR-RHZ-136: ReadFrame enforces W3 without unbounded buffering.
func TestReadFrameFRRHZ136(t *testing.T) {
	exact := strings.Repeat(" ", MaxFrameBytes-1) + "\n"
	over := strings.Repeat(" ", MaxFrameBytes) + "\n"
	r := bufio.NewReaderSize(strings.NewReader("{}\n"+exact+"{}\n"), 16)
	for i, want := range []string{"{}\n", exact, "{}\n"} {
		got, err := ReadFrame(r)
		if err != nil || string(got) != want {
			t.Fatalf("frame %d: len %d err %v", i, len(got), err)
		}
	}
	if _, err := ReadFrame(r); err != io.EOF {
		t.Fatalf("clean EOF: %v", err)
	}
	if _, err := ReadFrame(bufio.NewReaderSize(strings.NewReader(over), 16)); err != ErrFrameTooLarge {
		t.Fatalf("oversize: %v", err)
	}
	if _, err := ReadFrame(bufio.NewReader(strings.NewReader(`{"v":1}`))); err != ErrFrameMalformed {
		t.Fatalf("EOF without newline: %v", err)
	}
	if _, err := ReadFrame(bufio.NewReader(io.MultiReader(strings.NewReader("{"), errReader{}))); err != ErrFrameMalformed {
		t.Fatalf("I/O error text must not pass through: %v", err)
	}
	if _, err := ReadFrame(bufio.NewReader(timeoutReader{})); err != ErrTimeout {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := DecodeFromPlane([]byte(over), TypeBatch, 1); err != ErrFrameTooLarge {
		t.Fatalf("decode oversize: %v", err)
	}
}

type errReader struct{}

var errBoom = fmt.Errorf("boom")

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

type timeoutReader struct{}

func (timeoutReader) Read([]byte) (int, error) { return 0, os.ErrDeadlineExceeded }
