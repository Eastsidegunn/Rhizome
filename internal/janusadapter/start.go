package janusadapter

// RHZ-092 (FR-RHZ-123): mission.start — the explicit program path that
// starts one JANUS execution for a board mission. The sequence mirrors
// the legacy direct caller path (intent → claim → hx run) and is the only production caller of
// StartExecution; the loop still never starts anything (RHZ-046 D7).
//
// Operating defaults and the policy ceiling come from one JSON ledger
// file (-janus-exec-config). Changing the ceiling is a governance act:
// edit the file and restart serve — never an intent.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/policy"
)

// ExecCeiling is the configured per-execution ceiling. Every effective
// policy is the intersection/minimum of this and the request.
type ExecCeiling struct {
	Tokens       int64    `json:"tokens"`
	TimeMs       int64    `json:"timeMs"`
	MaxDepth     int64    `json:"maxDepth"`
	Capabilities []string `json:"capabilities"`
	Domains      []string `json:"domains"`
}

// ExecConfig is the operator-owned execution ledger (no secrets: unknown
// keys are rejected on load, so nothing undeclared can ride in).
type ExecConfig struct {
	AdapterID      string      `json:"adapterId"`
	AdapterVersion string      `json:"adapterVersion"`
	ProfileID      string      `json:"profileId"`
	ProfileHash    string      `json:"profileHash"`
	WorkspaceRef   string      `json:"workspaceRef"`
	Scope          string      `json:"scope"`
	SessionMode    string      `json:"sessionMode"`
	Ceiling        ExecCeiling `json:"ceiling"`

	// digest is the canonical ledger digest, computed once at parse time
	// (FR-RHZ-124). Unexported: never decoded from nor encoded into JSON.
	digest string
}

// Digest is the ExecConfigDigest journaled with every mission.start intent
// (RHZ-096, FR-RHZ-124): "sha256:<hex>" over the canonical form of the
// VALIDATED ledger — the struct (defaults applied, nil lists as []) is
// marshalled, decoded into a generic map and re-marshalled, so keys are
// sorted and insignificant whitespace/key order in the file never changes
// it, while any value change does. It identifies the EFFECTIVE ledger, not
// the file bytes: a file that omits capabilities digests the same as one that
// lists DefaultExecCapabilities explicitly. Computed once by ParseExecConfig;
// a hand-built config is digested on demand.
func (c ExecConfig) Digest() string {
	if c.digest != "" {
		return c.digest
	}
	return canonicalDigest(c)
}

func canonicalDigest(c ExecConfig) string {
	c.digest = ""
	if c.Ceiling.Capabilities == nil {
		c.Ceiling.Capabilities = []string{}
	}
	if c.Ceiling.Domains == nil {
		c.Ceiling.Domains = []string{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // plain strings and ints only: unreachable
	}
	var m any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		panic(err)
	}
	if b, err = json.Marshal(m); err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// DefaultExecCapabilities is the ceiling capability set when the file omits
// it: workspace file access only (the legacy direct caller's choice).
var DefaultExecCapabilities = []string{"fs:workspace"}

// ParseExecConfig decodes and validates the ledger bytes (strict: unknown
// fields fail).
func ParseExecConfig(b []byte) (ExecConfig, error) {
	var c ExecConfig
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return ExecConfig{}, fmt.Errorf("exec config: %w", err)
	}
	if dec.More() {
		return ExecConfig{}, fmt.Errorf("exec config: trailing data")
	}
	if err := c.validate(); err != nil {
		return ExecConfig{}, fmt.Errorf("exec config: %w", err)
	}
	c.digest = canonicalDigest(c)
	return c, nil
}

// LoadExecConfig reads and validates the ledger file.
func LoadExecConfig(path string) (ExecConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ExecConfig{}, fmt.Errorf("exec config: %w", err)
	}
	return ParseExecConfig(b)
}

func (c *ExecConfig) validate() error {
	for name, v := range map[string]string{"adapterId": c.AdapterID, "profileId": c.ProfileID, "profileHash": c.ProfileHash, "workspaceRef": c.WorkspaceRef, "scope": c.Scope, "sessionMode": c.SessionMode} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s required", name)
		}
	}
	if !validAdapter(c.AdapterID) {
		return fmt.Errorf("unsupported adapter %q", c.AdapterID)
	}
	if !profileHashRE.MatchString(c.ProfileHash) {
		return fmt.Errorf("profileHash must be 64 hex")
	}
	if !validSessionMode(c.SessionMode) {
		return fmt.Errorf("sessionMode must be oneshot|multiturn")
	}
	for name, v := range map[string]int64{"ceiling.tokens": c.Ceiling.Tokens, "ceiling.timeMs": c.Ceiling.TimeMs, "ceiling.maxDepth": c.Ceiling.MaxDepth} {
		if v <= 0 {
			return fmt.Errorf("%s must be > 0", name)
		}
	}
	if len(c.Ceiling.Capabilities) == 0 {
		c.Ceiling.Capabilities = append([]string(nil), DefaultExecCapabilities...)
	}
	if _, err := policy.MapToJanus(c.ceilingPolicy()); err != nil {
		return fmt.Errorf("ceiling: %w", err)
	}
	return nil
}

