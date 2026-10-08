// Package trust provides the cryptographic anchor, event-chain guard, and
// signed-decision derivation for a journal trust domain.
package trust

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
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
	"regexp"
	"rhizome/internal/events"
	"strings"
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
	TrustAggregateType        = "trust"
	TrustAggregateID          = "trust-root"
	TrustGenesisType          = "trust.key.genesis"
	TrustAddedType            = "trust.key.add"
	TrustRevokedType          = "trust.key.revoke"

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

var (
	keyIDPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	journalIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	noncePattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

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

type genesisPayload struct {
	Format, Principal, Algorithm, Assurance, PublicKey, KeyID, JournalID string
}

type signature struct {
	KeyID, SignedAt, Nonce, Sig string
}

type addedPayload struct {
	KeyID, Algorithm, PublicKey, Principal, Assurance string
	Signature                                         signature
}

type revokedPayload struct {
	KeyID, Reason string
	Signature     signature
}

// Summary is the journal trust-domain identity derived from genesis.
type Summary struct {
	JournalID, GenesisKeyID string
}

// DecisionStatus is a verified decision projection. Status is empty when the
// verifier is anchorless or the event has no signature.
type DecisionStatus struct {
	Status, Assurance, KeyID string
	KeyRevokedNow            bool
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

func exactObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if !utf8.Valid(raw) || json.Unmarshal(raw, &object) != nil || object == nil || len(object) != len(keys) {
		return nil, errors.New("invalid trust event")
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return nil, errors.New("invalid trust event")
		}
	}
	return object, nil
}

func parseGenesis(raw []byte) (genesisPayload, error) {
	if _, err := exactObject(raw, "Format", "Principal", "Algorithm", "Assurance", "PublicKey", "KeyID", "JournalID"); err != nil {
		return genesisPayload{}, err
	}
	var p genesisPayload
	if json.Unmarshal(raw, &p) != nil || p.Format != AnchorFormat || p.Principal != "H" || p.Assurance != AssuranceKey || !journalIDPattern.MatchString(p.JournalID) {
		return genesisPayload{}, errors.New("invalid trust genesis")
	}
	key, _, err := parsePublicKey(p.Algorithm, p.PublicKey)
	if err != nil {
		return genesisPayload{}, errors.New("invalid trust genesis")
	}
	derived, err := KeyID(key)
	if err != nil || p.KeyID != derived {
		return genesisPayload{}, errors.New("invalid trust genesis")
	}
	return p, nil
}

func anchorBytes(a Anchor) []byte {
	raw, _ := json.Marshal(a)
	return raw
}

func genesisAnchorBytes(p genesisPayload) []byte {
	return anchorBytes(Anchor{Format: p.Format, Principal: p.Principal, Algorithm: p.Algorithm, Assurance: p.Assurance, PublicKey: p.PublicKey})
}

