package journal

import "rhizome/internal/trust"

func openTestJournal(path string) (*Journal, error) {
	return OpenGuarded(path, trust.NewAnchorless())
}
