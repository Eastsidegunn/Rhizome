# Rhizome

Rhizome is an event-sourced coordinator, or "board", for work done by AI agents.
It keeps the long-lived state of that work: **goals** (outcomes), **missions**
(work toward a goal), **gates / questions** (decisions that need a human),
**notes / knowledge** (memories, relations, procedures), **deliverables**
(registered outputs), and **executions** (references to agent runs in an
external execution system).

Every change is an event appended to a single journal. There is exactly one
writer, the journal is append-only, and all state is derived by replaying it.

> Public-facing files are in English. Some code comments are in Korean.

## Why

Agents can do a lot of work, but people still need to decide what to do, what an
agent may touch, and when the work counts as done. Rhizome is the record-keeping
and governance layer for that loop:

- **A human stays in the loop.** Agents and tools submit *intents*. Decisions
  that need a person become gates, and a gate's answer is a durable event with
  the decider recorded on it.
- **Permissions only narrow.** The chain runs person/org policy → goal → mission
  → execution, and each step can only narrow what the step above allows (allow
  lists are intersected, budgets take the minimum). An execution records the
  ceiling and profile it ran under.
- **Everything can be audited and rebuilt.** Projections, indexes and views are
  caches. Delete them and they are rebuilt from the journal.

## Status

**Alpha.** Rhizome is run by a single operator on a local machine. HTTP and
intent APIs change without notice, there are no releases or tags yet, and
nothing here is tuned or hardened for multi-user production use. A proposed
first version is described in [docs/release/0.1.0.md](docs/release/0.1.0.md).

## 30-second start

