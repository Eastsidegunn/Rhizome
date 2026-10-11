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

## Observation and interactive launch (RHZ-142, FR-RHZ-186)

`session.opened` may carry `kind` (`background|interactive`, the claude
session kind of a session claude itself manages) and `attachId` (the short
id `claude attach` takes). A launcher `-p` session has neither.

serve runs `claudePath agents --json` every `-session-observe-interval`
(default 15s, `0` = off; needs the launcher ledger) and reconciles through
`session.opened`/`session.closed` only — an unchanged observation writes
nothing, a failed run writes nothing:

- an unknown or closed session is opened (`mode interactive`, no mission
  unless the stream already had one); a claude `background` session gets its
  `attachId`, an `interactive` one (another terminal) does not;
- an open session whose `kind`/`attachId` changed is closed
  (`observed: session changed`) and reopened with the observed values, links
  kept;
- an open observed session (`kind` set) missing from the observation is
  closed (`observed: session ended`); launcher `-p` sessions are never
  touched.

`mission.start` with `sessionMode: "interactive"` (session launcher) runs
`claude --bg <prompt> --session-id <uuid> --model … --permission-mode …
[--allowedTools …]` with the usual session env (TERM=xterm-256color) and
waits for it to exit, then records the session with the short id found in
`claude agents --json`. No child is tracked and no outcome journaled: the
session ends when a human ends it and the observation closes it.

`GET /v1/terminals` lists `sessions[]{…, attachable, kind, status}`
(`status` = last observed, not journaled); a session target attaches by
`attachId` when present; a claude `interactive` session is 409.

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
