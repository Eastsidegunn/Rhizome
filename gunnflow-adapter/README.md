# gunnflow-adapter

`gunnflow-adapter` translates Rhizome's HTTP surface into the Gunnflow cockpit
wire contract. It provides the direct-wire server in `src/serveDirect.ts`.

## Requirements and install

Node.js 22.12 or newer is required. The Gunnflow contract packages are installed
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
