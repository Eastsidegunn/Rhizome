# Session registry and terminal driver contract (RHZ-139, FR-RHZ-183)

## Registry

Every agent session Rhizome spawns is recorded on the `session/<sessionId>`
stream. The board (serve) is the single writer.

| event | payload |
|---|---|
| `session.opened` | `{sessionId, name, missionId?, mode: background\|interactive, cwd, driverRef?, executionId?}` |
| `session.closed` | `{sessionId, reason}` |

A stream may cycle `opened → closed → opened` (a gate resume relaunches the
same claude session id). Background sessions of the session launcher carry
`executionId`: the run itself stays on its `execution/<id>` stream.

`GET /v1/workspace` projects `sessions[]{sessionId, name, missionId, mode,
state, openedAt, driverRef}` (omitted when no session exists). An open session
whose mission is terminal raises attention kind `session_stale` (human
cleanup). The intent `session.close {sessionId, reason}` closes a session.

## Terminal driver

The core never names a terminal. Interactive tabs are handled by an external
executable named in the launcher ledger (`config.json`):

```json
{ "terminalDriver": "/absolute/path/to/driver" }
```

One invocation per op. Rhizome writes **one JSON line** to the driver's stdin
and reads **one JSON object** from its stdout. The driver must exit 0 within
**10 s**. A non-zero exit, a timeout, invalid JSON, trailing data, a missing
`ok` or `ok: false` is a fixed error (`terminal driver failed` / `timed out`
/ `returned an invalid response`); the driver's output is never echoed and
Rhizome writes nothing to the journal. stderr is discarded.

| op | stdin | stdout |
|---|---|---|
| open | `{"op":"open","name":…,"cwd":…,"command":[argv…],"env":{"RHIZOME_BOARD":…,"RHIZOME_MISSION":…,"RHIZOME_SESSION":…}}` | `{"ok":true,"driverRef":"<non-empty>"}` |
| list | `{"op":"list"}` | `{"ok":true,"tabs":[{"driverRef":…,"name":…,"cwd":…,"alive":true}]}` |
| close | `{"op":"close","driverRef":…}` | `{"ok":true}` |
| send | `{"op":"send","driverRef":…,"text":…}` | `{"ok":true}` |

`driverRef` is opaque to Rhizome. Terminal-specific drivers live outside this
repository.

## CLI

- `rhizome session list` — registry sessions reconciled with the driver's
  `list`: `missing` = registered open but the driver has no live tab,
  `unregistered` = a driver tab no open session claims.
- `rhizome session close <sessionId>` — driver `close` (interactive), then
  `session.close`.
- `rhizome session send <sessionId> <text>` — driver `send`.

The CLI reads the driver path from the launcher ledger (`-config`, default
the launcher's ledger path).
