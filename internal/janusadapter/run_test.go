package janusadapter

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/execution"
	"rhizome/internal/mission"
	"rhizome/internal/policy"
)

const runTrace = "abcdef0123456789abcdef0123456789"

func runStore(t *testing.T) (*events.Store, execution.Ref) {
	t.Helper()
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "work", "done"); err != nil {
		t.Fatal(err)
	}
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"fs:workspace"}, Budget: 5, Timeout: 100, MaxDepth: 2, Units: "tokens-ms-v1"}
	r, err := es.IntentWithPolicy("m", "run-key", p, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = es.ClaimDispatch(r.ID, "janus", "corr-1"); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func runParamsFixture() RunParams {
	return RunParams{OperationID: "op-1", Scope: "example/test", RequestFingerprint: "fp-1", CorrelationID: "corr-1", AdapterID: "claudecode", AdapterVersion: "1.0.0", WorkspaceRef: "ws-main", Instruction: "do the work", ProfileID: "manual", ProfileHash: strings.Repeat("ab", 32)}
}
func runConfigFixture() RunConfig {
	return RunConfig{ProfilePath: "/tmp/profile.yaml", AcceptRoot: "/tmp/accept", WorldConfigPath: "/tmp/world.json", ApprovalEndpoint: "/tmp/approval.sock"}
}

type recRunner struct {
	calls int
	got   [][]byte
	out   string
	err   error
}

func (r *recRunner) run(cfg RunConfig, req []byte) (io.Reader, error) {
	r.calls++
	r.got = append(r.got, append([]byte(nil), req...))
	if r.err != nil {
		return nil, r.err
	}
	return strings.NewReader(r.out), nil
}

func controlLine(t *testing.T, mut func(map[string]any)) string {
	t.Helper()
	m := map[string]any{"version": 1, "status": "accepted", "session_ref": map[string]any{"session_db": "/tmp/run.db", "trace_id": runTrace}, "idempotency_key": "run-key", "request_fingerprint": "fp-1", "policy_hash": "ph", "acceptance_seq": 1, "launch_claimed": true}
	if mut != nil {
		mut(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// B1: strict v1 request.json golden fixture, deterministic bytes,
// all 16 fields always serialized (L3).
func TestRunRequestGoldenFRRHZ076(t *testing.T) {
	_, r := runStore(t)
	one, err := BuildRunRequest(r, runParamsFixture(), runConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	two, err := BuildRunRequest(r, runParamsFixture(), runConfigFixture())
	if err != nil || string(one) != string(two) {
		t.Fatal("nondeterministic request bytes")
	}
	golden := `{"version":1,"operation_id":"op-1","scope":"example/test","idempotency_key":"run-key","request_fingerprint":"fp-1","rhizome_execution_id":"exec-run-key","mission_id":"m","correlation_id":"corr-1","adapter_id":"claudecode","adapter_version":"1.0.0","workspace_ref":"ws-main","task_ref":{"instruction":"do the work"},"profile_id":"manual","profile_hash":"` + strings.Repeat("ab", 32) + `","policy_mapping_version":"janus-map-v1","budget":{"tokens":5,"time_ms":100,"max_depth":2}}`
	if string(one) != golden {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", one, golden)
	}
	empty := runParamsFixture()
	empty.RequestFingerprint, empty.CorrelationID, empty.AdapterVersion = "", "", ""
	b, err := BuildRunRequest(r, empty, runConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"request_fingerprint":""`, `"correlation_id":""`, `"adapter_version":""`} {
		if !strings.Contains(string(b), field) {
			t.Fatalf("optional field not serialized as empty: %s", field)
		}
	}
}

// B2: absent approval endpoint refuses assembly with zero process contact.
func TestRunApprovalEndpointRequiredFRRHZ076(t *testing.T) {
	s, r := runStore(t)
	for _, endpoint := range []string{"", "   ", "relative/approval.sock"} {
		cfg := runConfigFixture()
		cfg.ApprovalEndpoint = endpoint
		if _, err := BuildRunRequest(r, runParamsFixture(), cfg); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
		rec := &recRunner{out: controlLine(t, nil)}
		before := s.All()
		if _, err := StartExecution(execution.Service{Store: s}, rec.run, cfg, r.ID, runParamsFixture()); err == nil || rec.calls != 0 {
			t.Fatalf("runner contacted without approval endpoint: calls=%d err=%v", rec.calls, err)
		}
		unchanged(t, before, s)
	}
}

// B3: required fields, adapter allowlist, profile hash shape, and an
// unmappable policy each refuse assembly without producing bytes.
func TestRunRequestFieldValidationFRRHZ076(t *testing.T) {
	_, r := runStore(t)
	mutations := map[string]func(*RunParams){
		"operation_id":  func(p *RunParams) { p.OperationID = "" },
		"scope":         func(p *RunParams) { p.Scope = " " },
		"workspace_ref": func(p *RunParams) { p.WorkspaceRef = "" },
		"profile_id":    func(p *RunParams) { p.ProfileID = "" },
		"instruction":   func(p *RunParams) { p.Instruction = "  " },
		"adapter":       func(p *RunParams) { p.AdapterID = "gpt" },
		"adapter_empty": func(p *RunParams) { p.AdapterID = "" },
		"hash_short":    func(p *RunParams) { p.ProfileHash = strings.Repeat("ab", 31) + "a" },
		"hash_nonhex":   func(p *RunParams) { p.ProfileHash = strings.Repeat("zz", 32) },
		"hash_upper":    func(p *RunParams) { p.ProfileHash = strings.Repeat("AB", 32) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := runParamsFixture()
			mutate(&p)
			if b, err := BuildRunRequest(r, p, runConfigFixture()); err == nil || b != nil {
				t.Fatal("invalid params produced a request")
			}
		})
	}
	t.Run("unmappable_policy", func(t *testing.T) {
		s := &events.Store{}
		ms := mission.Service{Store: s}
		if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := ms.Create("m", "g", "work", "done"); err != nil {
			t.Fatal(err)
		}
		es := execution.Service{Store: s}
		p := policy.Policy{Capabilities: []string{"run"}, Budget: 1, Timeout: 1}
		bad, err := es.IntentWithPolicy("m", "unmappable", p, p)
		if err != nil {
			t.Fatal(err)
		}
		if bad, err = es.ClaimDispatch(bad.ID, "janus", "corr"); err != nil {
			t.Fatal(err)
		}
		if b, err := BuildRunRequest(bad, runParamsFixture(), runConfigFixture()); err == nil || b != nil {
			t.Fatal("unmappable policy produced a request")
		}
	})
}

// B4: no hx invocation without a durable dispatch claim.
func TestStartRequiresDispatchClaimFRRHZ076(t *testing.T) {
	s := &events.Store{}
	ms := mission.Service{Store: s}
	if _, err := ms.CreateGoal("g", "goal", "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Create("m", "g", "work", "done"); err != nil {
		t.Fatal(err)
	}
	es := execution.Service{Store: s}
	p := policy.Policy{Capabilities: []string{"fs:workspace"}, Budget: 5, Timeout: 100, MaxDepth: 2, Units: "tokens-ms-v1"}
	r, err := es.IntentWithPolicy("m", "run-key", p, p)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recRunner{out: controlLine(t, nil)}
	before := s.All()
	if _, err := StartExecution(es, rec.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil || rec.calls != 0 {
		t.Fatalf("hx invoked without claim: calls=%d err=%v", rec.calls, err)
	}
	unchanged(t, before, s)
	s2, r2 := runStore(t)
	rec2 := &recRunner{out: controlLine(t, nil)}
	if _, err := StartExecution(execution.Service{Store: s2}, rec2.run, runConfigFixture(), r2.ID, runParamsFixture()); err != nil || rec2.calls != 1 {
		t.Fatal(rec2.calls, err)
	}
	want, err := BuildRunRequest(r2, runParamsFixture(), runConfigFixture())
	if err != nil || string(rec2.got[0]) != string(want) {
		t.Fatal("runner received different bytes than the assembled request")
	}
}

// B5 (+L2): accepted maps to Accept(trace_id) only; binding is left to
// replay observation; every acceptance field is mandatory.
func TestControlAcceptedMapsToAcceptFRRHZ076(t *testing.T) {
	s, r := runStore(t)
	rec := &recRunner{out: controlLine(t, nil)}
	res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
	if err != nil || res.Status != "accepted" || res.Duplicate || res.PolicyHash != "ph" || res.AcceptanceSeq != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Ref.State != execution.Accepted || res.Ref.ExternalID != runTrace {
		t.Fatalf("%+v", res.Ref)
	}
	for _, ev := range s.All() {
		if ev.Type == "execution.binding" {
			t.Fatal("binding must be left to replay observation")
		}
	}
	replayed, err := execution.Replay(s.List("execution", r.ID))
	if err != nil || !reflect.DeepEqual(res.Ref, replayed) {
		t.Fatal("returned/replayed ref mismatch")
	}
	required := []string{"session_ref", "idempotency_key", "request_fingerprint", "policy_hash", "acceptance_seq", "launch_claimed"}
	for _, field := range required {
		t.Run("missing_"+field, func(t *testing.T) {
			s, r := runStore(t)
			rec := &recRunner{out: controlLine(t, func(m map[string]any) { delete(m, field) })}
			before := s.All()
			if _, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil {
				t.Fatal("incomplete acceptance accepted")
			}
			unchanged(t, before, s)
		})
	}
	for name, mut := range map[string]func(map[string]any){
		"key_mismatch":         func(m map[string]any) { m["idempotency_key"] = "other" },
		"fingerprint_mismatch": func(m map[string]any) { m["request_fingerprint"] = "fp-2" },
		"acceptance_seq_2":     func(m map[string]any) { m["acceptance_seq"] = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			s, r := runStore(t)
			rec := &recRunner{out: controlLine(t, mut)}
			before := s.All()
			if _, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil {
				t.Fatal("acceptance echo mismatch accepted")
			}
			unchanged(t, before, s)
		})
	}
}

// B6: duplicate=true is a lookup classification, never a repeated event.
func TestControlDuplicateIsLookupFRRHZ076(t *testing.T) {
	s, r := runStore(t)
	es := execution.Service{Store: s}
	r, err := es.Accept(r.ID, runTrace)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recRunner{out: controlLine(t, func(m map[string]any) { m["duplicate"] = true })}
	before := s.All()
	res, err := StartExecution(es, rec.run, runConfigFixture(), r.ID, runParamsFixture())
	if err != nil || res.Status != "accepted" || !res.Duplicate || !reflect.DeepEqual(res.Ref, r) {
		t.Fatalf("%+v %v", res, err)
	}
	unchanged(t, before, s)
	s2, r2 := runStore(t)
	rec2 := &recRunner{out: controlLine(t, func(m map[string]any) { m["duplicate"] = true })}
	res2, err := StartExecution(execution.Service{Store: s2}, rec2.run, runConfigFixture(), r2.ID, runParamsFixture())
	if err != nil || res2.Ref.State != execution.Accepted {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range s2.All() {
		if ev.Type == "execution.accepted" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("acceptance recorded more than once:", count)
	}
	// Additional classification-only statuses: initializing and terminal
	// return without any write (terminal facts belong to replay).
	for name, line := range map[string]string{
		"initializing": `{"version":1,"status":"initializing"}` + "\n",
		"terminal":     `{"version":1,"status":"terminal","done":{"status":"ok","result":"r"},"last_seq":9}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, r := runStore(t)
			rec := &recRunner{out: line}
			before := s.All()
			res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
			if err != nil || res.Status != name {
				t.Fatal(res, err)
			}
			if name == "terminal" && (res.DoneStatus != "ok" || res.LastSeq != 9) {
				t.Fatalf("%+v", res)
			}
			unchanged(t, before, s)
		})
	}
}

// B7: every rejection code is preserved verbatim; retryable never
// triggers an automatic re-run; unknown codes are refused.
func TestControlRejectedPreservesReasonFRRHZ076(t *testing.T) {
	codes := []string{"UNSUPPORTED_CONTRACT", "UNSUPPORTED_ADAPTER", "POLICY_CHANGED", "POLICY_DENIED", "BUDGET_INVALID", "KEY_CONFLICT", "SESSION_CORRUPT", "IO_ERROR", "LAUNCH_FAILED"}
	for i, code := range codes {
		t.Run(code, func(t *testing.T) {
			s, r := runStore(t)
			retryable := i%2 == 0
			line, err := json.Marshal(map[string]any{"version": 1, "status": "rejected", "error": map[string]any{"code": code, "retryable": retryable, "message": "m-" + code}})
			if err != nil {
				t.Fatal(err)
			}
			rec := &recRunner{out: string(line) + "\n"}
			before := s.All()
			res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
			var re *RunError
			if !errors.As(err, &re) || re.Code != code || re.Retryable != retryable || re.Message != "m-"+code {
				t.Fatalf("classification lost: %v", err)
			}
			if res.Status != "rejected" || res.Error != re {
				t.Fatalf("%+v", res)
			}
			if rec.calls != 1 {
				t.Fatal("automatic retry detected:", rec.calls)
			}
			unchanged(t, before, s) // D3: refusal is not durable; execution stays dispatch_claimed.
			got, err := execution.Replay(s.List("execution", r.ID))
			if err != nil || got.State != execution.DispatchClaimed {
				t.Fatal(got.State, err)
			}
		})
	}
	t.Run("unknown_code", func(t *testing.T) {
		s, r := runStore(t)
		rec := &recRunner{out: `{"version":1,"status":"rejected","error":{"code":"WEIRD","retryable":false,"message":"m"}}` + "\n"}
		before := s.All()
		var re *RunError
		if _, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil || errors.As(err, &re) {
			t.Fatal("unknown code classified:", err)
		}
		unchanged(t, before, s)
	})
	t.Run("rejected_without_error", func(t *testing.T) {
		s, r := runStore(t)
		rec := &recRunner{out: `{"version":1,"status":"rejected"}` + "\n"}
		before := s.All()
		if _, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil {
			t.Fatal("rejected without error object accepted")
		}
		unchanged(t, before, s)
	})
}

// B8 (D6): from dispatch_claimed — where the submission outcome itself
// is uncertain — every acceptance-uncertain outcome marks the execution
// unknown (external_effect_possible) and never re-runs. From accepted, the
// same failures are transient lookup errors and never demote the execution.
func TestControlUnknownMapsToMarkUnknownFRRHZ076(t *testing.T) {
	assertUnknown := func(t *testing.T, s *events.Store, id string) execution.Ref {
		t.Helper()
		got, err := execution.Replay(s.List("execution", id))
		if err != nil || got.State != execution.Unknown || got.UnknownClass != execution.ExternalEffectPossible {
			t.Fatalf("%+v %v", got, err)
		}
		return got
	}
	t.Run("status_unknown", func(t *testing.T) {
		s, r := runStore(t)
		rec := &recRunner{out: `{"version":1,"status":"unknown"}` + "\n"}
		res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
		if err != nil || res.Status != "unknown" {
			t.Fatal(res, err)
		}
		ref := assertUnknown(t, s, r.ID)
		if !reflect.DeepEqual(res.Ref, ref) {
			t.Fatal("returned/replayed ref mismatch")
		}
		// An unknown execution is not restartable: only explicit reconciliation.
		rec2 := &recRunner{out: controlLine(t, nil)}
		if _, err := StartExecution(execution.Service{Store: s}, rec2.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil || rec2.calls != 0 {
			t.Fatal("unknown execution re-ran hx:", rec2.calls, err)
		}
	})
	t.Run("acceptance_unknown_code", func(t *testing.T) {
		s, r := runStore(t)
		rec := &recRunner{out: `{"version":1,"status":"rejected","error":{"code":"ACCEPTANCE_UNKNOWN","retryable":false,"message":"lost"}}` + "\n"}
		res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
		var re *RunError
		if !errors.As(err, &re) || re.Code != "ACCEPTANCE_UNKNOWN" || res.Status != "unknown" {
			t.Fatal(res, err)
		}
		assertUnknown(t, s, r.ID)
	})
	t.Run("runner_error", func(t *testing.T) {
		s, r := runStore(t)
		rec := &recRunner{err: errors.New("spawn state lost")}
		res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
		if err == nil || res.Status != "unknown" {
			t.Fatal(res, err)
		}
		assertUnknown(t, s, r.ID)
	})
	t.Run("no_control_output", func(t *testing.T) {
		s, r := runStore(t)
		rec := &recRunner{out: ""}
		res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
		if err != nil || res.Status != "unknown" {
			t.Fatal(res, err)
		}
		assertUnknown(t, s, r.ID)
	})
	// Mandatory-fix cases: an already accepted execution is never demoted by
	// a transient lookup failure — no writes, retryable error (contract §5.2).
	acceptedLookup := func(t *testing.T, rec *recRunner) {
		t.Helper()
		s, r := runStore(t)
		es := execution.Service{Store: s}
		if _, err := es.Accept(r.ID, runTrace); err != nil {
			t.Fatal(err)
		}
		before := s.All()
		res, err := StartExecution(es, rec.run, runConfigFixture(), r.ID, runParamsFixture())
		if !errors.Is(err, ErrUnavailable) || !reflect.DeepEqual(res, StartResult{}) {
			t.Fatalf("lookup failure not retryable: %+v %v", res, err)
		}
		unchanged(t, before, s)
		ref, err := execution.Replay(s.List("execution", r.ID))
		if err != nil || ref.State != execution.Accepted {
			t.Fatal("accepted execution demoted:", ref.State, err)
		}
	}
	t.Run("accepted_runner_error", func(t *testing.T) {
		acceptedLookup(t, &recRunner{err: errors.New("socket busy")})
	})
	t.Run("accepted_no_output", func(t *testing.T) {
		acceptedLookup(t, &recRunner{out: ""})
	})
}

// B9: unknown status, unknown fields, and type pollution are refused
// with zero service writes.
func TestControlStrictParseFRRHZ076(t *testing.T) {
	direct := map[string]string{
		"forwarded_status": `{"version":1,"status":"forwarded"}`,
		"invented_status":  `{"version":1,"status":"running-ish"}`,
		"missing_version":  `{"status":"accepted"}`,
		"missing_status":   `{"version":1}`,
		"broken_json":      `{oops`,
		"array":            `[]`,
		"null_line":        `null`,
	}
	mutated := map[string]func(map[string]any){
		"unknown_field":       func(m map[string]any) { m["extra"] = 1 },
		"status_number":       func(m map[string]any) { m["status"] = 7 },
		"status_null":         func(m map[string]any) { m["status"] = nil },
		"version_string":      func(m map[string]any) { m["version"] = "1" },
		"duplicate_string":    func(m map[string]any) { m["duplicate"] = "yes" },
		"acceptance_string":   func(m map[string]any) { m["acceptance_seq"] = "x" },
		"session_ref_string":  func(m map[string]any) { m["session_ref"] = "str" },
		"session_ref_extra":   func(m map[string]any) { m["session_ref"].(map[string]any)["extra"] = 1 },
		"error_code_number":   func(m map[string]any) { m["error"] = map[string]any{"code": 1} },
		"done_status_number":  func(m map[string]any) { m["done"] = map[string]any{"status": 3} },
		"launch_claimed_text": func(m map[string]any) { m["launch_claimed"] = "true" },
	}
	lines := map[string]string{}
	for name, line := range direct {
		lines[name] = line + "\n"
	}
	for name, mut := range mutated {
		lines[name] = controlLine(t, mut)
	}
	for name, line := range lines {
		t.Run(name, func(t *testing.T) {
			s, r := runStore(t)
			rec := &recRunner{out: line}
			before := s.All()
			res, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture())
			if err == nil || !reflect.DeepEqual(res, StartResult{}) {
				t.Fatalf("polluted control accepted: %+v %v", res, err)
			}
			unchanged(t, before, s)
		})
	}
}

// B10 (+L5): session identity is validated before acceptance is recorded.
func TestControlSessionRefValidationFRRHZ076(t *testing.T) {
	cases := map[string]func(map[string]any){
		"trace_nonhex":    func(m map[string]any) { m["session_ref"].(map[string]any)["trace_id"] = "xyz" },
		"trace_short":     func(m map[string]any) { m["session_ref"].(map[string]any)["trace_id"] = runTrace[:31] },
		"trace_zero":      func(m map[string]any) { m["session_ref"].(map[string]any)["trace_id"] = strings.Repeat("0", 32) },
		"trace_upper":     func(m map[string]any) { m["session_ref"].(map[string]any)["trace_id"] = strings.ToUpper(runTrace) },
		"db_relative":     func(m map[string]any) { m["session_ref"].(map[string]any)["session_db"] = "rel/run.db" },
		"db_empty":        func(m map[string]any) { m["session_ref"].(map[string]any)["session_db"] = "" },
		"session_ref_nil": func(m map[string]any) { m["session_ref"] = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s, r := runStore(t)
			rec := &recRunner{out: controlLine(t, mut)}
			before := s.All()
			if _, err := StartExecution(execution.Service{Store: s}, rec.run, runConfigFixture(), r.ID, runParamsFixture()); err == nil {
				t.Fatal("invalid session_ref accepted")
			}
			unchanged(t, before, s)
		})
	}
	t.Run("recovery_trace_mismatch", func(t *testing.T) {
		s, r := runStore(t)
		es := execution.Service{Store: s}
		if _, err := es.Accept(r.ID, runTrace); err != nil {
			t.Fatal(err)
		}
		other := strings.Repeat("a", 32)
		rec := &recRunner{out: controlLine(t, func(m map[string]any) { m["session_ref"].(map[string]any)["trace_id"] = other })}
		before := s.All()
		if _, err := StartExecution(es, rec.run, runConfigFixture(), r.ID, runParamsFixture()); !errors.Is(err, ErrSessionReplaced) {
			t.Fatal(err)
		}
		unchanged(t, before, s)
	})
}

// B11: crash-recovery resubmission reproduces identical request bytes and
// never mints a second dispatch claim.
func TestStartResubmitByteIdenticalFRRHZ076(t *testing.T) {
	s, _ := runStore(t)
	one, err := execution.Replay(s.List("execution", "exec-run-key"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := execution.Replay(s.List("execution", "exec-run-key"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := BuildRunRequest(one, runParamsFixture(), runConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildRunRequest(two, runParamsFixture(), runConfigFixture())
	if err != nil || string(a) != string(b) {
		t.Fatal("resubmission bytes differ")
	}
	claims := 0
	for _, ev := range s.All() {
		if ev.Type == "execution.dispatch_claimed" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatal("claim duplicated:", claims)
	}
}
