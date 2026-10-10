package secretwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// Channel wire v1 limits (W3, W7, W9).
const (
	MaxFrameBytes = 65536 // one frame including its trailing newline
	MaxEntries    = 64
	MaxPending    = 64
	// HandshakeVersion is the fixed v of hello and error frames (W5).
	HandshakeVersion = 1
)

// supportedVersions is Rhizome's hello.versions (W5).
var supportedVersions = []uint64{1}

// SupportedVersions returns a copy of the wire versions this build speaks.
func SupportedVersions() []uint64 { return append([]uint64(nil), supportedVersions...) }

// Negotiate returns the highest version both sides speak, or false when there
// is none (the plane then answers error{code:"unsupported-version"}).
func Negotiate(peer []uint64) (uint64, bool) {
	best, ok := uint64(0), false
	for _, v := range peer {
		if supported(v) && (!ok || v > best) {
			best, ok = v, true
		}
	}
	return best, ok
}

func supported(v uint64) bool {
	for _, s := range supportedVersions {
		if v == s {
			return true
		}
	}
	return false
}

// Frame types, roles and entry facts (§3.3).
const (
	TypeHello = "hello"
	TypePoll  = "poll"
	TypeBatch = "batch"
	TypeError = "error"

	RoleRhizome = "rhizome"
	RolePlane   = "plane"

	FactOutcome    = "outcome"
	FactObserved   = "observed"
	FactReconciled = "reconciled"
)

// Error frame codes (§3.3). The first four may come from either side; the
// last three are sent by Rhizome only.
const (
	CodeUnsupportedVersion = "unsupported-version"
	CodeMalformed          = "malformed"
	CodePeerRejected       = "peer-rejected"
	CodeBusy               = "busy"
	CodeRetiredPlane       = "retired-plane"
	CodeEntryRejected      = "entry-rejected"
	CodeJournalPoisoned    = "journal-poisoned"
)

// Entry rejection codes <e> (§3.6), carried by E7, entry-rejected and
// lastRejected.
const (
	EntrySeqConflict      = "seq-conflict"
	EntrySeqGap           = "seq-gap"
	EntryUnknownOp        = "unknown-op"
	EntryUnknownRef       = "unknown-ref"
	EntryRefMismatch      = "ref-mismatch"
	EntryVersionMismatch  = "version-mismatch"
	EntryRedundantOutcome = "redundant-outcome"
	EntryRefExists        = "ref-exists"
	EntryState            = "state"
	// EntryAttestedOp stays in the enum but is unreachable: the approval
	// record (Q2) allows rotate/revoke of attested refs.
	EntryAttestedOp = "attested-op"
)

// ValidEntryReason reports whether s is a §3.6 entry code.
func ValidEntryReason(s string) bool {
	return in(s, EntrySeqConflict, EntrySeqGap, EntryUnknownOp, EntryUnknownRef, EntryRefMismatch,
		EntryVersionMismatch, EntryRedundantOutcome, EntryRefExists, EntryState, EntryAttestedOp)
}

// Local channel report messages (§3.6, scope "secret channel"). Frame and
// entry rejections are reported as the Err* frame errors' strings and
// EntryRejectedPrefix + <e>.
const (
	ReportSocketDirUnsafe     = "socket-dir-unsafe"
	ReportDialFailed          = "dial-failed"
	ReportTimeout             = "timeout"
	ReportPeerRejected        = "peer-rejected"
	ReportRetiredPlane        = "retired-plane"
	ReportAppendFailed        = "append-failed"
	ReportRegistryUnavailable = "registry-unavailable"
	ReportJournalPoisoned     = "journal-poisoned"
	ReportStallCleared        = "stall-cleared"
	EntryRejectedPrefix       = "entry-rejected:"
)

