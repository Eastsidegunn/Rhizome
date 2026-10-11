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
   before doing work. What records the start depends on how you got here:
   a session the board launcher spawned already has its mission moved to
   running by the launch (`mission.start`), so `context` only sets the
   current action to "착수 — context 읽음". For a queued or paused mission,
   `context` itself moves it to running (`task.resume`) and sets that
   current action. A terminal mission is left alone. If recording fails,
   `context` still prints the bundle and exits 0 with
   `warning: start not recorded: …` on stderr — then run
   `rhizome agent start <mission>`, which reports the reason. `--no-record`
   reads without writing.
   Note filtering: if the mission name has an `RHZ-<n>` code, goal notes are
   shown only when tagged with that code; otherwise the latest goal and
   mission notes are shown (handoff first, capped at 10).
2. **While working**: `rhizome agent progress <mission> --action "what now" [--pct 40]`
   at meaningful steps, not every command.
3. **Stuck** (missing access, failing precondition, waiting on someone):
   `rhizome agent blocked <mission> "one line why" [--body-file details.md]`,
   then stop or switch to other unblocked work. Never wait silently.
4. **Needs a decision** (choice between options, scope change):
   `rhizome agent ask <mission> "short title" --body-file q.md --recommendation "your pick" [--card-file card.json]`.
   The optional card is a JSON object with only these keys: `risk`
   `{level: low|medium|high, line}`, `ifSkipped`, `whyHuman`, `options`
   `[{key, label, meaning, consequence}]`, `rollback`, `context`
   `{project, what, state}`. The board shows it before the body.
5. **Finish**: `rhizome agent done <mission> "one line result" [--body-file summary.md]`.
   `done` does not close the mission; a human does that after review.
6. **Usage** (only when you know the real numbers):
   `rhizome agent usage <mission> --model <model> --in N --out N [--cache-read N] [--cache-write N]`.
   A session the board launcher spawned must not send usage: the launcher
   records it when the session ends. Otherwise send it only with the actual
   token counts from your runtime; if you do not know them, do not send
   usage at all — never estimate and never send 0.

Other notes: `rhizome agent note <mission> "line" [--kind decision|fact|handoff|...] [--tag T]`.
A follow-up question or answer about a gate or note:
`rhizome agent note <mission> "line" --kind question|answer --re <gate or note id>`
(`--re` is required for `question`; it shows in the gate's thread).
Do not post an `answer` re: a blocked note or h-request: an answer clears that
note from the human's attention, so answering is the human's (or ops') move.

## Rules

- Summaries are one line; put details in `--body-file`. Content over 16KiB is
  rejected.
- Write temporary files (`--body-file`, `--card-file`, drafts) inside your
  own working directory, not in `/tmp` or another shared location, and do
  not commit them.
- Exit `1` with `rejected: <reason>` means the board refused the write: read
  the reason and act on it (for example, a reason naming `task.resume` means
  the mission is not started; run `rhizome agent start`). Exit `2` is a
  mistake in your command line.
- Local git work (branches, commits in your working tree) is fine. Push, pull
  requests, merges and releases are a human's call: report `done` and let the
  board route it.
