package workspace

// RHZ-122 / FR-RHZ-169: relation.create direct HTTP intent tests pin the
// deterministic identity, kernel-owned event wire, validation reasons, and
// zero-write refusal/idempotency behavior.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
)

func relationIntentFixture(t *testing.T) (*events.Store, http.Handler) {
	t.Helper()
	s := &events.Store{}
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: "mem-1", Kind: memory.Fact, Content: "evidence", SourceType: "note", SourceID: "mem-1", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []knowledge.KnowledgeItem{
		{ID: "know-a", Kind: knowledge.Claim, Statement: "a", SourceMemoryID: "mem-1", Confidence: .8},
		{ID: "know-b", Kind: knowledge.Claim, Statement: "b", SourceMemoryID: "mem-1", Confidence: .8},
	} {
		if _, err := (knowledge.Service{Store: s}).Create(item); err != nil {
			t.Fatal(err)
		}
	}
	return s, NewHTTP(s).Handler()
}

func postRelationIntent(t *testing.T, handler http.Handler, body map[string]any) RelayResult {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/intent", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result RelayResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func relationID(from, typ, to string) string {
	sum := sha256.Sum256([]byte(from + "\x00" + typ + "\x00" + to))
	return "rel-" + hex.EncodeToString(sum[:])[:12]
}

func validRelationIntent() map[string]any {
	return map[string]any{
		"kind": "relation.create", "from": "know-a", "to": "know-b",
		"relationType": "supports", "sourceMemoryIds": []string{"mem-1"},
		"confidence": .7, "actor": "operator",
	}
}

func TestRelationCreateHTTPEventWireAndIdempotencyFRRHZ169(t *testing.T) {
	s, handler := relationIntentFixture(t)
	body := validRelationIntent()
	before := len(s.All())
	if result := postRelationIntent(t, handler, body); !result.Accepted || result.Reason != "" {
		t.Fatalf("create=%+v", result)
	}
	rid := relationID("know-a", "supports", "know-b")
	if rid != "rel-1e8154ede562" {
		t.Fatalf("deterministic id=%q", rid)
	}
	log := s.List("relation", rid)
	wantPayload := `{"relation_id":"` + rid + `","from":"know-a","type":"supports","to":"know-b","source_memory_ids":["mem-1"],"confidence":0.7}`
	if len(s.All()) != before+1 || len(log) != 1 || log[0].Type != "relation.created" || string(log[0].Payload) != wantPayload {
		t.Fatalf("events=%d log=%+v payload=%s", len(s.All())-before, log, log[0].Payload)
	}
	afterCreate := len(s.All())
	if result := postRelationIntent(t, handler, body); !result.Accepted || len(s.All()) != afterCreate {
		t.Fatalf("repeat=%+v writes=%d", result, len(s.All())-afterCreate)
	}
	body["confidence"] = .8
	if result := postRelationIntent(t, handler, body); result.Accepted || result.Reason != "relation conflicts with existing relation" || len(s.All()) != afterCreate {
		t.Fatalf("conflict=%+v writes=%d", result, len(s.All())-afterCreate)
	}
}

func TestRelationCreateHTTPDefaultConfidenceFRRHZ169(t *testing.T) {
	s, handler := relationIntentFixture(t)
	body := validRelationIntent()
	delete(body, "confidence")
	if result := postRelationIntent(t, handler, body); !result.Accepted {
		t.Fatal(result)
	}
	log := s.List("relation", relationID("know-a", "supports", "know-b"))
	if len(log) != 1 || !bytes.Contains(log[0].Payload, []byte(`"confidence":0.5`)) {
		t.Fatalf("payload=%s", log[0].Payload)
	}
}

func TestRelationCreateHTTPRelayValidationReasonsFRRHZ169(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		reason string
	}{
		{"actor", func(v map[string]any) { v["actor"] = " " }, "actor required"},
		{"from", func(v map[string]any) { v["from"] = " " }, "from required"},
		{"to", func(v map[string]any) { v["to"] = "" }, "to required"},
		{"type", func(v map[string]any) { v["relationType"] = " " }, "relationType required"},
		{"sources", func(v map[string]any) { v["sourceMemoryIds"] = []string{} }, "sourceMemoryIds required"},
		{"blank source", func(v map[string]any) { v["sourceMemoryIds"] = []string{" "} }, "sourceMemoryIds contains blank id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, handler := relationIntentFixture(t)
			body := validRelationIntent()
			test.mutate(body)
			before := len(s.All())
			result := postRelationIntent(t, handler, body)
			if result.Accepted || result.Reason != test.reason || len(s.All()) != before {
				t.Fatalf("result=%+v writes=%d", result, len(s.All())-before)
			}
		})
	}
}

func TestRelationCreateHTTPKernelValidationReasonsFRRHZ169(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		reason string
	}{
		{"unknown type", func(v map[string]any) { v["relationType"] = "unknown" }, `unknown relation type "unknown"`},
		{"self", func(v map[string]any) { v["to"] = "know-a" }, "relation cannot reference itself"},
		{"missing from", func(v map[string]any) { v["from"] = "know-missing" }, "from knowledge: knowledge event stream is empty"},
		{"missing to", func(v map[string]any) { v["to"] = "know-missing" }, "to knowledge: knowledge event stream is empty"},
		{"missing source", func(v map[string]any) { v["sourceMemoryIds"] = []string{"mem-missing"} }, `source memory "mem-missing": memory event stream is empty`},
		{"confidence", func(v map[string]any) { v["confidence"] = 1.1 }, "confidence must be finite and in [0,1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, handler := relationIntentFixture(t)
			body := validRelationIntent()
			test.mutate(body)
			before := len(s.All())
			result := postRelationIntent(t, handler, body)
			if result.Accepted || result.Reason != test.reason || len(s.All()) != before {
				t.Fatalf("result=%+v writes=%d", result, len(s.All())-before)
			}
		})
	}
}
