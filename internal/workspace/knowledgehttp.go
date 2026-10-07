package workspace

import (
	"encoding/json"
	"fmt"
	"net/http"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/memory"
)

// KnowledgeSnapshot uses one immutable event snapshot for both the global
// revision (§2) and memories.Search semantics (FR-RHZ-080).
func KnowledgeSnapshot(s events.Port, kind memory.Kind, tag string) (KnowledgeProjection, error) {
	p := KnowledgeProjection{Notes: []memory.Memory{}}
	if s == nil {
		return p, fmt.Errorf("nil store")
	}
	snapshot := &events.Store{}
	for _, e := range s.All() {
		if err := snapshot.AppendRevision(e); err != nil {
			return p, err
		}
		if e.Sequence > p.Revision {
			p.Revision = e.Sequence
		}
	}
	var err error
	p.Notes, err = (memory.Service{Store: snapshot}).Search(kind, tag, "")
	return p, err
}

type KnowledgeProjection struct {
	Revision uint64
	Notes    []memory.Memory
}

type knowledgeNoteDTO struct {
	ID         string      `json:"id"`
	Kind       memory.Kind `json:"kind"`
	Content    string      `json:"content"`
	Tags       []string    `json:"tags"`
	SourceID   string      `json:"sourceId"`
	SourceType string      `json:"sourceType"`
}
type knowledgeBodyDTO struct {
	Notes []knowledgeNoteDTO `json:"notes"`
}
type knowledgeEnvelope struct {
	Revision uint64           `json:"revision"`
	Body     knowledgeBodyDTO `json:"body"`
}

// AboutTarget is one node an about edge links a memory to (RHZ-057,
// FR-RHZ-087 reverse traversal).
type AboutTarget struct{ Type, ID, EdgeID string }

// KnowledgeAboutProjection answers "which goals/missions is this memory
// about?" from first-class edges only — never from the coexisting FK fields.
type KnowledgeAboutProjection struct {
	Revision uint64
	Targets  []AboutTarget
}

// KnowledgeAbout derives the reverse query from one immutable event snapshot,
// like KnowledgeSnapshot (recomputable from events, FR-RHZ-087).
func KnowledgeAbout(s events.Port, memoryID string) (KnowledgeAboutProjection, error) {
	p := KnowledgeAboutProjection{Targets: []AboutTarget{}}
	if s == nil {
		return p, fmt.Errorf("nil store")
	}
	snapshot := &events.Store{}
	for _, e := range s.All() {
		if err := snapshot.AppendRevision(e); err != nil {
			return p, err
		}
		if e.Sequence > p.Revision {
			p.Revision = e.Sequence
		}
	}
	edges, err := (edge.Service{Store: snapshot}).ByNode("memory", memoryID)
	if err != nil {
		return p, err
	}
	for _, x := range edges {
		if x.Kind == edge.About && x.From.Type == "memory" && x.From.ID == memoryID {
			p.Targets = append(p.Targets, AboutTarget{Type: x.To.Type, ID: x.To.ID, EdgeID: x.ID})
		}
	}
	return p, nil
}

type knowledgeAboutDTO struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	EdgeID string `json:"edgeId"`
}
type knowledgeAboutEnvelope struct {
	Revision uint64 `json:"revision"`
	Body     struct {
		About []knowledgeAboutDTO `json:"about"`
	} `json:"body"`
}

func (h *HTTPServer) serveKnowledgeAbout(w http.ResponseWriter, r *http.Request, memoryID string) {
	p, err := KnowledgeAbout(h.Store, memoryID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	env := knowledgeAboutEnvelope{Revision: p.Revision}
	env.Body.About = []knowledgeAboutDTO{}
	for _, t := range p.Targets {
		env.Body.About = append(env.Body.About, knowledgeAboutDTO{Type: t.Type, ID: t.ID, EdgeID: t.EdgeID})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(env)
}

func (h *HTTPServer) serveKnowledgeSnapshot(w http.ResponseWriter, r *http.Request) {
	// RHZ-057: ?about=<memoryID> switches to the reverse-traversal projection;
	// the parameterless response shape is unchanged.
	if about := r.URL.Query().Get("about"); about != "" {
		h.serveKnowledgeAbout(w, r, about)
		return
	}
	p, err := KnowledgeSnapshot(h.Store, memory.Kind(r.URL.Query().Get("kind")), r.URL.Query().Get("tag"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := knowledgeBodyDTO{Notes: []knowledgeNoteDTO{}}
	for _, m := range p.Notes {
		d.Notes = append(d.Notes, knowledgeNoteDTO{m.ID, m.Kind, m.Content, append([]string{}, m.Tags...), m.SourceID, m.SourceType})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(knowledgeEnvelope{p.Revision, d})
}

func validMemoryKind(k memory.Kind) bool {
	switch k {
	case memory.Fact, memory.Decision, memory.Preference, memory.Observation, memory.Hypothesis, memory.Reference:
		return true
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
