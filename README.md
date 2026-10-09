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
intent APIs change without notice, and nothing here is tuned or hardened for
multi-user production use. The first release, 0.1.0, is described in
[docs/release/0.1.0.md](docs/release/0.1.0.md).

## 30-second start

You need macOS or Linux and Go 1.23 or newer, and nothing else: the Go module
depends only on `golang.org/x/sys` and `golang.org/x/term` for peer credentials
and terminal I/O, and the board needs no other repository or service. (Node.js
22.12+ is only needed for the optional
[Gunnflow adapter](#gunnflow-adapter-optional).) Windows is not supported yet:
the journal's single-writer lock uses `flock(2)`.

```sh
git clone https://github.com/Eastsidegunn/Rhizome.git
cd Rhizome
go build -o rhizome ./cmd/rhizome

# Start the board with the per-user data directory (journal and blobs are
# created there); -addr defaults to 127.0.0.1:8080. Port 8790 is used here
# because it is the adapter's default upstream; any free port works.
./rhizome serve -addr 127.0.0.1:8790

# Or keep data in an explicitly chosen location:
./rhizome serve -data-dir ./rhizome-data -addr 127.0.0.1:8790
# Existing scripts may continue to name the journal explicitly:
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

Each accepted intent appends events to `journal.ndjson` (or to the explicitly
named journal). Stop the server and start it again: the same state comes back
from replay. The `/v1/workspace` projection
uses the vocabulary of the cockpit UI it serves, so a Rhizome goal appears under
`missions` and a Rhizome mission appears under `tasks`.

Other subcommands: `ingest`, `memories`, `index`, `graph` (see
[cmd/rhizome/main.go](cmd/rhizome/main.go)). Only one process may write a
journal at a time: while `serve` runs, send writes through `POST /v1/intent`.

Main HTTP routes:

| Method and path | Purpose |
|---|---|
| `GET /v1/workspace` | Project the operator board; `/v1/workspace/stream` is its SSE requery signal. |
| `POST /v1/intent` | Relay one direct, local-trust write intent. |
| `GET /v1/knowledge` | Project `{notes,items,relations}` from one revision; each relation is `{id,type,from,to,sourceMemoryIds,confidence}` in that order; `items` lists every knowledge item including superseded ones (tell them apart by `status` and `supersedes`). `kind` filters notes by memory kind; `itemKind` filters items by the knowledge-kind vocabulary and an unknown value returns `400 invalid knowledge kind`; `tag` filters both notes and items. When `itemKind` or `tag` is present, relations survive only when both endpoint items survive; without either item filter, all relations are returned. `about=<memoryId>` retains the separate reverse-about response. |
| `GET /v1/context` | Project task handoff context, or note context with `goal` / `mission`. |
| `GET /v1/execution/{missionId}` | Project execution output; `/stream` is the streaming form. |
| `POST /v1/blob`, `GET /v1/blob/{id}` | Store and read content-addressed blobs. |
| `GET /v1/codeindex` | Read the derived code index. |

Direct intent kinds are implemented in
[internal/workspace/workspace.go](internal/workspace/workspace.go):

| Intent kind | Purpose |
|---|---|
| `goal.*`, `mission.*`, `task.pause`, `task.resume`, `task.instruct` | Operate goals and missions. |
| `question.ask`, `gate.approve`, `gate.reject`, `gate.requestChanges` | Open and decide gates. |
| `note.create`, `knowledge.create`, `knowledge.promote`, `procedure.define`, `procedure.run` | Author notes, structured knowledge, and procedures. |
| `relation.create` | Create a typed knowledge relation from lowerCamel `{from,to,relationType,sourceMemoryIds,confidence,actor}`. `actor` and all graph fields are required; a zero/omitted confidence defaults to `0.5`. Resubmitting an identical relation is idempotent. |
| `edge.declare`, `edge.rewire`, `deliverable.register` | Link operational nodes and register outputs. |
| `request.create`, `request.complete`, `request.unable`, `request.cancel`, `attest.create` | Record human work and signed attestations. |

## Data location

The data directory is selected in this order: `-data-dir`, then
`RHIZOME_DATA_DIR`, then the operating-system default. Relative flag and
environment paths are made absolute from the current working directory. The
defaults are macOS `$HOME/Library/Application Support/rhizome` and Linux/Unix
`$XDG_DATA_HOME/rhizome` when `XDG_DATA_HOME` is absolute, otherwise
`$HOME/.local/share/rhizome`. (The resolver also knows the Windows location,
`%LocalAppData%\rhizome`, for when Windows is supported.)

The directory contains `journal.ndjson`, its adjacent `journal.ndjson.lock`
while a writer is running, `blobs/` when the default blob store is enabled, and
the optional derived code index under `index/`. Derived files can be rebuilt
from the journal. The standalone `index` and `graph` commands require an
explicit `-out`; `graph` uses the data directory only to default its journal
when `-journal` is omitted.

To migrate an existing in-repository data folder:

1. Stop `rhizome serve` with `SIGTERM` and wait until the process is gone. Never
   delete the `.lock` file by hand.
2. Rename the existing journal file to `journal.ndjson` and the blob directory
   to `blobs/`, then move them (and any derived `index/` or notes) into the
   selected data directory. Alternatively, keep passing `-journal <file>` and
   `-blobs <dir>` explicitly.
3. If serving the code index, pass both `-index-repo <repo>` and
   `-index-out <data>/index` explicitly; the code index is not inferred from
   the data directory.
4. Restart with `-data-dir <dir>` or the default location.
5. Verify that `GET /v1/workspace` reports the same `revision` as before.

## Configuration

Rhizome reads the data-directory environment variable described above and no
config file. Everything else is a command-line flag.

### `rhizome serve` flags

Invalid `serve` flag syntax exits with status 2 and prints only
`serve: usage`; argument values are not echoed. `serve -h` and `serve -help`
still print the full static flag list and exit with status 2.

| Flag | Default | Meaning |
|---|---|---|
| `-data-dir <dir>` | OS/user default | Data directory used when `-journal` is omitted. |
| `-journal <file>` | `<data-dir>/journal.ndjson` | NDJSON event journal. Created if missing, locked while `serve` runs. |
| `-addr <host:port>` | `127.0.0.1:8080` | Listen address. There is no authentication, so keep it on loopback. |
| `-blobs <dir>` | `<data-dir>/blobs` when `-journal` is omitted; otherwise *(off)* | Content-addressed blob store. Enables `POST /v1/blob` and `GET /v1/blob/{id}`. |
| `-index-repo <dir>` and `-index-out <dir>` | *(off)* | Code index of a git checkout, served at `GET /v1/codeindex`. Give both or neither. |
| `-trust-anchor <file>` | *(off)* | Validate the journal trust root against an operator-owned public-key anchor and verify signed gate decisions. |
| `-trust-enforce-janus=all` | *(off)* | Require verified signatures for both allow and deny decisions on every JANUS gate. Requires `-trust-anchor`. |
| `-janus-*` | *(off)* | Optional JANUS execution wiring, see below. |

### Signed gate verification

With `-trust-anchor`, `serve` records or validates the journal's trust genesis
and projects valid signed decisions as `verification.status:"verified"`.
An operator can also append a signed `attest.recorded` manifest through the
direct `attest.create` intent to attest terminal decisions that were originally
recorded without signatures; those decisions project as
`verification.status:"attested"`. The intent carries the signer-computed
`manifestDigest` alongside its exact lowerCamel `items` and `signature`
objects, and Rhizome rejects it if recomputation differs. The loopback-only,
read-only `GET /v1/trust/signing?gate=<id>` endpoint returns canonical signer
input, and `?unverified=1` lists terminal decisions eligible for a manifest.
`-trust-enforce-janus=all` additionally rejects unsigned JANUS allow and deny
inputs and rechecks them immediately before dispatch and submission. It does
not change internal question gates. Without enforcement, signed verification
is available but existing unsigned decision behavior is unchanged.

With `serve` stopped, inspect one gate directly from the journal:

```sh
./rhizome gate verify -data-dir ./rhizome-data -trust-anchor ./trust-anchor.json -show-domain q-...
```

The final argument may be a gate ID or handle. With an anchor, a verified
decision exits 0; an `attested` decision is reported distinctly and exits 3.
Without an anchor, a cryptographically valid journal chain
is reported as `chain-valid` and exits 3; pending and other unverified states
also exit 3. Missing or corrupt journals exit 1, usage errors exit 2. The
command never creates a journal and refuses to read one held by a running
writer. Anchor files contain public keys only; keep signing keys outside
Rhizome.

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
| `-janus-env-mode` | `inherit` | `inherit` gives `hx` the full `rhizome serve` environment; `allowlist` supplies only the variables listed below. |
| `-janus-env-passthrough` | *(empty)* | In `allowlist` mode, adds named, already-present variables (exact, case-sensitive names; comma-separated). |
| `-janus-observe-interval` | `5s` | Observation poll interval. |
| `-janus-idle-timeout` | `10m` | Stop a session after this long without activity. |
| `-janus-exec-config` | *(off)* | Operator-owned JSON ledger (adapter, profile id/hash, workspace, scope, session mode, ceiling) that enables `mission.start`. Requires the five flags above. |

`-janus-hx`, `-janus-approval-endpoint`, `-janus-profile`, `-janus-accept-root`
and `-janus-world-config` go together: giving only some of them is an error, so
nothing is silently disabled. The execution adapter speaks JANUS's CLI and
NDJSON protocol.

`inherit` is the current environment-mode default and preserves the behavior
of earlier releases. After the allowlist is verified on the real Linux
rootless-Podman deployment, a later release will make `allowlist` the default.
Opt in with `-janus-env-mode=allowlist` once you have verified your runtime
(`hx` and its agent backends) still works; backends that authenticate via
environment variables need those names in `-janus-env-passthrough` or a
file-based credential. Allowlist mode passes only `PATH`, `HOME`, `TMPDIR`,
`LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`,
`XDG_DATA_HOME`, and `HX_RUNTIME_DIR`, plus any existing passthrough variables.
Passthrough is rejected unless allowlist mode is selected. In a first
token-free check on a Linux rootless-Podman host, the default allowlist alone
was enough for `podman info`, `podman run` and `hx --version`, also without
`XDG_RUNTIME_DIR`; no passthrough variable was needed there.

The execution adapter IDs currently accepted are `claudecode` and `codex`.
Adding another ID requires a code change in `validAdapter` in
`internal/janusadapter/run.go`.

### Gunnflow adapter (optional)

[gunnflow-adapter/](gunnflow-adapter/) serves the Gunnflow direct wire in front
of a running board. It needs Node.js 22.12+. This is a source-only integration:
the adapter package itself is private (not published to npm). It installs
`@gunnflow/contract` and `@gunnflow/upstream-port` from npm.

```sh
cd gunnflow-adapter
npm ci
RHIZOME_URL=http://127.0.0.1:8790 PORT=8792 npx tsx src/serveDirect.ts
```

| Variable | Default | Meaning |
|---|---|---|
| `RHIZOME_URL` | `http://127.0.0.1:8790` | Base URL of the running `rhizome serve`. |
| `PORT` | `8792` | Port the adapter listens on (bound to `127.0.0.1`). |

#### Human-action requests

`request.create` records an external hand task and exposes it as a Gunnflow
`request` node. Closed requests stay visible while their target is live.
Rhizome and the adapter only describe the work; they never execute the listed
commands. Do not put secret values in those commands. Write commands so the
terminal prompts for each secret value at execution time.

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
 approval, question,
 attest, ...
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
