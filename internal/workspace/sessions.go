package workspace

import (
	"rhizome/internal/domain"
	"rhizome/internal/events"
	"rhizome/internal/session"
)

// RHZ-139 (FR-RHZ-183): the session registry on the workspace surface.

// Session is one replayed session. Stale = open while its mission is
// terminal (attention kind session_stale).
type Session struct {
	session.Session
	Stale bool
}

func replaySessions(s events.Port, ids []string, missionStates map[string]domain.MissionState) ([]Session, error) {
	var out []Session
	for _, id := range ids {
		x, err := session.Replay(s.List(session.AggregateType, id))
		if err != nil {
			return nil, err
		}
		stale := false
		if x.State == session.Open && x.MissionID != "" {
			switch missionStates[x.MissionID] {
			case domain.MissionSucceeded, domain.MissionFailed, domain.MissionCancelled:
				stale = true
			}
		}
		out = append(out, Session{Session: x, Stale: stale})
	}
	return out, nil
}

type sessionDTO struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	MissionID string `json:"missionId,omitempty"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
	OpenedAt  int64  `json:"openedAt,omitempty"`
	DriverRef string `json:"driverRef,omitempty"`
}

func sessionsDTO(d *dto, p Projection) {
	for _, x := range p.Sessions {
		d.Sessions = append(d.Sessions, sessionDTO{SessionID: x.SessionID, Name: x.Name, MissionID: x.MissionID, Mode: x.Mode, State: x.State, OpenedAt: x.OpenedAt, DriverRef: x.DriverRef})
	}
}
