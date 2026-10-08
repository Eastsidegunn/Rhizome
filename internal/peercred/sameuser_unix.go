//go:build linux || darwin

package peercred

import "os"

// SameUser reports whether the verified peer has the current process's UID.
func (v Verified) SameUser() bool {
	return v.verified && v.uid == uint32(os.Getuid())
}
