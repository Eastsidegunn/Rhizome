package trust

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"rhizome/internal/events"
	"strings"
	"testing"
)

func genesisEvent148(t *testing.T, changes map[string]any) events.Event {
	t.Helper()
	payload := map[string]any{
		"Format": AnchorFormat, "Principal": "H", "Algorithm": AlgorithmEd25519,
		"Assurance": AssuranceKey, "PublicKey": "MCowBQYDK2VwAyEA11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=",
		"KeyID":     "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9",
		"JournalID": "00112233445566778899aabbccddeeff",
	}
	for key, value := range changes {
		if value == nil {
			delete(payload, key)
		} else {
			payload[key] = value
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{Sequence: 1, AggregateType: TrustAggregateType, AggregateID: TrustAggregateID, Revision: 1, Type: TrustGenesisType, Payload: raw}
}

func TestGenesisCanonicalMatchTableFRRHZ148(t *testing.T) {
	anchor, err := ParseAnchor([]byte(testAnchorJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAnchored(anchor).CheckReplay(&events.Store{}, genesisEvent148(t, nil)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]map[string]any{
		"format":        {"Format": "other"},
		"principal":     {"Principal": "X"},
		"algorithm":     {"Algorithm": "ecdsa-p256"},
		"assurance":     {"Assurance": "presence"},
		"public key":    {"PublicKey": "AA=="},
		"key id":        {"KeyID": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"journal id":    {"JournalID": "ABC"},
		"unknown key":   {"Extra": true},
		"missing field": {"Assurance": nil},
	} {
		t.Run(name, func(t *testing.T) {
			if err := NewAnchored(anchor).CheckReplay(&events.Store{}, genesisEvent148(t, change)); err == nil {
				t.Fatal("invalid genesis accepted")
			}
		})
	}
}

func TestAnchoredGenesisRejectsDifferentValidKeyFRRHZ148(t *testing.T) {
	anchor, err := ParseAnchor([]byte(testAnchorJSON))
	if err != nil {
		t.Fatal(err)
	}
	otherPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	otherPublic := otherPrivate.Public().(ed25519.PublicKey)
	otherDER, err := x509.MarshalPKIXPublicKey(otherPublic)
	if err != nil {
		t.Fatal(err)
	}
	otherKeyID, err := KeyID(otherPublic)
	if err != nil {
		t.Fatal(err)
	}
	genesis := genesisEvent148(t, map[string]any{
		"PublicKey": base64.StdEncoding.EncodeToString(otherDER),
		"KeyID":     otherKeyID,
	})
	if err := NewAnchored(anchor).CheckReplay(&events.Store{}, genesis); err == nil || !strings.Contains(err.Error(), "trust anchor does not match genesis") {
		t.Fatalf("anchored replay error = %v, want anchor/genesis mismatch", err)
	}
	if err := NewUnanchored().CheckReplay(&events.Store{}, genesis); err != nil {
		t.Fatalf("valid unanchored genesis rejected: %v", err)
	}
}

func TestUnanchoredGenesisStructureTableFRRHZ148(t *testing.T) {
	if err := NewUnanchored().CheckReplay(&events.Store{}, genesisEvent148(t, nil)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]map[string]any{
		"format":      {"Format": "other"},
		"principal":   {"Principal": "X"},
		"assurance":   {"Assurance": "presence"},
		"algorithm":   {"Algorithm": "rsa"},
		"public key":  {"PublicKey": "AA=="},
		"key id":      {"KeyID": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"journal id":  {"JournalID": "not-hex"},
		"unknown key": {"extra": true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := NewUnanchored().CheckReplay(&events.Store{}, genesisEvent148(t, change)); err == nil {
				t.Fatal("invalid unanchored genesis accepted")
			}
		})
	}
}