You need macOS or Linux and Go 1.23 or newer, and nothing else: the Go module
has no third-party dependencies, and the board needs no other repository, service or environment
variable. (Node.js 22+ is only needed for the optional
[Gunnflow adapter](#gunnflow-adapter-optional).) Windows is not supported yet:
the journal's single-writer lock uses `flock(2)`.

```sh
git clone https://github.com/Eastsidegunn/Rhizome.git
cd Rhizome
go build -o rhizome ./cmd/rhizome

# Start the board. -journal is required (the file is created if missing);
# -addr defaults to 127.0.0.1:8080. Port 8790 is used here because it is the
# adapter's default upstream; any free port works.
./rhizome serve -journal ./dev.ndjson -addr 127.0.0.1:8790
```

In another shell:

```sh
# Register a goal (an outcome with a success criterion)
curl -s -X POST http://127.0.0.1:8790/v1/intent \
  -H 'Content-Type: application/json' \
  -d '{"kind":"goal.create","name":"hello","success":"the quickstart works","actor":"operator"}'
# -> {"Accepted":true,"Reason":""}

# Put a mission under it
curl -s -X POST http://127.0.0.1:8790/v1/intent \
  -H 'Content-Type: application/json' \
  -d '{"kind":"mission.create","name":"first","prompt":"say hello","goalId":"goal-hello","actor":"operator"}'

# Read the projected board state: {revision, body:{missions, tasks, gates, ...}}
curl -s http://127.0.0.1:8790/v1/workspace
```

Each accepted intent appends events to `dev.ndjson`. Stop the server and start
it again: the same state comes back from replay. The `/v1/workspace` projection
uses the vocabulary of the cockpit UI it serves, so a Rhizome goal appears under
`missions` and a Rhizome mission appears under `tasks`.

Other subcommands: `ingest`, `memories`, `index`, `graph` (see
[cmd/rhizome/main.go](cmd/rhizome/main.go)). Only one process may write a
journal at a time: while `serve` runs, send writes through `POST /v1/intent`.

Main HTTP routes: `GET /v1/workspace` (+ `/v1/workspace/stream` SSE),
`POST /v1/intent`, `GET /v1/knowledge`, `GET /v1/context`,
`GET /v1/execution/{missionId}` (+ `/stream`), `POST /v1/blob`,
`GET /v1/blob/{id}`, `GET /v1/codeindex`. Intent kinds include `goal.*`,
`mission.*`, `question.ask`, `gate.approve|reject|requestChanges`,
`note.create`, `knowledge.*`, `procedure.define|run`, `edge.declare|rewire`,
`deliverable.register` and `task.instruct`. They are implemented in
[internal/workspace/workspace.go](internal/workspace/workspace.go).

## Configuration

Rhizome reads no environment variables and no config file. Everything is a
command-line flag.

### `rhizome serve` flags

| Flag | Default | Meaning |
|---|---|---|
| `-journal <file>` | *(required)* | NDJSON event journal. Created if missing, locked while `serve` runs. |
| `-addr <host:port>` | `127.0.0.1:8080` | Listen address. There is no authentication, so keep it on loopback. |
| `-blobs <dir>` | *(off)* | Content-addressed blob store. Enables `POST /v1/blob` and `GET /v1/blob/{id}`. |
| `-index-repo <dir>` and `-index-out <dir>` | *(off)* | Code index of a git checkout, served at `GET /v1/codeindex`. Give both or neither. |
| `-janus-*` | *(off)* | Optional JANUS execution wiring, see below. |

### JANUS execution (advanced, optional)

Without any `-janus-*` flag, the board works fully but real agent executions are
not wired (`mission.start` is rejected). Wiring them needs an external JANUS
installation (the `hx` CLI) that you have set up yourself. All paths below are
placeholders: set the variables to your own files before running.

```sh
HX=/path/to/hx                              # JANUS hx binary
APPROVAL_SOCK=/path/to/approval.sock        # JANUS approval socket (JANUS creates it)
PROFILE=/path/to/profile.yaml
ACCEPT_ROOT=/path/to/accept-dir
WORLD_CONFIG=/path/to/world-config.json
EXEC_CONFIG=/path/to/exec-config.json       # optional: enables mission.start

./rhizome serve -journal ./dev.ndjson -addr 127.0.0.1:8790 \
  -janus-hx "$HX" \
  -janus-approval-endpoint "$APPROVAL_SOCK" \
  -janus-profile "$PROFILE" \
  -janus-accept-root "$ACCEPT_ROOT" \
  -janus-world-config "$WORLD_CONFIG" \
  -janus-exec-config "$EXEC_CONFIG"
```

| Flag | Default | Meaning |
|---|---|---|
| `-janus-hx` | — | Path to the `hx` binary. |
| `-janus-approval-endpoint` | — | Absolute path of the JANUS approval endpoint. |
| `-janus-profile`, `-janus-accept-root`, `-janus-world-config` | — | Passed to `hx run` as `--profile`, `--accept-root`, `--world-config`. |
| `-janus-session-db` | *(empty)* | Optional JANUS session database to observe. |
| `-janus-observe-interval` | `5s` | Observation poll interval. |
| `-janus-idle-timeout` | `10m` | Stop a session after this long without activity. |
| `-janus-exec-config` | *(off)* | Operator-owned JSON ledger (adapter, profile id/hash, workspace, scope, session mode, ceiling) that enables `mission.start`. Requires the five flags above. |

`-janus-hx`, `-janus-approval-endpoint`, `-janus-profile`, `-janus-accept-root`
and `-janus-world-config` go together: giving only some of them is an error, so
nothing is silently disabled. The execution adapter speaks JANUS's CLI and
NDJSON protocol.

The execution adapter IDs currently accepted are `claudecode` and `codex`.
Adding another ID requires a code change in `validAdapter` in
`internal/janusadapter/run.go`.

### Gunnflow adapter (optional)

[gunnflow-adapter/](gunnflow-adapter/) serves the Gunnflow direct wire in front
of a running board. It needs Node.js 22+. This is a source-only integration:
the package is private (not published to npm) and installs its Gunnflow contract
packages from the vendored tarballs in `gunnflow-adapter/vendor/` via `npm ci`.

```sh
cd gunnflow-adapter
npm ci
RHIZOME_URL=http://127.0.0.1:8790 PORT=8792 npx tsx src/serveDirect.ts
```

| Variable | Default | Meaning |
|---|---|---|
| `RHIZOME_URL` | `http://127.0.0.1:8790` | Base URL of the running `rhizome serve`. |
| `PORT` | `8792` | Port the adapter listens on (bound to `127.0.0.1`). |

## Build and test

```sh
make ci                                          # gofmt check + go test + go test -race + go vet
cd gunnflow-adapter && npm ci && npm test        # TypeScript adapter: tsc + vitest
```

`make ci` is the completion gate for every change. CI runs both commands on
every push to `main` and on every pull request (`.github/workflows/ci.yml`).

## Architecture

Rhizome is one Go binary. The core is a journal of events
(`internal/journal`, `internal/events`) behind a single-writer boundary. Domain
services append events, and projections replay them. Internal packages are
split into layers, and the dependency direction is fixed:

```
                 surface        workspace, surface      (HTTP/SSE, intent relay)
                    |
                 assembly       assembly                (procedure templates -> instances)
                    |
                 bridge         trace, deliverable, edge
                /        \
 execution kernel        knowledge kernel
 mission, domain,        memory, source, knowledge,
 decision, coordinator,  relation, procedure,
 wake, execution,        retrieval, evaluation
 approval, question, ...
                \        /
                 substrate      events, journal, blob, policy, codeindex
```

Read it as `substrate <- {execution kernel || knowledge kernel} <- bridge <-
assembly <- surface`: a package may import only the layers below it, and the two
kernels never import each other. `internal/archtest` enforces this in CI. It
classifies every internal package, and its exception list must match exactly
(an unlisted violation fails, and so does an exception that no longer applies).

## Sibling projects

Rhizome is one of three repositories. **Gunnflow** is the human cockpit UI.
**Rhizome** is the board and coordinator. **JANUS** (its CLI is `hx`) runs agent
sessions in isolation, enforces execution policy and approvals, and keeps the
authoritative execution log. Rhizome never imports code from either one. It
calls JANUS through its CLI and NDJSON protocol (`internal/janusadapter`), and it
serves Gunnflow over HTTP, with the TypeScript translation layer in
[gunnflow-adapter/](gunnflow-adapter/). Rhizome stores references to JANUS
executions. It does not copy JANUS's execution facts into a second
authoritative record.

## Repository map

| Path | What |
|---|---|
| `cmd/rhizome` | the `rhizome` binary (serve, ingest, memories, index, graph) |
| `internal/` | all Go packages, layered as above |
| `internal/archtest` | the architecture test that enforces the layer rules |
| `gunnflow-adapter/` | TypeScript adapter from Rhizome's HTTP surface to the Gunnflow wire contract |
| `docs/release/` | release notes |

Contributions and changes are described in [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT — see [LICENSE](LICENSE).
