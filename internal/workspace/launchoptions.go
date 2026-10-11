package workspace

// RHZ-148 (FR-RHZ-192): GET /v1/launcher/options — the choices a cockpit may
// offer when it starts a mission (mission.start sessionMode / workdir). It
// names the ledger's workdirs by key only; their directories never leave
// serve. Read-only: GET never writes.

import (
	"encoding/json"
	"net/http"
)

// LaunchOptions is the mission.start choice set of the wired execution
// backend. Models lists the ledger's model (mission.start carries no model
// field; the ledger's one model is what a session runs with).
type LaunchOptions struct {
	SessionModes       []string `json:"sessionModes"`
	DefaultSessionMode string   `json:"defaultSessionMode"`
	Workdirs           []string `json:"workdirs"`
	DefaultWorkdir     string   `json:"defaultWorkdir"`
	Models             []string `json:"models"`
	DefaultModel       string   `json:"defaultModel"`
}

// serveLaunchOptions answers GET /v1/launcher/options; no options wired = 404.
func (h *HTTPServer) serveLaunchOptions(w http.ResponseWriter, r *http.Request) {
	if h.LaunchOptions == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.LaunchOptions)
}
