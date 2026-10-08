// Package termio provides the small terminal primitives used by interactive
// secret entry.
package termio

import "golang.org/x/term"

// IsTerminal reports whether fd refers to a terminal.
func IsTerminal(fd int) bool { return term.IsTerminal(fd) }

// ReadPassword reads a line from a terminal without echoing the input.
func ReadPassword(fd int) ([]byte, error) { return term.ReadPassword(fd) }
