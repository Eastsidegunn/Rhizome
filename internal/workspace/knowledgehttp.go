package workspace

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"rhizome/internal/edge"
	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/memory"
	"rhizome/internal/relation"
)

// KnowledgeSnapshot uses one immutable event snapshot for the global revision
// and every projected collection (FR-RHZ-080, FR-RHZ-170). The public helper
// preserves its original signature; the HTTP surface adds itemKind below.
func KnowledgeSnapshot(s events.Port, kind memory.Kind, tag string) (KnowledgeProjection, error) {
	return knowledgeSnapshot(s, kind, "", tag)
}

func knowledgeSnapshot(s events.Port, kind memory.Kind, itemKind knowledge.Kind, tag string) (KnowledgeProjection, error) {
	p := KnowledgeProjection{Notes: []memory.Memory{}, Items: []knowledge.KnowledgeItem{}, Relations: []relation.Relation{}}
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
	if p.Notes, err = (memory.Service{Store: snapshot}).Search(kind, tag, ""); err != nil {
		return p, err
	}
	if p.Items, err = (knowledge.Service{Store: snapshot}).Search(itemKind, "", tag); err != nil {
		return p, err
	}

	surviving := make(map[string]bool, len(p.Items))
	for _, item := range p.Items {
		surviving[item.ID] = true
	}
	itemFilterActive := itemKind != "" || tag != ""
	streams := make(map[string][]events.Event)
	for _, e := range snapshot.All() {
		if e.AggregateType == "relation" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rel, replayErr := relation.Replay(streams[id])
		if replayErr != nil {
			return p, fmt.Errorf("relation %q: %w", id, replayErr)
		}
		if itemFilterActive && (!surviving[rel.From] || !surviving[rel.To]) {
			continue
		}
		p.Relations = append(p.Relations, rel)
	}
	return p, nil
}

type KnowledgeProjection struct {
	Revision  uint64
	Notes     []memory.Memory
	Items     []knowledge.KnowledgeItem
	Relations []relation.Relation
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
	Notes     []knowledgeNoteDTO     `json:"notes"`
	Items     []knowledgeItemDTO     `json:"items"`
	Relations []knowledgeRelationDTO `json:"relations"`
}
type knowledgeItemDTO struct {
	ID             string           `json:"id"`
	Kind           knowledge.Kind   `json:"kind"`
	Statement      string           `json:"statement"`
	Status         knowledge.Status `json:"status"`
	Confidence     float64          `json:"confidence"`
	SourceMemoryID string           `json:"sourceMemoryId"`
	Tags           []string         `json:"tags"`
	Supersedes     string           `json:"supersedes"`
}
type knowledgeRelationDTO struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	From            string   `json:"from"`
	To              string   `json:"to"`
	SourceMemoryIDs []string `json:"sourceMemoryIds"`
	Confidence      float64  `json:"confidence"`
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
	itemKind := knowledge.Kind(r.URL.Query().Get("itemKind"))
	if itemKind != "" && !validKnowledgeKind(itemKind) {
		http.Error(w, "invalid knowledge kind", http.StatusBadRequest)
		return
	}
	p, err := knowledgeSnapshot(h.Store, memory.Kind(r.URL.Query().Get("kind")), itemKind, r.URL.Query().Get("tag"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := knowledgeBodyDTO{Notes: []knowledgeNoteDTO{}, Items: []knowledgeItemDTO{}, Relations: []knowledgeRelationDTO{}}
	for _, m := range p.Notes {
		d.Notes = append(d.Notes, knowledgeNoteDTO{m.ID, m.Kind, m.Content, append([]string{}, m.Tags...), m.SourceID, m.SourceType})
	}
	for _, item := range p.Items {
		d.Items = append(d.Items, knowledgeItemDTO{item.ID, item.Kind, item.Statement, item.Status, item.Confidence, item.SourceMemoryID, append([]string{}, item.Tags...), item.Supersedes})
	}
	for _, rel := range p.Relations {
		d.Relations = append(d.Relations, knowledgeRelationDTO{rel.ID, string(rel.Type), rel.From, rel.To, append([]string{}, rel.SourceMemoryIDs...), rel.Confidence})
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

func validKnowledgeKind(k knowledge.Kind) bool {
	switch k {
	case knowledge.Concept, knowledge.Claim, knowledge.Procedure, knowledge.Constraint, knowledge.Assumption, knowledge.FailureMode:
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
