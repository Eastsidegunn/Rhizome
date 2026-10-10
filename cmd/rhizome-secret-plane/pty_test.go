//go:build darwin || linux

package main

import (
	"encoding/base64"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"rhizome/internal/termio"
)

// TestPTYNonEchoFRRHZ139 drives a real session on a real pseudo-terminal:
// the real termio.IsTerminal and termio.ReadPassword, stdin and stdout on
// the PTY slave. A sentinel value typed at the master is never echoed back
// (the master's output carries no §8.1 form) and lands only in values.json.
func TestPTYNonEchoFRRHZ139(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	defer master.Close()
	var term syncBuf
	readDone := make(chan struct{})
	go func() {
		io.Copy(&term, master) // ends with EIO/EOF once the slave is closed
		close(readDone)
	}()

	s := sentinel(t)
	base := shortDir(t)
	dir, sock := filepath.Join(base, "st"), filepath.Join(base, "run", "p.sock")
	var errb syncBuf
	e := &env{stdin: slave, stdout: slave, stderr: &errb, stdinFd: int(slave.Fd()),
		isTerminal: termio.IsTerminal, readPassword: termio.ReadPassword,
		configDir: func() (string, error) { return "", errors.New("unused") },
		exit:      func(int) { t.Error("fatal exit") }}
	done := make(chan int, 1)
	go func() { done <- run([]string{"session", "-state-dir", dir, "-socket", sock}, e) }()

	wait := func(substr string, n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for strings.Count(term.String(), substr) < n {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for a prompt; stderr %q", errb.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// echoOff waits until ReadPassword has cleared ECHO on the terminal, so
	// the value is typed only under the real non-echo mode.
	echoOff := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			tio, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermios)
			if err != nil {
				t.Fatal(err)
			}
			if tio.Lflag&unix.ECHO == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("echo never disabled")
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	typeIn := func(text string) {
		t.Helper()
		if _, err := master.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}

	wait("plane> ", 1)
	typeIn("register\nptykey\nother\n\n")
	wait("value (not echoed): ", 1)
	echoOff()
	typeIn(s + "\n")
	wait("value again (not echoed): ", 1)
	echoOff()
	typeIn(s + "\n")
	wait(`type "yes" to continue: `, 1)
	typeIn("yes\n")
	wait("outcome: ok version 1", 1)
	wait("plane> ", 2)
	typeIn("quit\n")
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not quit")
	}
	slave.Close()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		master.Close()
		<-readDone
	}

	out := term.String()
	if !strings.Contains(out, "register") || !strings.Contains(out, "ptykey") {
		t.Fatal("the terminal did not echo ordinary input (PTY not in echo mode)")
	}
	assertNoSentinel(t, "pty output", []byte(out), s)
	assertNoSentinel(t, "stderr", []byte(errb.String()), s)
	assertNoSentinel(t, "ledger", readFile(t, filepath.Join(dir, "ledger.ndjson")), s)
	if !strings.Contains(string(readFile(t, filepath.Join(dir, "values.json"))), base64.StdEncoding.EncodeToString([]byte(s))) {
		t.Fatal("values file lacks the typed value")
	}
}
