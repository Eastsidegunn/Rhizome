# Security Policy

## Supported versions

Rhizome is pre-1.0 and has no releases yet. Only the latest `main` branch gets
security fixes.

| Version | Supported |
|---|---|
| `main` | yes |
| anything else | no |

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub Security Advisories:
open the repository's **Security** tab and click **"Report a vulnerability"**.
This sends a private report to the maintainer.

Please include:

- what is affected (component, file, endpoint or intent kind)
- steps to reproduce, or a proof of concept
- the impact you expect, and any mitigation you know of

Responses are best effort. We aim to acknowledge a report within a week.

## Please do not post publicly

Until a fix is available, please do not put any of the following in public
issues, pull requests or discussions:

- vulnerability details or exploit code
- secrets, API keys or tokens, even ones you think are revoked
- journal files (`*.ndjson`), blob stores or logs from a running instance
- hostnames, IP addresses or other details of someone's deployment

If you find a credential committed to this repository, report it privately as
described above.

## Scope

In scope:

- the `rhizome` binary and the Go packages in this repository, including the
  HTTP surface (`/v1/*`) and intent handling
- journal integrity: the append-only and single-writer guarantees, and replay
- policy narrowing: any path that lets an execution run with more permission or
  budget than its ceiling allows
- `gunnflow-adapter/`

Out of scope (please report these to their own projects):

- JANUS / `hx` and Gunnflow
- third-party model providers or proxies
- risks of exposing `rhizome serve` beyond localhost. It has no built-in
  authentication, and the default bind address is `127.0.0.1`.

## Bounty

There is no bug bounty program.
