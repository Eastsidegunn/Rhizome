package question_test

// RHZ-078 (FR-RHZ-109) K1: kernel answer sequencing. pending → any decision;
// requestChanges → only approve/reject (second requestChanges rejected);
// approve/reject → terminal. Replay accepts the new decision value and keeps
// the LATEST answer's Reason/ActorRef; unknown decisions still fail replay.

import (
	"encoding/json"
	"rhizome/internal/events"
	question "rhizome/internal/question"
	"testing"
	"time"
)

func TestQuestionAnswerSequencingRequestChangesFRRHZ109(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	q, err := svc.Ask("t", "b", "r", "", "", "req", "")
	if err != nil {
		t.Fatal(err)
	}
	// reason required for requestChanges
	if _, err := svc.Answer(q.ID, question.RequestChanges, "  ", "alice", q.Digest); err == nil || err.Error() != "reason required" {
		t.Fatalf("empty reason: %v", err)
	}
	if _, err := svc.Answer(q.ID, question.RequestChanges, "fix", "alice", "rhz-question-v1:bad"); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	r, err := svc.Answer(q.ID, question.RequestChanges, "fix", "alice", q.Digest)
	if err != nil || r.Decision != question.RequestChanges || r.Reason != "fix" || r.ActorRef != "unverified-local-operator:alice" || r.Revision != 2 {
		t.Fatalf("requestChanges: %+v %v", r, err)
	}
	if r.Decision.Terminal() {
		t.Fatal("requestChanges must be non-terminal")
	}
	before := len(s.All())
	if _, err := svc.Answer(q.ID, question.RequestChanges, "again", "alice", q.Digest); err == nil || err.Error() != "changes already requested" {
		t.Fatalf("second requestChanges: %v", err)
	}
	if _, err := svc.Answer(q.ID, question.Decision("maybe"), "x", "alice", q.Digest); err == nil || err.Error() != "invalid decision" {
		t.Fatalf("unknown decision: %v", err)
	}
	if len(s.All()) != before {
		t.Fatal("journal changed on rejected answers")
	}
	// approve after requestChanges: latest answer wins in the Ref
	r, err = svc.Answer(q.ID, question.Approve, "", "bob", q.Digest)
	if err != nil || r.Decision != question.Approve || r.Reason != "" || r.ActorRef != "unverified-local-operator:bob" || r.Revision != 3 {
		t.Fatalf("approve after requestChanges: %+v %v", r, err)
	}
	for _, d := range []question.Decision{question.Approve, question.Reject, question.RequestChanges} {
		if _, err := svc.Answer(q.ID, d, "x", "bob", q.Digest); err == nil || err.Error() != "gate already has input" {
			t.Fatalf("%s after approve: %v", d, err)
		}
	}
	// reject after requestChanges on a second question
	q2, _ := svc.Ask("t2", "b", "r", "", "", "req", "")
	if _, err := svc.Answer(q2.ID, question.RequestChanges, "fix", "alice", q2.Digest); err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Answer(q2.ID, question.Reject, "no", "bob", q2.Digest)
	if err != nil || r2.Decision != question.Reject || r2.Reason != "no" {
		t.Fatalf("reject after requestChanges: %+v %v", r2, err)
	}
	if _, err := svc.Answer(q2.ID, question.RequestChanges, "x", "bob", q2.Digest); err == nil {
		t.Fatal("requestChanges after reject accepted")
	}
}

func TestQuestionReplayRequestChangesSequencingFRRHZ109(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	q, _ := svc.Ask("t", "b", "r", "", "", "req", "")
	answered := func(rev uint64, decision, reason string) events.Event {
		p, _ := json.Marshal(map[string]string{"Decision": decision, "Reason": reason, "ActorRef": "unverified-local-operator:x", "Digest": q.Digest})
		return events.Event{AggregateType: "question", AggregateID: q.ID, Revision: rev, Type: "question.answered", Payload: p, CreatedAt: time.Now().UTC()}
	}
	asked := s.List("question", q.ID)[0]
	// legacy shape: asked + approve → unchanged
	if r, err := question.Replay([]events.Event{asked, answered(2, "approve", "")}); err != nil || r.Decision != question.Approve || r.Revision != 2 {
		t.Fatalf("legacy approve: %+v %v", r, err)
	}
	// requestChanges → approve: latest answer projected
	if r, err := question.Replay([]events.Event{asked, answered(2, "requestChanges", "fix"), answered(3, "approve", "")}); err != nil || r.Decision != question.Approve || r.Reason != "" || r.Revision != 3 {
		t.Fatalf("requestChanges→approve: %+v %v", r, err)
	}
	bad := map[string][]events.Event{
		"requestChanges twice":          {asked, answered(2, "requestChanges", "a"), answered(3, "requestChanges", "b")},
		"answer after approve":          {asked, answered(2, "approve", ""), answered(3, "requestChanges", "b")},
		"answer after reject":           {asked, answered(2, "reject", "a"), answered(3, "approve", "")},
		"requestChanges without reason": {asked, answered(2, "requestChanges", " ")},
		"unknown decision":              {asked, answered(2, "maybe", "a")},
	}
	for name, log := range bad {
		if _, err := question.Replay(log); err == nil {
			t.Errorf("%s: replay accepted", name)
		}
	}
}
