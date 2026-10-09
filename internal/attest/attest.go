// Package attest records signed manifests of already-stored gate decisions.
package attest

import (
	"encoding/json"
	"errors"
	"rhizome/internal/events"
	"rhizome/internal/trust"
	"time"
)

// Item is one canonical manifest decision.
type Item = trust.AttestItem

// Service appends attest manifests through the globally guarded event port.
type Service struct{ Store events.Port }

// Create records one all-or-nothing manifest. Cryptographic and decision
// validation belongs to the trust Guard; this execution-layer service only
// turns the accepted intent into the exact stored event shape.
func (s Service) Create(manifestDigest string, items []Item, sig trust.Signature) error {
	if s.Store == nil {
		return errors.New("nil store")
	}
	if manifestDigest != trust.ManifestDigest(items) {
		return errors.New("attest manifest digest mismatch")
	}
	payload, err := json.Marshal(struct {
		ManifestDigest string
		Items          []Item
		Signature      trust.Signature
	}{manifestDigest, append([]Item(nil), items...), sig})
	if err != nil {
		return err
	}
	log := s.Store.List(trust.AttestAggregateType, trust.AttestAggregateID)
	revision := uint64(len(log))
	return s.Store.Append(revision, events.Event{
		AggregateType: trust.AttestAggregateType,
		AggregateID:   trust.AttestAggregateID,
		Revision:      revision + 1,
		Type:          trust.AttestRecordedType,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	})
}
