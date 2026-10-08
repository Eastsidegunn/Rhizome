// Package trust provides the cryptographic and anchor substrate for journal
// guards. Trust-chain events and signed decision envelopes are layered on it
// in later slices.
package trust

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"rhizome/internal/events"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	AnchorFormat              = "rhizome-trust-anchor-v1"
	AssuranceKey              = "key"
	AlgorithmEd25519          = "ed25519"
	AlgorithmECDSAP256        = "ecdsa-p256"
	DecisionMessageTag        = "rhz-gate-decision-v1"
	TrustKeyMessageTag        = "rhz-trust-key-v1"
	QuestionAnsweredType      = "question.answered"
	ApprovalInputRecordedType = "approval.input_recorded"

	questionAggregateType   = "question"
	approvalAggregateType   = "approval"
	decisionPayloadKey      = "Decision"
	reasonPayloadKey        = "Reason"
	digestPayloadKey        = "Digest"
	requestDigestPayloadKey = "RequestDigest"
	verificationPayloadKey  = "Verification"
	signaturePayloadKey     = "Signature"
	actorVerifiedPayloadKey = "ActorVerified"
)

var ErrInvalidAnchor = errors.New("invalid trust anchor")

// Authority is an opaque write capability. Its zero value is unverified.
// Non-zero values can only be made inside this package.
type Authority struct{ granted bool }

func newAuthority() Authority { return Authority{granted: true} }

// Anchor is the exact five-field rhizome-trust-anchor-v1 document.
type Anchor struct {
	Format    string `json:"format"`
	Principal string `json:"principal"`
	Algorithm string `json:"algorithm"`
	Assurance string `json:"assurance"`
	PublicKey string `json:"publicKey"`
}

type anchorFileInfo struct {
	regular bool
	uid     uint32
	mode    os.FileMode
	size    int64
}

func validateAnchorFileInfo(info anchorFileInfo, processUID uint32) error {
	if !info.regular {
		return fmt.Errorf("%w: not a regular file", ErrInvalidAnchor)
	}
	if info.uid != processUID {
		return fmt.Errorf("%w: wrong owner", ErrInvalidAnchor)
	}
	if info.mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%w: insecure permissions", ErrInvalidAnchor)
	}
	if info.size > 4096 {
		return fmt.Errorf("%w: file too large", ErrInvalidAnchor)
	}
	return nil
}

// LoadAnchor opens and validates one anchor through a single descriptor.
func LoadAnchor(path string) (Anchor, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Anchor{}, fmt.Errorf("%w: open failed", ErrInvalidAnchor)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Anchor{}, fmt.Errorf("%w: stat failed", ErrInvalidAnchor)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Anchor{}, fmt.Errorf("%w: owner unavailable", ErrInvalidAnchor)
	}
	meta := anchorFileInfo{regular: info.Mode().IsRegular(), uid: stat.Uid, mode: info.Mode(), size: info.Size()}
	if err := validateAnchorFileInfo(meta, uint32(os.Getuid())); err != nil {
		return Anchor{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return Anchor{}, fmt.Errorf("%w: read failed", ErrInvalidAnchor)
	}
	if len(raw) > 4096 {
		return Anchor{}, fmt.Errorf("%w: file too large", ErrInvalidAnchor)
	}
	return ParseAnchor(raw)
}

// ParseAnchor validates the exact anchor JSON object and its PKIX public key.
func ParseAnchor(raw []byte) (Anchor, error) {
	if !utf8.Valid(raw) {
		return Anchor{}, fmt.Errorf("%w: invalid utf-8", ErrInvalidAnchor)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil || len(object) != 5 {
		return Anchor{}, fmt.Errorf("%w: invalid json object", ErrInvalidAnchor)
	}
	for _, key := range []string{"format", "principal", "algorithm", "assurance", "publicKey"} {
		if _, ok := object[key]; !ok {
			return Anchor{}, fmt.Errorf("%w: missing field", ErrInvalidAnchor)
		}
	}
	var anchor Anchor
	if json.Unmarshal(raw, &anchor) != nil || anchor.Format != AnchorFormat || anchor.Principal != "H" || anchor.Assurance != AssuranceKey {
		return Anchor{}, fmt.Errorf("%w: invalid fields", ErrInvalidAnchor)
	}
	if _, _, err := parsePublicKey(anchor.Algorithm, anchor.PublicKey); err != nil {
		return Anchor{}, err
	}
	return anchor, nil
}

func parsePublicKey(algorithm, encoded string) (any, []byte, error) {
	der, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid public key", ErrInvalidAnchor)
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid public key", ErrInvalidAnchor)
	}
	switch algorithm {
	case AlgorithmEd25519:
		if _, ok := key.(ed25519.PublicKey); !ok {
			return nil, nil, fmt.Errorf("%w: algorithm mismatch", ErrInvalidAnchor)
		}
	case AlgorithmECDSAP256:
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return nil, nil, fmt.Errorf("%w: algorithm mismatch", ErrInvalidAnchor)
		}
	default:
		return nil, nil, fmt.Errorf("%w: unknown algorithm", ErrInvalidAnchor)
	}
	return key, der, nil
}

