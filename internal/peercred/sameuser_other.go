//go:build !linux && !darwin

package peercred

// SameUser always reports false on unsupported operating systems.
func (v Verified) SameUser() bool { return false }
