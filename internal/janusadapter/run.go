package janusadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"rhizome/internal/execution"
	"rhizome/internal/policy"
)

// RunConfig carries operator-owned settings. They are injected, never derived
// from contract inputs, and never serialized into request.json.
type RunConfig struct {
	ProfilePath, AcceptRoot, WorldConfigPath, SessionDB, ApprovalEndpoint string
}

// Runner stands in for the hx run process: it receives the operator config and
// the strict v1 request.json bytes and returns the stdout NDJSON stream.
type Runner func(cfg RunConfig, requestJSON []byte) (io.Reader, error)

// RunParams supplies the contract request fields that ExecutionRef does not
// carry. Optional fields are serialized verbatim (empty string when absent) so
// resubmission after a crash reproduces identical bytes.
type RunParams struct {
	OperationID, Scope, RequestFingerprint, CorrelationID string
	AdapterID, AdapterVersion, WorkspaceRef, Instruction  string
	ProfileID, ProfileHash                                string
	// SessionMode is the contract v1.6 optional request field
	// (oneshot|multiturn, absent = oneshot). It is omitted from request.json
	// when empty so the strict v1 bytes of every earlier caller are unchanged
	// (FR-RHZ-123).
	SessionMode string
}

type runTaskRef struct {
	Instruction string `json:"instruction"`
}
type runBudget struct {
	Tokens   int64 `json:"tokens"`
	TimeMS   int64 `json:"time_ms"`
	MaxDepth int64 `json:"max_depth"`
}
type runRequest struct {
	Version              int64      `json:"version"`
	OperationID          string     `json:"operation_id"`
	Scope                string     `json:"scope"`
	IdempotencyKey       string     `json:"idempotency_key"`
	RequestFingerprint   string     `json:"request_fingerprint"`
	RhizomeExecutionID   string     `json:"rhizome_execution_id"`
	MissionID            string     `json:"mission_id"`
	CorrelationID        string     `json:"correlation_id"`
	AdapterID            string     `json:"adapter_id"`
	AdapterVersion       string     `json:"adapter_version"`
	WorkspaceRef         string     `json:"workspace_ref"`
	TaskRef              runTaskRef `json:"task_ref"`
	ProfileID            string     `json:"profile_id"`
	ProfileHash          string     `json:"profile_hash"`
	PolicyMappingVersion string     `json:"policy_mapping_version"`
	Budget               runBudget  `json:"budget"`
	SessionMode          string     `json:"session_mode,omitempty"`
}

// RunError preserves a rejected control classification verbatim.
type RunError struct {
	Code, Message string
	Retryable     bool // Not a permission to re-run automatically.
}

func (e *RunError) Error() string { return e.Code + ": " + e.Message }

type StartResult struct {
	Ref                    execution.Ref
	Status                 string
	Duplicate              bool
	PolicyHash             string
	AcceptanceSeq, LastSeq int64
	DoneStatus, DoneResult string
	Error                  *RunError
}

var (
	profileHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	zeroTrace     = strings.Repeat("0", 32)
	runErrorCodes = map[string]bool{"UNSUPPORTED_CONTRACT": true, "UNSUPPORTED_ADAPTER": true, "POLICY_CHANGED": true, "POLICY_DENIED": true, "BUDGET_INVALID": true, "KEY_CONFLICT": true, "SESSION_CORRUPT": true, "IO_ERROR": true, "ACCEPTANCE_UNKNOWN": true, "LAUNCH_FAILED": true}
)

func validAdapter(id string) bool { return id == "claudecode" || id == "codex" }

func validSessionMode(m string) bool { return m == "oneshot" || m == "multiturn" }