func parseSignature(raw json.RawMessage) (signature, error) {
	if _, err := exactObject(raw, "KeyID", "SignedAt", "Nonce", "Sig"); err != nil {
		return signature{}, err
	}
	var s signature
	if json.Unmarshal(raw, &s) != nil || !keyIDPattern.MatchString(s.KeyID) || !noncePattern.MatchString(s.Nonce) {
		return signature{}, errors.New("invalid signature envelope")
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", s.SignedAt)
	if err != nil || t.UTC().Format("2006-01-02T15:04:05Z") != s.SignedAt {
		return signature{}, errors.New("invalid signature envelope")
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(s.Sig); err != nil {
		return signature{}, errors.New("invalid signature envelope")
	}
	return s, nil
}

func parseAdded(raw []byte) (addedPayload, []byte, error) {
	object, err := exactObject(raw, "KeyID", "Algorithm", "PublicKey", "Principal", "Assurance", "Signature")
	if err != nil {
		return addedPayload{}, nil, err
	}
	var p addedPayload
	if json.Unmarshal(raw, &p) != nil || p.Principal != "H" || p.Assurance != AssuranceKey || !keyIDPattern.MatchString(p.KeyID) {
		return addedPayload{}, nil, errors.New("invalid trust key add")
	}
	p.Signature, err = parseSignature(object["Signature"])
	if err != nil {
		return addedPayload{}, nil, err
	}
	key, der, err := parsePublicKey(p.Algorithm, p.PublicKey)
	if err != nil {
		return addedPayload{}, nil, errors.New("invalid trust key add")
	}
	derived, err := KeyID(key)
	if err != nil || derived != p.KeyID {
		return addedPayload{}, nil, errors.New("invalid trust key add")
	}
	return p, der, nil
}

func parseRevoked(raw []byte) (revokedPayload, error) {
	object, err := exactObject(raw, "KeyID", "Reason", "Signature")
	if err != nil {
		return revokedPayload{}, err
	}
	var p revokedPayload
	if json.Unmarshal(raw, &p) != nil || !keyIDPattern.MatchString(p.KeyID) || len(strings.TrimSpace(p.Reason)) == 0 {
		return revokedPayload{}, errors.New("invalid trust key revoke")
	}
	p.Signature, err = parseSignature(object["Signature"])
	if err != nil {
		return revokedPayload{}, err
	}
	return p, nil
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

type keyRecord struct {
	algorithm string
	publicKey any
	revoked   bool
}

type chainState struct {
	journalID string
	genesisID string
	keys      map[string]*keyRecord
}

func fresh(s signature, now time.Time) bool {
	t, err := time.Parse("2006-01-02T15:04:05Z", s.SignedAt)
	return err == nil && !t.Before(now.Add(-15*time.Minute)) && !t.After(now.Add(2*time.Minute))
}

func verifyWith(key *keyRecord, message []byte, envelope signature) error {
	if key == nil || key.revoked {
		return errors.New("signing key is not active")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(envelope.Sig)
	if err != nil {
		return errors.New("invalid signature envelope")
	}
	return VerifySignature(key.algorithm, key.publicKey, message, raw)
}

func (v *Verifier) applyTrust(state *chainState, next events.Event, appendNow *time.Time) error {
	if next.AggregateType != TrustAggregateType || next.AggregateID != TrustAggregateID {
		return errors.New("invalid trust aggregate")
	}
	switch next.Type {
	case TrustGenesisType:
		if next.Revision != 1 || state.journalID != "" || len(state.keys) != 0 {
			return errors.New("invalid trust genesis position")
		}
		p, err := parseGenesis(next.Payload)
		if err != nil {
			return err
		}
		if v.mode == modeAnchored && !bytes.Equal(genesisAnchorBytes(p), anchorBytes(v.anchor)) {
			return errors.New("trust anchor does not match genesis")
		}
		key, _, err := parsePublicKey(p.Algorithm, p.PublicKey)
		if err != nil {
			return errors.New("invalid trust genesis")
		}
		state.journalID, state.genesisID = p.JournalID, p.KeyID
		state.keys[p.KeyID] = &keyRecord{algorithm: p.Algorithm, publicKey: key}
		return nil
	case TrustAddedType:
		if state.journalID == "" {
			return errors.New("trust key add before genesis")
		}
		p, der, err := parseAdded(next.Payload)
		if err != nil {
			return err
		}
		if appendNow != nil && !fresh(p.Signature, *appendNow) {
			return errors.New("signature outside freshness window")
		}
		if p.KeyID == p.Signature.KeyID {
			return errors.New("trust key cannot add itself")
		}
		if _, exists := state.keys[p.KeyID]; exists {
			return errors.New("trust key already registered")
		}
		if err := verifyWith(state.keys[p.Signature.KeyID], AddMessage(state.journalID, p.KeyID, p.Algorithm, der, p.Principal, p.Assurance, p.Signature.KeyID, p.Signature.SignedAt, p.Signature.Nonce), p.Signature); err != nil {
			return err
		}
		key, _, _ := parsePublicKey(p.Algorithm, p.PublicKey)
		state.keys[p.KeyID] = &keyRecord{algorithm: p.Algorithm, publicKey: key}
		return nil
	case TrustRevokedType:
		if state.journalID == "" {
			return errors.New("trust key revoke before genesis")
		}
		p, err := parseRevoked(next.Payload)
		if err != nil {
			return err
		}
		if appendNow != nil && !fresh(p.Signature, *appendNow) {
			return errors.New("signature outside freshness window")
		}
		target := state.keys[p.KeyID]
		if target == nil || target.revoked {
			return errors.New("trust key is not active")
		}
		if err := verifyWith(state.keys[p.Signature.KeyID], RevokeMessage(state.journalID, p.KeyID, p.Reason, p.Signature.KeyID, p.Signature.SignedAt, p.Signature.Nonce), p.Signature); err != nil {
			return err
		}
		target.revoked = true
		return nil
	default:
		return errors.New("unknown trust event")
	}
}

func (v *Verifier) stateFrom(eventsIn []events.Event, beforeSequence uint64) (*chainState, error) {
	state := &chainState{keys: map[string]*keyRecord{}}
	var revision uint64
	for _, event := range eventsIn {
		if beforeSequence != 0 && event.Sequence >= beforeSequence {
			continue
		}
		if event.AggregateType != TrustAggregateType || event.AggregateID != TrustAggregateID {
			continue
		}
		if event.Revision != revision+1 {
			return nil, events.ErrRevisionConflict
		}
		if err := v.applyTrust(state, event, nil); err != nil {
			return nil, err
		}
		revision = event.Revision
	}
	return state, nil
}

func signedDecision(next events.Event) (signature, string, string, string, string, bool, error) {
	if next.Type != QuestionAnsweredType && next.Type != ApprovalInputRecordedType {
		return signature{}, "", "", "", "", false, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(next.Payload, &object) != nil || object == nil {
		return signature{}, "", "", "", "", false, errors.New("invalid decision payload")
	}
	rawVerification, ok := object[verificationPayloadKey]
	if !ok {
		return signature{}, "", "", "", "", false, nil
	}
	verification, err := exactObject(rawVerification, signaturePayloadKey)
	if err != nil {
		// A claim is owned by question/approval replay, not by the cryptographic guard.
		var claim map[string]json.RawMessage
		if json.Unmarshal(rawVerification, &claim) == nil {
			if _, signed := claim[signaturePayloadKey]; !signed {
				return signature{}, "", "", "", "", false, nil
			}
		}
		return signature{}, "", "", "", "", false, errors.New("invalid signed verification")
	}
	sig, err := parseSignature(verification[signaturePayloadKey])
	if err != nil {
		return signature{}, "", "", "", "", false, err
	}
	var decision, reason, digest, consumer string
	if next.Type == QuestionAnsweredType {
		consumer = "question"
		if json.Unmarshal(object[decisionPayloadKey], &decision) != nil || json.Unmarshal(object[reasonPayloadKey], &reason) != nil || json.Unmarshal(object[digestPayloadKey], &digest) != nil {
			return signature{}, "", "", "", "", false, errors.New("invalid signed decision")
		}
	} else {
		consumer = "approval"
		var asserted bool
		if raw, exists := object[actorVerifiedPayloadKey]; exists && json.Unmarshal(raw, &asserted) == nil && asserted {
			return signature{}, "", "", "", "", false, errors.New("signature with legacy assertion")
		}
		if json.Unmarshal(object["decision"], &decision) != nil || json.Unmarshal(object[reasonPayloadKey], &reason) != nil || json.Unmarshal(object[requestDigestPayloadKey], &digest) != nil {
			return signature{}, "", "", "", "", false, errors.New("invalid signed decision")
		}
	}
	return sig, consumer, decision, reason, digest, true, nil
}

func (v *Verifier) verifyDecision(prefix events.View, next events.Event, appendNow *time.Time) (signature, bool, error) {
	sig, consumer, decision, reason, digest, signed, err := signedDecision(next)
	if err != nil || !signed {
		return sig, signed, err
	}
	if v.mode == modeAnchorless {
		if appendNow != nil {
			return sig, true, errors.New("signed decisions require a trust anchor")
		}
		return sig, true, nil
	}
	if appendNow != nil && !fresh(sig, *appendNow) {
		return sig, true, errors.New("signature outside freshness window")
	}
	state, err := v.stateFrom(prefix.List(TrustAggregateType, TrustAggregateID), next.Sequence)
	if err != nil || state.journalID == "" {
		if err == nil {
			err = errors.New("trust genesis missing")
		}
		return sig, true, err
	}
	message := DecisionMessage(state.journalID, consumer, next.AggregateID, digest, decision, reason, sig.KeyID, sig.SignedAt, sig.Nonce)
	if err := verifyWith(state.keys[sig.KeyID], message, sig); err != nil {
		return sig, true, err
	}
	return sig, true, nil
}

func (v *Verifier) CheckAppend(prefix events.View, next events.Event, now time.Time) error {
	if v == nil {
		return errors.New("nil trust verifier")
	}
	if next.AggregateType == TrustAggregateType || strings.HasPrefix(next.Type, "trust.key.") {
		if v.mode == modeAnchorless {
			return errors.New("trust events require a trust anchor")
		}
		state, err := v.stateFrom(prefix.List(TrustAggregateType, TrustAggregateID), 0)
		if err != nil {
			return err
		}
		return v.applyTrust(state, next, &now)
	}
	_, _, err := v.verifyDecision(prefix, next, &now)
	return err
}

func (v *Verifier) CheckReplay(prefix events.View, next events.Event) error {
	if v == nil {
		return errors.New("nil trust verifier")
	}
	if v.mode == modeAnchorless {
		return nil
	}
	if next.AggregateType == TrustAggregateType || strings.HasPrefix(next.Type, "trust.key.") {
		state, err := v.stateFrom(prefix.List(TrustAggregateType, TrustAggregateID), 0)
		if err != nil {
			return err
		}
		return v.applyTrust(state, next, nil)
	}
	_, _, err := v.verifyDecision(prefix, next, nil)
	return err
}

// TrustSummary derives the journal identity from its genesis event.
func (v *Verifier) TrustSummary(prefix events.View) (*Summary, error) {
	if v == nil {
		return nil, errors.New("nil trust verifier")
	}
	log := prefix.List(TrustAggregateType, TrustAggregateID)
	if len(log) == 0 {
		return nil, nil
	}
	if v.mode == modeAnchorless {
		genesis, err := parseGenesis(log[0].Payload)
		if err != nil || log[0].Type != TrustGenesisType || log[0].Revision != 1 {
			if err == nil {
				err = errors.New("invalid trust genesis")
			}
			return nil, err
		}
		return &Summary{JournalID: genesis.JournalID, GenesisKeyID: genesis.KeyID}, nil
	}
	state, err := v.stateFrom(log, 0)
	if err != nil {
		return nil, err
	}
	return &Summary{JournalID: state.journalID, GenesisKeyID: state.genesisID}, nil
}

// VerifyDecision derives the signed status of one stored decision and whether
// its key has since been revoked. It never reapplies the freshness window.
func (v *Verifier) VerifyDecision(prefix events.View, event events.Event) (*DecisionStatus, error) {
	if v == nil {
		return nil, errors.New("nil trust verifier")
	}
	if v.mode == modeAnchorless {
		return nil, nil
	}
	sig, signed, err := v.verifyDecision(prefix, event, nil)
	if err != nil || !signed {
		return nil, err
	}
	state, err := v.stateFrom(prefix.List(TrustAggregateType, TrustAggregateID), 0)
	if err != nil {
		return nil, err
	}
	key := state.keys[sig.KeyID]
	return &DecisionStatus{Status: v.statusCap, Assurance: AssuranceKey, KeyID: sig.KeyID, KeyRevokedNow: key != nil && key.revoked}, nil
}

// EnsureGenesis appends the single trust root for an empty trust aggregate.
// An existing genesis is validated against anchor and left byte-for-byte alone.
func EnsureGenesis(store events.Port, anchor Anchor) error {
	if store == nil {
		return errors.New("nil store")
	}
	verifier := NewAnchored(anchor)
	log := store.List(TrustAggregateType, TrustAggregateID)
	if len(log) != 0 {
		_, err := verifier.stateFrom(log, 0)
		return err
	}
	key, _, err := parsePublicKey(anchor.Algorithm, anchor.PublicKey)
	if err != nil {
		return err
	}
	keyID, err := KeyID(key)
	if err != nil {
		return err
	}
	var domain [16]byte
	if _, err := rand.Read(domain[:]); err != nil {
		return err
	}
	payload, _ := json.Marshal(genesisPayload{Format: anchor.Format, Principal: anchor.Principal, Algorithm: anchor.Algorithm, Assurance: anchor.Assurance, PublicKey: anchor.PublicKey, KeyID: keyID, JournalID: hex.EncodeToString(domain[:])})
	return store.Append(0, events.Event{AggregateType: TrustAggregateType, AggregateID: TrustAggregateID, Revision: 1, Type: TrustGenesisType, Payload: payload, CreatedAt: time.Now().UTC()})
}

// RecordKeyChange appends one strictly guarded add or revoke event to the
// single trust aggregate. The verifier remains the authority for its payload.
func RecordKeyChange(store events.Port, eventType string, payload json.RawMessage) error {
	if store == nil {
		return errors.New("nil store")
	}
	if eventType != TrustAddedType && eventType != TrustRevokedType {
		return errors.New("invalid trust key event")
	}
	log := store.List(TrustAggregateType, TrustAggregateID)
	revision := uint64(len(log))
	return store.Append(revision, events.Event{AggregateType: TrustAggregateType, AggregateID: TrustAggregateID, Revision: revision + 1, Type: eventType, Payload: payload, CreatedAt: time.Now().UTC()})
}
