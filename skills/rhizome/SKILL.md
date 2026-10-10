---
name: rhizome
description: "Use when an agent session (Claude Code, pi, Codex) is working a Rhizome board mission: read the mission with `rhizome agent context`, then report progress, blockers, completion, usage and decision questions through `rhizome agent` instead of raw HTTP or asking the human directly."
---

# Working a Rhizome board mission

The board is the channel. Everything you report goes through `rhizome agent`;
the board routes it to the right human. Do not ask the human directly in chat
for decisions or unblocking.

## Setup

```sh
export RHIZOME_SESSION=<your session name>   # required: actor on every write
export RHIZOME_MISSION=<mission id or handle> # e.g. m-0000abcd
export RHIZOME_BOARD=http://127.0.0.1:8790    # optional, this is the default
```

With `RHIZOME_MISSION` set, the `<mission>` argument can be left out.
If `rhizome` is not on PATH, build it from the repository root:
`go build -o rhizome ./cmd/rhizome`.

## Turn routine

1. **Start**: `rhizome agent context <mission>`. Read the goal, mission, notes
   (handoff notes come first) and steps. Check every stated precondition
   before doing work. This call also records that you started; you do not
   need to announce it.
   Note filtering: if the mission name has an `RHZ-<n>` code, goal notes are
   shown only when tagged with that code; otherwise the latest goal and
   mission notes are shown (handoff first, capped at 10).
2. **While working**: `rhizome agent progress <mission> --action "what now" [--pct 40]`
   at meaningful steps, not every command.
3. **Stuck** (missing access, failing precondition, waiting on someone):
   `rhizome agent blocked <mission> "one line why" [--body-file details.md]`,
   then stop or switch to other unblocked work. Never wait silently.
4. **Needs a decision** (choice between options, scope change):
   `rhizome agent ask <mission> "short title" --body-file q.md --recommendation "your pick"`.
5. **Finish**: `rhizome agent done <mission> "one line result" [--body-file summary.md]`,
   then `rhizome agent usage <mission> --model <model> --in N --out N [--cache-read N] [--cache-write N]`.
   `done` does not close the mission; a human does that after review.

Other notes: `rhizome agent note <mission> "line" [--kind decision|fact|handoff|...] [--tag T]`.

## Rules

- Summaries are one line; put details in `--body-file`. Content over 16KiB is
  rejected.
- Exit `1` with `rejected: <reason>` means the board refused the write: read
  the reason and act on it (for example, a reason naming `task.resume` means
  the mission is not started; run `rhizome agent start`). Exit `2` is a
  mistake in your command line.
- Local git work (branches, commits in your working tree) is fine. Push, pull
  requests, merges and releases are a human's call: report `done` and let the
  board route it.
