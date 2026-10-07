package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rhizome/internal/events"
)

type SourceRef struct {
	BlobID      string
	ContentHash string
	MediaType   string
	SizeBytes   int64
	SourceURI   string
	Revision    uint64
}

type Service struct{ Store events.Port }

// payload deliberately contains references and metadata only. It has no field
// capable of carrying source content (FR-RHZ-046).
type payload struct {
	BlobID      string `json:"blob_id"`
	ContentHash string `json:"content_hash"`
	MediaType   string `json:"media_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SourceURI   string `json:"source_uri"`
}

func blobID(hash string) string { return "sha256:" + hash }

func validHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func validateRef(r SourceRef, aggregateID string) error {
	if r.BlobID == "" || r.BlobID != aggregateID {
		return fmt.Errorf("blob id does not match aggregate")
	}
	if !strings.HasPrefix(r.BlobID, "sha256:") || r.ContentHash != strings.TrimPrefix(r.BlobID, "sha256:") || !validHash(r.ContentHash) {
		return fmt.Errorf("invalid content hash or blob id")
	}
	if strings.TrimSpace(r.MediaType) == "" {
		return fmt.Errorf("media type is required")
	}
	if r.SizeBytes < 0 {
		return fmt.Errorf("size cannot be negative")
	}
	if strings.TrimSpace(r.SourceURI) == "" {
		return fmt.Errorf("source URI is required")
	}
	return nil
}

func (s Service) Register(content []byte, mediaType, sourceURI string) (SourceRef, error) {
	if s.Store == nil {
		return SourceRef{}, fmt.Errorf("nil event store")
	}
	if len(content) == 0 {
		return SourceRef{}, fmt.Errorf("content is empty")
	}
	if strings.TrimSpace(mediaType) == "" {
		return SourceRef{}, fmt.Errorf("media type is required")
	}
	if strings.TrimSpace(sourceURI) == "" {
		return SourceRef{}, fmt.Errorf("source URI is required")
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	id := blobID(hash)
	if existing := s.Store.List("source", id); len(existing) > 0 {
		return Replay(existing)
	}
	// The content is intentionally not retained after this point. Only its
	// digest and size cross the event boundary.
	r := SourceRef{BlobID: id, ContentHash: hash, MediaType: mediaType, SizeBytes: int64(len(content)), SourceURI: sourceURI}
	p := payload{BlobID: r.BlobID, ContentHash: r.ContentHash, MediaType: r.MediaType, SizeBytes: r.SizeBytes, SourceURI: r.SourceURI}
	raw, err := json.Marshal(p)
	if err != nil {
		return SourceRef{}, fmt.Errorf("encode source reference: %w", err)
	}
	e := events.Event{AggregateType: "source", AggregateID: id, Revision: 1, Type: "source.registered", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return SourceRef{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return SourceRef{}, err
	}
	return Replay(s.Store.List("source", id))
}

func Replay(log []events.Event) (SourceRef, error) {
	if len(log) == 0 {
		return SourceRef{}, fmt.Errorf("source event stream is empty")
	}
	if len(log) != 1 {
		return SourceRef{}, fmt.Errorf("duplicate or unknown source event")
	}
	e := log[0]
	if e.AggregateType != "source" || e.AggregateID == "" || e.Revision != 1 || e.Type != "source.registered" {
		return SourceRef{}, fmt.Errorf("invalid source event")
	}
	var p payload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return SourceRef{}, fmt.Errorf("decode source reference: %w", err)
	}
	r := SourceRef{BlobID: p.BlobID, ContentHash: p.ContentHash, MediaType: p.MediaType, SizeBytes: p.SizeBytes, SourceURI: p.SourceURI, Revision: e.Revision}
	if err := validateRef(r, e.AggregateID); err != nil {
		return SourceRef{}, err
	}
	return r, nil
}

func (s Service) Get(blobID string) (SourceRef, error) {
	if s.Store == nil {
		return SourceRef{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(blobID) == "" {
		return SourceRef{}, fmt.Errorf("blob id is required")
	}
	return Replay(s.Store.List("source", blobID))
}

func (s Service) List() ([]SourceRef, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	streams := make(map[string][]events.Event)
	for _, e := range s.Store.All() {
		if e.AggregateType == "source" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]SourceRef, 0, len(ids))
	for _, id := range ids {
		r, err := Replay(streams[id])
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", id, err)
		}
		out = append(out, r)
	}
	return out, nil
}
