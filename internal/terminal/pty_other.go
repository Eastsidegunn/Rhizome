//go:build !darwin && !linux

package terminal

import (
	"errors"
	"os"
)

func openPTY() (master, slave *os.File, err error) {
	return nil, nil, errors.New("pseudo-terminals are supported on darwin and linux only")
}