func (c ExecConfig) ceilingPolicy() policy.Policy {
	return policy.Policy{Capabilities: append([]string(nil), c.Ceiling.Capabilities...), Domains: append([]string(nil), c.Ceiling.Domains...), Budget: c.Ceiling.Tokens, Timeout: c.Ceiling.TimeMs, MaxDepth: c.Ceiling.MaxDepth, Units: "tokens-ms-v1"}
}

// BudgetOverride is the per-mission budget request; nil axis = ceiling.
// USD is the session-launcher axis (RHZ-124 S1): carried only so it can be
// refused, never ignored.
type BudgetOverride struct {
	Tokens, TimeMs, MaxDepth *int64
	USD                      *float64
}

// StartRequest is one mission.start after the relay resolved the mission
// (instruction already defaulted to the mission description).
type StartRequest struct {
	MissionID, Instruction, SessionMode string
	// Workdir is a session-launcher field (RHZ-124 S1): refused here.
	Workdir string
	Budget  BudgetOverride
	// Actor is who asked (from the relay); journaled in the intent's
	// provenance only — never part of the idempotency key (FR-RHZ-124).
	Actor string
}

// StartPlan is the zero-write preview of a start: the deterministic
// execution id and, when the key already exists, that execution's state.
type StartPlan struct {
	ExecutionID string
	Existing    string
}

// Starter runs mission.start against the execution kernel and the injected
// Runner. Exec == nil means no ledger was loaded: every request is rejected
// before any write with "execution config not loaded".
type Starter struct {
	Store events.Port
	Run   Runner
	Cfg   RunConfig
	Exec  *ExecConfig
}

type startSpec struct {
	key, execID, fingerprint string
	requested, ceiling       policy.Policy
	params                   RunParams
	prov                     execution.Provenance
}

func budgetAxis(name string, v *int64, ceiling int64) (int64, string) {
	if v == nil {
		return ceiling, ""
	}
	if *v <= 0 {
		return 0, fmt.Sprintf("BUDGET_INVALID: budget.%s must be > 0", name)
	}
	if *v > ceiling {
		return 0, fmt.Sprintf("POLICY_DENIED: budget.%s %d exceeds ceiling %d", name, *v, ceiling)
	}
	return *v, ""
}

// spec derives everything deterministic from the request and the ledger:
// effective policy (ceiling ∩ request), idempotency key, request
// fingerprint and run params. Pure — no store access.
func (s Starter) spec(req StartRequest) (startSpec, string, error) {
	if s.Exec == nil {
		return startSpec{}, "execution config not loaded", nil
	}
	if strings.TrimSpace(req.MissionID) == "" || strings.TrimSpace(req.Instruction) == "" {
		return startSpec{}, "missionId and instruction required", nil
	}
	// RHZ-124 S1 (FR-RHZ-124-S1): session-launcher fields are refused,
	// never silently ignored.
	if req.Budget.USD != nil {
		return startSpec{}, "budget.usd not supported by janus backend", nil
	}
	if req.Workdir != "" {
		return startSpec{}, "workdir not supported by janus backend", nil
	}
	mode := req.SessionMode
	if mode == "" {
		mode = s.Exec.SessionMode
	}
	if !validSessionMode(mode) {
		return startSpec{}, "sessionMode must be oneshot|multiturn", nil
	}
	ceiling := s.Exec.ceilingPolicy()
	requested := ceiling
	var reason string
	if requested.Budget, reason = budgetAxis("tokens", req.Budget.Tokens, ceiling.Budget); reason != "" {
		return startSpec{}, reason, nil
	}
	if requested.Timeout, reason = budgetAxis("timeMs", req.Budget.TimeMs, ceiling.Timeout); reason != "" {
		return startSpec{}, reason, nil
	}
	if requested.MaxDepth, reason = budgetAxis("maxDepth", req.Budget.MaxDepth, ceiling.MaxDepth); reason != "" {
		return startSpec{}, reason, nil
	}
	merged := policy.Merge(requested, ceiling)
	if merged.Invalid || merged.Empty {
		return startSpec{}, "POLICY_DENIED: effective policy empty", nil
	}
	eff := merged.Policy
	if _, err := policy.MapToJanus(eff); err != nil {
		return startSpec{}, "POLICY_UNMAPPABLE: " + err.Error(), nil
	}
	// Contract §3.3: the key is a function of the request content, so a
	// re-submission finds the same execution and never spawns twice.
	key := hashOf("mission.start", req.MissionID, req.Instruction, fmt.Sprint(eff.Budget), fmt.Sprint(eff.Timeout), fmt.Sprint(eff.MaxDepth), mode)
	execID := "exec-" + key
	fp := "sha256:" + hashOf("request", s.Exec.AdapterID, s.Exec.AdapterVersion, s.Exec.ProfileID, s.Exec.ProfileHash, s.Exec.WorkspaceRef, s.Exec.Scope, req.Instruction, fmt.Sprint(eff.Budget), fmt.Sprint(eff.Timeout), fmt.Sprint(eff.MaxDepth), mode)
	params := RunParams{OperationID: execID, Scope: s.Exec.Scope, RequestFingerprint: fp, CorrelationID: "mission.start:" + req.MissionID,
		AdapterID: s.Exec.AdapterID, AdapterVersion: s.Exec.AdapterVersion, WorkspaceRef: s.Exec.WorkspaceRef, Instruction: req.Instruction,
		ProfileID: s.Exec.ProfileID, ProfileHash: s.Exec.ProfileHash, SessionMode: mode}
	// FR-RHZ-124: policy provenance for the journal (digests and numbers
	// only). Computed after the key so it can never feed into it.
	prov := execution.Provenance{Ceiling: ceiling, Requested: requested, Effective: eff, ProfileID: s.Exec.ProfileID, ProfileHash: s.Exec.ProfileHash, ExecConfigDigest: s.Exec.Digest(), Actor: req.Actor}
	return startSpec{key: key, execID: execID, fingerprint: fp, requested: requested, ceiling: ceiling, params: params, prov: prov}, "", nil
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s Starter) existing(execID, key string) (execution.Ref, bool, error) {
	if s.Store == nil {
		return execution.Ref{}, false, fmt.Errorf("nil event store")
	}
	log := s.Store.List("execution", execID)
	if len(log) == 0 {
		return execution.Ref{}, false, nil
	}
	r, err := execution.Replay(log)
	if err != nil {
		return execution.Ref{}, false, err
	}
	if r.IdempotencyKey != key {
		return execution.Ref{}, false, fmt.Errorf("execution %s carries a foreign idempotency key", execID)
	}
	return r, true, nil
}

