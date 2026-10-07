package retrieval

import (
	"fmt"
	"sort"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/relation"
)

type Query struct {
	Kind              knowledge.Kind
	Tag               string
	SourceMemoryID    string
	IncludeCandidates bool
	IncludeSuperseded bool
	FollowRelations   []relation.Type
}

type Item struct {
	Knowledge  knowledge.KnowledgeItem
	Superseded bool
}

type Result struct {
	Items     []Item
	Relations []relation.Relation
}

// KnowledgeIDs returns item IDs in the same deterministic order as Result.Items.
func (r Result) KnowledgeIDs() []string {
	ids := make([]string, 0, len(r.Items))
	for _, item := range r.Items {
		ids = append(ids, item.Knowledge.ID)
	}
	return ids
}

// RelationIDs returns relation IDs in deterministic order.
func (r Result) RelationIDs() []string {
	ids := make([]string, 0, len(r.Relations))
	for _, rel := range r.Relations {
		ids = append(ids, rel.ID)
	}
	return ids
}

type Retriever interface {
	Retrieve(q Query) (Result, error)
}

type Deterministic struct{ Store events.Port }

var _ Retriever = Deterministic{}

type snapshot struct {
	items      map[string]knowledge.KnowledgeItem
	relations  map[string]relation.Relation
	superseded map[string]bool
}

func cloneTypes(in []relation.Type) []relation.Type { return append([]relation.Type(nil), in...) }

func (d Deterministic) load() (snapshot, error) {
	if d.Store == nil {
		return snapshot{}, fmt.Errorf("nil event store")
	}
	all := d.Store.All()
	knowledgeStreams := make(map[string][]events.Event)
	relationStreams := make(map[string][]events.Event)
	for _, e := range all {
		switch e.AggregateType {
		case "knowledge":
			knowledgeStreams[e.AggregateID] = append(knowledgeStreams[e.AggregateID], e)
		case "relation":
			relationStreams[e.AggregateID] = append(relationStreams[e.AggregateID], e)
		}
	}
	s := snapshot{items: make(map[string]knowledge.KnowledgeItem), relations: make(map[string]relation.Relation), superseded: make(map[string]bool)}
	for id, stream := range knowledgeStreams {
		item, err := knowledge.Replay(stream)
		if err != nil {
			return snapshot{}, fmt.Errorf("knowledge %q: %w", id, err)
		}
		s.items[id] = item
		if item.Supersedes != "" {
			s.superseded[item.Supersedes] = true
		}
	}
	for id, stream := range relationStreams {
		r, err := relation.Replay(stream)
		if err != nil {
			return snapshot{}, fmt.Errorf("relation %q: %w", id, err)
		}
		s.relations[id] = r
	}
	return s, nil
}

func allowedKind(item knowledge.KnowledgeItem, q Query) bool {
	return q.Kind == "" || item.Kind == q.Kind
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func matches(item knowledge.KnowledgeItem, q Query, superseded bool) bool {
	if !allowedKind(item, q) || q.Tag != "" && !hasTag(item.Tags, q.Tag) || q.SourceMemoryID != "" && item.SourceMemoryID != q.SourceMemoryID {
		return false
	}
	if !q.IncludeCandidates && item.Status == knowledge.Candidate {
		return false
	}
	if !q.IncludeSuperseded && superseded {
		return false
	}
	return true
}

func stateAllowed(item knowledge.KnowledgeItem, q Query, superseded bool) bool {
	if !q.IncludeCandidates && item.Status == knowledge.Candidate {
		return false
	}
	return q.IncludeSuperseded || !superseded
}

func relationTypes(types []relation.Type) map[relation.Type]bool {
	set := make(map[relation.Type]bool)
	for _, typ := range types {
		set[typ] = true
	}
	return set
}

func (d Deterministic) Retrieve(q Query) (Result, error) {
	s, err := d.load()
	if err != nil {
		return Result{}, err
	}
	items := make(map[string]Item)
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := s.items[id]
		if matches(item, q, s.superseded[id]) {
			items[id] = Item{Knowledge: item, Superseded: s.superseded[id]}
		}
	}

	selectedTypes := relationTypes(cloneTypes(q.FollowRelations))
	usedRelations := make(map[string]relation.Relation)
	if len(selectedTypes) > 0 {
		base := make(map[string]bool, len(items))
		for id := range items {
			base[id] = true
		}
		relationIDs := make([]string, 0, len(s.relations))
		for id := range s.relations {
			relationIDs = append(relationIDs, id)
		}
		sort.Strings(relationIDs)
		for _, id := range relationIDs {
			rel := s.relations[id]
			if !selectedTypes[rel.Type] {
				continue
			}
			fromBase, toBase := base[rel.From], base[rel.To]
			if !fromBase && !toBase {
				continue
			}
			if fromBase && !toBase {
				if target, exists := s.items[rel.To]; exists && stateAllowed(target, q, s.superseded[rel.To]) {
					items[rel.To] = Item{Knowledge: target, Superseded: s.superseded[rel.To]}
					usedRelations[id] = rel
				}
				continue
			}
			if toBase && !fromBase {
				if target, exists := s.items[rel.From]; exists && stateAllowed(target, q, s.superseded[rel.From]) {
					items[rel.From] = Item{Knowledge: target, Superseded: s.superseded[rel.From]}
					usedRelations[id] = rel
				}
				continue
			}
			usedRelations[id] = rel
		}
	}
	itemIDs := make([]string, 0, len(items))
	for id := range items {
		itemIDs = append(itemIDs, id)
	}
	sort.Strings(itemIDs)
	result := Result{Items: make([]Item, 0, len(itemIDs)), Relations: make([]relation.Relation, 0, len(usedRelations))}
	for _, id := range itemIDs {
		result.Items = append(result.Items, items[id])
	}
	relIDs := make([]string, 0, len(usedRelations))
	for id := range usedRelations {
		relIDs = append(relIDs, id)
	}
	sort.Strings(relIDs)
	for _, id := range relIDs {
		result.Relations = append(result.Relations, usedRelations[id])
	}
	return result, nil
}
