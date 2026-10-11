package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// FR-RHZ-192 (RHZ-148): GET /v1/launcher/options names the ledger's
// workdir keys (sorted, never their directories), both session modes and the
// ledger model; serve without a session launcher answers 404.
func TestServeLaunchOptionsFRRHZ192(t *testing.T) {
	s := rhz092Store(t)
	f := slFake(t, slBlocking)
	handler, _ := assembleServeWith(s, nil, "", "", nil, slLauncher(t, f), io.Discard, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	before := len(s.All())
	res, err := http.Get(srv.URL + "/v1/launcher/options")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"sessionModes": []any{"oneshot", "interactive"}, "defaultSessionMode": "oneshot",
		"workdirs": []any{"rhizome"}, "defaultWorkdir": "rhizome",
		"models": []any{"claude-opus-5-5"}, "defaultModel": "claude-opus-5-5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
	if strings.Contains(string(b), "/") {
		t.Fatalf("options leak a directory: %s", b)
	}
	if len(s.All()) != before {
		t.Fatal("GET wrote to the journal")
	}

	plain, _ := assembleServeWith(s, nil, "", "", nil, nil, io.Discard, false)
	srv2 := httptest.NewServer(plain)
	defer srv2.Close()
	res2, err := http.Get(srv2.URL + "/v1/launcher/options")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("no launcher: status %d, want 404", res2.StatusCode)
	}
}
