## Summary

<!-- What does this change and why? One focused change per PR. -->

FR ID(s): <!-- FR-RHZ-NNN, or "none — docs/CI only", or "needs assignment" -->

## Testing

- [ ] `make ci` passes locally (gofmt check + `go test` + `go test -race` + `go vet`)
- [ ] If `gunnflow-adapter/` changed: `cd gunnflow-adapter && npm ci && npm test` passes
- [ ] New/changed behaviour has tests, and the FR ID appears in the test name or a comment
- [ ] No existing test was deleted, skipped, or weakened

## Invariants

- [ ] Journal stays append-only; writes go through the single writer boundary (no mutation via returned values or inputs)
- [ ] All derived state remains recomputable from events
- [ ] Policy is only narrowed (allow intersection, budget minimum)
- [ ] Layer direction respected; no new `internal/archtest` exception (or justified below)
- [ ] No new dependency without prior approval
- [ ] No contract/fixture change without recorded approval
- [ ] No secrets, tokens, personal hosts or IPs in code, tests, logs, or this description

## Notes for reviewers
