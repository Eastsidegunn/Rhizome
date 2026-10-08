package workspace

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/procedure"
	"rhizome/internal/question"
)

func restampJournalFRRHZ142(t *testing.T, source events.Port, stamps map[string]time.Time) *events.Store {
	t.Helper()
	stable := &events.Store{}
	for _, event := range source.All() {
		if stamp, ok := stamps[eventStampKeyFRRHZ142(event.AggregateType, event.AggregateID, event.Type, event.Revision)]; ok {
			event.CreatedAt = stamp
		}
		event.ID = ""
		if err := stable.Append(stable.Revision(event.AggregateType, event.AggregateID), event); err != nil {
			t.Fatal(err)
		}
	}
	return stable
}

func eventStampKeyFRRHZ142(aggregateType, aggregateID, eventType string, revision uint64) string {
	return aggregateType + ":" + aggregateID + ":" + eventType + ":" + strconv.FormatUint(revision, 10)
}

func gatesByIDFRRHZ142(t *testing.T, store events.Port) map[string]Gate {
	t.Helper()
	p, err := Snapshot(store)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]Gate, len(p.Gates))
	for _, gate := range p.Gates {
		out[gate.ID] = gate
	}
	return out
}

func TestGateDecidedAtDerivationFRRHZ142(t *testing.T) {
	source := &events.Store{}
	questions := question.Service{Store: source}
	approved, err := questions.Ask("approved-142", "body", "r", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := questions.Answer(approved.ID, question.RequestChanges, "revise", "ops", approved.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := questions.Answer(approved.ID, question.Approve, "", "ops", approved.Digest); err != nil {
		t.Fatal(err)
	}
	rejected, err := questions.Ask("rejected-142", "body", "r", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := questions.Answer(rejected.ID, question.Reject, "no", "ops", rejected.Digest); err != nil {
		t.Fatal(err)
	}
	changes, err := questions.Ask("changes-142", "body", "r", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := questions.Answer(changes.ID, question.RequestChanges, "fix", "ops", changes.Digest); err != nil {
		t.Fatal(err)
	}
	pending, err := questions.Ask("pending-142", "body", "r", "", "", "asker", "")
	if err != nil {
		t.Fatal(err)
	}

	approvals := approval.Service{Store: source}
	allowKey := approval.RequestKey{TraceID: "14200000000000000000000000000001", SpanID: "1420000000000001", RequestID: "allow"}
	allow, err := approvals.RecordInput(allowKey, approval.Allow, "", "response-allow", "digest-allow", "ops", "", "", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvals.Observe(allow.ID, 1, approval.Allow, ""); err != nil {
		t.Fatal(err)
	}
	denyKey := approval.RequestKey{TraceID: "14200000000000000000000000000002", SpanID: "1420000000000002", RequestID: "deny"}
	deny, err := approvals.RecordInput(denyKey, approval.Deny, "human no", "response-deny", "digest-deny", "ops", "", "", noAuthority())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvals.Observe(deny.ID, 1, approval.Deny, "janus no"); err != nil {
		t.Fatal(err)
	}
	pendingKey := approval.RequestKey{TraceID: "14200000000000000000000000000003", SpanID: "1420000000000003", RequestID: "pending"}
	pendingApproval, err := approvals.RecordInput(pendingKey, approval.Allow, "", "response-pending", "digest-pending", "ops", "", "", noAuthority())
	if err != nil {
		t.Fatal(err)
	}

	zone := time.FixedZone("test-east", 9*60*60)
	approvedOld := time.Date(2026, 10, 8, 12, 0, 0, 1, zone)
	approvedLatest := time.Date(2026, 10, 8, 12, 0, 0, 987654321, zone)
	rejectedAt := time.Date(2026, 10, 8, 12, 1, 0, 0, zone)
	allowAt := time.Date(2026, 10, 8, 12, 2, 0, 123, zone)
	denyAt := time.Date(2026, 10, 8, 12, 3, 0, 0, zone)
	stable := restampJournalFRRHZ142(t, source, map[string]time.Time{
		eventStampKeyFRRHZ142("question", approved.ID, "question.answered", 2):    approvedOld,
		eventStampKeyFRRHZ142("question", approved.ID, "question.answered", 3):    approvedLatest,
		eventStampKeyFRRHZ142("question", rejected.ID, "question.answered", 2):    rejectedAt,
		eventStampKeyFRRHZ142("approval", allow.ID, "approval.input_recorded", 1): allowAt,
		eventStampKeyFRRHZ142("approval", deny.ID, "approval.input_recorded", 1):  denyAt,
	})

	gates := gatesByIDFRRHZ142(t, stable)
	for id, want := range map[string]string{
		approved.ID: approvedLatest.UTC().Format(time.RFC3339Nano),
		rejected.ID: rejectedAt.UTC().Format(time.RFC3339Nano),
		allow.ID:    allowAt.UTC().Format(time.RFC3339Nano),
		deny.ID:     denyAt.UTC().Format(time.RFC3339Nano),
	} {
		if got := gates[id].DecidedAt; got != want {
			t.Errorf("gate %s decidedAt=%q want %q", id, got, want)
		}
		parsed, err := time.Parse(time.RFC3339Nano, gates[id].DecidedAt)
		if err != nil || parsed.Location() != time.UTC {
			t.Errorf("gate %s decidedAt is not RFC3339 UTC: %q (%v)", id, gates[id].DecidedAt, err)
		}
	}
	for _, id := range []string{changes.ID, pending.ID, pendingApproval.ID} {
		if got := gates[id].DecidedAt; got != "" {
			t.Errorf("open gate %s has decidedAt %q", id, got)
		}
	}

	first, err := json.Marshal(toDTO(mustSnapshot105(t, stable)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(toDTO(mustSnapshot105(t, stable)))
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("decidedAt projection is nondeterministic: %v\n%s\n%s", err, first, second)
	}
	for _, event := range stable.All() {
		if bytes.Contains(event.Payload, []byte("decidedAt")) || bytes.Contains(event.Payload, []byte("DecidedAt")) {
			t.Fatalf("decidedAt leaked into journal payload: %s", event.Payload)
		}
	}

	// Replay derives the field from the envelope: changing only the envelope
	// timestamp changes the projection while every persisted payload is equal.
	replayedAt := approvedLatest.Add(2 * time.Hour)
	replayed := restampJournalFRRHZ142(t, stable, map[string]time.Time{
		eventStampKeyFRRHZ142("question", approved.ID, "question.answered", 3): replayedAt,
	})
	if got := gatesByIDFRRHZ142(t, replayed)[approved.ID].DecidedAt; got != replayedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("replayed decidedAt=%q", got)
	}
	for i, event := range stable.All() {
		if !bytes.Equal(event.Payload, replayed.All()[i].Payload) {
			t.Fatalf("replay changed payload %d", i)
		}
	}
}

func TestContextGateDecidedAtFRRHZ142(t *testing.T) {
	s := fixture068(t)
	defineProc(t, s, "proc-decided-at", procedure.Step{ID: "a", Action: "act-a", NeedsGate: true})
	if res, err := RelayIntent(s, Intent{Kind: "procedure.run", ID: "proc-decided-at", Name: "decided-at", GoalID: "goal-dev"}, "op", noAuthority()); err != nil || !res.Accepted {
		t.Fatalf("procedure.run: %+v %v", res, err)
	}
	q, err := (question.Service{Store: s}).Get(mustQuestionID("decided-at · a 게이트", "act-a", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (question.Service{Store: s}).Answer(q.ID, question.Approve, "", "ops", q.Digest); err != nil {
		t.Fatal(err)
	}
	answer := s.List("question", q.ID)[1]
	rec := httptest.NewRecorder()
	NewHTTP(s).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/context?task=mission-decided-at", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("context status=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	want := `"decidedAt":"` + answer.CreatedAt.UTC().Format(time.RFC3339Nano) + `"`
	if !bytes.Contains(rec.Body.Bytes(), []byte(want)) {
		t.Fatalf("context missing %s: %s", want, rec.Body.Bytes())
	}
}
