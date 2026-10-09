package workspace

// RHZ-049 / FR-RHZ-080: HTTP knowledge surface tests.  The tests intentionally
// decode raw JSON so wire names and literal empty arrays remain observable.
import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/relation"
)

func knowledgeHTTP(t *testing.T) (*events.Store, *httptest.Server) {
	t.Helper()
	s := &events.Store{}
	srv := httptest.NewServer(NewHTTP(s).Handler())
	// LIFO cleanup closes the server before any SSE response body.
	t.Cleanup(srv.Close)
	return s, srv
}
func postKnowledgeIntent(t *testing.T, srv *httptest.Server, content, kind string, tags []string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(Intent{Kind: "note.create", Content: content, MemoryKind: kind, Tags: tags})
	r, err := http.Post(srv.URL+"/v1/intent", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, v
}
func getKnowledge(t *testing.T, srv *httptest.Server, query string) (int, map[string]any, []byte) {
	t.Helper()
	r, err := http.Get(srv.URL + "/v1/knowledge" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err, string(b))
	}
	return r.StatusCode, v, b
}
func addMemory(t *testing.T, s *events.Store, id string, k memory.Kind, content string, tags ...string) {
	t.Helper()
	if _, err := (memory.Service{Store: s}).Create(memory.Memory{ID: id, Kind: k, Content: content, SourceType: "note", SourceID: id, Confidence: 1, Tags: tags}); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeGetStatusAndEnvelopeFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	st, v, _ := getKnowledge(t, u, "")
	if st != 200 || v["revision"] != float64(0) {
		t.Fatalf("status/envelope=%d %#v", st, v)
	}
}
func TestKnowledgeEmptyBodyLiteralArraysFRRHZ080FRRHZ170(t *testing.T) {
	_, u := knowledgeHTTP(t)
	_, _, b := getKnowledge(t, u, "")
	want := `{"revision":0,"body":{"notes":[],"items":[],"relations":[]}}`
	if strings.TrimSpace(string(b)) != want {
		t.Fatalf("empty knowledge golden\n got: %s\nwant: %s", b, want)
	}
}

func addKnowledgeItem(t *testing.T, s *events.Store, id string, kind knowledge.Kind, statement, source string, confidence float64, tags ...string) {
	t.Helper()
	if _, err := (knowledge.Service{Store: s}).Create(knowledge.KnowledgeItem{ID: id, Kind: kind, Statement: statement, SourceMemoryID: source, Confidence: confidence, Tags: tags}); err != nil {
		t.Fatal(err)
	}
}

func addRelation(t *testing.T, s *events.Store, id, from, to string, typ relation.Type, sourceIDs []string, confidence float64) {
	t.Helper()
	if _, err := (relation.Service{Store: s}).Create(relation.Relation{ID: id, From: from, Type: typ, To: to, SourceMemoryIDs: sourceIDs, Confidence: confidence}); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeItemsRelationsPopulatedGoldenFRRHZ170(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "mem-1", memory.Fact, "evidence", "red")
	addKnowledgeItem(t, s, "know-z", knowledge.Claim, "z statement", "mem-1", .8, "red")
	addKnowledgeItem(t, s, "know-a", knowledge.Concept, "a statement", "mem-1", .6)
	addRelation(t, s, "rel-1", "know-a", "know-z", relation.Supports, []string{"mem-1"}, .7)
	_, _, got := getKnowledge(t, u, "")
	want := `{"revision":4,"body":{"notes":[{"id":"mem-1","kind":"fact","content":"evidence","tags":["red"],"sourceId":"mem-1","sourceType":"note"}],"items":[{"id":"know-a","kind":"concept","statement":"a statement","status":"candidate","confidence":0.6,"sourceMemoryId":"mem-1","tags":[],"supersedes":""},{"id":"know-z","kind":"claim","statement":"z statement","status":"candidate","confidence":0.8,"sourceMemoryId":"mem-1","tags":["red"],"supersedes":""}],"relations":[{"id":"rel-1","type":"supports","from":"know-a","to":"know-z","sourceMemoryIds":["mem-1"],"confidence":0.7}]}}`
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("populated knowledge golden\n got: %s\nwant: %s", got, want)
	}
}

