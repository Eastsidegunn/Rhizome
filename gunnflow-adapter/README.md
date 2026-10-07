# gunnflow-adapter

`gunnflow-adapter` translates Rhizome's HTTP surface into the Gunnflow cockpit
wire contract. It provides the direct-wire server in `src/serveDirect.ts`.

Gate bodies may use the canonical `##`/`###` sections `문제 상태`, `권고`,
`승인 입력`, `선택지`, `되돌림`, `판단 정보`, and `연결` (or their supported
synonyms). `GET /detail/:nodeId` exposes recognised sections in that fixed,
conclusion-first order as plain-text detail items and keeps other prose in the
`request` item. Inline code spans and fenced blocks are passed through
unchanged. A structured `권고` replaces the legacy `recommendation` item.

## Requirements and install

Node.js 22 or newer is required. The Gunnflow contract packages are installed
from vendored tarballs in `vendor/`; they come from the Gunnflow project
under MIT (license notice: `vendor/LICENSE-gunnflow`).

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