// Frame rejection errors: exactly the §3.6 frame-rejected:<f> set.
var (
	ErrFrameTooLarge        = errors.New("frame-rejected:too-large")
	ErrFrameMalformed       = errors.New("frame-rejected:malformed")
	ErrFrameUnknownField    = errors.New("frame-rejected:unknown-field")
	ErrFrameBadVersion      = errors.New("frame-rejected:bad-version")
	ErrFrameBadType         = errors.New("frame-rejected:bad-type")
	ErrFramePlaneIDMismatch = errors.New("frame-rejected:plane-id-mismatch")
	ErrFrameSeqOrder        = errors.New("frame-rejected:seq-order")
	ErrFrameTooManyEntries  = errors.New("frame-rejected:too-many-entries")
)

// Ack is the ack object (§3.3). PlaneEntrySeq 0 means nothing durable yet.
type Ack struct {
	PlaneID       string `json:"planeId"`
	PlaneEntrySeq uint64 `json:"planeEntrySeq"`
	JournalSeq    uint64 `json:"journalSeq"`
}

// Rejected is poll.lastRejected (R7b).
type Rejected struct {
	Seq    uint64 `json:"seq"`
	Reason string `json:"reason"`
}

// Pending is one poll.pending[] element. ScopeTags appears only for register
// (when non-empty), ReasonCode only (and always) for revoke.
type Pending struct {
	OpID       string   `json:"opId"`
	Op         string   `json:"op"`
	Ref        string   `json:"ref"`
	Name       string   `json:"name"`
	RefKind    string   `json:"refKind"`
	ScopeTags  []string `json:"scopeTags,omitempty"`
	ReasonCode string   `json:"reasonCode,omitempty"`
	JournalSeq uint64   `json:"journalSeq"`
}

// Entry is one batch.entries[] element; Fact selects the key set:
// outcome (OpID, Ref, Version, Result, FailureCode iff failed), observed
// (Ref, Version, Present, PlaneObservedAt), reconciled (OpID, Ref, Name,
// RefKind, Version=1, Origin, ScopeTags if any).
type Entry struct {
	Seq             uint64   `json:"seq"`
	Fact            string   `json:"fact"`
	OpID            string   `json:"opId"`
	Ref             string   `json:"ref"`
	Version         uint64   `json:"version"`
	Result          string   `json:"result"`
	FailureCode     string   `json:"failureCode"`
	Present         bool     `json:"present"`
	PlaneObservedAt string   `json:"planeObservedAt"`
	Name            string   `json:"name"`
	RefKind         string   `json:"refKind"`
	Origin          string   `json:"origin"`
	ScopeTags       []string `json:"scopeTags"`
}

// MarshalJSON emits exactly the key set of e.Fact.
func (e Entry) MarshalJSON() ([]byte, error) {
	switch e.Fact {
	case FactOutcome:
		return json.Marshal(struct {
			Seq         uint64 `json:"seq"`
			Fact        string `json:"fact"`
			OpID        string `json:"opId"`
			Ref         string `json:"ref"`
			Version     uint64 `json:"version"`
			Result      string `json:"result"`
			FailureCode string `json:"failureCode,omitempty"`
		}{e.Seq, e.Fact, e.OpID, e.Ref, e.Version, e.Result, e.FailureCode})
	case FactObserved:
		return json.Marshal(struct {
			Seq             uint64 `json:"seq"`
			Fact            string `json:"fact"`
			Ref             string `json:"ref"`
			Version         uint64 `json:"version"`
			Present         bool   `json:"present"`
			PlaneObservedAt string `json:"planeObservedAt"`
		}{e.Seq, e.Fact, e.Ref, e.Version, e.Present, e.PlaneObservedAt})
	case FactReconciled:
		return json.Marshal(struct {
			Seq       uint64   `json:"seq"`
			Fact      string   `json:"fact"`
			OpID      string   `json:"opId"`
			Ref       string   `json:"ref"`
			Name      string   `json:"name"`
			RefKind   string   `json:"refKind"`
			Version   uint64   `json:"version"`
			Origin    string   `json:"origin"`
			ScopeTags []string `json:"scopeTags,omitempty"`
		}{e.Seq, e.Fact, e.OpID, e.Ref, e.Name, e.RefKind, e.Version, e.Origin, e.ScopeTags})
	}
	return nil, ErrFrameMalformed
}

