---
name: Feature request
about: Propose a capability or change
title: "feature: "
labels: enhancement
---

<!-- Rhizome is alpha. Proposals are welcome, but may be declined or reshaped
     to fit the project's direction. -->

## Problem

<!-- What are you trying to do, and what stops you today? -->

## Proposal

<!-- What should Rhizome do? Which surface: intent kind, HTTP route, CLI, adapter? -->

## Invariants check

- [ ] Keeps the journal append-only with a single writer
- [ ] All new state is derivable by replaying events
- [ ] Does not widen policy (only intersection / minimum)
- [ ] Respects the layer direction enforced by `internal/archtest`
- [ ] Needs no new dependency (or explains why one is necessary)
- [ ] Needs no contract change with JANUS / Gunnflow (or describes the proposed change)

## Alternatives considered
