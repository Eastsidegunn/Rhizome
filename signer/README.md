# rhizome-signer

`rhizome-signer` is the macOS-only signing helper for Rhizome trust decisions. It keeps an ECDSA P-256 key in the Secure Enclave and requires one LocalAuthentication user-presence check for every signature. The package has no external dependencies and needs only Apple Command Line Tools; Xcode, Swift Testing, and XCTest are not required.

## Build and test

From this directory:

```sh
swift build -c release
.build/release/rhizome-signer-selftest
```

From the repository root, the equivalent opt-in targets are `make signer` and `make signer-test`. They are intentionally not part of `make ci`, because the Go CI runners are not macOS hosts.

The framework-free self-test executable runs all 14 cases with a protocol-conforming software P-256 signer. It never creates or accesses a Secure Enclave key and never displays a Touch ID prompt.

## Data and network boundary

The default data directory is `~/Library/Application Support/rhizome-signer`. Set `SIGNER_DIR` only when an isolated data directory is needed. The directory is mode `0700`; `key.sep`, `anchor.json`, and `requests.log` are mode `0600`.

The spawn command always talks to the compile-time URL `http://127.0.0.1:8790`:

```sh
rhizome-signer sign-stdin
```

It accepts no command-line arguments and reads exactly one JSON object from standard input. CLI network commands accept `-rhizome` only for an `http` URL whose host is a literal address in `127.0.0.0/8` or `::1`. Redirects and proxies are disabled, and requests time out after 10 seconds.

## Commands

```text
rhizome-signer version
rhizome-signer key init
rhizome-signer key show [-anchor]
rhizome-signer sign <gateId> <approve|reject|requestChanges|allow|deny> [-reason TEXT] [-submit] [-rhizome URL]
rhizome-signer attest -list -out FILE [-rhizome URL]
rhizome-signer attest -all -expect sha256:... [-rhizome URL]
rhizome-signer attest <gateId>... -expect sha256:... [-rhizome URL]
rhizome-signer key add <pub.der> -principal H [-submit] [-rhizome URL]
rhizome-signer key revoke <keyId> -reason TEXT [-submit] [-rhizome URL]
```

`sign` and key-lifecycle commands print an intent unless `-submit` is present. Attestation signing submits `attest.create` directly. `attest -list` writes the complete local review document at mode `0600`; its stdout summary deliberately omits reason text.

Exit codes are `0` success, `2` refusal/rate limit/usage, `3` Rhizome read or submit failure, `4` authentication cancellation, `5` missing key, and `6` a decision or gate that is not signable.

Install the release binary as a root-owned, non-writable executable only after independently checking its SHA-256. The release binary hash is per-build and is not deterministic, so verify the exact artifact being installed rather than comparing it with a hash from another build. Key creation and every real signing command must be run by the human operator from a GUI login session, never by an agent or unattended process.
