package evaluation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rhizome/internal/events"
	"rhizome/internal/knowledge"
	"rhizome/internal/relation"
	"rhizome/internal/trace"
)

type CaseKind string

const (
	RelationRecall     CaseKind = "relation_recall"
	PathReconstruction CaseKind = "path_reconstruction"
	Application        CaseKind = "application"
)

type Case struct {
	Kind                CaseKind
	Question            string
	RequiredItemIDs     []string
	RequiredRelationIDs []string
	ExpectedOutcome     string
}

type Scores struct {
	EvidenceRecall    float64 `json:"evidence_recall"`
	EvidencePrecision float64 `json:"evidence_precision"`
	NodeCoverage      float64 `json:"node_coverage"`
	EdgeCoverage      float64 `json:"edge_coverage"`
	ConditionOK       bool    `json:"condition_ok"`
}

type Evaluation struct {
	ID       string
	Case     Case
	TraceID  string
	Scores   Scores
	Revision uint64
}

type Service struct{ Store events.Port }

type payload struct {
	ID                  string   `json:"evaluation_id"`
	Kind                CaseKind `json:"kind"`
	Question            string   `json:"question"`
	RequiredItemIDs     []string `json:"required_item_ids"`
	RequiredRelationIDs []string `json:"required_relation_ids"`
	ExpectedOutcome     string   `json:"expected_outcome,omitempty"`
	TraceID             string   `json:"trace_id"`
	Scores              Scores   `json:"scores"`
}

func clone(in []string) []string { return append([]string(nil), in...) }

func validateIDs(name string, ids []string, required bool) error {
	if required && len(ids) == 0 {
		return fmt.Errorf("%s is required", name)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("invalid %s", name)
		}
		seen[id] = true
	}
	return nil
}

func relationFromStore(store events.Port, id string) (relation.Relation, error) {
	if store == nil {
		return relation.Relation{}, fmt.Errorf("nil event store")
	}
	r, err := relation.Replay(store.List("relation", id))
	if err != nil {
		return relation.Relation{}, fmt.Errorf("relation %q: %w", id, err)
	}
	if _, err := knowledge.Replay(store.List("knowledge", r.From)); err != nil {
		return relation.Relation{}, fmt.Errorf("relation from: %w", err)
	}
	if _, err := knowledge.Replay(store.List("knowledge", r.To)); err != nil {
		return relation.Relation{}, fmt.Errorf("relation to: %w", err)
	}
	return r, nil
}

func RelationRecallCase(store events.Port, relationID string) (Case, error) {
	r, err := relationFromStore(store, relationID)
	if err != nil {
		return Case{}, err
	}
	question := fmt.Sprintf("%s와 %s의 관계를 설명하라.", r.From, r.To)
	if r.Type == relation.FailsWhen {
		question = fmt.Sprintf("%s는 언제 실패하는가?", r.From)
	}
	return Case{Kind: RelationRecall, Question: question, RequiredItemIDs: []string{r.From, r.To}, RequiredRelationIDs: []string{r.ID}}, nil
}