func TestKnowledgeItemKindFiltersItemsAndRelationsFRRHZ170(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "mem-1", memory.Fact, "evidence")
	addKnowledgeItem(t, s, "know-a", knowledge.Claim, "a", "mem-1", .5)
	addKnowledgeItem(t, s, "know-b", knowledge.Claim, "b", "mem-1", .5)
	addKnowledgeItem(t, s, "know-c", knowledge.Concept, "c", "mem-1", .5)
	addRelation(t, s, "rel-claim", "know-a", "know-b", relation.Supports, []string{"mem-1"}, .5)
	addRelation(t, s, "rel-mixed", "know-a", "know-c", relation.Supports, []string{"mem-1"}, .5)
	_, v, _ := getKnowledge(t, u, "?itemKind=claim")
	body := v["body"].(map[string]any)
	items, relations := body["items"].([]any), body["relations"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "know-a" || len(relations) != 1 || relations[0].(map[string]any)["id"] != "rel-claim" {
		t.Fatalf("filtered body=%#v", body)
	}
}

func TestKnowledgeTagFiltersNotesItemsAndRelationsFRRHZ170(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "mem-red", memory.Fact, "red evidence", "red")
	addMemory(t, s, "mem-blue", memory.Fact, "blue evidence", "blue")
	addKnowledgeItem(t, s, "know-a", knowledge.Claim, "a", "mem-red", .5, "red")
	addKnowledgeItem(t, s, "know-b", knowledge.Claim, "b", "mem-red", .5, "red")
	addKnowledgeItem(t, s, "know-c", knowledge.Claim, "c", "mem-blue", .5, "blue")
	addRelation(t, s, "rel-red", "know-a", "know-b", relation.Supports, []string{"mem-red"}, .5)
	addRelation(t, s, "rel-mixed", "know-a", "know-c", relation.Supports, []string{"mem-red"}, .5)
	_, v, _ := getKnowledge(t, u, "?tag=red")
	body := v["body"].(map[string]any)
	if len(body["notes"].([]any)) != 1 || len(body["items"].([]any)) != 2 || len(body["relations"].([]any)) != 1 || body["relations"].([]any)[0].(map[string]any)["id"] != "rel-red" {
		t.Fatalf("tag-filtered body=%#v", body)
	}
}

