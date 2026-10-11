package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/workspace"
)

// FR-RHZ-190: rhizome ops brief writes the board's /v1/ops/brief bytes
// unchanged; -json asks for the JSON form.
func TestOpsBriefCLIFRRHZ190(t *testing.T) {
	board := workspace.NewHTTP(&events.Store{})
	board.Clock = func() time.Time { return time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC) }
	srv := httptest.NewServer(board.Handler())
	defer srv.Close()
	t.Setenv("RHIZOME_BOARD", srv.URL)

	for _, c := range []struct {
		args  []string
		query string
	}{{nil, ""}, {[]string{"-json"}, "?format=json"}} {
		resp, err := http.Get(srv.URL + "/v1/ops/brief" + c.query)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out, errOut bytes.Buffer
		if code := run(append([]string{"ops", "brief"}, c.args...), &out, &errOut); code != 0 || !bytes.Equal(out.Bytes(), want) || len(want) == 0 {
			t.Fatalf("%v exit %d %s:\n%q\nwant\n%q", c.args, code, errOut.String(), out.String(), want)
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"ops", "nope"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown subcommand exit %d", code)
	}
}