func PathReconstructionCase(store events.Port, firstID, secondID string) (Case, error) {
	a, err := relationFromStore(store, firstID)
	if err != nil {
		return Case{}, err
	}
	b, err := relationFromStore(store, secondID)
	if err != nil {
		return Case{}, err
	}
	endsA := []string{a.From, a.To}
	endsB := []string{b.From, b.To}
	shared := ""
	for _, x := range endsA {
		for _, y := range endsB {
			if x == y {
				if shared != "" && shared != x {
					return Case{}, fmt.Errorf("relations share two endpoints")
				}
				shared = x
			}
		}
	}
	if shared == "" {
		return Case{}, fmt.Errorf("relations do not form a chain")
	}
	items := map[string]bool{a.From: true, a.To: true, b.From: true, b.To: true}
	if len(items) != 3 {
		return Case{}, fmt.Errorf("relations do not form a three-item chain")
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	question := fmt.Sprintf("%s와 %s를 연결하는 지식 경로를 재구성하라.", ids[0], ids[2])
	rels := []string{a.ID, b.ID}
	sort.Strings(rels)
	return Case{Kind: PathReconstruction, Question: question, RequiredItemIDs: ids, RequiredRelationIDs: rels}, nil
}

func ApplicationCase(store events.Port, relationID string) (Case, error) {
	r, err := relationFromStore(store, relationID)
	if err != nil {
		return Case{}, err
	}
	outcome := "applicable"
	question := fmt.Sprintf("%s를 %s에 적용해도 되는가?", r.From, r.To)
	if r.Type == relation.FailsWhen {
		outcome = "not_applicable"
		question = fmt.Sprintf("%s를 %s인 상황에 적용해도 되는가?", r.From, r.To)
	} else if r.Type != relation.AppliesWhen {
		return Case{}, fmt.Errorf("relation is not applies_when")
	}
	return Case{Kind: Application, Question: question, RequiredItemIDs: []string{r.From, r.To}, RequiredRelationIDs: []string{r.ID}, ExpectedOutcome: outcome}, nil
}

func Score(c Case, tr trace.Trace) (Scores, error) {
	if err := validateCase(c); err != nil {
		return Scores{}, err
	}
	used := map[string]bool{}
	for _, id := range tr.UsedItemIDs {
		used[id] = true
	}
	required := map[string]bool{}
	for _, id := range c.RequiredItemIDs {
		required[id] = true
	}
	matched := 0
	for id := range required {
		if used[id] {
			matched++
		}
	}
	recall := 0.0
	if len(required) > 0 {
		recall = float64(matched) / float64(len(required))
	}
	precision := 0.0
	if len(tr.UsedItemIDs) > 0 {
		precision = float64(matched) / float64(len(tr.UsedItemIDs))
	}
	edgeSet := map[string]bool{}
	for _, id := range tr.TraversedRelationIDs {
		edgeSet[id] = true
	}
	edgeMatched := 0
	for _, id := range c.RequiredRelationIDs {
		if edgeSet[id] {
			edgeMatched++
		}
	}
	edgeCoverage := 0.0
	if len(c.RequiredRelationIDs) > 0 {
		edgeCoverage = float64(edgeMatched) / float64(len(c.RequiredRelationIDs))
	}
	condition := true
	if c.Kind == Application {
		condition = tr.Outcome == c.ExpectedOutcome
	}
	return Scores{EvidenceRecall: recall, EvidencePrecision: precision, NodeCoverage: recall, EdgeCoverage: edgeCoverage, ConditionOK: condition}, nil
}

func validateCase(c Case) error {
	if c.Kind != RelationRecall && c.Kind != PathReconstruction && c.Kind != Application {
		return fmt.Errorf("unknown case kind")
	}
	if strings.TrimSpace(c.Question) == "" {
		return fmt.Errorf("question is required")
	}
	if err := validateIDs("required items", c.RequiredItemIDs, true); err != nil {
		return err
	}
	if err := validateIDs("required relations", c.RequiredRelationIDs, true); err != nil {
		return err
	}
	if c.Kind == Application && c.ExpectedOutcome != "applicable" && c.ExpectedOutcome != "not_applicable" {
		return fmt.Errorf("invalid expected outcome")
	}
	if c.Kind != Application && c.ExpectedOutcome != "" {
		return fmt.Errorf("unexpected expected outcome")
	}
	return nil
}

func (s Service) Record(id string, c Case, traceID string) (Evaluation, error) {
	if s.Store == nil {
		return Evaluation{}, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(traceID) == "" {
		return Evaluation{}, fmt.Errorf("evaluation and trace IDs are required")
	}
	if err := validateCase(c); err != nil {
		return Evaluation{}, err
	}
	tr, err := trace.Replay(s.Store.List("trace", traceID))
	if err != nil {
		return Evaluation{}, fmt.Errorf("trace %q: %w", traceID, err)
	}
	scores, err := Score(c, tr)
	if err != nil {
		return Evaluation{}, err
	}
	p := payload{ID: id, Kind: c.Kind, Question: c.Question, RequiredItemIDs: clone(c.RequiredItemIDs), RequiredRelationIDs: clone(c.RequiredRelationIDs), ExpectedOutcome: c.ExpectedOutcome, TraceID: traceID, Scores: scores}
	raw, err := json.Marshal(p)
	if err != nil {
		return Evaluation{}, err
	}
	e := events.Event{AggregateType: "evaluation", AggregateID: id, Revision: 1, Type: "evaluation.recorded", Payload: raw}
	if _, err := Replay([]events.Event{e}); err != nil {
		return Evaluation{}, err
	}
	if err := s.Store.Append(0, e); err != nil {
		return Evaluation{}, err
	}
	return Replay(s.Store.List("evaluation", id))
}

func Replay(log []events.Event) (Evaluation, error) {
	if len(log) == 0 {
		return Evaluation{}, fmt.Errorf("evaluation event stream is empty")
	}
	if len(log) != 1 {
		return Evaluation{}, fmt.Errorf("duplicate or unknown evaluation event")
	}
	e := log[0]
	if e.AggregateType != "evaluation" || e.AggregateID == "" || e.Revision != 1 || e.Type != "evaluation.recorded" {
		return Evaluation{}, fmt.Errorf("invalid evaluation event")
	}
	var p payload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Evaluation{}, err
	}
	c := Case{Kind: p.Kind, Question: p.Question, RequiredItemIDs: clone(p.RequiredItemIDs), RequiredRelationIDs: clone(p.RequiredRelationIDs), ExpectedOutcome: p.ExpectedOutcome}
	if p.ID != e.AggregateID {
		return Evaluation{}, fmt.Errorf("evaluation id does not match aggregate")
	}
	if err := validateCase(c); err != nil {
		return Evaluation{}, err
	}
	return Evaluation{ID: p.ID, Case: c, TraceID: p.TraceID, Scores: p.Scores, Revision: e.Revision}, nil
}

func (s Service) query(field, value string) ([]Evaluation, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("nil event store")
	}
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("query value is required")
	}
	streams := map[string][]events.Event{}
	for _, e := range s.Store.All() {
		if e.AggregateType == "evaluation" {
			streams[e.AggregateID] = append(streams[e.AggregateID], e)
		}
	}
	ids := make([]string, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Evaluation, 0)
	for _, id := range ids {
		ev, err := Replay(streams[id])
		if err != nil {
			return nil, fmt.Errorf("evaluation %q: %w", id, err)
		}
		if field == "trace" && ev.TraceID == value || field == "kind" && string(ev.Case.Kind) == value {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s Service) ByTrace(id string) ([]Evaluation, error)    { return s.query("trace", id) }
func (s Service) ByKind(kind CaseKind) ([]Evaluation, error) { return s.query("kind", string(kind)) }
