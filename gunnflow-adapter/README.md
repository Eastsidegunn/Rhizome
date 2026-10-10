# gunnflow-adapter

`gunnflow-adapter` translates Rhizome's HTTP surface into the Gunnflow cockpit
wire contract. It provides the direct-wire server in `src/serveDirect.ts`.

Gate bodies may use the canonical `##`/`###` sections `문제 상태`, `권고`,
`승인 입력`, `선택지`, `되돌림`, `판단 정보`, and `연결` (or their supported
synonyms). `GET /detail/:nodeId` exposes recognised sections in that fixed,
conclusion-first order as plain-text detail items and keeps other prose in the
`request` item. Inline code spans and fenced blocks are passed through
unchanged. A structured `권고` replaces the legacy `recommendation` item.

Gate decision provenance from `/v1/workspace` and its SSE stream is shown as
`승인 상태` in approved/rejected gate detail. Unverified and `legacy-asserted`
terminal decisions remain visible with `approval_unverified` attention. They
are capped to the N most recent by `decidedAt`; gate-ID order breaks ties and
is the fallback for older servers that omit it. `decidedAt` is RFC3339 UTC
with an optional variable-length fraction (trailing zeros trimmed), so compare
it as an instant, never as a string. A JANUS gate carries it only once JANUS
has observed the decision; a human input not yet observed still shows as
pending without it. N defaults to 10 and is
configurable with `RHIZOME_UNVERIFIED_DECIDED_MAX`; the root detail counts the
full uncapped set. A `claimed` status with a missing or unknown claim kind
displays as `claimed`. A future `verified` status displays as `verified`,
followed by ` (assurance)` when assurance is present.
Retained decisions carry the exact `decidedAt` string as attention `since`,
and any approved or rejected gate with `decidedAt` shows it as a `결정 시각`
detail item; `decidedAt` is never added to the closed node shape.

Since FR-RHZ-171, `/v1/workspace` `attention[].kind` also includes
`gate_pending` (pending internal gate), `request_waiting` (waiting request) and
`note_blocked` (a `blocked`-kind or `blocked`/`h-request`-tagged note on an
open mission), and attention entries may carry an optional `missionId`.
`counts.needsYou` counts these plus waiting missions, once per id. The adapter
keeps the first two as causes on the existing gate/request nodes and projects
each `note_blocked` entry as a `note` node (label = first line, state
`blocked`, `member-of` its mission when visible) with a `note.answer` action;
its detail is the full note text from `/v1/context?mission=`, and an answer is
sent as `note.create` with `memoryKind: answer` and tags `answer`, `re:<note id>`.

## Requirements and install

Node.js 22.12 or newer is required. Both Gunnflow packages,
`@gunnflow/contract` and `@gunnflow/upstream-port`, are installed from npm
(MIT); nothing is vendored.

```sh
npm ci
```

## Run

`RHIZOME_URL` defaults to `http://127.0.0.1:8790` and `PORT` defaults to
`8792`.

```sh
npx tsx src/serveDirect.ts
```

## Test

```sh
npm test
```
