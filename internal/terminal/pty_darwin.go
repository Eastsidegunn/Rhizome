//go:build darwin

package terminal

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair (posix_openpt / grantpt / unlockpt /
// ptsname via the Darwin ioctls). The master keeps Go's poller mode: its fd
// is only touched through SyscallConn, never Fd.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var name string
	err = control(m, func(fd int) error {
		if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
			return err
		}
		if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
			return err
		}
		buf := make([]byte, 128)
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
			return e
		}
		name = string(buf[:bytes.IndexByte(buf, 0)])
		return nil
	})
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
