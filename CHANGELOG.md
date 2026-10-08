# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Until 1.0, minor versions may contain breaking changes.

## [Unreleased]

### Changed

- Journal write or sync failures now poison the process-wide writer until
  restart. Write-capable HTTP routes fail with a fixed 503 while read-only
  routes remain available; invalid `serve` flags no longer echo arguments
  (FR-RHZ-144).
- The adapter installs `@gunnflow/contract` 0.3.2 and
  `@gunnflow/upstream-port` 0.1.0 from npm instead of vendored tarballs
  (the wire is unchanged; 0.3.1 only widened the package's vitest peer
  range and 0.3.2 only adds a rule rejecting live URLs that carry
  credentials) and claims contract version 0.3.2. The directory
  `gunnflow-adapter/vendor/` and its vendoring instructions are gone. The
  adapter test toolchain moves from Vitest 3 to Vitest 5, which clears the
  known `npm audit` advisories; the adapter now needs Node.js 22.12 or
  newer.

### Added

- Human-action requests are now durable `request` aggregates with create,
  complete, unable, and local cancel intents. The workspace and Gunnflow
  adapter expose waiting work, structured command detail, and operator
  capabilities without changing request-free workspace bytes (FR-RHZ-154–162).
- The Gunnflow adapter carries the decision time of retained unverified
  decisions through attention `since` and a `결정 시각` detail item, without
  widening the closed node schema (FR-RHZ-143).
- Decided gates expose their decision-event time as `decidedAt`, and the
  Gunnflow adapter uses it to retain the 10 most recent unverified decisions
  (FR-RHZ-142). The value is RFC3339 UTC with an optional variable-length
  fraction; compare it as an instant, not as a string.
- Unix-socket peer credential verification and terminal I/O primitives, backed
  only by `golang.org/x/sys` and `golang.org/x/term` (FR-RHZ-130).
- The Gunnflow adapter retains capped unverified gate decisions with
  provenance detail, attention, and an uncapped workspace-root count
  (FR-RHZ-133).
- Gunnflow gate details expose seven canonical Markdown body sections as
  conclusion-first plain-text items while retaining unstructured prose as the
  request and suppressing a duplicate recommendation (FR-RHZ-128).
- Configurable data location: `-data-dir`, then `RHIZOME_DATA_DIR`, then the
  OS per-user data folder. Without `-journal`, the journal and blob store live
  there (FR-RHZ-125).
- `make ci` runs a repository hygiene check that blocks committed runtime data,
  key and token patterns, home-directory paths and CGNAT addresses.
- JANUS `hx run` and `hx replay` have a new opt-in
  `-janus-env-mode=allowlist` (default unchanged: `inherit`); the default will
  flip after deployment verification. The allowlist is `PATH`, `HOME`,
  `TMPDIR`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `XDG_RUNTIME_DIR`,
  `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, and `HX_RUNTIME_DIR`; operators can add
  exact, case-sensitive names with `-janus-env-passthrough` (FR-RHZ-126).

## [0.1.0] - 2026-10-08

First public release.

### Added

#### Project

- MIT license, English README, CONTRIBUTING, SECURITY, this changelog, a GitHub
  Actions CI workflow (`make ci` + adapter tests), issue and PR templates, and
  release notes.

#### Journal and event model

- Event-sourced core: a single-process, append-only NDJSON journal with a global
  event sequence, a store port, and services that only append through it.
- Replay of every aggregate from the journal. Append is O(1) and replay O(n),
  via a journal index.
- A kernel advisory lock (`flock`) on the journal enforces a single writer.
  Stale locks clear themselves, and the writer drains before exit on a signal.
- Policy ceilings with narrowing-only merge (intersection of allows, minimum of
  budgets), with the effective policy pinned onto execution intents.
- Architecture test `internal/archtest` that pins the layer boundary
  `substrate <- {execution kernel || knowledge kernel} <- bridge <- assembly <-
  surface`, with an exact-match exception list.

#### Goals, missions, gates and questions

- Goals and missions with decisions, evidence and terminal transitions, plus a
  planner port, wake events with trigger dedupe, and a coordinator tick.
- Operator lifecycle intents: `goal.create`, `goal.update`, `goal.resolve`,
  `goal.fail`, `goal.cancel`, `mission.create` (optionally under an existing
  goal), `mission.cancel`, `mission.complete`, `mission.fail`,
  `mission.assign`, `mission.progress`, `task.pause` / `task.resume`.
- Approval relay events and gate projection. Gate decisions require echoing the
  request digest, so the decider signs what they saw.
- Internal decision gates via `question.ask`, bound to exactly one mission or
  goal, answered with `gate.approve`, `gate.reject` or `gate.requestChanges`.
- Procedure steps marked as needing a gate open a question automatically.

#### Knowledge plane, notes, edges and code index

- Structured memories with evidence references, knowledge items, relations,
  procedures, knowledge-use traces, deterministic retrieval and an initial
  knowledge evaluation.
- Note ingest from files into a content-addressed blob store (`rhizome ingest`,
  `rhizome memories`).
- Knowledge surface: `GET /v1/knowledge` and the `note.create` intent.
  Authoring intents: `knowledge.create`, `knowledge.promote`,
  `procedure.define`.
- First-class edges (`contains`, `about`, `produces`, ...) with reverse lookup,
  plus the `edge.declare` and `edge.rewire` intents.
- Codebase indexer: a derived cache anchored to `main`, generated when `serve`
  is queried (`rhizome index`, `rhizome graph`, `GET /v1/codeindex`).

#### Procedures and assembly

- An assembly runner that turns procedure templates into instances
  (`procedure.run`), with step instances projected into `GET /v1/context`.

#### Deliverables and blobs

- Deliverable aggregate. Completion deliverables are derived deterministically.
- `POST /v1/blob` (content-addressed upload), `GET /v1/blob/{id}`, and the
  `deliverable.register` intent with `produces` edges.
- Filtering, sorting and truncation of deliverables in `/v1/workspace`.

#### HTTP surface (`/v1/*`)

- `rhizome serve`: `GET /v1/workspace` with SSE stream, `POST /v1/intent`
  (the single writer path while serving), and `GET /v1/context` (handoff read
  path, `?task=`, `?goal=`, `?mission=`).
- Workspace projection with capabilities and gate capabilities, short node
  handles (`g-…`, `m-…`) accepted anywhere an ID is, goal success criteria, and
  pending-gate counts.

#### JANUS execution integration

- Execution intents and `ExecutionRef` with dispatch claim, binding, unknown
  recovery and cursor normalisation.
- JANUS adapter: approval relay client, `hx replay` NDJSON observation,
  `hx run` / stop client, streaming runner, and a serve-side loop that wires
  observation, approval and stop together.
- `GET /v1/execution/{missionId}` with SSE, projecting sessions, incremental
  `events_tail` observation, session state and usage, and an idle timeout that
  triggers stop.
- `task.instruct` injects a message into a running multi-turn session.
- `mission.start` starts a JANUS execution from a board mission. It is gated
  by an operator-owned exec config, and the execution intent records the
  ceiling, profile and effective limits.

#### Gunnflow adapter (`gunnflow-adapter/`)

- TypeScript adapter that serves the Gunnflow direct wire (`/nodes`, `/stream`,
  `/intent`, `/detail`, `/artifact`, `/execution/:taskId`) in front of
  Rhizome, against the vendored Gunnflow contract.

[Unreleased]: https://github.com/Eastsidegunn/Rhizome/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Eastsidegunn/Rhizome/releases/tag/v0.1.0
