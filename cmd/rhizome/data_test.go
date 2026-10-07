package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestResolveDataDirBranchesFRRHZ125(t *testing.T) {
	cwd := "/work/tree"
	home := "/users/tester"
	tests := []struct {
		name, flag, goos, home string
		env                    map[string]string
		want                   string
		wantErr                string
	}{
		{name: "flag wins and relative is absolute", flag: "state", goos: "linux", home: home, env: map[string]string{"RHIZOME_DATA_DIR": "/env"}, want: filepath.Join(cwd, "state")},
		{name: "env wins", goos: "linux", home: home, env: map[string]string{"RHIZOME_DATA_DIR": "env-state"}, want: filepath.Join(cwd, "env-state")},
		{name: "darwin default", goos: "darwin", home: home, env: map[string]string{}, want: filepath.Join(home, "Library", "Application Support", "rhizome")},
		{name: "linux xdg absolute", goos: "linux", home: home, env: map[string]string{"XDG_DATA_HOME": "/xdg"}, want: "/xdg/rhizome"},
		{name: "linux xdg relative falls back", goos: "linux", home: home, env: map[string]string{"XDG_DATA_HOME": "relative"}, want: filepath.Join(home, ".local", "share", "rhizome")},
		{name: "linux xdg unset", goos: "linux", home: home, env: map[string]string{}, want: filepath.Join(home, ".local", "share", "rhizome")},
		{name: "windows local app data", goos: "windows", home: "", env: map[string]string{"LOCALAPPDATA": "/local"}, want: "/local/rhizome"},
		{name: "empty home", goos: "linux", home: "", env: map[string]string{}, wantErr: "home directory is unset"},
		{name: "windows local app data missing", goos: "windows", home: home, env: map[string]string{}, wantErr: "LocalAppData is unset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDataDir(tt.flag, envMap(tt.env), tt.goos, tt.home, cwd)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("resolveDataDir = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestDefaultServeDataDirFRRHZ125(t *testing.T) {
	d := t.TempDir()
	data := filepath.Join(d, "data")
	var errOut bytes.Buffer
	code := serveCtx(context.Background(), []string{"-data-dir", data, "-addr", "bad-address"}, &bytes.Buffer{}, &errOut)
	if code != 1 {
		t.Fatalf("serve exit %d, want listen failure", code)
	}
	journalPath := filepath.Join(data, "journal.ndjson")
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatal("default journal was not created:", err)
	}
	info, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("data dir mode %o, want 700", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(data, "blobs")); err != nil {
		t.Fatal("default blobs directory missing:", err)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "rhizome: data dir "+data+"\n") || strings.Count(got, "rhizome: data dir ") != 1 {
		t.Fatalf("stderr = %q", got)
	}
}

func TestExplicitServeJournalUnchangedFRRHZ125(t *testing.T) {
	d := t.TempDir()
	journalPath := filepath.Join(d, "journal.ndjson")
	var errOut bytes.Buffer
	code := serveCtx(context.Background(), []string{"-journal", journalPath, "-addr", "bad-address"}, &bytes.Buffer{}, &errOut)
	if code != 1 {
		t.Fatalf("serve exit %d, want listen failure", code)
	}
	if strings.Contains(errOut.String(), "rhizome: data dir") {
		t.Fatalf("explicit journal emitted data-dir line: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(d, "blobs")); !os.IsNotExist(err) {
		t.Fatalf("unexpected blobs directory: %v", err)
	}
}

func TestExplicitEmptyServeJournalSilentFRRHZ125(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	var errOut bytes.Buffer
	if code := serveCtx(context.Background(), []string{"-journal", "", "-data-dir", data}, &bytes.Buffer{}, &errOut); code != 2 {
		t.Fatalf("empty explicit journal exit %d, want 2", code)
	}
	if errOut.Len() != 0 {
		t.Fatalf("empty explicit journal wrote stderr: %q", errOut.String())
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("empty explicit journal created data directory: %v", err)
	}
}

func TestDefaultSubcommandDataPathsFRRHZ125(t *testing.T) {
	d := t.TempDir()
	t.Setenv("RHIZOME_DATA_DIR", d)
	note := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(note, []byte("default data"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := ingest([]string{"-file", note}, &out, &errOut); code != 0 {
		t.Fatalf("ingest exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(d, "journal.ndjson")); err != nil {
		t.Fatal("default ingest journal missing:", err)
	}
	if _, err := os.Stat(filepath.Join(d, "blobs")); err != nil {
		t.Fatal("default ingest blobs missing:", err)
	}
	var memoriesOut bytes.Buffer
	if code := memories(nil, &memoriesOut, &errOut); code != 0 || memoriesOut.Len() == 0 {
		t.Fatalf("memories defaulting exit %d, output %q, errors %q", code, memoriesOut.String(), errOut.String())
	}
}

func TestDefaultGraphAndIndexDataPathsFRRHZ125(t *testing.T) {
	repo, explicitJournal, _ := fixture058(t)
	data := t.TempDir()
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	// The graph command reads the journal from the selected data directory.
	sha := runGit058(t, repo, "rev-parse", "refs/heads/main")
	seedMainAdvanced(t, explicitJournal, sha)
	if err := os.Rename(explicitJournal, filepath.Join(data, "journal.ndjson")); err != nil {
		t.Fatal(err)
	}
	var graphOut, errOut bytes.Buffer
	if code := graphCmd([]string{"-data-dir", data, "-repo", repo, "-out", filepath.Join(data, "index")}, &graphOut, &errOut); code != 0 {
		t.Fatalf("graph defaulting exit %d: %s", code, errOut.String())
	}
	if graphOut.Len() == 0 {
		t.Fatal("graph default output is empty")
	}
	indexData := filepath.Join(t.TempDir(), "index-data")
	var indexOut bytes.Buffer
	if code := indexCmd([]string{"-repo", repo, "-out", filepath.Join(indexData, "index")}, &indexOut, &errOut); code != 0 {
		t.Fatalf("index explicit output exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(indexData, "index", sha, "graph.json")); err != nil {
		t.Fatal("explicit index output missing:", err)
	}
	if code := indexCmd([]string{"-data-dir", filepath.Join(t.TempDir(), "ignored"), "-repo", repo, "-out", filepath.Join(t.TempDir(), "index")}, &indexOut, &errOut); code != 2 {
		t.Fatalf("index accepted removed -data-dir flag: %d", code)
	}
}

func TestDefaultJournalSubcommandsFRRHZ125(t *testing.T) {
	data := t.TempDir()
	t.Setenv("RHIZOME_DATA_DIR", data)
	note := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(note, []byte("default data location"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"ingest", "-file", note}, &out, &errOut); code != 0 {
		t.Fatalf("default ingest exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(data, "journal.ndjson")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "blobs")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"memories"}, &out, &errOut); code != 0 {
		t.Fatalf("default memories exit %d: %s", code, errOut.String())
	}
	// graph resolves the same default journal before reporting that this fresh
	// journal has no recorded code index.
	out.Reset()
	errOut.Reset()
	if code := run([]string{"graph", "-repo", t.TempDir(), "-out", filepath.Join(t.TempDir(), "index")}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no index recorded") {
		t.Fatalf("default graph exit %d: %s", code, errOut.String())
	}
}

func TestDataDirFlagPrecedesEnvironmentFRRHZ125(t *testing.T) {
	envDir := t.TempDir()
	flagDir := t.TempDir()
	t.Setenv("RHIZOME_DATA_DIR", envDir)
	note := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(note, []byte("flag wins"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"ingest", "-data-dir", flagDir, "-file", note}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("flag-selected ingest exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(flagDir, "journal.ndjson")); err != nil {
		t.Fatal("flag data directory was not selected:", err)
	}
	if _, err := os.Stat(filepath.Join(envDir, "journal.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("environment data directory was selected: %v", err)
	}
}
