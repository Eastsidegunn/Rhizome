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

- only claude `background` entries are journaled. A claude `interactive`
  entry (a session in some other terminal) carries no ids — `claude agents
  --json` reports it as `{name, kind, status, cwd, pid}` — so serve keeps it
  in memory only, keyed by pid, and never writes it (RHZ-143); an
  interactive stream journaled by RHZ-142 is closed on the first tick;
- an unknown or closed background session is opened (`mode interactive`, no
  mission unless the stream already had one) with its `attachId`, except one
  closed `ceiling-time`, which stays closed;
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

Ceilings of an interactive launch (RHZ-143, FR-RHZ-187):

- **Dollars are not enforced.** `--max-budget-usd` is a `claude -p` flag;
  `claude --bg` has none, so `ceiling.usd` / `budget.usd` bound only the
  recorded policy of an interactive execution, never its spend.
- **Time is.** Every 15s serve checks the open launched sessions (registry
  `kind background` with an `executionId`); one whose acceptance is older
  than its effective `timeMs` is stopped with `claudePath stop <attachId>`
  and closed `{reason: "ceiling-time"}`. A failed stop writes nothing and is
  retried. The check reads the journal, so it survives a serve restart.
- **Count.** The ledger's optional `maxInteractive` (default 2) caps the open
  launched sessions plus in-flight launches; at the cap a start is refused
  (`session launcher busy: N interactive open (maxInteractive M)`, zero
  writes). `maxConcurrent` counts `-p` children only.
- The start lock covers the checks, intent/claim and the slot reservation;
  `claude --bg`, the acceptance and `claude agents` run outside it. An
  identical start meanwhile is refused `session launch in progress (<id>)`.

`GET /v1/terminals` lists `sessions[]{…, attachable, kind, status}`
(`status` = last observed, not journaled) and, after the registry sessions,
the observed interactive ones as `{name, mode, cwd, kind: "interactive",
status, pid, attachable: false}` with no `sessionId`. A session target
attaches by `attachId`; every session without one (a launcher `-p` run, an
interactive stream) is 409 — the same answer the list's `attachable: false`
gives.

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
