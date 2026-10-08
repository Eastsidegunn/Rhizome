package janusadapter

import (
	"rhizome/internal/journal"
	"rhizome/internal/trust"
)

func noAuthority() trust.Authority { return trust.Authority{} }

func openTestJournal(path string) (*journal.Journal, error) {
	return journal.OpenGuarded(path, trust.NewAnchorless())
}
