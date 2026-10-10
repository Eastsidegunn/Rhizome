package workspace

// RHZ-107 G1 (FR-RHZ-140, SCP §5): mission.start refs — format-only check
// (G2, unique, ≤16) with the fixed X7 reason and zero writes; valid refs
// reach the starter sorted. The hook is a fake starter.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func wsRef(c byte) string { return "sref-" + strings.Repeat(string(c), 24) }

func TestRelayStartRefsFRRHZ140(t *testing.T) {
	a, b := wsRef('a'), wsRef('b')
	s := runningMission(t)
	f := &fakeStarter{s: s}
	in := []string{b, a}
	if r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: "m", Refs: RefsOf(in...)}, f); !r.Accepted {
		t.Fatalf("%+v", r)
	}
	if got := f.prepared[0].Refs; len(got) != 2 || got[0] != a || got[1] != b || in[0] != b {
		t.Fatalf("refs %v input %v", got, in)
	}
	if got := f.started[0].Refs; len(got) != 2 || got[0] != a {
		t.Fatalf("%v", got)
	}
	s2 := runningMission(t)
	f2 := &fakeStarter{s: s2}
	if r := startRelay(t, s2, Intent{Kind: "mission.start", MissionID: "m"}, f2); !r.Accepted || f2.started[0].Refs != nil {
		t.Fatalf("%+v %+v", r, f2.started)
	}
}

func TestRelayStartInvalidRefsX7FRRHZ140(t *testing.T) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sn := "rhzsntl-" + hex.EncodeToString(b)
	many := make([]string, 17)
	for i := range many {
		many[i] = "sref-" + strings.Repeat("0", 22) + hex.EncodeToString([]byte{byte(i)})
	}
	for name, refs := range map[string][]string{
		"sentinel":  {sn},
		"mixed":     {wsRef('a'), sn},
		"duplicate": {wsRef('a'), wsRef('a')},
		"too many":  many,
		"upper":     {strings.ToUpper(wsRef('a'))},
	} {
		s := runningMission(t)
		before, _ := json.Marshal(s.All())
		f := &fakeStarter{s: s}
		// Unknown mission too: the refs check comes first and still writes nothing.
		for _, id := range []string{"m", "nope"} {
			r := startRelay(t, s, Intent{Kind: "mission.start", MissionID: id, Refs: RefsOf(refs...)}, f)
			if r.Accepted || r.Reason != "secret intent: invalid ref" || r.ExecutionID != "" {
				t.Errorf("%s/%s: %+v", name, id, r)
			}
		}
		after, _ := json.Marshal(s.All())
		if !bytes.Equal(before, after) || len(f.prepared)+len(f.started) != 0 {
			t.Errorf("%s: wrote or called the starter", name)
		}
	}
	// HTTP wire: the response is the fixed reason only (ZV ③).
	s := runningMission(t)
	h := NewHTTP(s)
	f := &fakeStarter{s: s}
	h.ExecStart = f
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	before, _ := json.Marshal(s.All())
	resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(`{"kind":"mission.start","missionId":"m","refs":["`+sn+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"secret intent: invalid ref"`) || strings.Contains(string(body), sn[8:16]) || strings.Contains(string(body), sn[len(sn)-8:]) {
		t.Fatalf("%s", body)
	}
	after, _ := json.Marshal(s.All())
	if !bytes.Equal(before, after) || len(f.prepared) != 0 {
		t.Fatal("HTTP rejection wrote")
	}
}

// Present-but-malformed refs on the wire (null, non-array, non-string
// element) are X7 with zero writes and no starter call — never "invalid
// json" and never treated as absent. Other kinds keep decoding as before
// (the key was ignored), and an absent refs marshals as the old null.
func TestRelayStartRefsWireFRRHZ140(t *testing.T) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sn := "rhzsntl-" + hex.EncodeToString(b)
	s := runningMission(t)
	h := NewHTTP(s)
	f := &fakeStarter{s: s}
	h.ExecStart = f
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	before, _ := json.Marshal(s.All())
	for _, v := range []string{`null`, `"` + sn + `"`, `[1]`, `[null]`, `["` + wsRef('a') + `",2]`, `{"` + sn + `":1}`, `true`, `[["` + sn + `"]]`, `[{"a":"` + sn + `"}]`} {
		resp, err := http.Post(srv.URL+"/v1/intent", "application/json", strings.NewReader(`{"kind":"mission.start","missionId":"m","refs":`+v+`}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), `"Accepted":false,"Reason":"secret intent: invalid ref"`) || strings.Contains(string(body), sn[8:16]) {
			t.Errorf("refs %s: %d %s", v, resp.StatusCode, body)
		}
	}
	after, _ := json.Marshal(s.All())
	if !bytes.Equal(before, after) || len(f.prepared)+len(f.started) != 0 {
		t.Fatal("malformed wire refs wrote or reached the starter")
	}
	// Present empty array: nothing requested, start proceeds.
	var in Intent
	if err := json.Unmarshal([]byte(`{"Kind":"mission.start","MissionID":"m","refs":[]}`), &in); err != nil {
		t.Fatal(err)
	}
	if r := startRelay(t, s, in, f); !r.Accepted || f.started[0].Refs != nil {
		t.Fatalf("%+v %+v", r, f.started)
	}
	// Other kinds: a stray refs value never fails the decode.
	if err := json.Unmarshal([]byte(`{"Kind":"note.create","refs":"x"}`), &in); err != nil {
		t.Fatal(err)
	}
	// Absent refs marshal exactly as the former nil []string.
	out, _ := json.Marshal(Intent{Kind: "x"})
	if !strings.Contains(string(out), `"refs":null`) {
		t.Fatalf("%s", out)
	}
}