// RhizomeHello is hello R→P; Versions is always SupportedVersions on send.
type RhizomeHello struct{ Versions []uint64 }

// PlaneHello is hello P→R.
type PlaneHello struct {
	Version uint64
	PlaneID string
}

// Poll is poll R→P. V is the negotiated version.
type Poll struct {
	V            uint64
	Pending      []Pending
	Ack          Ack
	LastRejected *Rejected
}

// Batch is batch P→R. V is the negotiated version.
type Batch struct {
	V       uint64
	PlaneID string
	Entries []Entry
}

// ErrorFrame is error (v always 1). Reason, Ack and RejectedSeq are present
// iff Code == CodeEntryRejected.
type ErrorFrame struct {
	Code        string
	Reason      string
	Ack         *Ack
	RejectedSeq uint64
}

// Frame is a decoded frame; Type names the populated member.
type Frame struct {
	Type         string
	RhizomeHello RhizomeHello
	PlaneHello   PlaneHello
	Poll         Poll
	Batch        Batch
	Error        ErrorFrame
}

// Wire structs: field order is the deterministic encoding order.
type (
	rhizomeHelloWire struct {
		V        uint64   `json:"v"`
		Type     string   `json:"type"`
		Role     string   `json:"role"`
		Versions []uint64 `json:"versions"`
	}
	planeHelloWire struct {
		V       uint64 `json:"v"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Version uint64 `json:"version"`
		PlaneID string `json:"planeId"`
	}
	pollWire struct {
		V            uint64    `json:"v"`
		Type         string    `json:"type"`
		Pending      []Pending `json:"pending"`
		Ack          Ack       `json:"ack"`
		LastRejected *Rejected `json:"lastRejected,omitempty"`
	}
	batchWire struct {
		V       uint64  `json:"v"`
		Type    string  `json:"type"`
		PlaneID string  `json:"planeId"`
		Entries []Entry `json:"entries"`
	}
	errorWire struct {
		V           uint64 `json:"v"`
		Type        string `json:"type"`
		Code        string `json:"code"`
		Reason      string `json:"reason,omitempty"`
		Ack         *Ack   `json:"ack,omitempty"`
		RejectedSeq uint64 `json:"rejectedSeq,omitempty"`
	}
)

// Specs (W4: exact keys at every object level, §3.3).
var (
	anyInt     = Int(0, MaxSafeInt)
	seqSpec    = Int(1, MaxSafeInt)         // G11
	versionInt = Int(0, MaxVersion)         // G12
	opIDSpec   = String(ValidOpID)          // G1
	refSpec    = String(ValidRef)           // G2
	nameSpec   = String(ValidName)          // G3
	tagsSpec   = StringList(ValidScopeTags) // G4
	kindSpec   = String(ValidRefKind)       // G5
	planeSpec  = String(ValidPlaneID)       // G10
	reasonSpec = String(ValidEntryReason)   // §3.6 <e>
	jseqSpec   = Int(0, MaxSafeInt)         // G16
	typeSpec   = func(t string) *Spec { return Enum(t) }

	ackSpec = Object(map[string]Field{
		"planeId":       Required(planeSpec),
		"planeEntrySeq": Required(Int(0, MaxSafeInt)),
		"journalSeq":    Required(jseqSpec),
	})

	rhizomeHelloSpec = Object(map[string]Field{
		"v":        Required(anyInt),
		"type":     Required(typeSpec(TypeHello)),
		"role":     Required(Enum(RoleRhizome)),
		"versions": Required(Array(anyInt, 1, 8)),
	})
	planeHelloSpec = Object(map[string]Field{
		"v":       Required(anyInt),
		"type":    Required(typeSpec(TypeHello)),
		"role":    Required(Enum(RolePlane)),
		"version": Required(anyInt),
		"planeId": Required(planeSpec),
	})

	pendingSpec = func() *Spec {
		base := func(op string, extra map[string]Field) *Spec {
			fields := map[string]Field{
				"opId":       Required(opIDSpec),
				"op":         Required(Enum(op)),
				"ref":        Required(refSpec),
				"name":       Required(nameSpec),
				"refKind":    Required(kindSpec),
				"journalSeq": Required(jseqSpec),
			}
			for k, f := range extra {
				fields[k] = f
			}
			return Object(fields)
		}
		return Union("op", map[string]*Spec{
			OpRegister: base(OpRegister, map[string]Field{"scopeTags": Optional(tagsSpec)}),
			OpRotate:   base(OpRotate, nil),
			OpRevoke:   base(OpRevoke, map[string]Field{"reasonCode": Required(String(ValidReasonCode))}),
		})
	}()

	pollSpec = Object(map[string]Field{
		"v":       Required(anyInt),
		"type":    Required(typeSpec(TypePoll)),
		"pending": Required(Array(pendingSpec, 0, MaxPending)),
		"ack":     Required(ackSpec),
		"lastRejected": Optional(Object(map[string]Field{
			"seq":    Required(seqSpec),
			"reason": Required(reasonSpec),
		})),
	})

	entrySpec = func() *Spec {
		outcome := func(result string, failed bool) *Spec {
			fields := map[string]Field{
				"seq":     Required(seqSpec),
				"fact":    Required(Enum(FactOutcome)),
				"opId":    Required(opIDSpec),
				"ref":     Required(refSpec),
				"version": Required(versionInt),
				"result":  Required(Enum(result)),
			}
			if failed {
				fields["failureCode"] = Required(String(ValidFailureCode))
			}
			return Object(fields)
		}
		return Union("fact", map[string]*Spec{
			FactOutcome: Union("result", map[string]*Spec{
				ResultOK:       outcome(ResultOK, false),
				ResultRejected: outcome(ResultRejected, false),
				ResultFailed:   outcome(ResultFailed, true),
			}),
			FactObserved: Object(map[string]Field{
				"seq":             Required(seqSpec),
				"fact":            Required(Enum(FactObserved)),
				"ref":             Required(refSpec),
				"version":         Required(versionInt),
				"present":         Required(Bool()),
				"planeObservedAt": Required(String(ValidObservedAt)),
			}),
			FactReconciled: Object(map[string]Field{
				"seq":       Required(seqSpec),
				"fact":      Required(Enum(FactReconciled)),
				"opId":      Required(opIDSpec),
				"ref":       Required(refSpec),
				"name":      Required(nameSpec),
				"refKind":   Required(kindSpec),
				"version":   Required(Int(1, 1)), // R6
				"origin":    Required(String(ValidOrigin)),
				"scopeTags": Optional(tagsSpec),
			}),
		})
	}()

	// entries has no element cap here: the count is checked separately so
	// that it reports too-many-entries.
	batchSpec = Object(map[string]Field{
		"v":       Required(anyInt),
		"type":    Required(typeSpec(TypeBatch)),
		"planeId": Required(planeSpec),
		"entries": Required(Array(entrySpec, 0, -1)),
	})

	planeErrorSpec   = errorSpec(CodeUnsupportedVersion, CodeMalformed, CodePeerRejected, CodeBusy)
	rhizomeErrorSpec = errorSpec(CodeUnsupportedVersion, CodeMalformed, CodePeerRejected, CodeBusy,
		CodeRetiredPlane, CodeEntryRejected, CodeJournalPoisoned)
)

func errorSpec(codes ...string) *Spec {
	variants := map[string]*Spec{}
	for _, c := range codes {
		fields := map[string]Field{
			"v":    Required(anyInt),
			"type": Required(typeSpec(TypeError)),
			"code": Required(Enum(c)),
		}
		if c == CodeEntryRejected {
			fields["reason"] = Required(reasonSpec)
			fields["ack"] = Required(ackSpec)
			fields["rejectedSeq"] = Required(seqSpec)
		}
		variants[c] = Object(fields)
	}
	return Union("code", variants)
}

// DecodeFromPlane validates one frame Rhizome read from the plane (W3–W5,
// W9 seq/count). frame is a whole NDJSON line including its '\n'. want is
// the type the protocol step expects (TypeHello or TypeBatch); an error frame
// with a plane code is always admissible. negotiated is the version from the
// plane hello and is ignored for hello and error frames. W9's planeId match
// is CheckBatchPlane.
func DecodeFromPlane(frame []byte, want string, negotiated uint64) (Frame, error) {
	return decodeFrame(frame, want, negotiated, false)
}

// DecodeFromRhizome validates one frame the plane read from Rhizome. want is
// TypeHello, TypePoll or TypeError; error frames are always admissible.
func DecodeFromRhizome(frame []byte, want string, negotiated uint64) (Frame, error) {
	return decodeFrame(frame, want, negotiated, true)
}

func decodeFrame(frame []byte, want string, negotiated uint64, fromRhizome bool) (Frame, error) {
	if len(frame) > MaxFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	line, ok := bytes.CutSuffix(frame, []byte{'\n'})
	if !ok || bytes.IndexByte(line, '\n') >= 0 {
		return Frame{}, ErrFrameMalformed
	}
	tree, err := parseStrict(line)
	if err != nil {
		return Frame{}, ErrFrameMalformed
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return Frame{}, ErrFrameMalformed
	}
	typ, ok := obj["type"].(string)
	if !ok || (typ != want && typ != TypeError) {
		return Frame{}, ErrFrameBadType
	}
	var spec *Spec
	switch {
	case typ == TypeError && fromRhizome:
		spec = rhizomeErrorSpec
	case typ == TypeError:
		spec = planeErrorSpec
	case typ == TypeHello && fromRhizome:
		spec = rhizomeHelloSpec
	case typ == TypeHello:
		spec = planeHelloSpec
	case typ == TypePoll && fromRhizome:
		spec = pollSpec
	case typ == TypeBatch && !fromRhizome:
		spec = batchSpec
	default:
		return Frame{}, ErrFrameBadType
	}
	// W5 before V-K: a later version may add keys, which must read as
	// bad-version rather than unknown-field.
	v, ok := intValue(obj["v"], 0, MaxSafeInt)
	if !ok {
		return Frame{}, ErrFrameMalformed
	}
	if typ == TypeHello || typ == TypeError {
		if v != HandshakeVersion {
			return Frame{}, ErrFrameBadVersion
		}
	} else if v != negotiated || !supported(v) {
		return Frame{}, ErrFrameBadVersion
	}
	if err := validate(tree, spec); err != nil {
		if err == ErrUnknownField {
			return Frame{}, ErrFrameUnknownField
		}
		return Frame{}, ErrFrameMalformed
	}
	out := Frame{Type: typ}
	switch {
	case typ == TypeError:
		var w errorWire
		err = json.Unmarshal(line, &w)
		out.Error = ErrorFrame{Code: w.Code, Reason: w.Reason, Ack: w.Ack, RejectedSeq: w.RejectedSeq}
	case typ == TypeHello && fromRhizome:
		var w rhizomeHelloWire
		err = json.Unmarshal(line, &w)
		out.RhizomeHello = RhizomeHello{Versions: w.Versions}
	case typ == TypeHello:
		var w planeHelloWire
		err = json.Unmarshal(line, &w)
		if err == nil && !supported(w.Version) {
			return Frame{}, ErrFrameBadVersion // W5: must be in Rhizome's list
		}
		out.PlaneHello = PlaneHello{Version: w.Version, PlaneID: w.PlaneID}
	case typ == TypePoll:
		var w pollWire
		err = json.Unmarshal(line, &w)
		out.Poll = Poll{V: w.V, Pending: w.Pending, Ack: w.Ack, LastRejected: w.LastRejected}
	case typ == TypeBatch:
		var w batchWire
		err = json.Unmarshal(line, &w)
		if err == nil {
			if len(w.Entries) > MaxEntries {
				return Frame{}, ErrFrameTooManyEntries
			}
			for i := 1; i < len(w.Entries); i++ {
				if w.Entries[i].Seq != w.Entries[i-1].Seq+1 {
					return Frame{}, ErrFrameSeqOrder
				}
			}
		}
		out.Batch = Batch{V: w.V, PlaneID: w.PlaneID, Entries: w.Entries}
	}
	if err != nil {
		return Frame{}, ErrFrameMalformed
	}
	return out, nil
}

// CheckBatchPlane is W9's planeId rule: batch.planeId must equal the plane
// hello's planeId.
func CheckBatchPlane(b Batch, helloPlaneID string) error {
	if b.PlaneID != helloPlaneID {
		return ErrFramePlaneIDMismatch
	}
	return nil
}

// encode marshals w, appends '\n' and re-validates the bytes with the same
// decoder the peer will run, so an invalid frame is never emitted.
func encode(w any, check func([]byte) error) ([]byte, error) {
	b, err := json.Marshal(w)
	if err != nil {
		return nil, ErrFrameMalformed
	}
	b = append(b, '\n')
	if err := check(b); err != nil {
		return nil, err
	}
	return b, nil
}

// EncodeRhizomeHello encodes hello R→P with versions = SupportedVersions.
func EncodeRhizomeHello() ([]byte, error) {
	w := rhizomeHelloWire{V: HandshakeVersion, Type: TypeHello, Role: RoleRhizome, Versions: SupportedVersions()}
	return encode(w, func(b []byte) error { _, err := DecodeFromRhizome(b, TypeHello, 0); return err })
}

// EncodePlaneHello encodes hello P→R.
func EncodePlaneHello(h PlaneHello) ([]byte, error) {
	w := planeHelloWire{V: HandshakeVersion, Type: TypeHello, Role: RolePlane, Version: h.Version, PlaneID: h.PlaneID}
	return encode(w, func(b []byte) error { _, err := DecodeFromPlane(b, TypeHello, 0); return err })
}

// EncodePoll encodes poll R→P at version p.V.
func EncodePoll(p Poll) ([]byte, error) {
	pending := p.Pending
	if pending == nil {
		pending = []Pending{}
	}
	w := pollWire{V: p.V, Type: TypePoll, Pending: pending, Ack: p.Ack, LastRejected: p.LastRejected}
	return encode(w, func(b []byte) error { _, err := DecodeFromRhizome(b, TypePoll, p.V); return err })
}

// EncodeBatch encodes batch P→R at version b.V.
func EncodeBatch(b Batch) ([]byte, error) {
	entries := b.Entries
	if entries == nil {
		entries = []Entry{}
	}
	w := batchWire{V: b.V, Type: TypeBatch, PlaneID: b.PlaneID, Entries: entries}
	return encode(w, func(f []byte) error { _, err := DecodeFromPlane(f, TypeBatch, b.V); return err })
}

// EncodeError encodes an error frame (v 1). It admits every §3.3 code; a
// plane must restrict itself to the four shared codes, which DecodeFromPlane
// enforces on the receiving side.
func EncodeError(e ErrorFrame) ([]byte, error) {
	w := errorWire{V: HandshakeVersion, Type: TypeError, Code: e.Code, Reason: e.Reason, Ack: e.Ack, RejectedSeq: e.RejectedSeq}
	return encode(w, func(b []byte) error { _, err := DecodeFromRhizome(b, TypeError, 0); return err })
}

// ReadFrame reads one NDJSON frame (including its '\n') of at most
// MaxFrameBytes without unbounded buffering (W3). Oversize lines yield
// ErrFrameTooLarge, EOF after a partial line ErrFrameMalformed, and EOF before
// any byte io.EOF. Other read errors are never passed through (their text is
// not ours to echo, S5): a deadline yields ErrTimeout, anything else
// ErrFrameMalformed.
// After ErrFrameTooLarge the stream position is undefined: close the
// connection.
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, ErrFrameMalformed
		case errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err):
			return nil, ErrTimeout
		default:
			return nil, ErrFrameMalformed
		}
	}
}

// ErrTimeout is ReadFrame's fixed error for an expired deadline (§3.6
// "timeout").
var ErrTimeout = errors.New("timeout")

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
