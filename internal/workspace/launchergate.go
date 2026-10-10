package workspace

// RHZ-124 S2 (FR-RHZ-124-S2): the vocabulary RelayHooks.GateDecided shares
// with the session launcher. Workspace never imports the launcher; the
// correlation prefix is the only shared word.

// LauncherGatePrefix marks the correlation id of a session-launcher denial
// gate.
const LauncherGatePrefix = "launcher:"

// GateDecision is one recorded internal gate decision as handed to
// RelayHooks.GateDecided.
type GateDecision struct {
	GateID, CorrelationID, MissionID string
	// Decision is "approve" or "reject".
	Decision, Reason, Actor string
}
