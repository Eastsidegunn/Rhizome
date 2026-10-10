package workspace

// RHZ-107 G1 (FR-RHZ-135, SCP §4.3): a journal without secret events keeps
// /v1/workspace, the workspace SSE, /v1/context and non-secret /v1/intent
// responses (and the decoded/recorded payloads) byte-identical. The goldens
// below were captured from the tree before the secret intents existed.

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rhizome/internal/events"
)

// sseFrames reads whole SSE frames ("event: …\ndata: …") from a stream.
type sseFrames struct{ ch chan string }

func openSSEFrames(t *testing.T, url string) *sseFrames {
	t.Helper()
	resp, err := http.Get(url + "/v1/workspace/stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	f := &sseFrames{ch: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var cur []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(cur) > 0 {
					f.ch <- strings.Join(cur, "\n")
				}
				cur = nil
				continue
			}
			cur = append(cur, line)
		}
	}()
	return f
}

func (f *sseFrames) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-f.ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE frame")
		return ""
	}
}

func (f *sseFrames) none(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case s := <-f.ch:
		t.Fatalf("unexpected SSE frame %q", s)
	case <-time.After(within):
	}
}

func postRaw(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/intent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func getRaw(t *testing.T, url, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(url + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// recordedPayloads is the decode evidence: what the relay recorded for the
// non-secret intents (type, aggregate, payload; no clock fields).
func recordedPayloads(s events.Port, from int) string {
	var b bytes.Buffer
	for _, e := range s.All()[from:] {
		b.WriteString(e.AggregateType + "|" + e.AggregateID + "|" + e.Type + "|" + string(e.Payload) + "\n")
	}
	return b.String()
}

// compatTranscriptFRRHZ135 drives the §4.3 surfaces over a secret-free
// journal and returns every response, in order, as one string.
func compatTranscriptFRRHZ135(t *testing.T) string {
	t.Helper()
	s := fixture064(t)
	srv := httptest.NewServer(NewHTTP(s).Handler())
	t.Cleanup(srv.Close)
	var out strings.Builder
	add := func(name string, code int, body string) {
		out.WriteString("== " + name + " " + http.StatusText(code) + "\n" + body + "\n")
	}
	code, body := getRaw(t, srv.URL, "/v1/workspace")
	add("workspace", code, body)
	sse := openSSEFrames(t, srv.URL)
	out.WriteString("== sse snapshot\n" + sse.next(t) + "\n")
	code, body = getRaw(t, srv.URL, "/v1/context?task=mission-x")
	add("context", code, body)
	n := len(s.All())
	for _, c := range []struct{ name, body string }{
		{"goal.create", `{"kind":"goal.create","actor":"op","name":"g2","success":"done"}`},
		{"note.create", `{"kind":"note.create","actor":"op","content":"c","memoryKind":"fact","missionId":"mission-x"}`},
		{"unknown", `{"kind":"nope","actor":"op"}`},
		{"secretlike", `{"kind":"secretx.register.request","actor":"op"}`},
		{"trailing", `{"kind":"nope","actor":"op"} {"x":1}`},
		{"dupkey", `{"kind":"nope","kind":"goal.create","actor":"op","name":"g3","success":"s"}`},
		{"start refs", `{"kind":"mission.start","actor":"op","missionId":"mission-x","refs":[1]}`},
		{"secret keys on non-secret kind", `{"kind":"goal.create","actor":"op","name":"g4","success":"s","opId":5,"Ref":[1],"scopeTags":"x","refKind":{},"reasonCode":null}`},
		{"invalid json", `{"kind":`},
		{"null", `null`},
	} {
		code, body := postRaw(t, srv.URL, c.body)
		add("intent "+c.name, code, body)
	}
	out.WriteString("== sse after intents\n" + sse.next(t) + "\n")
	out.WriteString("== recorded\n" + recordedPayloads(s, n))
	code, body = getRaw(t, srv.URL, "/v1/workspace")
	add("workspace after", code, body)
	return out.String()
}

func TestSecretFreeJournalSurfacesByteIdenticalFRRHZ135(t *testing.T) {
	got := compatTranscriptFRRHZ135(t)
	if got != compatGoldenFRRHZ135 {
		t.Fatalf("§4.3 byte compatibility broken:\n%s", got)
	}
}

const compatGoldenFRRHZ135 = `== workspace OK
{"revision":14,"body":{"missions":[{"id":"goal-g","name":"dev goal","attention":false,"state":"active","success":"done","handle":"g-6b36d8a1"}],"tasks":[{"id":"mission-x","missionId":"goal-g","name":"dev mission","state":"queued","hasProgress":false,"attention":false,"handle":"m-cf10b7f0"}],"gates":[],"deliverables":[],"edges":[{"id":"e-about-g","from":{"id":"mem-b","type":"memory"},"to":{"id":"goal-g","type":"goal"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"},{"id":"e-about-m","from":{"id":"mem-a","type":"memory"},"to":{"id":"mission-x","type":"mission"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"}],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{"mission-x":{"pause":"disabled","resume":"enabled","instruct":"enabled"}},"gateCapabilities":{}}}

== sse snapshot
event: snapshot
data: {"revision":14,"body":{"missions":[{"id":"goal-g","name":"dev goal","attention":false,"state":"active","success":"done","handle":"g-6b36d8a1"}],"tasks":[{"id":"mission-x","missionId":"goal-g","name":"dev mission","state":"queued","hasProgress":false,"attention":false,"handle":"m-cf10b7f0"}],"gates":[],"deliverables":[],"edges":[{"id":"e-about-g","from":{"id":"mem-b","type":"memory"},"to":{"id":"goal-g","type":"goal"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"},{"id":"e-about-m","from":{"id":"mem-a","type":"memory"},"to":{"id":"mission-x","type":"mission"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"}],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{"mission-x":{"pause":"disabled","resume":"enabled","instruct":"enabled"}},"gateCapabilities":{}}}
== context OK
{"task":{"id":"mission-x","name":"dev mission","state":"queued","hasProgress":false},"goal":{"id":"goal-g","description":"dev goal","state":"active"},"memories":[{"id":"mem-a","kind":"fact","content":"content of mem-a","sourceId":"n-mem-a","tags":["dev"],"seq":3},{"id":"mem-b","kind":"fact","content":"content of mem-b","sourceId":"n-mem-b","tags":["dev"],"seq":4}],"knowledge":[{"id":"k-1","kind":"claim","statement":"statement k-1","sourceMemoryId":"mem-a","confidence":0.8},{"id":"k-2","kind":"claim","statement":"statement k-2","sourceMemoryId":"mem-a","confidence":0.8},{"id":"k-3","kind":"claim","statement":"statement k-3","sourceMemoryId":"mem-c","confidence":0.8}],"relations":[{"id":"rel-1","type":"supports","from":"k-1","to":"k-3"}],"steps":[]}

== intent goal.create OK
{"Accepted":true,"Reason":""}

== intent note.create OK
{"Accepted":true,"Reason":""}

== intent unknown OK
{"Accepted":false,"Reason":"unknown intent"}

== intent secretlike OK
{"Accepted":false,"Reason":"unknown intent"}

== intent trailing OK
{"Accepted":false,"Reason":"unknown intent"}

== intent dupkey OK
{"Accepted":true,"Reason":""}

== intent start refs OK
{"Accepted":false,"Reason":"secret intent: invalid ref"}

== intent secret keys on non-secret kind OK
{"Accepted":true,"Reason":""}

== intent invalid json Bad Request
invalid json

== intent null OK
{"Accepted":false,"Reason":"unknown intent"}

== sse after intents
event: projection
data: {"revision":16,"body":{"missions":[{"id":"goal-g","name":"dev goal","attention":false,"state":"active","success":"done","handle":"g-6b36d8a1"},{"id":"goal-g2","name":"g2","attention":false,"state":"active","success":"done","handle":"g-f520d785"}],"tasks":[{"id":"mission-x","missionId":"goal-g","name":"dev mission","state":"queued","hasProgress":false,"attention":false,"handle":"m-cf10b7f0"}],"gates":[],"deliverables":[],"edges":[{"id":"e-about-g","from":{"id":"mem-b","type":"memory"},"to":{"id":"goal-g","type":"goal"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"},{"id":"e-about-m","from":{"id":"mem-a","type":"memory"},"to":{"id":"mission-x","type":"mission"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"}],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{"mission-x":{"pause":"disabled","resume":"enabled","instruct":"enabled"}},"gateCapabilities":{}}}
== recorded
goal|goal-g2|goal.created|{"ID":"goal-g2","Description":"g2","Success":"done","PolicyRef":""}
source|sha256:2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6|source.registered|{"blob_id":"sha256:2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6","content_hash":"2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6","media_type":"text/markdown","size_bytes":1,"source_uri":"note://ui"}
memory|note-2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6|memory.created|{"id":"note-2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6","kind":"fact","content":"c","source_type":"blob","source_id":"sha256:2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6","confidence":1,"goal_id":"","mission_id":"mission-x","tags":null}
edge|edge-about-2e7d2c03a950-mission-x|edge.declared|{"ID":"edge-about-2e7d2c03a950-mission-x","From":{"Type":"memory","ID":"note-2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6"},"To":{"Type":"mission","ID":"mission-x"},"Kind":"about","Actor":"unverified-local-operator:op","Correlation":"relay","Verified":false,"Supersedes":""}
goal|goal-g3|goal.created|{"ID":"goal-g3","Description":"g3","Success":"s","PolicyRef":""}
goal|goal-g4|goal.created|{"ID":"goal-g4","Description":"g4","Success":"s","PolicyRef":""}
== workspace after OK
{"revision":21,"body":{"missions":[{"id":"goal-g","name":"dev goal","attention":false,"state":"active","success":"done","handle":"g-6b36d8a1"},{"id":"goal-g2","name":"g2","attention":false,"state":"active","success":"done","handle":"g-f520d785"},{"id":"goal-g3","name":"g3","attention":false,"state":"active","success":"s","handle":"g-fbf2b1a3"},{"id":"goal-g4","name":"g4","attention":false,"state":"active","success":"s","handle":"g-148cd708"}],"tasks":[{"id":"mission-x","missionId":"goal-g","name":"dev mission","state":"queued","hasProgress":false,"attention":false,"handle":"m-cf10b7f0"}],"gates":[],"deliverables":[],"edges":[{"id":"e-about-g","from":{"id":"mem-b","type":"memory"},"to":{"id":"goal-g","type":"goal"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"},{"id":"e-about-m","from":{"id":"mem-a","type":"memory"},"to":{"id":"mission-x","type":"mission"},"edgeKind":"about","actor":"unverified-local-operator:tester","correlation":"test"},{"id":"edge-about-2e7d2c03a950-mission-x","from":{"id":"note-2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6","type":"memory"},"to":{"id":"mission-x","type":"mission"},"edgeKind":"about","actor":"unverified-local-operator:op","correlation":"relay"}],"counts":{"running":0,"needsYou":0,"blocked":0},"attention":[],"capabilities":{"mission-x":{"pause":"disabled","resume":"enabled","instruct":"enabled"}},"gateCapabilities":{}}}

`
