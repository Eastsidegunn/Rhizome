package janusadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"rhizome/internal/approval"
	"strings"
)

type Dialer func() (net.Conn, error)
type Client struct{ Dial Dialer }
type Result struct {
	Status                                                string
	Decision                                              approval.Decision
	Reason                                                string
	ResponseSeq                                           int64
	ResponseID, RequestDigest, PolicyHash, DisplaySummary string
	ExpiresAt                                             int64
}

var ErrUnavailable = errors.New("UNAVAILABLE")

// exchange performs one request/response line exchange on a fresh socket
// connection. Dial, write and empty-read failures are all ErrUnavailable.
func (c Client) exchange(req map[string]any) ([]byte, error) {
	if c.Dial == nil {
		return nil, ErrUnavailable
	}
	conn, e := c.Dial()
	if e != nil {
		return nil, ErrUnavailable
	}
	defer conn.Close()
	b, e := json.Marshal(req)
	if e != nil {
		return nil, e
	}
	if _, e = conn.Write(append(b, '\n')); e != nil {
		return nil, ErrUnavailable
	}
	line, e := bufio.NewReader(conn).ReadBytes('\n')
	if e != nil && len(line) == 0 {
		return nil, ErrUnavailable
	}
	return line, nil
}

func (c Client) call(req map[string]any) (Result, error) {
	line, e := c.exchange(req)
	if e != nil {
		return Result{}, e
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(line, &raw) != nil {
		return Result{}, fmt.Errorf("invalid response")
	}
	for k := range raw {
		if k != "status" && k != "decision" && k != "reason" && k != "response_seq" && k != "response_id" && k != "request_digest" && k != "policy_hash" && k != "display_summary" && k != "expires_at" {
			return Result{}, fmt.Errorf("unknown response field: %s", k)
		}
	}
	var r Result
	for k, v := range raw {
		var err error
		switch k {
		case "status":
			err = json.Unmarshal(v, &r.Status)
		case "decision":
			err = json.Unmarshal(v, &r.Decision)
		case "reason":
			err = json.Unmarshal(v, &r.Reason)
		case "response_seq":
			err = json.Unmarshal(v, &r.ResponseSeq)
		case "response_id":
			err = json.Unmarshal(v, &r.ResponseID)
		case "request_digest":
			err = json.Unmarshal(v, &r.RequestDigest)
		case "policy_hash":
			err = json.Unmarshal(v, &r.PolicyHash)
		case "display_summary":
			err = json.Unmarshal(v, &r.DisplaySummary)
		case "expires_at":
			err = json.Unmarshal(v, &r.ExpiresAt)
		}
		if err != nil {
			return Result{}, fmt.Errorf("invalid response field %s", k)
		}
	}
	if r.Status == "error" {
		return Result{}, errors.New(r.Reason)
	}
	if r.Status != "pending" && r.Status != "decided" && r.Status != "expired" && r.Status != "unknown" {
		return Result{}, fmt.Errorf("invalid status")
	}
	if r.Status == "decided" && !validDecision(r.Decision) {
		return Result{}, fmt.Errorf("invalid decision")
	}
	if r.Status == "expired" && r.Decision != approval.Deny {
		return Result{}, fmt.Errorf("invalid expired decision")
	}
	return r, nil
}
func validDecision(d approval.Decision) bool { return d == approval.Allow || d == approval.Deny }
func (c Client) Submit(k approval.RequestKey, response string, d approval.Decision, reason string) (Result, error) {
	if k.TraceID == "" || k.SpanID == "" || k.RequestID == "" || response == "" || !validDecision(d) || (d == approval.Deny && reason == "") {
		return Result{}, fmt.Errorf("invalid submit")
	}
	return c.call(map[string]any{"op": "submit", "trace_id": k.TraceID, "span_id": k.SpanID, "request_id": k.RequestID, "response_id": response, "decision": d, "reason": reason})
}
func (c Client) Query(k approval.RequestKey) (Result, error) {
	if k.TraceID == "" || k.SpanID == "" || k.RequestID == "" {
		return Result{}, fmt.Errorf("invalid query")
	}
	return c.call(map[string]any{"op": "query", "trace_id": k.TraceID, "span_id": k.SpanID, "request_id": k.RequestID})
}

// TailEvent is one events_tail envelope (contract ② v1.6 / JANUS T25): seq,
// ts, kind, actor, span and trace identity plus the optional usage counters.
// It carries NO payload — approval keys, decisions and done status are only
// readable through hx replay (ParseReplay), which is why the loop consumes
// the tail as a hybrid (FR-RHZ-119).
type TailEvent struct {
	Seq          int64  `json:"seq"`
	TS           int64  `json:"ts"`
	Kind         string `json:"kind"`
	Actor        string `json:"actor"`
	SpanID       string `json:"span_id"`
	TraceID      string `json:"trace_id"`
	ParentSpanID string `json:"parent_span_id"`
	UsageIn      int64  `json:"usage_in"`
	UsageOut     int64  `json:"usage_out"`
}

// TailSession is the per-call session status JANUS recomputes from seq 1.
type TailSession struct {
	State          string `json:"state"`
	DoneStatus     string `json:"done_status"`
	TerminalSeq    int64  `json:"terminal_seq"`
	SessionMode    string `json:"session_mode"`
	LastSeq        int64  `json:"last_seq"`
	LastActivityTS int64  `json:"last_activity_ts"`
	UsageInTotal   int64  `json:"usage_in_total"`
	UsageOutTotal  int64  `json:"usage_out_total"`
}

// TailResult is the parsed events_tail answer. Events are the envelopes with
// seq > from_seq (at most one server page); More says the page was truncated
// and NextFromSeq is the from_seq of the next call.
type TailResult struct {
	Status      string      `json:"status"`
	Reason      string      `json:"reason"`
	SessionID   string      `json:"session_id"`
	Events      []TailEvent `json:"events"`
	NextFromSeq int64       `json:"next_from_seq"`
	More        bool        `json:"more"`
	Session     TailSession `json:"session"`
}

// ErrTail is a structured events_tail rejection: Reason is the JANUS reason
// verbatim (UNKNOWN_SESSION, LOG_UNAVAILABLE, LOG_INVALID, REQUEST_MISMATCH).
type ErrTail struct{ Reason string }

func (e ErrTail) Error() string { return e.Reason }

// EventsTail issues {"op":"events_tail","session_id","from_seq"} (session_id
// is the session trace_id) and parses the TailResult strictly: unknown fields
// at any level are an error, status must be ok or error, an error status is
// returned as ErrTail, and a dial failure is ErrUnavailable (FR-RHZ-119).
func (c Client) EventsTail(sessionID string, fromSeq int64) (TailResult, error) {
	if sessionID == "" || fromSeq < 0 {
		return TailResult{}, fmt.Errorf("invalid events_tail")
	}
	line, e := c.exchange(map[string]any{"op": "events_tail", "session_id": sessionID, "from_seq": fromSeq})
	if e != nil {
		return TailResult{}, e
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var r TailResult
	if e = dec.Decode(&r); e != nil {
		return TailResult{}, fmt.Errorf("invalid tail response: %v", e)
	}
	switch r.Status {
	case "error":
		if r.Reason == "" {
			return TailResult{}, fmt.Errorf("invalid tail response: error without reason")
		}
		return TailResult{}, ErrTail{Reason: r.Reason}
	case "ok":
	default:
		return TailResult{}, fmt.Errorf("invalid tail status")
	}
	if r.Events == nil || r.NextFromSeq < fromSeq {
		return TailResult{}, fmt.Errorf("invalid tail response: events/next_from_seq")
	}
	return r, nil
}

// MaxMessageBytes is the JANUS relay line limit (server.go maxRelayMessage,
// 64 KiB). A longer text is refused locally as REQUEST_MISMATCH — the same
// reason the server would give — without any socket contact.
const MaxMessageBytes = 64 * 1024

// MessageResult is the parsed send_message answer: Status is
// "message_accepted" with MessageSeq the user/message seq in the session log.
type MessageResult struct {
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	MessageSeq  int64  `json:"message_seq"`
	TerminalRef int64  `json:"terminal_ref"`
}

// ErrSendMessage is a structured send_message rejection. Reason is the JANUS
// reason verbatim: UNKNOWN_SESSION, NOT_MULTITURN, SESSION_TERMINAL,
// SESSION_NOT_READY, DELIVERY_FAILED, REQUEST_MISMATCH, LOG_UNAVAILABLE.
// MessageSeq is set with DELIVERY_FAILED (the text was logged but not taken),
// TerminalRef with SESSION_TERMINAL.
type ErrSendMessage struct {
	Reason      string
	MessageSeq  int64
	TerminalRef int64
}

func (e ErrSendMessage) Error() string { return e.Reason }

// SendMessage issues {"op":"send_message","session_id","text"} (session_id is
// the session trace_id) and parses the answer strictly (FR-RHZ-119, contract
// ② v1.6 §3). A rejection is ErrSendMessage, a dial failure ErrUnavailable.
// The op is NOT idempotent by contract: a retry may inject the text twice,
// so callers submit once and consumers dedupe through the user/message kind
// (seq) in the session log.
func (c Client) SendMessage(sessionID, text string) (MessageResult, error) {
	if sessionID == "" || strings.TrimSpace(text) == "" {
		return MessageResult{}, fmt.Errorf("invalid send_message")
	}
	// The relay caps the WHOLE request line (server.go maxRelayMessage), and
	// json escaping can grow the text, so the guard runs on the marshalled line.
	// REQUEST_MISMATCH is synthesised locally to mirror what
	// the server would answer; no dial happens.
	req := map[string]any{"op": "send_message", "session_id": sessionID, "text": text}
	if b, me := json.Marshal(req); me != nil || len(b)+1 > MaxMessageBytes {
		return MessageResult{}, ErrSendMessage{Reason: "REQUEST_MISMATCH"}
	}
	line, e := c.exchange(req)
	if e != nil {
		return MessageResult{}, e
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var r MessageResult
	if e = dec.Decode(&r); e != nil {
		return MessageResult{}, fmt.Errorf("invalid send_message response: %v", e)
	}
	switch r.Status {
	case "error":
		if r.Reason == "" {
			return MessageResult{}, fmt.Errorf("invalid send_message response: error without reason")
		}
		return MessageResult{}, ErrSendMessage{Reason: r.Reason, MessageSeq: r.MessageSeq, TerminalRef: r.TerminalRef}
	case "message_accepted":
		if r.MessageSeq <= 0 {
			return MessageResult{}, fmt.Errorf("invalid send_message response: message_seq")
		}
		return r, nil
	default:
		return MessageResult{}, fmt.Errorf("invalid send_message status")
	}
}

// The digest primitives moved to the approval package in RHZ-047 (workspace
// relay needs them without importing janusadapter); these delegating aliases
// keep the RHZ-043 surface and its tests intact.
var ErrDigestMismatch = approval.ErrDigestMismatch

func VerifyDigest(local, remote string) error { return approval.VerifyDigest(local, remote) }
