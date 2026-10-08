//go:build darwin

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

	var cred *unix.Xucred
	var pid int
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if sockErr != nil {
			return
		}
		// LOCAL_PEERPID is not available on every Darwin version. Failure to
		// retrieve it does not invalidate the UID and GID supplied by xucred.
		peerPID, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if err == nil && peerPID > 0 {
			pid = peerPID
		}
	}); err != nil {
		return Verified{}, errInvalidConnection
	}
	if sockErr != nil {
		return Verified{}, fmt.Errorf("peercred: get peer credentials: %w", sockErr)
	}
	if cred.Ngroups < 1 {
		return Verified{}, errors.New("peercred: peer GID unavailable")
	}
	return Verified{
		uid:      cred.Uid,
		gid:      cred.Groups[0],
		pid:      int32(pid),
		verified: true,
	}, nil
}
