//go:build linux

package peercred

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

var errInvalidConnection = errors.New("peercred: invalid Unix connection")

// Verify returns the kernel-reported peer credentials captured when conn was
// connected. The returned PID is only a diagnostic hint and must never be used
// for authorization.
func Verify(conn *net.UnixConn) (Verified, error) {
	if conn == nil {
		return Verified{}, errInvalidConnection
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return Verified{}, errInvalidConnection
	}

	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Verified{}, errInvalidConnection
	}
	if sockErr != nil {
		return Verified{}, fmt.Errorf("peercred: get peer credentials: %w", sockErr)
	}
	return Verified{
		uid:      cred.Uid,
		gid:      cred.Gid,
		pid:      cred.Pid,
		verified: true,
	}, nil
}
