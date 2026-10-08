// Package peercred verifies the operating-system credentials of Unix socket
// peers.
package peercred

import "errors"

// ErrUnsupported reports that the target operating system has no peer
// credential implementation.
var ErrUnsupported = errors.New("peercred: unsupported operating system")

// Verified contains a read-only snapshot of credentials reported by the kernel
// for a Unix socket peer at connect time. GID is the peer's effective GID; on
// Darwin it is obtained from Groups[0].
//
// PID is only a hint for diagnostics. Zero means unknown, it may be zero across
// PID namespaces on Linux, and process IDs can be reused. PID must never be
// used for authorization.
type Verified struct {
	uid uint32
	gid uint32
	pid int32

	verified bool
}

// UID returns the peer's effective user ID.
func (v Verified) UID() uint32 { return v.uid }

// GID returns the peer's effective group ID.
func (v Verified) GID() uint32 { return v.gid }

// PID returns the peer process ID hint, or zero when it is unknown.
// It must never be used for authorization.
func (v Verified) PID() int32 { return v.pid }
