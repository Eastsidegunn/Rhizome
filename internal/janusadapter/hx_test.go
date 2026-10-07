package janusadapter

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHxEnvAllowlistAndSortingFRRHZ126(t *testing.T) {
	parent := []string{
		"XDG_RUNTIME_DIR=/run/user/1000", "SECRET=drop-me", "PATH=/bin",
		"TZ=UTC", "HOME=/home/operator", "LC_CTYPE=en_US.UTF-8",
		"TMPDIR=/tmp/custom", "XDG_DATA_HOME=/data", "LANG=C.UTF-8",
		"HX_RUNTIME_DIR=/run/hx", "LC_ALL=C", "XDG_CONFIG_HOME=/config",
	}
	want := []string{
		"HOME=/home/operator", "HX_RUNTIME_DIR=/run/hx", "LANG=C.UTF-8",
		"LC_ALL=C", "LC_CTYPE=en_US.UTF-8", "PATH=/bin", "TMPDIR=/tmp/custom",
		"TZ=UTC", "XDG_CONFIG_HOME=/config", "XDG_DATA_HOME=/data",
		"XDG_RUNTIME_DIR=/run/user/1000",
	}
	first := hxEnv(parent, nil)
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("hxEnv() = %#v, want %#v", first, want)
	}
	if second := hxEnv(parent, nil); !reflect.DeepEqual(second, first) {
		t.Fatalf("hxEnv output is not deterministic: first=%#v second=%#v", first, second)
	}
}

func TestHxEnvPassthroughAndAbsentNamesFRRHZ126(t *testing.T) {
	parent := []string{"PATH=/bin", "CUSTOM_TOKEN=operator-choice", "custom_token=drop"}
	want := []string{"CUSTOM_TOKEN=operator-choice", "PATH=/bin"}
	got := hxEnv(parent, []string{"CUSTOM_TOKEN", "ABSENT_NAME"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hxEnv() = %#v, want %#v", got, want)
	}
}

func TestHxEnvEmptyParentFRRHZ126(t *testing.T) {
	if got := hxEnv(nil, []string{"ABSENT_NAME"}); len(got) != 0 {
		t.Fatalf("hxEnv(nil) = %#v, want empty", got)
	}
}

func TestRealHxRunAndReplayAllowlistEnvironmentFRRHZ126(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake hx uses a POSIX shell")
	}
	dir := t.TempDir()
	hx := filepath.Join(dir, "fake-hx.sh")
	if err := os.WriteFile(hx, []byte("#!/bin/sh\nenv\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/bin:/usr/bin")
	t.Setenv("RHZ_TEST_SECRET_SENTINEL", "must-not-reach-hx")
	t.Setenv("RHZ_TEST_PASSTHROUGH", "operator-approved")
	cfg := RunConfig{EnvMode: EnvModeAllowlist, Passthrough: []string{"RHZ_TEST_PASSTHROUGH"}}

	runReader, err := RealRunner(hx, dir)(cfg, []byte(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertHxEnvironmentFRRHZ126(t, runReader)

	replayReader, err := RealReplay(hx)(cfg, "/tmp/session.db", "trace")
	if err != nil {
		t.Fatal(err)
	}
	assertHxEnvironmentFRRHZ126(t, replayReader)
}

func TestRealHxRunAndReplayInheritEnvironmentFRRHZ126(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake hx uses a POSIX shell")
	}
	dir := t.TempDir()
	hx := filepath.Join(dir, "fake-hx.sh")
	if err := os.WriteFile(hx, []byte("#!/bin/sh\nenv\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RHZ_TEST_SECRET_SENTINEL", "inherited-by-default")
	cfg := RunConfig{EnvMode: EnvModeInherit}
	if cmd := hxCommand(hx, cfg, "version"); cmd.Env != nil {
		t.Fatalf("inherit mode assigned cmd.Env: %#v", cmd.Env)
	}

	runReader, err := RealRunner(hx, dir)(cfg, []byte(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertHxInheritedSentinelFRRHZ126(t, runReader)

	replayReader, err := RealReplay(hx)(cfg, "/tmp/session.db", "trace")
	if err != nil {
		t.Fatal(err)
	}
	assertHxInheritedSentinelFRRHZ126(t, replayReader)
}

func assertHxEnvironmentFRRHZ126(t *testing.T, r io.Reader) {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	env := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	if _, ok := env["RHZ_TEST_SECRET_SENTINEL"]; ok {
		t.Fatalf("ambient sentinel reached hx: %q", raw)
	}
	if got := env["PATH"]; got != "/bin:/usr/bin" {
		t.Fatalf("PATH = %q, want /bin:/usr/bin; env=%q", got, raw)
	}
	if got := env["RHZ_TEST_PASSTHROUGH"]; got != "operator-approved" {
		t.Fatalf("passthrough = %q, want operator-approved; env=%q", got, raw)
	}
}

func assertHxInheritedSentinelFRRHZ126(t *testing.T, r io.Reader) {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "RHZ_TEST_SECRET_SENTINEL=inherited-by-default\n") {
		t.Fatalf("ambient sentinel did not reach hx in inherit mode: %q", raw)
	}
}

// FR-RHZ-082: RealRunner returns the streaming control pipe while hx is still
// running, then reaps the child and removes the request file at session end.
func TestRealRunnerStreamsAndCleansUpFRRHZ082(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake hx uses a POSIX shell")
	}
	dir := t.TempDir()
	hx := filepath.Join(dir, "fake-hx.sh")
	pidFile := filepath.Join(dir, "hx.pid")
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nprintf 'accepted\\n'\nsleep 1\n"
	if err := os.WriteFile(hx, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	reader, err := RealRunner(hx, dir)(RunConfig{}, []byte(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("runner waited for hx session to finish: %s", elapsed)
	}
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatalf("read streamed control line: %v", err)
	}
	if strings.TrimSpace(line) != "accepted" {
		t.Fatalf("control line = %q, want accepted", line)
	}
	var pid int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if raw, readErr := os.ReadFile(pidFile); readErr == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("fake hx did not write a pid marker")
	}
	files, err := filepath.Glob(filepath.Join(dir, "request-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("request file was not retained during session: files=%v err=%v", files, err)
	}
	cleanupDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(cleanupDeadline) {
		files, err = filepath.Glob(filepath.Join(dir, "request-*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			if err := syscall.Kill(pid, 0); err == nil {
				t.Fatalf("hx process %d remains signalable after session cleanup", pid)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("request file was not removed after hx session ended: %v", files)
}
