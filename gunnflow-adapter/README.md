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
are capped to the last N by emitted (gate-ID) order — NOT recency; a
decision-time field will replace this. N defaults to 10 and is configurable
with `RHIZOME_UNVERIFIED_DECIDED_MAX`; the root detail counts the full uncapped
set. A `claimed` status with a missing or unknown claim kind displays as
`claimed`. A future `verified` status displays as `verified`, followed by
` (assurance)` when assurance is present.

## Requirements and install

Node.js 22.12 or newer is required. The Gunnflow contract package
`@gunnflow/contract` is installed from npm (MIT). `@gunnflow/upstream-port` is
not published yet and is still installed from the vendored tarball in
`vendor/` (license notice: `vendor/LICENSE-gunnflow`).

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
