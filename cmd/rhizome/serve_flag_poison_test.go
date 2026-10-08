package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

const serveHelpGoldenFRRHZ144 = `Usage of serve:
  -addr string
    	 (default "127.0.0.1:8080")
  -blobs string
    	
  -data-dir string
    	
  -index-out string
    	
  -index-repo string
    	
  -janus-accept-root string
    	
  -janus-approval-endpoint string
    	
  -janus-env-mode string
    	 (default "inherit")
  -janus-env-passthrough string
    	
  -janus-exec-config string
    	
  -janus-hx string
    	
  -janus-idle-timeout duration
    	 (default 10m0s)
  -janus-observe-interval duration
    	 (default 5s)
  -janus-profile string
    	
  -janus-session-db string
    	
  -janus-world-config string
    	
  -journal string
    	
  -trust-anchor string
` + "    \t\n" + `  -trust-enforce-janus string
` + "    \t\n"

// FR-RHZ-144: help remains byte-for-byte identical to the pre-change binary.
func TestServeHelpLiteralGoldenFRRHZ144(t *testing.T) {
	for _, arg := range []string{"-h", "-help"} {
		var errOut bytes.Buffer
		if code := serveCtx(context.Background(), []string{arg}, &bytes.Buffer{}, &errOut); code != 2 {
			t.Fatalf("%s exit = %d, want 2", arg, code)
		}
		if got := errOut.String(); got != serveHelpGoldenFRRHZ144 {
			t.Fatalf("%s stderr differs\n--- got ---\n%s--- want ---\n%s", arg, got, serveHelpGoldenFRRHZ144)
		}
	}
}

// FR-RHZ-144: non-help parse errors expose only a fixed usage line.
func TestServeFlagErrorsDoNotEchoArgumentsFRRHZ144(t *testing.T) {
	for _, args := range [][]string{
		{"-unknown-FR-RHZ-144-sentinel"},
		{"-janus-idle-timeout=FR-RHZ-144-sentinel"},
	} {
		var errOut bytes.Buffer
		if code := serveCtx(context.Background(), args, &bytes.Buffer{}, &errOut); code != 2 {
			t.Fatalf("%v exit = %d, want 2", args, code)
		}
		if got := errOut.String(); got != "serve: usage\n" {
			t.Fatalf("%v stderr = %q", args, got)
		}
	}
}

// FR-RHZ-144: serve logs the fixed sentinel once and never exposes the cause.
func TestServeJournalPoisonLogFRRHZ144(t *testing.T) {
	var errOut bytes.Buffer
	logPoison := journalPoisonLogger(&errOut)
	logPoison(errors.New("FR-RHZ-144-private-cause"))
	logPoison(errors.New("another private cause"))
	if got := errOut.String(); got != "journal poisoned: restart required\n" {
		t.Fatalf("stderr = %q", got)
	}
	if strings.Contains(errOut.String(), "private") {
		t.Fatalf("cause leaked: %q", errOut.String())
	}
}
