package source

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/memory"
)

func TestRegisterStoresReferenceWithoutContentFRRHZ046(t *testing.T) {
	s := &events.Store{}
	content := []byte(strings.Repeat("x", 1024*1024))
	r, err := (Service{Store: s}).Register(content, "text/plain", "note://large")
	if err != nil {
		t.Fatal(err)
	}
	eventsForSource := s.List("source", r.BlobID)
	if len(eventsForSource) != 1 || len(eventsForSource[0].Payload) >= len(content) {
		t.Fatalf("payload retained content: payload=%d content=%d", len(eventsForSource[0].Payload), len(content))
	}
	var fields map[string]any
	if err := json.Unmarshal(eventsForSource[0].Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["content"]; ok {
		t.Fatal("payload contains content field")
	}
	if _, ok := fields["bytes"]; ok {
		t.Fatal("payload contains bytes field")
	}
}

func TestContentAddressingAndIdempotentRegistrationFRRHZ047(t *testing.T) {
	s := &events.Store{}
	service := Service{Store: s}
	one, err := service.Register([]byte("same"), "text/plain", "note://one")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.All())
	repeated, err := service.Register([]byte("same"), "application/octet-stream", "note://two")
	if err != nil {
		t.Fatal(err)
	}
	if repeated.BlobID != one.BlobID || len(s.All()) != before {
		t.Fatalf("same content was not idempotent: repeated=%#v", repeated)
	}
	other, err := service.Register([]byte("different"), "text/plain", "note://three")
	if err != nil || other.BlobID == one.BlobID {
		t.Fatalf("different content identity invalid: other=%#v err=%v", other, err)
	}
}

func TestReplayRejectsHashMediaSizeAndEnvelopeErrorsFRRHZ046(t *testing.T) {
	validPayload := func(hash, id, media string, size int64) []byte {
		return []byte(`{"blob_id":"` + id + `","content_hash":"` + hash + `","media_type":"` + media + `","size_bytes":` + strconv.FormatInt(size, 10) + `,"source_uri":"note://x"}`)
	}
	goodHash := strings.Repeat("a", 64)
	cases := []struct {
		name  string
		id    string
		hash  string
		media string
		size  int64
	}{
		{"bad hash", "sha256:short", "short", "text/plain", 1},
		{"id mismatch", "sha256:" + strings.Repeat("b", 64), goodHash, "text/plain", 1},
		{"missing media", "sha256:" + goodHash, goodHash, "", 1},
		{"negative size", "sha256:" + goodHash, goodHash, "text/plain", -1},
	}
	for _, tc := range cases {
		log := []events.Event{{AggregateType: "source", AggregateID: tc.id, Revision: 1, Type: "source.registered", Payload: validPayload(tc.hash, tc.id, tc.media, tc.size)}}
		if _, err := Replay(log); err == nil {
			t.Fatalf("%s: expected replay error", tc.name)
		}
	}
	malformedSize := []events.Event{{AggregateType: "source", AggregateID: "sha256:" + goodHash, Revision: 1, Type: "source.registered", Payload: []byte(`{"blob_id":"sha256:` + goodHash + `","content_hash":"` + goodHash + `","media_type":"text/plain","size_bytes":"1","source_uri":"note://x"}`)}}
	if _, err := Replay(malformedSize); err == nil {
		t.Fatal("expected malformed size error")
	}
	if _, err := Replay([]events.Event{{AggregateType: "source", AggregateID: "sha256:" + goodHash, Revision: 2, Type: "source.registered", Payload: validPayload(goodHash, "sha256:"+goodHash, "text/plain", 1)}}); err == nil {
		t.Fatal("expected revision error")
	}
}

func TestMemoryCanReferenceBlobWithoutSchemaChange(t *testing.T) {
	s := &events.Store{}
	r, err := (Service{Store: s}).Register([]byte("document"), "text/plain", "note://doc")
	if err != nil {
		t.Fatal(err)
	}
	m, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-blob", Kind: memory.Reference, Content: "document reference", SourceType: "blob", SourceID: r.BlobID, Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceType != "blob" || m.SourceID != r.BlobID {
		t.Fatalf("memory blob reference = %#v", m)
	}
}

func TestListGetAndCorruption(t *testing.T) {
	s := &events.Store{}
	service := Service{Store: s}
	first, err := service.Register([]byte("a"), "text/plain", "note://a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Register([]byte("b"), "text/plain", "note://b")
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List()
	if err != nil || len(list) != 2 || list[0].BlobID >= list[1].BlobID {
		t.Fatalf("list = %#v, err=%v", list, err)
	}
	if _, err := service.Get(first.BlobID); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(0, events.Event{AggregateType: "source", AggregateID: "bad", Revision: 1, Type: "source.registered", Payload: []byte(`{"blob_id":"sha256:bad","content_hash":"bad","media_type":"text/plain","size_bytes":1,"source_uri":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(); err == nil {
		t.Fatal("expected corrupt source error")
	}
	_ = second
}

func TestEmptyInputAndNilStore(t *testing.T) {
	s := &events.Store{}
	if _, err := (Service{Store: s}).Register(nil, "text/plain", "x"); err == nil {
		t.Fatal("expected empty content error")
	}
	var service Service
	if _, err := service.Register([]byte("x"), "text/plain", "x"); err == nil {
		t.Fatal("expected nil register error")
	}
	if _, err := service.Get("id"); err == nil {
		t.Fatal("expected nil get error")
	}
	if _, err := service.List(); err == nil {
		t.Fatal("expected nil list error")
	}
}
