//go:build unix

package server

import (
	"os"
	"syscall"
)

// ownedBySelf reports whether fi belongs to this process's UID (W1).
func ownedBySelf(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Getuid())
}

// lockSocketDir opens (0600, no symlink) and exclusively locks the socket
// directory lock file; a held lock means another listener: ErrSocketInUse.
func lockSocketDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, ErrSocketDirUnsafe
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !ownedBySelf(fi) {
		f.Close()
		return nil, ErrSocketDirUnsafe
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrSocketInUse
	}
	return f, nil
}
