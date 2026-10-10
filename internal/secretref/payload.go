// Package secretref is the secret registry (RHZ-107 G1, FR-RHZ-134, SCP §1):
// the seven secret events E1–E7 over the aggregates secretop, secretref and
// secretchan, the payload validation table V shared by writes and replay, the
// pure projection Project (R1–R9) and the single secret writer Service (§1.5).
//
// No payload has a value field (S2). Every error and refusal string is a
// fixed enum (S5): replay errors are "secret replay: <rule ID>", intent
// refusals are the §0.2 Reason strings, channel errors are §3.6 codes.
package secretref

import (
	"encoding/json"
	"errors"

	"rhizome/internal/events"
	"rhizome/internal/secretwire"
)

// Aggregate types (§1.1).
const (
	AggregateOp   = "secretop"
	AggregateRef  = "secretref"
	AggregateChan = "secretchan"
)

// Event types (§1.1).
const (
	TypeRegisterRequested = "secret.register.requested" // E1
	TypeRotateRequested   = "secret.rotate.requested"   // E2
	TypeRevokeRequested   = "secret.revoke.requested"   // E3
	TypeOpOutcome         = "secret.op.outcome"         // E4
	TypeReconciled        = "secret.reconciled"         // E5
	TypeObserved          = "secret.observed"           // E6
	TypeEntryRejected     = "secret.entry.rejected"     // E7
)

// Payloads (§1.2). The Go field names are the journal keys (S8): no json tags
// except omitempty. The structs are the only shapes ever marshalled.
type registerRequested struct { // E1
	OpID      string
	Name      string
	Kind      string
	ScopeTags []string `json:",omitempty"`
	ActorRef  string
}

type rotateRequested struct{ OpID, Ref, ActorRef string } // E2

type revokeRequested struct{ OpID, Ref, ReasonCode, ActorRef string } // E3

type opOutcome struct { // E4
	OpID, Ref     string
	Version       int64
	Result        string
	FailureCode   string `json:",omitempty"`
	PlaneID       string
	PlaneEntrySeq uint64
}

type reconciled struct { // E5
	Ref, OpID, Name, Kind string
	ScopeTags             []string `json:",omitempty"`
	Version               int64
	Origin                string
	PlaneID               string
	PlaneEntrySeq         uint64
}

type observed struct { // E6
	Ref             string
	Version         int64
	Present         bool
	PlaneObservedAt string
	PlaneID         string
	PlaneEntrySeq   uint64
}

type entryRejected struct { // E7
	PlaneID     string
	RejectedSeq uint64
	Reason      string
	AckSeq      uint64
}

// Rule IDs used as replay error codes (R8).
const (
	codeVJ  = "V-J"
	codeVK  = "V-K"
	codeVZ  = "V-Z"
	codeVA  = "V-A"
	codeVT  = "V-T"
	codeR1  = "R1"
	codeR2  = "R2"
	codeR3  = "R3"
	codeR4  = "R4"
	codeR5  = "R5"
	codeR6  = "R6"
	codeR7  = "R7"
	codeR7a = "R7a"
	codeR7b = "R7b"
)

// ReplayError is a V-table or R-table violation (R8). Its message is exactly
// "secret replay: <Code>" and never carries payload content.
type ReplayError struct{ Code string }

func (e *ReplayError) Error() string { return "secret replay: " + e.Code }

func replayErr(code string) error { return &ReplayError{Code: code} }

// shape describes one payload type's V-J/V-K position. Strings are only
// required to be non-empty here: the §0.1 grammar is V-Z, checked on the
// decoded struct so that the three rules report distinct codes.
type shape struct {
	full  *secretwire.Spec // required keys enforced
	loose *secretwire.Spec // same keys, all optional (tells V-K missing key from V-J)
}

func newShape(required, optional map[string]*secretwire.Spec) shape {
	full := map[string]secretwire.Field{}
	loose := map[string]secretwire.Field{}
	for k, s := range required {
		full[k] = secretwire.Required(s)
		loose[k] = secretwire.Optional(s)
	}
	for k, s := range optional {
		full[k] = secretwire.Optional(s)
		loose[k] = secretwire.Optional(s)
	}
	return shape{full: secretwire.Object(full), loose: secretwire.Object(loose)}
}

var (
	str      = secretwire.String(func(s string) bool { return s != "" })
	strList  = secretwire.StringList(func([]string) bool { return true })
	version  = secretwire.Int(0, secretwire.MaxVersion)          // G12
	planeSeq = secretwire.Int(1, secretwire.MaxSafeInt)          // G11
	ackSeq   = secretwire.Int(0, secretwire.MaxSafeInt)          // hw, 0 allowed
	boolean  = secretwire.Bool()                                 // G15
	noOpt    = map[string]*secretwire.Spec(nil)                  // no optional keys
	tagsOpt  = map[string]*secretwire.Spec{"ScopeTags": strList} // G4, omitempty
	failOpt  = map[string]*secretwire.Spec{"FailureCode": str}   // G8, omitempty

	shapes = map[string]shape{
		TypeRegisterRequested: newShape(map[string]*secretwire.Spec{
			"OpID": str, "Name": str, "Kind": str, "ActorRef": str}, tagsOpt),
		TypeRotateRequested: newShape(map[string]*secretwire.Spec{
			"OpID": str, "Ref": str, "ActorRef": str}, noOpt),
		TypeRevokeRequested: newShape(map[string]*secretwire.Spec{
			"OpID": str, "Ref": str, "ReasonCode": str, "ActorRef": str}, noOpt),
		TypeOpOutcome: newShape(map[string]*secretwire.Spec{
			"OpID": str, "Ref": str, "Version": version, "Result": str,
			"PlaneID": str, "PlaneEntrySeq": planeSeq}, failOpt),
		TypeReconciled: newShape(map[string]*secretwire.Spec{
			"Ref": str, "OpID": str, "Name": str, "Kind": str, "Version": version,
			"Origin": str, "PlaneID": str, "PlaneEntrySeq": planeSeq}, tagsOpt),
		TypeObserved: newShape(map[string]*secretwire.Spec{
			"Ref": str, "Version": version, "Present": boolean, "PlaneObservedAt": str,
			"PlaneID": str, "PlaneEntrySeq": planeSeq}, noOpt),
		TypeEntryRejected: newShape(map[string]*secretwire.Spec{
			"PlaneID": str, "RejectedSeq": planeSeq, "Reason": str, "AckSeq": ackSeq}, noOpt),
	}
)

