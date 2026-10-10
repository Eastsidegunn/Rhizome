// Package secretplane is the storage library of the secret plane (RHZ-107 G1,
// mission M5a, FR-RHZ-138): the plane state directory, the operation ledger
// with its outbox, the values file, the quarantine file, startup recovery and
// the epoch-replacement state machine (SCP §3.7 P1–P5a).
//
// It has no socket and no terminal; cmd/rhizome-secret-plane (M5b) owns
// those and calls the API here. Every operation that completes a human action
// (Begin/Commit, RegisterDirect, DeclareAttested, Freeze) must be driven by
// that interactive front end only; this package offers no non-interactive
// completion path (split plan §4, D19).
//
// Durability: every ledger and quarantine line is written and fsynced before
// the call returns. A failed fsync (or any write that may have left a partial
// line) makes the Plane dead: the call returns ErrFatal, OnFatal runs once,
// and every later call returns ErrFatal. The caller must exit the process;
// restart runs recovery (P3, P4). The library never retries.
//
// Error strings are fixed enum codes; none carries an input byte, a number,
// a path or a value (SCP S5).
package secretplane

import "errors"

// Banner is the startup notice the plane process prints (split plan M5a
// risk note): the values file is protected only by its file mode.
const Banner = "rhizome-secret-plane: local 0600 file = no same-user protection. " +
	"Values are kept in a mode 0600 file in the plane state directory; any process " +
	"running as the same OS user can read them."

// Errors. Every message is a fixed code.
var (
	ErrStateDirUnsafe    = errors.New("secretplane: state-dir-unsafe")
	ErrFileUnsafe        = errors.New("secretplane: file-mode")
	ErrBusy              = errors.New("secretplane: busy")
	ErrUnsupported       = errors.New("secretplane: unsupported-platform")
	ErrIO                = errors.New("secretplane: io")
	ErrLedgerCorrupt     = errors.New("secretplane: ledger-corrupt")
	ErrQuarantineCorrupt = errors.New("secretplane: quarantine-corrupt")
	ErrValuesCorrupt     = errors.New("secretplane: values-corrupt")
	ErrFatal             = errors.New("secretplane: fatal")
	ErrClosed            = errors.New("secretplane: closed")

	ErrInvalidPending  = errors.New("secretplane: invalid-pending")
	ErrInvalidInput    = errors.New("secretplane: invalid-input")
	ErrInvalidDecision = errors.New("secretplane: invalid-decision")
	ErrUnknownOp       = errors.New("secretplane: unknown-op")
	ErrUnknownRef      = errors.New("secretplane: unknown-ref")
	ErrDone            = errors.New("secretplane: done")
	ErrOpBusy          = errors.New("secretplane: op-busy")
	ErrDisplayOnly     = errors.New("secretplane: display-only")
	ErrFrozen          = errors.New("secretplane: frozen")
	ErrEpochPhase      = errors.New("secretplane: epoch-phase")

	ErrHalted     = errors.New("secretplane: halted")
	ErrStalePlane = errors.New("secretplane: stale-plane")
)
