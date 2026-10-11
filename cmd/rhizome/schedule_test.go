package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/workspace"
)

// FR-RHZ-180: rhizome schedule add/list against a board; the address comes
// from -addr, else RHIZOME_BOARD.
func TestScheduleCLIFRRHZ180(t *testing.T) {
	s := &events.Store{}
	board := workspace.NewHTTP(s)
	board.Clock = func() time.Time { return time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC) }
	srv := httptest.NewServer(board.Handler())
	defer srv.Close()
	t.Setenv("RHIZOME_BOARD", srv.URL)

	var out, errOut bytes.Buffer
	if code := run([]string{"schedule", "add", "-name", "review", "-weekly", "mon", "-at", "09:00"}, &out, &errOut); code != 0 {
		t.Fatalf("add exit %d: %s", code, errOut.String())
	}
	if out.String() != "schedule-review\n" {
		t.Fatalf("add out = %q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"schedule", "add", "-name", "bad", "-weekly", "mon", "-at", "9am"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "rejected") {
		t.Fatalf("bad time: exit %d %q", code, errOut.String())
	}
	if code := run([]string{"schedule", "add", "-name", "x", "-weekly", "someday", "-at", "09:00"}, &out, &errOut); code != 2 {
		t.Fatalf("bad weekday exit %d", code)
	}
	out.Reset()
	// -addr wins over RHIZOME_BOARD.
	if code := run([]string{"schedule", "list", "-addr", strings.TrimPrefix(srv.URL, "http://")}, &out, &errOut); code != 0 {
		t.Fatalf("list exit %d: %s", code, errOut.String())
	}
	if want := "schedule-review\treview\tweekly mon 09:00\tactive\t2026-10-12T09:00:00+09:00\t-\n"; out.String() != want {
		t.Fatalf("list out = %q, want %q", out.String(), want)
	}
	t.Setenv("RHIZOME_BOARD", "")
	if code := run([]string{"schedule", "list", "-addr", "127.0.0.1:1"}, &out, &errOut); code != 1 {
		t.Fatalf("unreachable board exit %d", code)
	}
}