// checkType is V-T: the Type/AggregateType/Revision combination of §1.1.
// Revision continuity within an aggregate is checked by the fold (R1 for
// secretop, V-T otherwise).
func checkType(e events.Event) bool {
	switch e.Type {
	case TypeRegisterRequested, TypeRotateRequested, TypeRevokeRequested:
		return e.AggregateType == AggregateOp && e.Revision == 1
	case TypeOpOutcome:
		return e.AggregateType == AggregateOp && e.Revision == 2
	case TypeReconciled:
		return e.AggregateType == AggregateRef && e.Revision == 1
	case TypeObserved:
		return e.AggregateType == AggregateRef && e.Revision >= 1
	case TypeEntryRejected:
		return e.AggregateType == AggregateChan && e.Revision >= 1
	}
	return false
}

// strictCode is V-J (§0.1a 0–5) and V-K on the raw payload bytes.
func strictCode(raw []byte, sh shape) string {
	err := secretwire.StrictDecode(raw, sh.full)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, secretwire.ErrUnknownField):
		return codeVK // key outside the allowlist
	case secretwire.StrictDecode(raw, sh.loose) == nil:
		return codeVK // a required key is missing
	}
	return codeVJ
}

// validate is the V table (§1.3) shared by every write (on the marshalled
// candidate) and by replay (on the journal's raw payload): V-T, V-J, V-K,
// V-Z, and the envelope part of V-A. It returns the decoded payload, one of
// the seven payload struct values. The R table is the fold's business.
func validate(e events.Event) (any, error) {
	if !checkType(e) {
		return nil, replayErr(codeVT)
	}
	if code := strictCode(e.Payload, shapes[e.Type]); code != "" {
		return nil, replayErr(code)
	}
	var (
		p                       any
		aggID, corrID           string
		grammarOK, decodeFailed bool
		derivedOK               = true
	)
	switch e.Type {
	case TypeRegisterRequested:
		var v registerRequested
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidOpID(v.OpID) && secretwire.ValidName(v.Name) &&
			secretwire.ValidRefKind(v.Kind) && (v.ScopeTags == nil || secretwire.ValidScopeTags(v.ScopeTags)) &&
			secretwire.ValidActorRef(v.ActorRef)
		p, aggID, corrID = v, v.OpID, v.OpID
	case TypeRotateRequested:
		var v rotateRequested
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidOpID(v.OpID) && secretwire.ValidRef(v.Ref) && secretwire.ValidActorRef(v.ActorRef)
		p, aggID, corrID = v, v.OpID, v.OpID
	case TypeRevokeRequested:
		var v revokeRequested
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidOpID(v.OpID) && secretwire.ValidRef(v.Ref) &&
			secretwire.ValidReasonCode(v.ReasonCode) && secretwire.ValidActorRef(v.ActorRef)
		p, aggID, corrID = v, v.OpID, v.OpID
	case TypeOpOutcome:
		var v opOutcome
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidOpID(v.OpID) && secretwire.ValidRef(v.Ref) &&
			secretwire.ValidResult(v.Result) && secretwire.ValidPlaneID(v.PlaneID) &&
			((v.Result == secretwire.ResultFailed) == (v.FailureCode != "")) &&
			(v.FailureCode == "" || secretwire.ValidFailureCode(v.FailureCode))
		p, aggID, corrID = v, v.OpID, v.OpID
	case TypeReconciled:
		var v reconciled
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidRef(v.Ref) && secretwire.ValidOpID(v.OpID) && secretwire.ValidName(v.Name) &&
			secretwire.ValidRefKind(v.Kind) && (v.ScopeTags == nil || secretwire.ValidScopeTags(v.ScopeTags)) &&
			secretwire.ValidOrigin(v.Origin) && secretwire.ValidPlaneID(v.PlaneID)
		p, aggID, corrID = v, v.Ref, v.OpID
		derivedOK = v.Ref == secretwire.DeriveRef(v.OpID)
	case TypeObserved:
		var v observed
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidRef(v.Ref) && secretwire.ValidObservedAt(v.PlaneObservedAt) &&
			secretwire.ValidPlaneID(v.PlaneID)
		p, aggID, corrID = v, v.Ref, v.PlaneID
	case TypeEntryRejected:
		var v entryRejected
		decodeFailed = json.Unmarshal(e.Payload, &v) != nil
		grammarOK = secretwire.ValidPlaneID(v.PlaneID) && secretwire.ValidEntryReason(v.Reason)
		p, aggID, corrID = v, v.PlaneID, v.PlaneID
	}
	if decodeFailed {
		return nil, replayErr(codeVJ)
	}
	if !grammarOK {
		return nil, replayErr(codeVZ)
	}
	if e.ID != "" || e.CausationID != "" || e.AggregateID != aggID || e.CorrelationID != corrID || !derivedOK {
		return nil, replayErr(codeVA)
	}
	return p, nil
}
