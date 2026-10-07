package janusadapter

// RHZ-093 client tests (FR-RHZ-119): send_message wire, every
// rejection reason, and the local size guard — fake sockets only.

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestClientSendMessageWireExactFRRHZ119(t *testing.T) {
	c := Client{Dial: fake(func(q map[string]any) map[string]any {
		assertWireKeys(t, q, "op", "session_id", "text")
		if q["op"] != "send_message" || q["session_id"] != replayTrace || q["text"] != "continue please" {
			t.Errorf("wire: %v", q)
		}
		return map[string]any{"status": "message_accepted", "message_seq": 42}
	})}
	r, err := c.SendMessage(replayTrace, "continue please")
	if err != nil || r.Status != "message_accepted" || r.MessageSeq != 42 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range [][2]string{{"", "x"}, {replayTrace, ""}, {replayTrace, "   "}} {
		if _, err = c.SendMessage(bad[0], bad[1]); err == nil {
			t.Fatalf("accepted invalid %q", bad)
		}
	}
}

func TestClientSendMessageRejectionsFRRHZ119(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		extra     map[string]any
		seq, tref int64
	}{
		{reason: "UNKNOWN_SESSION"},
		{reason: "NOT_MULTITURN"},
		{reason: "SESSION_TERMINAL", extra: map[string]any{"terminal_ref": 9}, tref: 9},
		{reason: "SESSION_NOT_READY"},
		{reason: "DELIVERY_FAILED", extra: map[string]any{"message_seq": 7}, seq: 7},
		{reason: "REQUEST_MISMATCH"},
		{reason: "LOG_UNAVAILABLE"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			c := Client{Dial: fake(func(q map[string]any) map[string]any {
				resp := map[string]any{"status": "error", "reason": tc.reason}
				for k, v := range tc.extra {
					resp[k] = v
				}
				return resp
			})}
			_, err := c.SendMessage(replayTrace, "hi")
			var rej ErrSendMessage
			if !errors.As(err, &rej) || rej.Reason != tc.reason || err.Error() != tc.reason || rej.MessageSeq != tc.seq || rej.TerminalRef != tc.tref {
				t.Fatalf("%s: %+v %v", tc.reason, rej, err)
			}
		})
	}
	// Invalid answers are neither ErrSendMessage nor ErrUnavailable.
	for name, resp := range map[string]map[string]any{
		"unknown_field":  {"status": "message_accepted", "message_seq": 1, "decision": "allow"},
		"unknown_status": {"status": "pending"},
		"no_reason":      {"status": "error"},
		"no_seq":         {"status": "message_accepted"},
	} {
		t.Run(name, func(t *testing.T) {
			c := Client{Dial: fake(func(q map[string]any) map[string]any { return resp })}
			_, err := c.SendMessage(replayTrace, "hi")
			var rej ErrSendMessage
			if err == nil || errors.As(err, &rej) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("%v", err)
			}
		})
	}
	down := Client{Dial: func() (net.Conn, error) { return nil, errors.New("down") }}
	if _, err := down.SendMessage(replayTrace, "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dial failure: %v", err)
	}
}

// A text over the 64 KiB relay line limit is refused locally as
// REQUEST_MISMATCH without any socket contact.
func TestClientSendMessageSizeGuardNoDialFRRHZ119(t *testing.T) {
	dialed := false
	c := Client{Dial: func() (net.Conn, error) { dialed = true; return nil, errors.New("must not dial") }}
	_, err := c.SendMessage(replayTrace, strings.Repeat("x", MaxMessageBytes+1))
	var rej ErrSendMessage
	if !errors.As(err, &rej) || rej.Reason != "REQUEST_MISMATCH" || dialed {
		t.Fatalf("size guard: %v dialed=%v", err, dialed)
	}
	// The guard is on the marshalled LINE, not the text: a text that fits but
	// whose JSON line (envelope + escaping of < > &) exceeds the cap is refused
	// locally; a comfortably small text is sent.
	c = Client{Dial: func() (net.Conn, error) { dialed = true; return nil, errors.New("must not dial") }}
	dialed = false
	if _, err = c.SendMessage(replayTrace, strings.Repeat("<", MaxMessageBytes-64)); !errors.As(err, &rej) || rej.Reason != "REQUEST_MISMATCH" || dialed {
		t.Fatalf("envelope guard: %v dialed=%v", err, dialed)
	}
	c = Client{Dial: fake(func(q map[string]any) map[string]any {
		return map[string]any{"status": "message_accepted", "message_seq": 1}
	})}
	if _, err = c.SendMessage(replayTrace, strings.Repeat("x", 1024)); err != nil {
		t.Fatal(err)
	}
}
