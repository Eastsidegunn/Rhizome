package terminal

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rhizome/internal/events"
	"rhizome/internal/projector"
	"rhizome/internal/session"
)

// LogTarget is a launcher -p run whose log a log terminal can show (RHZ-150).
type LogTarget struct {
	ExecutionID string `json:"executionId"`
	SessionID   string `json:"sessionId"`
	MissionID   string `json:"missionId,omitempty"`
	// Name is the head of the mission's description (the mission id without one).
	Name    string `json:"name"`
	Running bool   `json:"running"`
	// UpdatedAt is the log's modification time (Unix ms).
	UpdatedAt int64 `json:"updatedAt"`
}

const nameRunes = 40

// launcherRun says whether a session is a launcher -p run with a log.
func launcherRun(s session.Session) bool {
	return s.Mode == session.Background && s.Kind == "" && validExecID(s.ExecutionID)
}

func validExecID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\`)
}

func (h *Host) logPath(execID string) string { return filepath.Join(h.LogDir, execID+".ndjson") }

// missionName is the head of a mission's first description line.
func missionName(store events.Port, id string) string {
	if id == "" {
		return ""
	}
	m, err := projector.ReplayMission(store.List("mission", id))
	if err != nil || strings.TrimSpace(m.Description) == "" {
		return id
	}
	return one(strings.SplitN(m.Description, "\n", 2)[0], nameRunes)
}

// LogTargets lists the running launcher runs and those whose log changed
// within RecentLogs of now, newest first. No LogDir lists none.
func (h *Host) LogTargets(now time.Time) ([]LogTarget, error) {
	out := []LogTarget{}
	if h.LogDir == "" {
		return out, nil
	}
	all, err := session.AllSessions(h.Store)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if !launcherRun(s) {
			continue
		}
		fi, err := os.Stat(h.logPath(s.ExecutionID))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		running := s.State == session.Open
		if !running && now.Sub(fi.ModTime()) > RecentLogs {
			continue
		}
		out = append(out, LogTarget{ExecutionID: s.ExecutionID, SessionID: s.SessionID, MissionID: s.MissionID,
			Name: missionName(h.Store, s.MissionID), Running: running, UpdatedAt: fi.ModTime().UnixMilli()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// execution finds the launcher run of an execution id.
func (h *Host) execution(id string) (session.Session, error) {
	if h.LogDir == "" {
		return session.Session{}, ErrLogsDisabled
	}
	if !validExecID(id) {
		return session.Session{}, ErrExecutionAbsent
	}
	all, err := session.AllSessions(h.Store)
	if err != nil {
		return session.Session{}, err
	}
	for _, s := range all {
		if launcherRun(s) && s.ExecutionID == id {
			return s, nil
		}
	}
	return session.Session{}, ErrExecutionAbsent
}

// running says whether a session is still open (a journal fault counts as
// not running: the tail then ends after draining what is there).
func (h *Host) running(sid string) bool {
	s, err := session.Replay(h.Store.List(session.AggregateType, sid))
	return err == nil && s.State == session.Open
}

// openLog opens a read-only terminal on a launcher run's log.
func (h *Host) openLog(execID string, cols, rows int) (*Terminal, error) {
	s, err := h.execution(execID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(h.logPath(execID))
	if err != nil {
		return nil, ErrExecutionAbsent
	}
	h.mu.Lock()
	if h.shutdown {
		h.mu.Unlock()
		f.Close()
		return nil, ErrClosed
	}
	h.mu.Unlock()
	term := &Terminal{ID: newID(), Kind: KindLog, SessionID: s.SessionID, ExecutionID: execID, Cwd: s.Cwd, OpenedAt: time.Now(), host: h,
		cols: uint16(cols), rows: uint16(rows), ring: make([]byte, 0, RingSize), subs: map[chan []byte]bool{}, done: make(chan struct{}), stop: make(chan struct{}), watched: make(chan struct{})}
	b, _ := json.Marshal(openedPayload{TerminalID: term.ID, Kind: KindLog, SessionID: s.SessionID, ExecutionID: execID})
	if err := h.Store.Append(0, events.Event{AggregateType: AggregateType, AggregateID: term.ID, Revision: 1, Type: OpenedType, Payload: b}); err != nil {
		f.Close()
		return nil, err
	}
	h.mu.Lock()
	h.terms[term.ID] = term
	late := h.shutdown
	h.mu.Unlock()
	if late {
		defer term.Close()
	}
	term.mu.Lock()
	term.armIdle()
	term.mu.Unlock()
	go term.tail(f, func() bool { return h.running(s.SessionID) }, logPoll)
	return term, nil
}

// end exits once a stream has seen the rendering (or the viewer is closed:
// nobody streaming is reaped by the idle timeout).
func (t *Terminal) end(code int) {
	select {
	case <-t.watched:
	case <-t.stop:
	}
	t.finish(code)
}

// tail follows the log from its start, publishing rendered lines. It exits
// with the result's code; a run that ends without a result exits 1 once the
// log is drained; a closed viewer exits 0.
func (t *Terminal) tail(f *os.File, running func() bool, poll time.Duration) {
	defer f.Close()
	r := bufio.NewReader(f)
	var pending []byte
	ended := false
	for {
		chunk, err := r.ReadBytes('\n')
		pending = append(pending, chunk...)
		if err == nil {
			out, final, code := RenderLine(pending)
			pending = pending[:0]
			if len(out) > 0 {
				t.publish(out)
			}
			if final {
				t.end(code)
				return
			}
			select {
			case <-t.stop:
				t.finish(0)
				return
			default:
			}
			continue
		}
		if !errors.Is(err, io.EOF) {
			t.publish([]byte(ansiRed + "■ 로그를 읽지 못했습니다" + ansiReset + "\r\n"))
			t.end(1)
			return
		}
		if ended {
			if out, final, code := RenderLine(pending); final {
				t.publish(out)
				t.end(code)
				return
			}
			t.publish([]byte(ansiDim + "■ 실행 끝 (결과 기록 없음)" + ansiReset + "\r\n"))
			t.end(1)
			return
		}
		// one more pass after the run ends: lines written just before it
		// closed are read before the exit
		if !running() {
			ended = true
			continue
		}
		select {
		case <-t.stop:
			t.finish(0)
			return
		case <-time.After(poll):
		}
	}
}
