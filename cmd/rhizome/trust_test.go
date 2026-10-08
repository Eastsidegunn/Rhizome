package main

import (
	"rhizome/internal/journal"
	"rhizome/internal/trust"
)

func openTestJournal(path string) (*journal.Journal, error) {
	return journal.OpenGuarded(path, trust.NewAnchorless())
}
