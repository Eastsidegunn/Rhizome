//go:build !linux && !darwin

package peercred

import "net"

// Verify fails closed on targets without a reviewed peer-credential API.
func Verify(*net.UnixConn) (Verified, error) {
	return Verified{}, ErrUnsupported
}
