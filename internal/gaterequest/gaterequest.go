package gaterequest

// RHZ-047 (D17): aggregate "approvalrequest" — the durable pending surface of
// one observed JANUS approval request. Single event. The aggregate ID equals
// approval.IDFor(key), so the pending gate and the eventual human input share
// one identity. Raw request args never enter this record (비복제 헌장): only
// the key and the display metadata handed back by the socket pending query.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"rhizome/internal/approval"
	"rhizome/internal/events"
)

type Ref struct {
	ID           string
	Key          approval.RequestKey
	Name, Reason string
	// RequestDigest/PolicyHash/DisplaySummary arrive verbatim from the socket
	// pending query — the only contract source of digest and summary. Never
	// recomputed locally.
	RequestDigest, PolicyHash, DisplaySummary string
	// ExpiresAt is display-only material carried verbatim (RHZ-047 D25):
	// Rhizome never compares it against a local clock — expiry is JANUS's
	// lease to enforce, and finality arrives as durable expired{deny}.
	ExpiresAt                          int64
	MissionID, ExecutionID, DecisionID string
	Sequence                           uint64
}

type Service struct{ Store events.Port }

type payload struct {
	Key                                       approval.RequestKey
	Name, Reason                              string
	RequestDigest, PolicyHash, DisplaySummary string
	ExpiresAt                                 int64
	MissionID, ExecutionID, DecisionID        string
}

var (
	traceRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanRE  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// validate is shared by Record and Replay so the service and the replay
// enforce identical rules (L-i: reference fields are
// format-checked, the caller supplies them from the execution replay).
func validate(p payload, aggregate string) error {
	if !traceRE.MatchString(p.Key.TraceID) || !spanRE.MatchString(p.Key.SpanID) || p.Key.RequestID == "" {
		return fmt.Errorf("invalid request key")
	}
	if aggregate != approval.IDFor(p.Key) {
		return fmt.Errorf("aggregate/key identity mismatch")
	}
	if strings.TrimSpace(p.RequestDigest) == "" {
		return fmt.Errorf("request digest required")
	}
	if p.ExpiresAt < 0 {
		return fmt.Errorf("invalid expires_at")
	}
	if strings.TrimSpace(p.MissionID) == "" || strings.TrimSpace(p.ExecutionID) == "" || strings.TrimSpace(p.DecisionID) == "" {
		return fmt.Errorf("mission, execution and decision references required")
	}
	return nil
}

// Record persists one observed pending request. The ID is always derived,
// never caller-chosen; a second record for the same key is refused.
func (s Service) Record(r Ref) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	p := payload{Key: r.Key, Name: r.Name, Reason: r.Reason, RequestDigest: r.RequestDigest, PolicyHash: r.PolicyHash, DisplaySummary: r.DisplaySummary, ExpiresAt: r.ExpiresAt, MissionID: r.MissionID, ExecutionID: r.ExecutionID, DecisionID: r.DecisionID}
	id := approval.IDFor(r.Key)
	if err := validate(p, id); err != nil {
		return Ref{}, err
	}
	if len(s.Store.List("approvalrequest", id)) > 0 {
		return Ref{}, fmt.Errorf("duplicate approval request")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Ref{}, err
	}
	e := events.Event{AggregateType: "approvalrequest", AggregateID: id, Revision: 1, Type: "approvalrequest.observed", Payload: b, CreatedAt: time.Now().UTC()}
	if err = s.Store.Append(0, e); err != nil {
		return Ref{}, err
	}
	return Replay(s.Store.List("approvalrequest", id))
}

func Replay(log []events.Event) (Ref, error) {
	if len(log) != 1 {
		return Ref{}, fmt.Errorf("invalid approval request stream")
	}
	e := log[0]
	if e.AggregateType != "approvalrequest" || e.Type != "approvalrequest.observed" || e.Revision != 1 {
		return Ref{}, fmt.Errorf("invalid approval request event")
	}
	var p payload
	if json.Unmarshal(e.Payload, &p) != nil {
		return Ref{}, fmt.Errorf("malformed approval request")
	}
	if err := validate(p, e.AggregateID); err != nil {
		return Ref{}, err
	}
	return Ref{ID: e.AggregateID, Key: p.Key, Name: p.Name, Reason: p.Reason, RequestDigest: p.RequestDigest, PolicyHash: p.PolicyHash, DisplaySummary: p.DisplaySummary, ExpiresAt: p.ExpiresAt, MissionID: p.MissionID, ExecutionID: p.ExecutionID, DecisionID: p.DecisionID, Sequence: e.Sequence}, nil
}

func (s Service) Get(id string) (Ref, error) {
	if s.Store == nil {
		return Ref{}, fmt.Errorf("nil store")
	}
	return Replay(s.Store.List("approvalrequest", id))
}