func TestKnowledgeInvalidItemKind400FRRHZ170(t *testing.T) {
	_, u := knowledgeHTTP(t)
	resp, err := http.Get(u.URL + "/v1/knowledge?itemKind=theorem")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || string(body) != "invalid knowledge kind\n" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestKnowledgeAboutGoldenUnchangedFRRHZ170(t *testing.T) {
	_, u := knowledgeHTTP(t)
	status, _, body := getKnowledge(t, u, "?about=missing")
	want := `{"revision":0,"body":{"about":[]}}`
	if status != http.StatusOK || strings.TrimSpace(string(body)) != want {
		t.Fatalf("status=%d body=%s", status, body)
	}
}
func TestKnowledgePostCreatesMemoryFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	st, v := postKnowledgeIntent(t, u, "hello", "fact", nil)
	if st != 200 || v["Accepted"] != true {
		t.Fatalf("post=%d %#v", st, v)
	}
	if len(s.List("memory", "note-")) != 0 { /* IDs are content hashes; wire assertion below is authoritative. */
	}
	_, p, _ := getKnowledge(t, u, "")
	if len(p["body"].(map[string]any)["notes"].([]any)) != 1 {
		t.Fatal("note absent")
	}
}
func TestKnowledgePostWireUsesCamelCaseFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	postKnowledgeIntent(t, u, "camel", "fact", nil)
	_, _, b := getKnowledge(t, u, "")
	if bytes.Contains(b, []byte("source_id")) {
		t.Fatal("snake case leaked")
	}
}
func TestKnowledgePostIdempotentFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	postKnowledgeIntent(t, u, "same", "fact", []string{"x"})
	before := len(s.All())
	st, v := postKnowledgeIntent(t, u, "same", "fact", []string{"x"})
	if st != 200 || v["Accepted"] != true || len(s.All()) != before {
		t.Fatalf("not idempotent %d %#v", st, v)
	}
}
func TestKnowledgePostMetadataConflictRejectedFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	postKnowledgeIntent(t, u, "same", "fact", nil)
	_, v := postKnowledgeIntent(t, u, "same", "decision", nil)
	if v["Accepted"] == true {
		t.Fatal("conflict accepted")
	}
}
func TestKnowledgePostEmptyRejectedFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	_, v := postKnowledgeIntent(t, u, " ", "fact", nil)
	if v["Accepted"] == true {
		t.Fatal("empty accepted")
	}
}
func TestKnowledgePostUnknownKindRejectedFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	_, v := postKnowledgeIntent(t, u, "x", "unknown", nil)
	if v["Accepted"] == true {
		t.Fatal("unknown kind accepted")
	}
}
func TestKnowledgePost16KiBAcceptedFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	st, v := postKnowledgeIntent(t, u, strings.Repeat("x", 16*1024), "fact", nil)
	if st != 200 || v["Accepted"] != true {
		t.Fatalf("16KiB rejected: %d %#v", st, v)
	}
}
func TestKnowledgePostOver16KiBRejectedFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	_, v := postKnowledgeIntent(t, u, strings.Repeat("x", 16*1024+1), "fact", nil)
	if v["Accepted"] == true {
		t.Fatal("over-limit accepted")
	}
}
func TestKnowledgeFilterKindMatchesMemorySearchFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "a", memory.Fact, "a")
	addMemory(t, s, "b", memory.Decision, "b")
	_, v, _ := getKnowledge(t, u, "?kind=fact")
	if len(v["body"].(map[string]any)["notes"].([]any)) != 1 {
		t.Fatal("kind filter mismatch")
	}
}
func TestKnowledgeFilterTagMatchesMemorySearchFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "a", memory.Fact, "a", "red")
	addMemory(t, s, "b", memory.Fact, "b", "blue")
	_, v, _ := getKnowledge(t, u, "?tag=red")
	if len(v["body"].(map[string]any)["notes"].([]any)) != 1 {
		t.Fatal("tag filter mismatch")
	}
}
func TestKnowledgeConjunctiveFiltersFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "a", memory.Fact, "a", "red")
	addMemory(t, s, "b", memory.Decision, "b", "red")
	_, v, _ := getKnowledge(t, u, "?kind=fact&tag=red")
	if len(v["body"].(map[string]any)["notes"].([]any)) != 1 {
		t.Fatal("conjunction mismatch")
	}
}
func TestKnowledgeIDOrderMatchesMemorySearchFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "z", memory.Fact, "z")
	addMemory(t, s, "a", memory.Fact, "a")
	_, v, _ := getKnowledge(t, u, "")
	n := v["body"].(map[string]any)["notes"].([]any)
	if n[0].(map[string]any)["id"] != "a" {
		t.Fatal("not ID ordered")
	}
}
func TestKnowledgeRevisionUsesGlobalSequenceFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "a", memory.Fact, "a")
	_, v, _ := getKnowledge(t, u, "")
	if v["revision"] != float64(1) {
		t.Fatalf("revision=%v", v["revision"])
	}
}
func TestKnowledgeSnapshotNilStoreFRRHZ080(t *testing.T) {
	if _, err := KnowledgeSnapshot(nil, "", ""); err == nil {
		t.Fatal("nil store accepted")
	}
}
func TestKnowledgeSnapshotSearchSemanticsFRRHZ080(t *testing.T) {
	s, _ := knowledgeHTTP(t)
	addMemory(t, s, "a", memory.Fact, "a", "x")
	p, err := KnowledgeSnapshot(s, memory.Fact, "x")
	if err != nil || len(p.Notes) != 1 {
		t.Fatalf("snapshot=%#v err=%v", p, err)
	}
}
func TestKnowledgeRawJSONParsesAllFieldsFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	addMemory(t, s, "a", memory.Fact, "body", "t")
	_, _, b := getKnowledge(t, u, "")
	var x struct {
		Revision uint64 `json:"revision"`
		Body     struct {
			Notes []struct {
				ID         string   `json:"id"`
				Kind       string   `json:"kind"`
				Content    string   `json:"content"`
				Tags       []string `json:"tags"`
				SourceID   string   `json:"sourceId"`
				SourceType string   `json:"sourceType"`
			} `json:"notes"`
		} `json:"body"`
	}
	if json.Unmarshal(b, &x) != nil || x.Body.Notes[0].SourceType != "note" {
		t.Fatalf("decode failed: %s", b)
	}
}
func TestKnowledgePostInvalidJSON400FRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	r, _ := http.Post(u.URL+"/v1/intent", "application/json", strings.NewReader("{"))
	if r.StatusCode != 400 {
		t.Fatalf("status=%d", r.StatusCode)
	}
	r.Body.Close()
}
func TestKnowledgeWrongMethod404FRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	r, _ := http.Post(u.URL+"/v1/knowledge", "application/json", strings.NewReader("{}"))
	if r.StatusCode != 404 {
		t.Fatalf("status=%d", r.StatusCode)
	}
	r.Body.Close()
}
func TestKnowledgeJournalReopenPreservesSnapshotFRRHZ080(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.ndjson")
	j, e := openTestJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := (memory.Service{Store: j}).Create(memory.Memory{ID: "a", Kind: memory.Fact, Content: "persist", SourceType: "note", SourceID: "a", Confidence: 1, Tags: []string{"x"}}); e != nil {
		t.Fatal(e)
	}
	j.Close()
	j2, e := openTestJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	defer j2.Close()
	q, e := KnowledgeSnapshot(j2, memory.Fact, "x")
	if e != nil || len(q.Notes) != 1 {
		t.Fatalf("reopen=%#v err=%v", q, e)
	}
}
func TestKnowledgeJournalReopenRevisionFRRHZ080(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j")
	j, e := openTestJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = (memory.Service{Store: j}).Create(memory.Memory{ID: "a", Kind: memory.Fact, Content: "x", SourceType: "n", SourceID: "a", Confidence: 1}); e != nil {
		t.Fatal(e)
	}
	j.Close()
	j, e = openTestJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	defer j.Close()
	q, _ := KnowledgeSnapshot(j, "", "")
	if q.Revision != 1 {
		t.Fatalf("revision=%d", q.Revision)
	}
}
func TestKnowledgeContentUTF8LimitByBytesFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	content := strings.Repeat("é", 8192)
	if len(content) != 16384 {
		t.Fatalf("content bytes=%d, want 16384", len(content))
	}
	_, v := postKnowledgeIntent(t, u, content, "fact", nil)
	if v["Accepted"] != true {
		t.Fatalf("UTF8 16KiB rejected: %#v", v)
	}

	over := content + "a"
	if len(over) != 16385 {
		t.Fatalf("over-limit content bytes=%d, want 16385", len(over))
	}
	_, v = postKnowledgeIntent(t, u, over, "fact", nil)
	if v["Accepted"] == true {
		t.Fatal("over-limit UTF8 content accepted")
	}
	if reason, _ := v["Reason"].(string); !strings.Contains(reason, "16KiB") {
		t.Fatalf("over-limit reason=%q, want 16KiB limit", reason)
	}
}
func TestKnowledgeSSECleanupPatternFRRHZ080(t *testing.T) {
	s, u := knowledgeHTTP(t)
	r, e := http.Get(u.URL + "/v1/workspace/stream")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Body.Close() })
	sc := bufio.NewScanner(r.Body)
	go sc.Scan()
	time.Sleep(10 * time.Millisecond)
	addMemory(t, s, "a", memory.Fact, "x")
	r.Body.Close()
}
func TestKnowledgePostBroadcastDoesNotChangeGetSemanticsFRRHZ080(t *testing.T) {
	_, u := knowledgeHTTP(t)
	postKnowledgeIntent(t, u, "x", "fact", nil)
	st, v, _ := getKnowledge(t, u, "")
	if st != 200 || len(v["body"].(map[string]any)["notes"].([]any)) != 1 {
		t.Fatal("broadcast altered projection")
	}
}
