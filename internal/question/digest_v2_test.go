package question_test

import (
	"encoding/json"
	"errors"
	"rhizome/internal/events"
	question "rhizome/internal/question"
	"testing"
)

func TestQuestionDigestV2VectorsFRRHZ145(t *testing.T) {
	vectors := []struct {
		title, body, recommendation string
		v1, v2                      string
	}{
		{"t", "b", "", "rhz-question-v1:5ac9949949ec6008316f404bd658dec54524ddfbad8dfe038784cdefa2641dc1", "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61"},
		{"a\x00b", "c", "", "rhz-question-v1:13e228567e8249fce53337f25d7970de3bd68ab2653424c7b8f9fd05e33caedf", "rhz-question-v2:dd755a395981aa6cc361fd0a491f7305de71205d7f270cce9253f54224afbed3"},
		{"a", "b\x00c", "", "rhz-question-v1:13e228567e8249fce53337f25d7970de3bd68ab2653424c7b8f9fd05e33caedf", "rhz-question-v2:5daae261eed38326710fd2b3d1f724bc61190491ef1c4af0854353b6165d3e8b"},
		{"승인 요청", "본문", "권고", "rhz-question-v1:99beae144391eef0e4ecc21cae6a97dd7efb88231b5b02961e1ad9b4f5da7bb4", "rhz-question-v2:324eb6e94c4bcebf53b998a0e0f0140dabd4d4d48262e7d1cda8ee9815d71f6b"},
	}
	for _, vector := range vectors {
		if got := question.Digest(vector.title, vector.body, vector.recommendation); got != vector.v2 {
			t.Fatalf("Digest(%q,%q,%q) = %q, want %q", vector.title, vector.body, vector.recommendation, got, vector.v2)
		}
		id, err := question.IDForDigest(vector.v2)
		if err != nil || id != "q-"+vector.v2[len("rhz-question-v2:"):][:24] {
			t.Fatalf("v2 ID = %q err=%v", id, err)
		}
	}
	if vectors[1].v1 != vectors[2].v1 {
		t.Fatal("v1 NUL-collision pair no longer pins the same literal")
	}
	id1, _ := question.IDForDigest(vectors[1].v2)
	id2, _ := question.IDForDigest(vectors[2].v2)
	if id1 == id2 {
		t.Fatal("v2 did not separate the v1 NUL-collision pair")
	}
	if id, err := question.IDFor("t", "b", ""); err != nil || id != "q-92caa77d4e8cb1f9a861ff35" {
		t.Fatalf("IDFor vector = %q err=%v", id, err)
	}
}

func TestV1NULCollisionReplayAndV2SeparationFRRHZ145(t *testing.T) {
	const v1Digest = "rhz-question-v1:13e228567e8249fce53337f25d7970de3bd68ab2653424c7b8f9fd05e33caedf"
	v1ID, err := question.IDForDigest(v1Digest)
	if err != nil {
		t.Fatal(err)
	}
	replay := func(title, body string) question.Ref {
		t.Helper()
		payload, err := json.Marshal(map[string]string{
			"Title": title, "Body": body, "Recommendation": "", "MissionID": "",
			"RequestedBy": "unverified-local-operator:legacy", "CorrelationID": "", "Digest": v1Digest,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := question.Replay([]events.Event{{
			AggregateType: "question", AggregateID: v1ID, Revision: 1,
			Type: "question.asked", Payload: payload,
		}})
		if err != nil {
			t.Fatalf("replay v1 question (%q, %q): %v", title, body, err)
		}
		return got
	}

	first := replay("a\x00b", "c")
	second := replay("a", "b\x00c")
	if first.Digest != second.Digest || first.Digest != v1Digest {
		t.Fatalf("v1 collision not preserved: %q != %q", first.Digest, second.Digest)
	}
	firstV2 := question.Digest("a\x00b", "c", "")
	secondV2 := question.Digest("a", "b\x00c", "")
	if firstV2 == secondV2 {
		t.Fatalf("v2 digests collided: %q", firstV2)
	}
}

func TestMixedV1V2JournalReplayFRRHZ145(t *testing.T) {
	store := &events.Store{}
	v1Digest := "rhz-question-v1:5ac9949949ec6008316f404bd658dec54524ddfbad8dfe038784cdefa2641dc1"
	v1ID, _ := question.IDForDigest(v1Digest)
	payload, _ := json.Marshal(map[string]string{
		"Title": "t", "Body": "b", "Recommendation": "", "MissionID": "",
		"RequestedBy": "unverified-local-operator:legacy", "CorrelationID": "", "Digest": v1Digest,
	})
	if err := store.Append(0, events.Event{AggregateType: "question", AggregateID: v1ID, Revision: 1, Type: "question.asked", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got, err := question.Replay(store.List("question", v1ID)); err != nil || got.Digest != v1Digest {
		t.Fatalf("v1 replay = %+v err=%v", got, err)
	}
	v2, err := (question.Service{Store: store}).Ask("t", "b", "", "", "", "new", "")
	if err != nil || v2.Digest != "rhz-question-v2:92caa77d4e8cb1f9a861ff35a08c42ea7cc48b20fdbacb911cbf8580adafeb61" || v2.ID == v1ID {
		t.Fatalf("v2 ask = %+v err=%v", v2, err)
	}

	bad := store.List("question", v1ID)
	var object map[string]any
	_ = json.Unmarshal(bad[0].Payload, &object)
	object["Digest"] = v2.Digest
	bad[0].Payload, _ = json.Marshal(object)
	if _, err := question.Replay(bad); !errors.Is(err, question.ErrDigestMismatch) {
		t.Fatalf("v1 content with v2 digest = %v", err)
	}
}

func TestIDForDigestRejectsMalformedFRRHZ145(t *testing.T) {
	for _, digest := range []string{"", "unknown:012345678901234567890123", "rhz-question-v1:short", "rhz-question-v2:"} {
		if id, err := question.IDForDigest(digest); !errors.Is(err, question.ErrDigestMismatch) || id != "" {
			t.Fatalf("IDForDigest(%q) = %q, %v", digest, id, err)
		}
	}
}
