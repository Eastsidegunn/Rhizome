package workspace

import (
	"errors"
	"rhizome/internal/approval"
	"rhizome/internal/events"
	"rhizome/internal/gaterequest"
	"rhizome/internal/question"
	"rhizome/internal/trust"
	"sort"
)

var errSigningGateNotFound = errors.New("gate not found")

// SigningInputDTO is the exact read-only input a signer needs for one gate.
// The decision trio is nil together for a pending gate.
type SigningInputDTO struct {
	JournalID          string  `json:"journalId,omitempty"`
	Consumer           string  `json:"consumer"`
	GateID             string  `json:"gateId"`
	Title              string  `json:"title"`
	RequestDigest      string  `json:"requestDigest"`
	State              string  `json:"state"`
	DecisionSequence   *uint64 `json:"decisionSequence,omitempty"`
	Decision           *string `json:"decision,omitempty"`
	Reason             *string `json:"reason,omitempty"`
	VerificationStatus string  `json:"verificationStatus"`
}

// SigningListItemDTO is one terminal, not-yet-verified manifest candidate.
type SigningListItemDTO struct {
	Consumer         string `json:"consumer"`
	GateID           string `json:"gateId"`
	DecisionSequence uint64 `json:"decisionSequence"`
	RequestDigest    string `json:"requestDigest"`
	Decision         string `json:"decision"`
	Reason           string `json:"reason"`
	Title            string `json:"title"`
}

// SigningInputsDTO is the list-mode signing response.
type SigningInputsDTO struct {
	JournalID string               `json:"journalId,omitempty"`
	Items     []SigningListItemDTO `json:"items"`
}

func signingStatus(verification *GateVerification) string {
	if verification == nil {
		return "none"
	}
	return verification.Status
}

func signingInput(store events.Port, verifier *trust.Verifier, attestations map[string]trust.Attestation, journalID, gateID string) (SigningInputDTO, error) {
	if log := store.List("question", gateID); len(log) != 0 {
		q, err := (question.Service{Store: store}).Get(gateID)
		if err != nil {
			return SigningInputDTO{}, err
		}
		verification, err := questionVerification(store, verifier, attestations, q)
		if err != nil {
			return SigningInputDTO{}, err
		}
		out := SigningInputDTO{JournalID: journalID, Consumer: "question", GateID: q.ID, Title: q.Title, RequestDigest: q.Digest, State: questionState(q), VerificationStatus: signingStatus(verification)}
		if event, ok := storedDecision(log, trust.QuestionAnsweredType); ok {
			sequence, decision, reason := event.Sequence, string(q.Decision), q.Reason
			out.DecisionSequence, out.Decision, out.Reason = &sequence, &decision, &reason
		}
		return out, nil
	}
	if log := store.List("approval", gateID); len(log) != 0 {
		a, err := (approval.Service{Store: store}).Get(gateID)
		if err != nil {
			return SigningInputDTO{}, err
		}
		verification, err := approvalVerification(store, verifier, attestations, a)
		if err != nil {
			return SigningInputDTO{}, err
		}
		title := a.GateName
		if requestLog := store.List("approvalrequest", gateID); len(requestLog) != 0 {
			request, err := gaterequest.Replay(requestLog)
			if err != nil {
				return SigningInputDTO{}, err
			}
			if request.DisplaySummary != "" {
				title = request.DisplaySummary
			}
			if title == "" {
				title = request.Name
			}
		}
		state := string(a.State)
		out := SigningInputDTO{JournalID: journalID, Consumer: "approval", GateID: a.ID, Title: title, RequestDigest: a.RequestDigest, State: state, VerificationStatus: signingStatus(verification)}
		if event, ok := storedDecision(log, trust.ApprovalInputRecordedType); ok {
			sequence, decision, reason := event.Sequence, string(a.HumanDecision), a.Reason
			out.DecisionSequence, out.Decision, out.Reason = &sequence, &decision, &reason
		}
		return out, nil
	}
	if log := store.List("approvalrequest", gateID); len(log) != 0 {
		request, err := gaterequest.Replay(log)
		if err != nil {
			return SigningInputDTO{}, err
		}
		title := request.DisplaySummary
		if title == "" {
			title = request.Name
		}
		return SigningInputDTO{JournalID: journalID, Consumer: "approval", GateID: request.ID, Title: title, RequestDigest: request.RequestDigest, State: "pending", VerificationStatus: "none"}, nil
	}
	return SigningInputDTO{}, errSigningGateNotFound
}

func signingContext(store events.Port, verifier *trust.Verifier) (map[string]trust.Attestation, string, error) {
	if store == nil {
		return nil, "", errors.New("nil store")
	}
	if verifier == nil {
		verifier = trust.NewAnchorless()
	}
	attestations, err := verifier.Attestations(store)
	if err != nil {
		return nil, "", err
	}
	summary, err := verifier.TrustSummary(store)
	if err != nil {
		return nil, "", err
	}
	journalID := ""
	if summary != nil && verifier.Anchored() {
		journalID = summary.JournalID
	}
	return attestations, journalID, nil
}

// SigningInput returns one gate's canonical signing material without writes.
func SigningInput(store events.Port, verifier *trust.Verifier, gateID string) (SigningInputDTO, error) {
	if verifier == nil {
		verifier = trust.NewAnchorless()
	}
	attestations, journalID, err := signingContext(store, verifier)
	if err != nil {
		return SigningInputDTO{}, err
	}
	return signingInput(store, verifier, attestations, journalID, gateID)
}

// SigningInputs returns terminal gates that are neither verified nor attested.
func SigningInputs(store events.Port, verifier *trust.Verifier) (SigningInputsDTO, error) {
	if verifier == nil {
		verifier = trust.NewAnchorless()
	}
	attestations, journalID, err := signingContext(store, verifier)
	if err != nil {
		return SigningInputsDTO{}, err
	}
	ids := map[string]bool{}
	for _, event := range store.All() {
		switch event.AggregateType {
		case "question", "approval", "approvalrequest":
			ids[event.AggregateID] = true
		}
	}
	out := SigningInputsDTO{JournalID: journalID, Items: []SigningListItemDTO{}}
	for id := range ids {
		input, err := signingInput(store, verifier, attestations, journalID, id)
		if err != nil {
			return SigningInputsDTO{}, err
		}
		if input.DecisionSequence == nil || input.Decision == nil || input.Reason == nil || input.VerificationStatus == "verified" || input.VerificationStatus == "attested" {
			continue
		}
		switch input.Consumer {
		case "question":
			if input.State != "approved" && input.State != "rejected" {
				continue
			}
		case "approval":
			if input.State != string(approval.InputRecorded) && input.State != string(approval.Dispatched) && input.State != string(approval.Observed) {
				continue
			}
		default:
			continue
		}
		out.Items = append(out.Items, SigningListItemDTO{Consumer: input.Consumer, GateID: input.GateID, DecisionSequence: *input.DecisionSequence, RequestDigest: input.RequestDigest, Decision: *input.Decision, Reason: *input.Reason, Title: input.Title})
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].DecisionSequence < out.Items[j].DecisionSequence })
	return out, nil
}
