//go:build unix

package secretchannel

import (
	"os"
	"syscall"
)

// ownerUID returns the owner UID of fi.
func ownerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
