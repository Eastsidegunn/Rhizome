package audit

import (
	"encoding/json"
	"fmt"
	"rhizome/internal/decision"
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/projector"
)

func TerminalLink(p events.Port, decisionID string) error {
	d, e := decision.Replay(p.List("decision", decisionID))
	if e != nil {
		return e
	}
	if d.Kind != decision.Complete && d.Kind != decision.Fail {
		return fmt.Errorf("not terminal decision")
	}
	m, e := projector.ReplayMission(p.List("mission", d.MissionID))
	if e != nil {
		return e
	}
	if m.TerminalDecisionID != decisionID {
		return fmt.Errorf("decision link missing")
	}
	want := domain.MissionSucceeded
	if d.Kind == decision.Fail {
		want = domain.MissionFailed
	}
	if m.State != want {
		return fmt.Errorf("terminal state mismatch")
	}
	var ds, ms uint64
	var corr, termCorr string
	for _, x := range p.All() {
		if x.AggregateType == "decision" && x.AggregateID == decisionID {
			ds = x.Sequence
			corr = x.CorrelationID
		}
		if x.AggregateType == "mission" && x.AggregateID == d.MissionID && x.Type == "mission.transitioned" {
			var q struct {
				DecisionID string `json:"DecisionID"`
			}
			if json.Unmarshal(x.Payload, &q) == nil && q.DecisionID == decisionID {
				ms = x.Sequence
				termCorr = x.CorrelationID
			}
		}
	}
	if ds == 0 || ms == 0 || ms <= ds {
		return fmt.Errorf("terminal order invalid")
	}
	if termCorr != corr {
		return fmt.Errorf("correlation mismatch")
	}
	return nil
}
