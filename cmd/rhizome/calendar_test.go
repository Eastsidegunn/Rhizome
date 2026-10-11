package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/workspace"
)

// FR-RHZ-181: rhizome calendar export writes the board's /v1/calendar.ics
// bytes unchanged to stdout or -o; -goal narrows; a refusal exits 1.
func TestCalendarExportCLIFRRHZ181(t *testing.T) {
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
	resp, err := http.Get(srv.URL + "/v1/calendar.ics")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(want, []byte("UID:schedule-review@rhizome.local\r\n")) {
		t.Fatalf("feed lacks schedule:\n%s", want)
	}

	out.Reset()
	if code := run([]string{"calendar", "export"}, &out, &errOut); code != 0 || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("stdout export exit %d:\n%q\nwant\n%q", code, out.String(), want)
	}
	path := filepath.Join(t.TempDir(), "rhizome.ics")
	out.Reset()
	if code := run([]string{"calendar", "export", "-o", path}, &out, &errOut); code != 0 || out.Len() != 0 {
		t.Fatalf("-o export exit %d out %q", code, out.String())
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("-o file differs: %v", err)
	}
	errOut.Reset()
	if code := run([]string{"calendar", "export", "-goal", "goal-none"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "404") {
		t.Fatalf("unknown goal exit %d %q", code, errOut.String())
	}
	if code := run([]string{"calendar", "nope"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown subcommand exit %d", code)
	}
}
