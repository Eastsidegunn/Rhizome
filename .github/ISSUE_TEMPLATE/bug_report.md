---
name: Bug report
about: Something in Rhizome behaves differently from what the docs or contracts say
title: "bug: "
labels: bug
---

<!-- Security vulnerabilities: do NOT file here. See SECURITY.md (private advisory). -->
<!-- Never paste secrets, tokens, journal files, or hostnames/IPs of your deployment. -->

## What happened

## What you expected

## How to reproduce

1. `rhizome serve -journal ... -addr 127.0.0.1:...`
2. Request(s) sent (e.g. the `POST /v1/intent` body):
3. Response / output observed:

## Environment

- Rhizome commit (`git rev-parse --short HEAD`):
- OS / arch:
- Go version (`go version`):
- Component: core (`cmd/`, `internal/`) / `gunnflow-adapter` / docs
- JANUS wired (`-janus-*` flags)? yes / no

## Additional context

<!-- Relevant FR ID or task ID if you know it (FR-RHZ-NNN / RHZ-NNN). -->
