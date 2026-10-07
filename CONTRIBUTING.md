# Contributing to Rhizome

Rhizome is an early, alpha-stage project and its APIs change often. Issues and
focused pull requests are welcome; for larger changes, open an issue first.

## Build and test

Requirements: Go 1.23+, and Node.js 22+ for the TypeScript adapter.

```sh
make ci                                      # gofmt check, test, test -race, vet: must exit 0
cd gunnflow-adapter && npm ci && npm test    # adapter: tsc + vitest
```

CI runs both on every pull request. Run `gofmt -w .` before you commit.

## Rules

- **Do not weaken tests.** Never delete, skip or loosen a test to make it pass.
  If a test is wrong, explain why in the PR.
- **Requirement IDs.** Behaviour is tracked by functional-requirement IDs
  (`FR-RHZ-NNN`), which appear in test names. Reuse the ID of the behaviour you
  change; if there is none, say so in the PR and one will be assigned.
- **Layers.** `internal/archtest` enforces the dependency direction
  (`substrate <- {execution kernel || knowledge kernel} <- bridge <- assembly <-
  surface`). Classify any new internal package there.
- **The journal is append-only, with a single writer.** Never modify events,
  directly or through returned or input objects. Every derived state must be
  replayable from events alone.
- **Policy only narrows:** intersect allow lists, take the minimum budget.
- **No imports of sibling systems.** JANUS and Gunnflow are reached only through
  CLI, NDJSON and HTTP adapters.
- **No new dependencies** in Go or the adapter without approval in an issue.
- **Contract changes** — the integration contracts (JANUS CLI/NDJSON protocol,
  Gunnflow wire contract packages in `gunnflow-adapter/vendor`) and their
  fixtures need recorded project approval first. Open an issue.

## Commit messages

Commit messages are public and permanent.

- Say what changed and why, and cite the FR ID if the change has one.
- Never include private data: local or home paths, hostnames, IP addresses,
  account names or emails, tokens, private endpoints, or quotes of personal
  goals and conversations. Use placeholders such as `<server>`.

## Pull requests

Keep each PR to one focused change and fill in the PR template. `make ci` must
be green, plus the adapter tests if you touched `gunnflow-adapter/`. Never put
secrets, tokens, personal hosts or IP addresses in code, tests, logs or PR text.

## Security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
