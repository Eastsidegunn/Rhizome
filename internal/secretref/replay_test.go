package secretref

import (
	"encoding/json"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/secretwire"
)

const rOK, rFailed, rRejected = secretwire.ResultOK, secretwire.ResultFailed, secretwire.ResultRejected

// activeLog: op1 registered and active(1) on plane 1 (hw 1).
func activeLog() *logb {
	return newLog().add(e1(opN(1), "alpha")).add(e4(opN(1), ref(opN(1)), 1, rOK, planeN(1), 1))
}

func TestReplayRulesFRRHZ134(t *testing.T) {
	P1, P2 := planeN(1), planeN(2)
	r1 := ref(opN(1))
	cases := []struct {
		name string
		log  *logb
		want string
	}{
		// R1: one request, at most one outcome per opId.
		{"R1 accept request+outcome", activeLog(), ""},
		{"R1 reject outcome without request", newLog().add(e4(opN(1), r1, 1, rOK, P1, 1), func(e *events.Event) { e.Revision = 2 }), "R1"},
		{"R1 reject second request", newLog().add(e1(opN(1), "alpha")).add(e1(opN(1), "alpha"), func(e *events.Event) { e.Revision = 1 }), "R1"},
		// R2: a ref is introduced once.
		{"R2 accept reconciled new ref", newLog().add(e5(opN(2), "beta", P1, 1)), ""},
		{"R2 reject reconciled over requested ref", newLog().add(e1(opN(1), "alpha")).add(e5(opN(1), "beta", P1, 1)), "R2"},
		{"R2 reject request deriving reconciled ref", newLog().add(e5(opN(1), "beta", P1, 1)).add(e1(opN(1), "alpha")), "R2"},
		// R3: change only on an active ref.
		{"R3 accept rotate active", activeLog().add(e2(opN(2), r1)), ""},
		{"R3 reject rotate pending-registration", newLog().add(e1(opN(1), "alpha")).add(e2(opN(2), r1)), "R3"},
		{"R3 reject second change in flight", activeLog().add(e2(opN(2), r1)).add(e3(opN(3), r1)), "R3"},
		// R4: exact outcome version.
		{"R4 accept rotate ok v+1", activeLog().add(e2(opN(2), r1)).add(e4(opN(2), r1, 2, rOK, P1, 2)), ""},
		{"R4 accept rotate failed v", activeLog().add(e2(opN(2), r1)).add(e4(opN(2), r1, 1, rFailed, P1, 2)), ""},
		{"R4 accept register rejected 0", newLog().add(e1(opN(1), "alpha")).add(e4(opN(1), r1, 0, rRejected, P1, 1)), ""},
		{"R4 reject rotate ok same version", activeLog().add(e2(opN(2), r1)).add(e4(opN(2), r1, 1, rOK, P1, 2)), "R4"},
		{"R4 reject revoke ok v+1", activeLog().add(e3(opN(2), r1)).add(e4(opN(2), r1, 2, rOK, P1, 2)), "R4"},
		// R5: observed on a live state, 1 <= version <= current.
		{"R5 accept observed active", activeLog().add(e6(r1, 1, P1, 2)), ""},
		{"R5 reject observed pending-registration", newLog().add(e1(opN(1), "alpha")).add(e6(r1, 1, P1, 1)), "R5"},
		{"R5 reject observed future version", activeLog().add(e6(r1, 2, P1, 2)), "R5"},
		// R6: reconciled version is 1.
		{"R6 accept", newLog().add(e5(opN(2), "beta", P1, 1)), ""},
		{"R6 reject", newLog().add(func() reconciled { r := e5(opN(2), "beta", P1, 1); r.Version = 2; return r }()), "R6"},
		// R7: plane seq continuity.
		{"R7 accept continuous", activeLog().add(e6(r1, 1, P1, 2)).add(e6(r1, 1, P1, 3)), ""},
		{"R7 reject gap", activeLog().add(e6(r1, 1, P1, 3)), "R7"},
		{"R7 reject restart at 1", activeLog().add(e6(r1, 1, P1, 1)), "R7"},
		// R7a: retired planeIds.
		{"R7a accept new plane", activeLog().add(e6(r1, 1, P2, 1)), ""},
		{"R7a reject retired fact", activeLog().add(e6(r1, 1, P2, 1)).add(e6(r1, 1, P1, 2)), "R7a"},
		{"R7a reject fact after E7-only plane", activeLog().add(e7(P2, 1, secretwire.EntryState, 0)).add(e6(r1, 1, P1, 2)), "R7a"},
		{"R7a reject retired E7", activeLog().add(e6(r1, 1, P2, 1)).add(e7(P1, 2, secretwire.EntryState, 1)), "R7a"},
		// R7b: E7 relations and no duplicates.
		{"R7b accept seq-conflict", activeLog().add(e7(P1, 1, secretwire.EntrySeqConflict, 1)), ""},
		{"R7b accept seq-gap", activeLog().add(e7(P1, 3, secretwire.EntrySeqGap, 1)), ""},
		{"R7b accept other", activeLog().add(e7(P1, 2, secretwire.EntryUnknownOp, 1)), ""},
		{"R7b accept same pair new ack", activeLog().add(e7(P1, 2, secretwire.EntryState, 1)).add(e6(r1, 1, P1, 2)).add(e7(P1, 3, secretwire.EntryState, 2)), ""},
		{"R7b reject ack != hw", activeLog().add(e7(P1, 1, secretwire.EntryState, 0)), "R7b"},
		{"R7b reject seq-conflict above ack", activeLog().add(e7(P1, 2, secretwire.EntrySeqConflict, 1)), "R7b"},
		{"R7b reject seq-gap at ack+1", activeLog().add(e7(P1, 2, secretwire.EntrySeqGap, 1)), "R7b"},
		{"R7b reject other not ack+1", activeLog().add(e7(P1, 3, secretwire.EntryState, 1)), "R7b"},
		{"R7b reject duplicate E7", activeLog().add(e7(P1, 2, secretwire.EntryState, 1)).add(e7(P1, 2, secretwire.EntryState, 1)), "R7b"},
		// V-T and V-A envelope rules.
		{"V-T unknown type", activeLog().add(e6(r1, 1, P1, 2), func(e *events.Event) { e.Type = "secret.value.set" }), "V-T"},
		{"V-T outcome at revision 1", newLog().add(e4(opN(1), r1, 1, rOK, P1, 1)), "V-T"},
		{"V-T wrong aggregate type", newLog().add(e1(opN(1), "alpha"), func(e *events.Event) { e.AggregateType = AggregateRef }), "V-T"},
		{"V-T observed revision skips", activeLog().add(e6(r1, 1, P1, 2), func(e *events.Event) { e.Revision = 2 }), "V-T"},
		{"V-A aggregate id", newLog().add(e1(opN(1), "alpha"), func(e *events.Event) { e.AggregateID = opN(9) }), "V-A"},
		{"V-A correlation id", newLog().add(e1(opN(1), "alpha"), func(e *events.Event) { e.CorrelationID = "" }), "V-A"},
		{"V-A event id", newLog().add(e1(opN(1), "alpha"), func(e *events.Event) { e.ID = "x" }), "V-A"},
		{"V-A causation id", newLog().add(e1(opN(1), "alpha"), func(e *events.Event) { e.CausationID = "x" }), "V-A"},
		{"V-A reconciled ref not derived", newLog().add(func() reconciled { r := e5(opN(2), "beta", P1, 1); r.Ref = r1; return r }()), "V-A"},
		{"V-A rotate unknown ref", newLog().add(e2(opN(2), r1)), "V-A"},
		{"V-A outcome ref mismatch", newLog().add(e1(opN(1), "alpha")).add(e4(opN(1), ref(opN(7)), 1, rOK, P1, 1)), "V-A"},
		{"V-A observed unknown ref", newLog().add(e6(r1, 1, P1, 1)), "V-A"},
		{"V-Z failureCode without failed", newLog().add(e1(opN(1), "alpha")).add(func() opOutcome {
			o := e4(opN(1), r1, 1, rOK, P1, 1)
			o.FailureCode = secretwire.FailureNoTTY
			return o
		}()), "V-Z"},
		{"V-Z failed without failureCode", newLog().add(e1(opN(1), "alpha")).add(func() opOutcome { o := e4(opN(1), r1, 0, rFailed, P1, 1); o.FailureCode = ""; return o }()), "V-Z"},

		{"V-Z unsorted scopeTags", newLog().add(registerRequested{OpID: opN(1), Name: "alpha", Kind: "other", ScopeTags: []string{"b", "a"}, ActorRef: actorRef}), "V-Z"},
		{"V-Z actor prefix", newLog().add(registerRequested{OpID: opN(1), Name: "alpha", Kind: "other", ActorRef: actor}), "V-Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := replayCode(t, c.log.evs); got != c.want {
				t.Fatalf("code = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReplayStatesAndDerivedFRRHZ134(t *testing.T) {
	P1, P2 := planeN(1), planeN(2)
	r1, r2, r3 := ref(opN(1)), ref(opN(4)), ref(opN(6))
	log := newLog().
		add(e1(opN(1), "alpha")).                               // seq 1
		add(e4(opN(1), r1, 1, rOK, P1, 1)).                     // 2 active(1)
		add(e2(opN(2), r1)).                                    // 3 rotating
		add(e4(opN(2), r1, 2, rOK, P1, 2)).                     // 4 active(2)
		add(e3(opN(3), r1)).                                    // 5 revoking
		add(e6(r1, 2, P1, 3)).                                  // 6 observed
		add(e4(opN(3), r1, 2, rOK, P1, 4)).                     // 7 revoked
		add(e1(opN(4), "beta")).                                // 8 pending-registration
		add(e4(opN(4), r2, 0, rFailed, P1, 5)).                 // 9 registration-failed
		add(e7(P1, 7, secretwire.EntrySeqGap, 5)).              // 10 lastRejected(P1)
		add(e5(opN(6), "gamma", P2, 1), func(e *events.Event) { // 11 P2 first appearance
			var r reconciled
			_ = json.Unmarshal(e.Payload, &r)
			r.Origin = secretwire.OriginAttested
			e.Payload, _ = json.Marshal(r)
		}).
		add(e1(opN(5), "delta")) // 12 pending op
	p, err := Project(log.evs)
	if err != nil {
		t.Fatal(err)
	}
	refs := p.Refs()
	if len(refs) != 4 || refs[0].Ref != r1 || refs[1].Ref != r2 || refs[2].Ref != r3 || refs[3].Ref != ref(opN(5)) {
		t.Fatalf("ref order wrong")
	}
	a := refs[0]
	if a.State != StateRevoked || a.Version != 2 || a.Origin != OriginRequest || a.Pending != nil ||
		a.LastOutcome == nil || a.LastOutcome.Op != secretwire.OpRevoke || a.Observed == nil || a.Observed.Version != 2 ||
		!a.RegisteredAt.Equal(log.evs[1].CreatedAt) || !a.RotatedAt.Equal(log.evs[3].CreatedAt) || !a.RevokedAt.Equal(log.evs[6].CreatedAt) {
		t.Fatalf("revoked ref wrong: %+v", a)
	}
	if b := refs[1]; b.State != StateRegistrationFailed || b.Version != 0 || b.LastOutcome.FailureCode != secretwire.FailureBackendError {
		t.Fatalf("failed ref wrong: %+v", b)
	}
	if c := refs[2]; c.State != StateActive || c.Version != 1 || c.Origin != secretwire.OriginAttested || c.Kind != secretwire.RefKindProviderKey {
		t.Fatalf("reconciled ref wrong: %+v", c)
	}
	if d := refs[3]; d.State != StatePendingRegistration || d.Pending == nil || d.Pending.OpID != opN(5) || d.Pending.JournalSeq != 12 {
		t.Fatalf("pending ref wrong: %+v", d)
	}
	// R7/R7a/R7b derived values come from the events alone.
	if !p.Retired(P1) || p.Retired(P2) || p.CurrentPlane() != P2 || !reflect.DeepEqual(p.RetiredPlanes(), []string{P1}) {
		t.Fatal("plane retirement wrong")
	}
	if got := p.Ack(P1); got != (secretwire.Ack{PlaneID: P1, PlaneEntrySeq: 5, JournalSeq: 9}) {
		t.Fatalf("ack P1 = %+v", got)
	}
	if got := p.Ack(P2); got != (secretwire.Ack{PlaneID: P2, PlaneEntrySeq: 1, JournalSeq: 11}) {
		t.Fatalf("ack P2 = %+v", got)
	}
	if lr := p.LastRejected(P1); lr == nil || *lr != (secretwire.Rejected{Seq: 7, Reason: secretwire.EntrySeqGap}) {
		t.Fatalf("lastRejected = %+v", lr)
	}
	if p.LastRejected(P2) != nil || p.HighWater(planeN(9)) != 0 {
		t.Fatal("unexpected derived values")
	}
	// A later fact makes the old E7 stale: lastRejected needs AckSeq == hw.
	log2 := activeLog().add(e7(P1, 2, secretwire.EntryState, 1)).add(e6(r1, 1, P1, 2))
	p2, err := Project(log2.evs)
	if err != nil || p2.LastRejected(P1) != nil {
		t.Fatalf("stale lastRejected kept: %v", err)
	}
}

func TestReplayDeterminismFRRHZ134(t *testing.T) {
	r1 := ref(opN(1))
	log := activeLog().add(e2(opN(2), r1)).add(e6(r1, 1, planeN(1), 2)).add(e5(opN(3), "beta", planeN(1), 3)).add(e1(opN(4), "gamma"))
	// Interleave non-secret events; they are ignored.
	var mixed []events.Event
	for i, e := range log.evs {
		mixed = append(mixed, events.Event{Sequence: uint64(2*i + 1), AggregateType: "mission", AggregateID: "m", Revision: uint64(i + 1), Type: "mission.x"})
		e.Sequence = uint64(2*i + 2)
		mixed = append(mixed, e)
	}
	a, err := Project(mixed)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		shuffled := append([]events.Event(nil), mixed...)
		rand.New(rand.NewSource(int64(i))).Shuffle(len(shuffled), func(x, y int) { shuffled[x], shuffled[y] = shuffled[y], shuffled[x] })
		b, err := Project(shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a.Refs(), b.Refs()) || !reflect.DeepEqual(a.Pending(0), b.Pending(0)) {
			t.Fatal("projection depends on input order")
		}
	}
}

// journalLine is one hand-written journal line with a raw payload.
func journalLine(seq, rev int, aggType, aggID, typ, payload string) string {
	return `{"sequence":` + itoa(seq) + `,"id":"","aggregate_type":"` + aggType + `","aggregate_id":"` + aggID +
		`","revision":` + itoa(rev) + `,"type":"` + typ + `","payload":` + payload + `,"correlation_id":"` + aggID +
		`","created_at":"2026-10-10T00:00:00Z"}` + "\n"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestReplayStrictJournalFRRHZ134 plants V-J/V-K/V-Z violations into real
// journal lines (the payload is a json.RawMessage, checked verbatim).
func TestReplayStrictJournalFRRHZ134(t *testing.T) {
	s := sentinel(t)
	bs := string([]byte{0x5c}) // a literal backslash
	op := opN(1)
	reg := func(body string) string { return journalLine(1, 1, AggregateOp, op, TypeRegisterRequested, body) }
	valid := `{"OpID":"` + op + `","Name":"alpha","Kind":"other","ActorRef":"` + actorRef + `"}`
	outcome := func(version string) string {
		return reg(valid) + journalLine(2, 2, AggregateOp, op, TypeOpOutcome,
			`{"OpID":"`+op+`","Ref":"`+ref(op)+`","Version":`+version+`,"Result":"ok","PlaneID":"`+planeN(1)+`","PlaneEntrySeq":1}`)
	}
	cases := []struct{ name, lines, want string }{
		{"valid", outcome("1"), ""},
		{"step0a non-UTF-8 raw byte", reg(`{"OpID":"` + op + `","Name":"al` + "\xff\xfe" + s + `","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"step0b non-ASCII UTF-8", reg(`{"OpID":"` + op + `","Name":"al` + string([]byte{0xc3, 0xa9}) + `","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"step0c backslash u0061", reg(`{"OpID":"` + op + `","Name":"` + bs + `u0061lpha","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"step0c backslash escaped sentinel", reg(`{"OpID":"` + op + `","Name":"` + bs + `u0072` + s[1:] + `","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"V-Z empty scopeTags array", reg(`{"OpID":"` + op + `","Name":"alpha","Kind":"other","ScopeTags":[],"ActorRef":"` + actorRef + `"}`), "V-Z"},
		{"step2 duplicate Name", reg(`{"OpID":"` + op + `","Name":"` + s + `","Name":"alpha","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"step3 null", reg(`{"OpID":"` + op + `","Name":null,"Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"step4 exponent integer", outcome("1e0"), "V-J"},
		{"step4 negative integer", outcome("-1"), "V-J"},
		{"step4 string for integer", outcome(`"1"`), "V-J"},
		{"step4 number for string", reg(`{"OpID":"` + op + `","Name":7,"Kind":"other","ActorRef":"` + actorRef + `"}`), "V-J"},
		{"step5 grammar", reg(`{"OpID":"` + op + `","Name":"Alpha","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-Z"},
		{"step6 Value key", reg(`{"OpID":"` + op + `","Name":"alpha","Kind":"other","ActorRef":"` + actorRef + `","Value":"` + s + `"}`), "V-K"},
		{"step6 case-variant key", reg(`{"OpID":"` + op + `","name":"alpha","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-K"},
		{"step6 missing key", reg(`{"OpID":"` + op + `","Kind":"other","ActorRef":"` + actorRef + `"}`), "V-K"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := journalPath(t)
			if err := os.WriteFile(path, []byte(c.lines), 0o600); err != nil {
				t.Fatal(err)
			}
			j := openJournal(t, path)
			_, err := Project(j.All())
			got := ""
			if err != nil {
				got = err.(*ReplayError).Code
				assertNoSentinel(t, "replay error", []byte(err.Error()), s)
			}
			if got != c.want {
				t.Fatalf("code = %q, want %q", got, c.want)
			}
			// The writer refuses everything on a projection error (R8).
			if c.want != "" {
				res, _ := New(j).RequestRegister(RegisterRequest{OpID: opN(2), Name: "beta", RefKind: "other", Actor: actor})
				if res.Reason != ReasonUnavailable {
					t.Fatalf("writer not refusing: %q", res.Reason)
				}
			}
		})
	}
}

// TestReplayStrictTrailingFRRHZ134 is V-J step 1 (exactly one value): a
// journal line cannot carry trailing data inside its payload, so a Port
// returns the crafted event.
func TestReplayStrictTrailingFRRHZ134(t *testing.T) {
	s := sentinel(t)
	op := opN(1)
	body := `{"OpID":"` + op + `","Name":"alpha","Kind":"other","ActorRef":"` + actorRef + `"}`
	for _, payload := range []string{body + ` {"Value":"` + s + `"}`, body + ` 1`, `{"OpID":"` + op + `"`} {
		var st events.Store
		if err := st.Append(0, events.Event{AggregateType: AggregateOp, AggregateID: op, Revision: 1,
			Type: TypeRegisterRequested, Payload: json.RawMessage(payload), CorrelationID: op}); err != nil {
			t.Fatal(err)
		}
		_, err := Project(st.All())
		if err == nil || err.Error() != "secret replay: V-J" {
			t.Fatalf("err = %v", err)
		}
		assertNoSentinel(t, "replay error", []byte(err.Error()), s)
	}
}

func TestPayloadKeysFRRHZ134(t *testing.T) {
	// Marshalled payloads carry exactly the §1.2 PascalCase keys.
	want := map[string][]string{
		TypeRegisterRequested: {"ActorRef", "Kind", "Name", "OpID"},
		TypeRotateRequested:   {"ActorRef", "OpID", "Ref"},
		TypeRevokeRequested:   {"ActorRef", "OpID", "ReasonCode", "Ref"},
		TypeOpOutcome:         {"OpID", "PlaneEntrySeq", "PlaneID", "Ref", "Result", "Version"},
		TypeReconciled:        {"Kind", "Name", "OpID", "Origin", "PlaneEntrySeq", "PlaneID", "Ref", "Version"},
		TypeObserved:          {"PlaneEntrySeq", "PlaneID", "PlaneObservedAt", "Present", "Ref", "Version"},
		TypeEntryRejected:     {"AckSeq", "PlaneID", "Reason", "RejectedSeq"},
	}
	for _, p := range []any{e1(opN(1), "a"), e2(opN(1), ref(opN(1))), e3(opN(1), ref(opN(1))), e4(opN(1), ref(opN(1)), 1, rOK, planeN(1), 1),
		e5(opN(1), "a", planeN(1), 1), e6(ref(opN(1)), 1, planeN(1), 1), e7(planeN(1), 1, secretwire.EntryState, 0)} {
		typ, _, _, _ := envelope(p)
		raw, _ := json.Marshal(p)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		if strings.Join(sorted(keys), ",") != strings.Join(want[typ], ",") {
			t.Fatalf("%s keys = %v", typ, keys)
		}
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
