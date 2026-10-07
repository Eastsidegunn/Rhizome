package janusadapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"

	"rhizome/internal/execution"
)

// StopResult classifies a stop submission receipt. It is never a terminal
// fact: cancelled is concluded only from replay (subagent/done stopped +
// session end), never from this response (contract v1.4 §7).
type StopResult struct {
	Status, StopID, Reason string
	TerminalRef            int64
}

func validStopReason(r string) bool {
	return r == "user" || r == "budget_exceeded" || r == "policy" || r == "parent_done"
}

func (c Client) stopCall(req map[string]any) (StopResult, error) {
	if c.Dial == nil {
		return StopResult{}, ErrUnavailable
	}
	conn, e := c.Dial()
	if e != nil {
		return StopResult{}, ErrUnavailable
	}
	defer conn.Close()
	b, e := json.Marshal(req)
	if e != nil {
		return StopResult{}, e
	}
	if _, e = conn.Write(append(b, '\n')); e != nil {
		return StopResult{}, ErrUnavailable
	}
	line, e := bufio.NewReader(conn).ReadBytes('\n')
	if e != nil && len(line) == 0 {
		return StopResult{}, ErrUnavailable
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(line, &raw) != nil || raw == nil {
		return StopResult{}, fmt.Errorf("invalid stop response")
	}
	var r StopResult
	targets := map[string]any{"status": &r.Status, "stop_id": &r.StopID, "reason": &r.Reason, "terminal_ref": &r.TerminalRef}
	for k, v := range raw {
		target, ok := targets[k]
		if !ok {
			return StopResult{}, fmt.Errorf("unknown stop response field %s", k)
		}
		if rawNull(v) || json.Unmarshal(v, target) != nil {
			return StopResult{}, fmt.Errorf("invalid stop response field %s", k)
		}
	}
	if r.Status == "error" {
		if r.Reason == "" {
			return StopResult{}, fmt.Errorf("invalid stop response")
		}
		return StopResult{}, errors.New(r.Reason)
	}
	if r.Status != "stop_accepted" && r.Status != "already_terminal" && r.Status != "unknown" {
		return StopResult{}, fmt.Errorf("invalid stop status")
	}
	return r, nil
}

func validStopArgs(traceID, stopID string) error {
	if !replayTraceRE.MatchString(traceID) || traceID == zeroTrace || stopID == "" {
		return fmt.Errorf("invalid stop arguments")
	}
	return nil
}

func checkStopReceipt(r StopResult, stopID string) error {
	switch r.Status {
	case "stop_accepted", "already_terminal":
		if r.StopID != stopID {
			return fmt.Errorf("stop response identity mismatch")
		}
		if r.Status == "already_terminal" && r.TerminalRef <= 0 {
			return fmt.Errorf("already_terminal without terminal_ref")
		}
	}
	return nil
}

// Stop submits one stop request over the shared approval socket (one request
// per connection). The receipt classifies acceptance only; timeout or a dial
// failure is never cancelled.
func (c Client) Stop(traceID, stopID, targetSpan, reason string, evidenceSeq int64) (StopResult, error) {
	if err := validStopArgs(traceID, stopID); err != nil {
		return StopResult{}, err
	}
	if !validStopReason(reason) || (targetSpan != "" && !spanRE.MatchString(targetSpan)) {
		return StopResult{}, fmt.Errorf("invalid stop request")
	}
	if reason == "policy" && evidenceSeq <= 0 {
		return StopResult{}, fmt.Errorf("policy stop requires evidence_seq")
	}
	if reason != "policy" && evidenceSeq != 0 {
		return StopResult{}, fmt.Errorf("evidence_seq only valid for policy stop")
	}
	req := map[string]any{"op": "stop", "trace_id": traceID, "stop_id": stopID, "reason": reason}
	if targetSpan != "" {
		req["target_span_id"] = targetSpan
	}
	if evidenceSeq > 0 {
		req["evidence_seq"] = evidenceSeq
	}
	r, err := c.stopCall(req)
	if err != nil {
		return StopResult{}, err
	}
	if r.Status == "unknown" {
		return StopResult{}, fmt.Errorf("invalid stop status")
	}
	if r.Status == "stop_accepted" && r.Reason != reason {
		return StopResult{}, fmt.Errorf("stop response reason mismatch")
	}
	if err := checkStopReceipt(r, stopID); err != nil {
		return StopResult{}, err
	}
	return r, nil
}

// StopQuery re-reads the receipt for an existing stop_id without resubmitting.
// An unknown key answers {"status":"unknown","stop_id":...} (JANUS handleStop).
func (c Client) StopQuery(traceID, stopID string) (StopResult, error) {
	if err := validStopArgs(traceID, stopID); err != nil {
		return StopResult{}, err
	}
	r, err := c.stopCall(map[string]any{"op": "stop_query", "trace_id": traceID, "stop_id": stopID})
	if err != nil {
		return StopResult{}, err
	}
	if err := checkStopReceipt(r, stopID); err != nil {
		return StopResult{}, err
	}
	return r, nil
}

// SubmitStop submits the durable stop intent. It refuses, without any socket
// contact, when execution.stop_requested is not durable yet, and performs no
// execution writes on any response: state changes stay with replay.
func SubmitStop(es execution.Service, c Client, execID string) (StopResult, error) {
	if es.Store == nil {
		return StopResult{}, fmt.Errorf("nil event store")
	}
	r, err := execution.Replay(es.Store.List("execution", execID))
	if err != nil {
		return StopResult{}, err
	}
	if !r.StopRequested {
		return StopResult{}, fmt.Errorf("stop_requested must be durable before submission")
	}
	return c.Stop(r.ExternalID, r.StopID, "", r.StopReason, r.StopEvidenceSeq)
}
