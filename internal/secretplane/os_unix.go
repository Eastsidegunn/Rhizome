//go:build unix

package secretplane

import (
	"os"
	"syscall"
)

// ownedBySelf reports whether fi belongs to the effective UID of this process.
func ownedBySelf(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

// lockDir takes an exclusive, non-blocking advisory lock on the state
// directory so that one plane process is the single writer.
func lockDir(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrBusy
	}
	return nil
}

// oNoFollow makes an open fail on a symlink (read-only plane file reads).
const oNoFollow = syscall.O_NOFOLLOW

// ownerUID returns the owner of fi.
func ownerUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