// BuildRunRequest assembles strict v1 request.json from the execution intent
// and injected params. It refuses assembly, before any process contact, when
// the approval endpoint opt-in is absent (contract v1.4 ratification condition:
// no execution without a stop surface).
func BuildRunRequest(r execution.Ref, p RunParams, cfg RunConfig) ([]byte, error) {
	if strings.TrimSpace(cfg.ApprovalEndpoint) == "" || !strings.HasPrefix(cfg.ApprovalEndpoint, "/") {
		return nil, fmt.Errorf("approval endpoint required")
	}
	for _, req := range []string{p.OperationID, p.Scope, r.IdempotencyKey, r.ID, p.WorkspaceRef, p.ProfileID} {
		if strings.TrimSpace(req) == "" {
			return nil, fmt.Errorf("missing required request field")
		}
	}
	if strings.TrimSpace(p.Instruction) == "" {
		return nil, fmt.Errorf("task_ref.instruction required")
	}
	if !validAdapter(p.AdapterID) {
		return nil, fmt.Errorf("unsupported adapter %q", p.AdapterID)
	}
	if !profileHashRE.MatchString(p.ProfileHash) {
		return nil, fmt.Errorf("profile_hash must be 64 hex")
	}
	if p.SessionMode != "" && !validSessionMode(p.SessionMode) {
		return nil, fmt.Errorf("session_mode must be oneshot|multiturn")
	}
	if r.Policy == nil {
		return nil, fmt.Errorf("policy snapshot required")
	}
	j, err := policy.MapToJanus(*r.Policy)
	if err != nil {
		return nil, err
	}
	return json.Marshal(runRequest{
		Version: 1, OperationID: p.OperationID, Scope: p.Scope,
		IdempotencyKey: r.IdempotencyKey, RequestFingerprint: p.RequestFingerprint,
		RhizomeExecutionID: r.ID, MissionID: r.MissionID, CorrelationID: p.CorrelationID,
		AdapterID: p.AdapterID, AdapterVersion: p.AdapterVersion, WorkspaceRef: p.WorkspaceRef,
		TaskRef: runTaskRef{Instruction: p.Instruction}, ProfileID: p.ProfileID, ProfileHash: p.ProfileHash,
		PolicyMappingVersion: policy.MappingVersion,
		Budget:               runBudget{Tokens: j.Budget.Tokens, TimeMS: j.Budget.TimeMS, MaxDepth: j.Budget.MaxDepth},
		SessionMode:          p.SessionMode,
	})
}

type controlSessionRef struct{ SessionDB, TraceID string }
type control struct {
	Version                                        int64
	OperationID, Status                            string
	SessionRef                                     *controlSessionRef
	IdempotencyKey, RequestFingerprint, PolicyHash string
	AcceptanceSeq, LastSeq                         int64
	LaunchClaimed                                  *bool
	HasAcceptanceSeq                               bool
	Duplicate                                      bool
	Done                                           *SubagentDone
	Err                                            *RunError
}

func rawNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

func strictObject(raw json.RawMessage, decode map[string]any) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return fmt.Errorf("invalid control object")
	}
	for k, v := range fields {
		target, ok := decode[k]
		if !ok {
			return fmt.Errorf("unknown control field %s", k)
		}
		if rawNull(v) || json.Unmarshal(v, target) != nil {
			return fmt.Errorf("invalid control field %s", k)
		}
	}
	return nil
}

func parseControl(line []byte) (control, error) {
	var c control
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return control{}, fmt.Errorf("invalid control message")
	}
	var sessionRaw, doneRaw, errRaw json.RawMessage
	targets := map[string]any{
		"version": &c.Version, "operation_id": &c.OperationID, "status": &c.Status,
		"session_ref": &sessionRaw, "idempotency_key": &c.IdempotencyKey,
		"request_fingerprint": &c.RequestFingerprint, "policy_hash": &c.PolicyHash,
		"acceptance_seq": &c.AcceptanceSeq, "launch_claimed": &c.LaunchClaimed,
		"duplicate": &c.Duplicate, "done": &doneRaw, "last_seq": &c.LastSeq, "error": &errRaw,
	}
	for k, v := range fields {
		target, ok := targets[k]
		if !ok {
			return control{}, fmt.Errorf("unknown control field %s", k)
		}
		if rawNull(v) || json.Unmarshal(v, target) != nil {
			return control{}, fmt.Errorf("invalid control field %s", k)
		}
	}
	if _, ok := fields["acceptance_seq"]; ok {
		c.HasAcceptanceSeq = true
	}
	if len(sessionRaw) != 0 {
		ref := controlSessionRef{}
		if err := strictObject(sessionRaw, map[string]any{"session_db": &ref.SessionDB, "trace_id": &ref.TraceID}); err != nil {
			return control{}, err
		}
		c.SessionRef = &ref
	}
	if len(doneRaw) != 0 {
		d := SubagentDone{}
		if err := strictObject(doneRaw, map[string]any{"status": &d.Status, "result": &d.Result}); err != nil {
			return control{}, err
		}
		c.Done = &d
	}
	if len(errRaw) != 0 {
		e := RunError{}
		if err := strictObject(errRaw, map[string]any{"code": &e.Code, "retryable": &e.Retryable, "message": &e.Message}); err != nil {
			return control{}, err
		}
		if !runErrorCodes[e.Code] {
			return control{}, fmt.Errorf("unknown error code %q", e.Code)
		}
		c.Err = &e
	}
	if c.Version != 1 {
		return control{}, fmt.Errorf("unsupported control version")
	}
	switch c.Status {
	case "accepted", "initializing", "unknown", "rejected", "terminal":
	default:
		return control{}, fmt.Errorf("unknown control status %q", c.Status)
	}
	return c, nil
}

func validControlTrace(ref *controlSessionRef) error {
	if ref == nil || !replayTraceRE.MatchString(ref.TraceID) || ref.TraceID == zeroTrace || !strings.HasPrefix(ref.SessionDB, "/") {
		return fmt.Errorf("invalid session_ref")
	}
	return nil
}

