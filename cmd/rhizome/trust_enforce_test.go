package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
)

func TestEnforceSelectorParsingFRRHZ151(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		set, want   bool
		wantErr     bool
	}{
		{name: "absent"},
		{name: "all", value: "all", set: true, want: true},
		{name: "empty", set: true, wantErr: true},
		{name: "name", value: "deploy", set: true, wantErr: true},
		{name: "list", value: "all,deploy", set: true, wantErr: true},
		{name: "case", value: "ALL", set: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustEnforceJANUS(tc.value, tc.set)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got=%t err=%v", got, err)
			}
		})
	}
	for _, value := range []string{"", "deploy", "all,deploy", "ALL"} {
		var stderr bytes.Buffer
		code := serveCtx(context.Background(), []string{"-journal", filepath.Join(t.TempDir(), "journal.ndjson"), "-trust-enforce-janus=" + value}, io.Discard, &stderr)
		if code != 2 || stderr.String() != "serve: usage\n" {
			t.Fatalf("value=%q exit=%d stderr=%q", value, code, stderr.String())
		}
	}
}

func TestEnforceAllRequiresAnchorFRRHZ151(t *testing.T) {
	var stderr bytes.Buffer
	code := serveCtx(context.Background(), []string{"-journal", filepath.Join(t.TempDir(), "journal.ndjson"), "-trust-enforce-janus=all"}, io.Discard, &stderr)
	if code != 2 || stderr.String() != "trust-enforce-janus requires -trust-anchor\n" {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}