// existingReason classifies an execution found under the request's key:
// "" = the relay may proceed (running → lookup only; intent/claimed →
// converge), otherwise the rejection reason (zero writes).
func existingReason(r execution.Ref) string {
	switch r.State {
	case execution.Intent, execution.DispatchClaimed, execution.Accepted, execution.Observing:
		return ""
	case execution.Unknown:
		return "execution unknown: human resolution required (" + r.ID + ")"
	}
	return "execution already ended: " + string(r.State) + " (" + r.ID + ")"
}

// Prepare validates the request against the ledger and looks the key up.
// It never writes; a non-empty reason is the relay's rejection verbatim.
func (s Starter) Prepare(req StartRequest) (StartPlan, string, error) {
	sp, reason, err := s.spec(req)
	if err != nil || reason != "" {
		return StartPlan{}, reason, err
	}
	r, ok, err := s.existing(sp.execID, sp.key)
	if err != nil {
		return StartPlan{}, "", err
	}
	plan := StartPlan{ExecutionID: sp.execID}
	if ok {
		plan.Existing = string(r.State)
		if reason := existingReason(r); reason != "" {
			return plan, reason, nil
		}
	}
	return plan, "", nil
}

// Start appends execution.intent and execution.dispatch_claimed (both
// durable before the Runner is contacted, contract §3.3) and submits hx run.
// On the same key it converges from whatever durable state remains: a
// missing claim is appended, a claimed or accepted execution is resubmitted
// as the official lookup, and no second intent is ever written. The
// returned reason (with the execution id) reports a run-level refusal or an
// unknown outcome; err is reserved for store faults.
func (s Starter) Start(req StartRequest) (string, string, error) {
	sp, reason, err := s.spec(req)
	if err != nil || reason != "" {
		return "", reason, err
	}
	if s.Run == nil {
		return "", "", fmt.Errorf("nil runner")
	}
	es := execution.Service{Store: s.Store}
	r, ok, err := s.existing(sp.execID, sp.key)
	if err != nil {
		return "", "", err
	}
	if !ok {
		if r, err = es.IntentWithProvenance(req.MissionID, sp.key, sp.requested, sp.ceiling, sp.prov); err != nil {
			return "", "", err
		}
	} else if reason := existingReason(r); reason != "" {
		return r.ID, reason, nil
	}
	if r.State == execution.Intent {
		if r, err = es.ClaimDispatch(r.ID, "janus", sp.params.CorrelationID); err != nil {
			return r.ID, "", err
		}
	}
	if r.State == execution.Observing {
		return r.ID, "", nil
	}
	res, err := StartExecution(es, s.Run, s.Cfg, r.ID, sp.params)
	switch {
	case err != nil && res.Status == "":
		var re *RunError
		if errors.As(err, &re) {
			return r.ID, re.Error(), nil
		}
		return r.ID, "hx run failed: " + err.Error(), nil
	case err != nil:
		// Classified failure with a durable trace (unknown / rejected).
		return r.ID, "hx run " + res.Status + ": " + err.Error(), nil
	}
	switch res.Status {
	case "accepted":
		return r.ID, "", nil
	case "unknown":
		return r.ID, "hx run acceptance unknown: human resolution required", nil
	}
	return r.ID, "hx run " + res.Status + ": not accepted (resubmit with the same request)", nil
}
