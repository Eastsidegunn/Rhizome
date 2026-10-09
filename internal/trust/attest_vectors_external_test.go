package trust_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"rhizome/internal/trust"
	"strconv"
	"testing"
)

const (
	attestVectorKeyID    = "sha256:06e3fd8fda29bb60ab59557de61edb0aecdb231134be30e75b455f8e1b792fa9"
	attestN1CanonicalSHA = "fc1357ed3d9d7505ba38f6f5a96e797798a1f1765c4be71d1ab4808367ceeef8"
	attestN1MessageSHA   = "5b3a2e92061f1192913e6ffc5501876e35da300b425c472cc1f9e2e66dbfb1f3"
	attestN1Signature    = "bi5STH4NZsXIiX4bG7cueEO2uqWox47LfVs7NfOCVQQ8/8Ybv/b+rXuEtfFIrwYIoUOTU2D49YPHLjgvSxIGAA=="
	attestN3CanonicalSHA = "fa7e41333d324cf8e089e455e41af5bbf5257131ae8d098e7d65c0c77d934217"
	attestN3MessageSHA   = "7927f04de51c0208b4a689aa1b9b9ff5c4948a2175c551f1cc70de05917c2bad"
	attestN3Signature    = "0nuE1Ag5ieSgdGI3nSqC/1KlKneBZz8I8/Z9RPKA0V62vI0FLQyE4FYbuAxqwBaIGijfRcV3oPRSxQrj8MC+CQ=="
	attestP256Public     = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaxfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li/hp/m47n60p8D54WK84zV2sxXs7LtkBoN79R9Q=="
	attestP256Signature  = "MEUCIQCDvLGgBTvipIpWARc8EA0ETbHOtTQxs+ihQ62KEdQfeQIgaB3qZwELfCVnhHMoob64tOdJXYWvy6BQawMELgzyN8c="
)

func vectorItems164() ([]trust.AttestItem, []trust.AttestItem) {
	n1 := []trust.AttestItem{{Consumer: "question", GateID: "q-92caa77d4e8cb1f9a861ff35", DecisionSequence: 7, Digest: "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61", Decision: "approve", Reason: ""}}
	n3 := append(append([]trust.AttestItem(nil), n1...),
		trust.AttestItem{Consumer: "approval", GateID: "appr-111111111111111111111111", DecisionSequence: 11, Digest: "hx-args-digest-v1:vector-approval", Decision: "allow", Reason: "approved by operator"},
		trust.AttestItem{Consumer: "question", GateID: "q-222222222222222222222222", DecisionSequence: 19, Digest: "rhz-question-v2:2222222222222222222222222222222222222222222222222222222222222222", Decision: "reject", Reason: "needs revision"},
	)
	return n1, n3
}

func TestAttestManifestVectorsFRRHZ164(t *testing.T) {
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	n1, n3 := vectorItems164()
	for _, tc := range []struct {
		name, canonicalSHA, messageSHA, signature, nonce string
		items                                            []trust.AttestItem
	}{
		{"n1", attestN1CanonicalSHA, attestN1MessageSHA, attestN1Signature, "101112131415161718191a1b1c1d1e1f", n1},
		{"n3", attestN3CanonicalSHA, attestN3MessageSHA, attestN3Signature, "202122232425262728292a2b2c2d2e2f", n3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical := sha256.Sum256(trust.AttestManifestBytes(tc.items))
			if got := hex.EncodeToString(canonical[:]); got != tc.canonicalSHA {
				t.Fatalf("canonical sha = %s", got)
			}
			message := trust.AttestMessage("00112233445566778899aabbccddeeff", trust.ManifestDigest(tc.items), strconv.Itoa(len(tc.items)), attestVectorKeyID, "2026-10-09T00:00:00Z", tc.nonce)
			messageHash := sha256.Sum256(message)
			if got := hex.EncodeToString(messageHash[:]); got != tc.messageSHA {
				t.Fatalf("message sha = %s", got)
			}
			sig, _ := base64.StdEncoding.DecodeString(tc.signature)
			if err := trust.VerifySignature(trust.AlgorithmEd25519, public, message, sig); err != nil {
				t.Fatal(err)
			}
		})
	}
	der, _ := base64.StdEncoding.DecodeString(attestP256Public)
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	publicP256 := parsed.(*ecdsa.PublicKey)
	message := trust.AttestMessage("00112233445566778899aabbccddeeff", trust.ManifestDigest(n1), "1", attestVectorKeyID, "2026-10-09T00:00:00Z", "101112131415161718191a1b1c1d1e1f")
	sig, _ := base64.StdEncoding.DecodeString(attestP256Signature)
	if err := trust.VerifySignature(trust.AlgorithmECDSAP256, publicP256, message, sig); err != nil {
		t.Fatal(err)
	}
}