// StartExecution submits (or, after a crash, resubmits as the official lookup)
// one execution request through the injected Runner. The Runner is never
// invoked without a durable dispatch claim, and acceptance binding is left to
// stage-B replay observation: only execution.accepted is written here.
func StartExecution(es execution.Service, run Runner, cfg RunConfig, execID string, p RunParams) (StartResult, error) {
	if es.Store == nil || run == nil {
		return StartResult{}, fmt.Errorf("nil store or runner")
	}
	r, err := execution.Replay(es.Store.List("execution", execID))
	if err != nil {
		return StartResult{}, err
	}
	if r.State != execution.DispatchClaimed && r.State != execution.Accepted {
		return StartResult{}, fmt.Errorf("dispatch claim required before hx run")
	}
	req, err := BuildRunRequest(r, p, cfg)
	if err != nil {
		return StartResult{}, err
	}
	markUnknown := func(cause string) (StartResult, error) {
		ref, e := es.MarkUnknown(execID, execution.ExternalEffectPossible, "", "hx-run:"+p.OperationID, cause)
		if e != nil {
			return StartResult{}, e
		}
		return StartResult{Ref: ref, Status: "unknown"}, nil
	}
	stdout, err := run(cfg, req)
	if err != nil {
		if r.State == execution.Accepted {
			// Resubmission is only a lookup here: the durable acceptance
			// stands, so a transient failure stays retryable (§5.2), never
			// a demotion to unknown.
			return StartResult{}, fmt.Errorf("%w: hx run lookup failed: %v", ErrUnavailable, err)
		}
		// The process was invoked without prior acceptance: absence of
		// output does not prove no spawn.
		res, e := markUnknown("hx run failed without control output: " + err.Error())
		if e != nil {
			return StartResult{}, e
		}
		return res, fmt.Errorf("hx run failed: %w", err)
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return StartResult{}, fmt.Errorf("control read: %w", err)
	}
	if len(bytes.TrimSpace(line)) == 0 {
		if r.State == execution.Accepted {
			return StartResult{}, fmt.Errorf("%w: hx run ended without a control message", ErrUnavailable)
		}
		return markUnknown("hx run ended without a control message")
	}
	c, err := parseControl(line)
	if err != nil {
		return StartResult{}, err
	}
	switch c.Status {
	case "accepted":
		if err := validControlTrace(c.SessionRef); err != nil {
			return StartResult{}, err
		}
		if c.IdempotencyKey != r.IdempotencyKey || c.RequestFingerprint != p.RequestFingerprint || c.PolicyHash == "" || !c.HasAcceptanceSeq || c.AcceptanceSeq != 1 || c.LaunchClaimed == nil {
			return StartResult{}, fmt.Errorf("incomplete acceptance")
		}
		if r.State == execution.Accepted {
			if c.SessionRef.TraceID != r.ExternalID {
				return StartResult{}, ErrSessionReplaced
			}
			// Duplicate acceptance is a lookup: no execution event is repeated.
			return StartResult{Ref: r, Status: "accepted", Duplicate: c.Duplicate, PolicyHash: c.PolicyHash, AcceptanceSeq: c.AcceptanceSeq}, nil
		}
		ref, err := es.Accept(execID, c.SessionRef.TraceID)
		if err != nil {
			return StartResult{}, err
		}
		return StartResult{Ref: ref, Status: "accepted", Duplicate: c.Duplicate, PolicyHash: c.PolicyHash, AcceptanceSeq: c.AcceptanceSeq}, nil
	case "initializing":
		return StartResult{Ref: r, Status: "initializing"}, nil
	case "terminal":
		// Classification only: the terminal fact is concluded by replay.
		res := StartResult{Ref: r, Status: "terminal", LastSeq: c.LastSeq}
		if c.Done != nil {
			res.DoneStatus, res.DoneResult = c.Done.Status, c.Done.Result
		}
		return res, nil
	case "unknown":
		return markUnknown("hx run reported unknown acceptance")
	case "rejected":
		if c.Err == nil {
			return StartResult{}, fmt.Errorf("rejected without error")
		}
		if c.Err.Code == "ACCEPTANCE_UNKNOWN" {
			res, e := markUnknown("hx run: " + c.Err.Error())
			if e != nil {
				return StartResult{}, e
			}
			res.Error = c.Err
			return res, c.Err
		}
		// Observation (D3): the refusal itself is not durable anywhere yet —
		// the execution stays dispatch_claimed and only this classification
		// carries the reason. Audit-gap follow-up candidate.
		return StartResult{Ref: r, Status: "rejected", Error: c.Err}, c.Err
	}
	return StartResult{}, fmt.Errorf("unreachable control status")
}
