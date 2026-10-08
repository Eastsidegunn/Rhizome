//go:build linux || darwin

package peercred_test

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func connectedUnixConn(t *testing.T) *net.UnixConn {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	leftFile := os.NewFile(uintptr(fds[0]), "peercred-left")
	rightFile := os.NewFile(uintptr(fds[1]), "peercred-right")
	defer leftFile.Close()
	defer rightFile.Close()
	left, err := net.FileConn(leftFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { left.Close() })
	right, err := net.FileConn(rightFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { right.Close() })
	leftUnix, ok := left.(*net.UnixConn)
	if !ok {
		t.Fatalf("left connection type = %T, want *net.UnixConn", left)
	}
	_, ok = right.(*net.UnixConn)
	if !ok {
		t.Fatalf("right connection type = %T, want *net.UnixConn", right)
	}
	return leftUnix
}
