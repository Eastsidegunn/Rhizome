package janusadapter

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
