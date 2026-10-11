//go:build linux

package terminal

import (
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair (/dev/ptmx, unlock, ptsname). The
// master keeps Go's poller mode: its fd is only touched through SyscallConn.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n int
	err = control(m, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		var err error
		n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		return err
	})
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
