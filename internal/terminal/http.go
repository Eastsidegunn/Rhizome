package terminal

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"rhizome/internal/session"
)

const maxBody = 1 << 20

// LoopbackAddr says whether a serve -addr binds a loopback IP only. The
// bridge is enabled for such binds alone; otherwise every route is 501.
func LoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Routes says whether a path belongs to the bridge.
func Routes(path string) bool {
	return path == "/v1/terminals" || strings.HasPrefix(path, "/v1/terminals/")
}

type targetDTO struct {
	Session *struct {
		SessionID string `json:"sessionId"`
	} `json:"session,omitempty"`
	Shell *struct {
		Cwd string `json:"cwd"`
	} `json:"shell,omitempty"`
	Execution *struct {
		ExecutionID string `json:"executionId"`
	} `json:"execution,omitempty"`
}

type openDTO struct {
	Target targetDTO `json:"target"`
	Cols   int       `json:"cols"`
	Rows   int       `json:"rows"`
}

type sessionTargetDTO struct {
	// SessionID is "" for an observed interactive session (RHZ-143: claude
	// reports it by pid only and it is not in the registry).
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name"`
	MissionID string `json:"missionId,omitempty"`
	Mode      string `json:"mode"`
	Cwd       string `json:"cwd"`
	// RHZ-142: Attachable = a claude background session with a known short
	// id (an interactive one runs in some other terminal); Kind is the claude
	// session kind ("" = a launcher -p session); Status the last observed one.
	Attachable bool   `json:"attachable"`
	Kind       string `json:"kind,omitempty"`
	Status     string `json:"status,omitempty"`
	Pid        int    `json:"pid,omitempty"`
}

type listDTO struct {
	// SessionTargets is false when no launcher claudePath is configured.
	SessionTargets bool               `json:"sessionTargets"`
	Sessions       []sessionTargetDTO `json:"sessions"`
	// Logs are launcher runs openable read only (RHZ-150); empty without a
	// launcher logDir.
	Logs      []LogTarget `json:"logs"`
	Terminals []Info      `json:"terminals"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid json body")
	}
	if d.More() {
		return errors.New("invalid json body")
	}
	return nil
}

// Handler serves /v1/terminals. enabled=false (a non-loopback bind) answers
// 501 on every route; a non-loopback peer is 403.
func (h *Host) Handler(enabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !enabled {
			http.Error(w, "terminal bridge requires a loopback bind", http.StatusNotImplemented)
			return
		}
		if !loopbackRemote(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/terminals" {
			switch r.Method {
			case http.MethodGet:
				h.serveList(w)
			case http.MethodPost:
				h.serveOpen(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/terminals/"), "/")
		if len(parts) > 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		t, err := h.get(parts[0])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		op := ""
		if len(parts) == 2 {
			op = parts[1]
		}
		switch {
		case op == "" && r.Method == http.MethodDelete:
			t.Close()
			code, exited := t.Exit()
			writeJSON(w, http.StatusOK, map[string]any{"terminalId": t.ID, "exited": exited, "exitCode": code})
		case op == "stream" && r.Method == http.MethodGet:
			serveStream(w, r, t)
		case op == "input" && r.Method == http.MethodPost:
			var in struct {
				Data string `json:"data"`
			}
			if err := decode(r, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b, err := base64.StdEncoding.DecodeString(in.Data)
			if err != nil {
				http.Error(w, "data must be base64", http.StatusBadRequest)
				return
			}
			if err := t.Input(b); errors.Is(err, ErrReadOnly) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			} else if err != nil {
				http.Error(w, "terminal input failed", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case op == "resize" && r.Method == http.MethodPost:
			var in struct {
				Cols int `json:"cols"`
				Rows int `json:"rows"`
			}
			if err := decode(r, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !validSize(in.Cols, in.Rows) {
				http.Error(w, "cols and rows must be 1..1000", http.StatusBadRequest)
				return
			}
			if err := t.Resize(in.Cols, in.Rows); errors.Is(err, ErrReadOnly) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			} else if err != nil {
				http.Error(w, "terminal resize failed", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func (h *Host) serveList(w http.ResponseWriter) {
	out := listDTO{SessionTargets: h.ClaudePath != "", Sessions: []sessionTargetDTO{}, Terminals: h.List()}
	ss, err := OpenSessions(h.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if out.Logs, err = h.LogTargets(time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, s := range ss {
		d := sessionTargetDTO{SessionID: s.SessionID, Name: s.Name, MissionID: s.MissionID, Mode: s.Mode, Cwd: s.Cwd, Attachable: s.AttachID != "", Kind: s.Kind}
		if h.Status != nil {
			d.Status = h.Status(s.SessionID)
		}
		out.Sessions = append(out.Sessions, d)
	}
	if h.Interactive != nil {
		for _, a := range h.Interactive() {
			out.Sessions = append(out.Sessions, sessionTargetDTO{Name: a.Name, Mode: session.Interactive, Cwd: a.Cwd, Kind: a.Kind, Status: a.StatusText(), Pid: a.Pid})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Host) serveOpen(w http.ResponseWriter, r *http.Request) {
	var in openDTO
	if err := decode(r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var t Target
	if in.Target.Session != nil {
		t.Session = in.Target.Session.SessionID
		if t.Session == "" {
			http.Error(w, "target.session.sessionId required", http.StatusBadRequest)
			return
		}
	}
	if in.Target.Shell != nil {
		t.Shell = in.Target.Shell.Cwd
		if t.Shell == "" {
			http.Error(w, "target.shell.cwd required", http.StatusBadRequest)
			return
		}
	}
	if in.Target.Execution != nil {
		t.Execution = in.Target.Execution.ExecutionID
		if t.Execution == "" {
			http.Error(w, "target.execution.executionId required", http.StatusBadRequest)
			return
		}
	}
	term, err := h.Open(t, in.Cols, in.Rows)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, map[string]string{"terminalId": term.ID})
	case errors.Is(err, ErrSessionNotOpen), errors.Is(err, ErrExecutionAbsent):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrSessionsDisabled), errors.Is(err, ErrClosed), errors.Is(err, ErrNotAttachable), errors.Is(err, ErrLogsDisabled):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

type chunkDTO struct {
	Data string `json:"data"`
}

// serveStream is SSE: one snapshot (the ring, base64), then output chunks,
// then exit {exitCode}. A subscriber that falls behind is cut off without an
// exit event and reconnects for a fresh snapshot.
func serveStream(w http.ResponseWriter, r *http.Request, t *Terminal) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(kind string, v any) {
		b, _ := json.Marshal(v)
		w.Write([]byte("event: " + kind + "\ndata: " + string(b) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	snap, ch, exited, code := t.Subscribe()
	send("snapshot", chunkDTO{base64.StdEncoding.EncodeToString(snap)})
	if exited {
		send("exit", map[string]int{"exitCode": code})
		return
	}
	defer t.Unsubscribe(ch)
	for {
		select {
		case <-r.Context().Done():
			return
		case p, ok := <-ch:
			if !ok {
				if code, exited := t.Exit(); exited {
					send("exit", map[string]int{"exitCode": code})
				}
				return
			}
			send("output", chunkDTO{base64.StdEncoding.EncodeToString(p)})
		}
	}
}
