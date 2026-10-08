package workspace

import (
	"encoding/json"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/journal"
	"rhizome/internal/question"
	"rhizome/internal/trust"
)

func noAuthority() trust.Authority { return trust.Authority{} }

func mustQuestionID(title, body, recommendation string) string {
	id, err := question.IDFor(title, body, recommendation)
	if err != nil {
		panic(err)
	}
	return id
}

func mustQuestionIDForDigest(digest string) string {
	id, err := question.IDForDigest(digest)
	if err != nil {
		panic(err)
	}
	return id
}

func openTestJournal(path string) (*journal.Journal, error) {
	return journal.OpenGuarded(path, trust.NewAnchorless())
}

// appendLegacyApprovalInput is test-only replay setup for historical
// ActorVerified:true bytes. New writer APIs cannot create this value.
func appendLegacyApprovalInput(store events.Port, key approval.RequestKey, decision approval.Decision, reason, response, digest, actor, correlation, decisionID string, gate approval.GateFields) (approval.Ref, error) {
	payload, err := json.Marshal(struct {
		approval.RequestKey
		Decision                                                               approval.Decision `json:"decision"`
		Reason, ResponseID, RequestDigest, ActorRef, CorrelationID, DecisionID string
		ActorVerified                                                          bool
		approval.GateFields
	}{key, decision, reason, response, digest, actor, correlation, decisionID, true, gate})
	if err != nil {
		return approval.Ref{}, err
	}
	id := approval.IDFor(key)
	event := events.Event{AggregateType: "approval", AggregateID: id, Revision: 1, Type: "approval.input_recorded", Payload: payload, CreatedAt: time.Now().UTC()}
	if err := store.Append(0, event); err != nil {
		return approval.Ref{}, err
	}
	return (approval.Service{Store: store}).Get(id)
}