// KeyID derives the stable sha256 identifier of a public key's PKIX DER.
func KeyID(publicKey any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifySignature verifies a raw decision/key message with the registered
// algorithm. ECDSA signatures use ASN.1 DER.
func VerifySignature(algorithm string, publicKey any, message, signature []byte) error {
	switch algorithm {
	case AlgorithmEd25519:
		pub, ok := publicKey.(ed25519.PublicKey)
		if !ok || len(signature) != ed25519.SignatureSize || !ed25519.Verify(pub, message, signature) {
			return errors.New("signature verification failed")
		}
	case AlgorithmECDSAP256:
		pub, ok := publicKey.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return errors.New("signature verification failed")
		}
		digest := sha256.Sum256(message)
		if !ecdsa.VerifyASN1(pub, digest[:], signature) {
			return errors.New("signature verification failed")
		}
	default:
		return errors.New("signature verification failed")
	}
	return nil
}

func lengthPrefixed(fields ...[]byte) []byte {
	var out []byte
	for _, field := range fields {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		out = append(out, length[:]...)
		out = append(out, field...)
	}
	return out
}

// DecisionMessage returns the ten-field signed gate-decision message.
func DecisionMessage(journalID, consumer, gateID, requestDigest, decision, reason, keyID, signedAt, nonce string) []byte {
	values := []string{DecisionMessageTag, journalID, consumer, gateID, requestDigest, decision, reason, keyID, signedAt, nonce}
	fields := make([][]byte, len(values))
	for i := range values {
		fields[i] = []byte(values[i])
	}
	return lengthPrefixed(fields...)
}

// AddMessage returns the signed trust-key add message.
func AddMessage(journalID, keyID, algorithm string, publicKeyDER []byte, principal, assurance, signingKeyID, signedAt, nonce string) []byte {
	return lengthPrefixed([]byte(TrustKeyMessageTag), []byte(journalID), []byte("add"), []byte(keyID), []byte(algorithm), publicKeyDER, []byte(principal), []byte(assurance), []byte(signingKeyID), []byte(signedAt), []byte(nonce))
}

// RevokeMessage returns the signed trust-key revoke message.
func RevokeMessage(journalID, keyID, reason, signingKeyID, signedAt, nonce string) []byte {
	return lengthPrefixed([]byte(TrustKeyMessageTag), []byte(journalID), []byte("revoke"), []byte(keyID), []byte(reason), []byte(signingKeyID), []byte(signedAt), []byte(nonce))
}

type verifierMode uint8

const (
	modeAnchored verifierMode = iota + 1
	modeAnchorless
	modeUnanchored
)

// Verifier is the journal Guard shared by replay and append paths.
type Verifier struct {
	mode      verifierMode
	anchor    Anchor
	statusCap string
}

func NewAnchored(anchor Anchor) *Verifier {
	return &Verifier{mode: modeAnchored, anchor: anchor, statusCap: "verified"}
}
func NewAnchorless() *Verifier { return &Verifier{mode: modeAnchorless} }
func NewUnanchored() *Verifier {
	return &Verifier{mode: modeUnanchored, statusCap: "chain-valid"}
}

func (v *Verifier) CheckAppend(_ events.View, next events.Event, _ time.Time) error {
	if v == nil {
		return errors.New("nil trust verifier")
	}
	// Signed envelopes and trust.key events are intentionally introduced in
	// later slices. Existing unsigned events are unaffected here.
	_ = next
	return nil
}

func (v *Verifier) CheckReplay(_ events.View, _ events.Event) error {
	if v == nil {
		return errors.New("nil trust verifier")
	}
	return nil
}
