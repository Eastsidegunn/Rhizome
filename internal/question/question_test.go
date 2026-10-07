package question_test

import (
	"errors"
	"rhizome/internal/events"
	question "rhizome/internal/question"
	"rhizome/internal/workspace"
	"testing"
)

func TestQuestionLifecycleAndDigestFRRHZ081(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	body := "<x>\nquote \"한글\""
	q, e := svc.Ask("Title", body, "recommend", "", "", "req", "corr")
	if e != nil {
		t.Fatal(e)
	}
	if q.Digest != question.Digest(q.Title, q.Body, q.Recommendation) || q.ID != question.IDFor(q.Title, q.Body, q.Recommendation) {
		t.Fatal("digest/id")
	}
	q2, e := svc.Ask("Title", body, "recommend", "", "", "req", "corr")
	if e != nil || q2.Revision != 1 || len(s.All()) != 1 {
		t.Fatal("idempotency")
	}
	if _, e = svc.Answer(q.ID, question.Reject, "because", "actor", q.Digest); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Answer(q.ID, question.Approve, "", "actor", q.Digest); e == nil {
		t.Fatal("terminal answer accepted")
	}
}
func TestQuestionReplayRecomputesDigestFRRHZ081(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	q, _ := svc.Ask("t", "b", "r", "", "", "a", "")
	log := s.List("question", q.ID)
	log[0].Payload = []byte(`{"title":"t","body":"changed","recommendation":"r","digest":"` + q.Digest + `"}`)
	if _, e := question.Replay(log); e == nil {
		t.Fatal("tamper accepted")
	}
}
func TestQuestionVerifyDigestPrefixFRRHZ081(t *testing.T) {
	d := question.Digest("t", "b", "r")
	for _, x := range [][2]string{{d, d}, {"hx-args-digest-v1:" + d[len("rhz-question-v1:"):], d}, {d, "hx-args-digest-v1:" + d[len("rhz-question-v1:"):]}, {d, d + "x"}} {
		if (question.VerifyDigest(x[0], x[1]) == nil) != (x[0] == x[1]) {
			t.Fatalf("%q %q", x[0], x[1])
		}
	}
}

func TestQuestionAskAndReplayRequireRequestedByFRRHZ081(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	if _, err := svc.Ask("title", "body", "recommendation", "", "", "", ""); err == nil {
		t.Fatal("empty requestedBy accepted")
	}
	q, err := svc.Ask("title", "body", "recommendation", "", "", "operator", "")
	if err != nil || q.RequestedBy != "unverified-local-operator:operator" {
		t.Fatalf("requestedBy normalization: %+v %v", q, err)
	}
	log := s.List("question", q.ID)
	log[0].Payload = []byte(`{"title":"title","body":"body","recommendation":"recommendation","requestedBy":"operator","digest":"` + q.Digest + `"}`)
	if _, err := question.Replay(log); err == nil {
		t.Fatal("unmarked requestedBy accepted during replay")
	}
}

func TestQuestionReplayValidatesAnsweredActorFRRHZ081(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	q, err := svc.Ask("title", "body", "recommendation", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Answer(q.ID, question.Approve, "", "operator", q.Digest); err != nil {
		t.Fatal(err)
	}
	log := s.List("question", q.ID)
	log[1].Payload = []byte(`{"decision":"approve","reason":"","actorRef":"operator","digest":"` + q.Digest + `"}`)
	if _, err := question.Replay(log); err == nil {
		t.Fatal("unmarked answered actor accepted during replay")
	}
	if _, err := svc.Answer(q.ID, question.Approve, "", "unverified-local-operator:", q.Digest); err == nil {
		t.Fatal("empty prefixed actor accepted")
	}
}

func TestQuestionReplayRejectsEmptyTitleOrBodyFRRHZ081(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	q, err := svc.Ask("title", "body", "recommendation", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "body"} {
		payload := `{"title":"title","body":"body","recommendation":"recommendation","requestedBy":"unverified-local-operator:operator","digest":"` + q.Digest + `"}`
		if field == "title" {
			payload = `{"title":"","body":"body","recommendation":"recommendation","requestedBy":"unverified-local-operator:operator","digest":"` + q.Digest + `"}`
		} else {
			payload = `{"title":"title","body":"","recommendation":"recommendation","requestedBy":"unverified-local-operator:operator","digest":"` + q.Digest + `"}`
		}
		log := s.List("question", q.ID)
		log[0].Payload = []byte(payload)
		if _, err := question.Replay(log); err == nil {
			t.Fatalf("empty %s accepted during replay", field)
		}
	}
}

// TestQuestionAnswerDigestGuardsFRRHZ081 fixes the "sign what was seen" guard
// at both the question service and RelayIntent gate boundary.
func TestQuestionAnswerDigestGuardsFRRHZ081(t *testing.T) {
	s := &events.Store{}
	svc := question.Service{Store: s}
	q1, err := svc.Ask("one", "body", "rec", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	q2, err := svc.Ask("two", "body", "rec", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	if _, err = svc.Answer(q1.ID, question.Approve, "", "operator", q2.Digest); !errors.Is(err, question.ErrDigestMismatch) {
		t.Fatalf("other question digest: %v", err)
	}
	if got := len(s.All()); got != before {
		t.Fatalf("digest mismatch appended event: %d -> %d", before, got)
	}
	prefixed := "hx-args-digest-v1:" + q1.Digest[len("rhz-question-v1:"):]
	if _, err = svc.Answer(q1.ID, question.Approve, "", "operator", prefixed); !errors.Is(err, question.ErrDigestMismatch) {
		t.Fatalf("foreign digest prefix accepted: %v", err)
	}
	if got := len(s.All()); got != before {
		t.Fatalf("prefix mismatch appended event: %d -> %d", before, got)
	}

	q3, err := svc.Ask("three", "body", "rec", "", "", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{q2.Digest, "hx-args-digest-v1:" + q3.Digest[len("rhz-question-v1:"):]} {
		before = len(s.All())
		res, relayErr := workspace.RelayIntent(s, workspace.Intent{Kind: "gate.approve", GateID: q3.ID, Digest: digest}, "operator", false)
		if relayErr != nil || res.Accepted || res.Reason == "" {
			t.Fatalf("relay accepted invalid digest: result=%+v err=%v", res, relayErr)
		}
		if got := len(s.All()); got != before {
			t.Fatalf("relay mismatch appended event: %d -> %d", before, got)
		}
	}
	res, err := workspace.RelayIntent(s, workspace.Intent{Kind: "gate.approve", GateID: q3.ID, Digest: q3.Digest}, "operator", false)
	if err != nil || !res.Accepted {
		t.Fatalf("valid relay digest rejected: %+v %v", res, err)
	}
	if got := len(s.All()); got != before+1 {
		t.Fatalf("valid relay answer event count: got %d want %d", got, before+1)
	}
}
