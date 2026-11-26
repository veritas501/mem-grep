# mgd (mem-grep distill)

`mg` finds what happened; `mgd` is the bookkeeping for writing down what is worth knowing next time. It keeps three things: a **ledger** of which turns of which sessions have been read for memories, a **review queue** of proposed memories, and the **store** of accepted ones, which `mg -r memory` searches.

mgd has no model and makes no network call. Distilling — reading the unread turns and deciding what to keep — is done by an agent that calls mgd, following [`skill/mem-distill/SKILL.md`](../skill/mem-distill/SKILL.md). mgd finds the work, checks what the agent hands back, and keeps the files. That keeps `mg` as it was (offline, stateless) and puts all state in one directory.

## The loop

```
 mgd pending ──► mgd read REF ──► (the agent decides) ──► mgd add … ──► mgd mark REF
                                                              │
                                              review queue ───┴──► mgd review ──► mgd accept / reject
                                                                                      │
                                                                         store ◄──────┘   ──► mg -r memory …
```

By hand, in any agent that has the skill: "distill the pending sessions of the last week". Unattended, with a headless agent that leaves no session of its own behind (it would be distilled again otherwise):

```bash
{ echo "Follow the skill below to do the task at the end."; echo
  awk 'f>=2{print} /^---$/{f++}' skill/mem-distill/SKILL.md; echo
  echo 'Task: distill the 3 most recent sessions `mgd pending --since 7d` lists. Do not accept anything.'
} > /tmp/distill-prompt.txt
omp --model <model> --no-session -p "$(cat /tmp/distill-prompt.txt)"
```

Tried on a real 42-turn session with a small model: 7 proposed memories in about 3 minutes, each with the turn it came from.

## Commands

| Command | Does |
|---|---|
| `mgd pending [-a AGENT] [-p TEXT] [--since WHEN] [--until WHEN] [--idle D] [-n N]` | Sessions with unread turns, most recent first, with the number of new turns and their signals. Exit 1 when nothing is pending |
| `mgd read REF [--max-chars N] [--width N] [--signals] [--all]` | The unread turns of a session, each headed `#N  time  [signals]`. Stops after about `--max-chars` (default 40000) and says where to continue |
| `mgd add --name N --type T --description D --evidence REF#N… [--project DIR \| --global] [--body TEXT]` | Propose a memory (body from standard input unless `--body`); it goes to the review queue |
| `mgd mark REF[#N]…` | Record a session, or its first N turns, as read |
| `mgd mark --pending [--until WHEN] [-p TEXT] [-a AGENT]` | Mark every selected pending session as read without reading it: how to leave an old history behind (`mgd mark --pending --until 30d`) |
| `mgd review [ID…]` | The queue, in full |
| `mgd accept ID… \| --all`, `mgd reject ID… \| --all` | Move queued memories into the store, or drop them. An ID may be shortened to a unique prefix |
| `mgd list [-p TEXT] [--full]`, `mgd rm ID…` | The accepted memories; delete some |

Output follows mg's conventions: one line per item, footers starting with `-- ` that name the next command, errors on stderr. Exit status: `0` done, `1` nothing to do (nothing pending, empty queue, empty store), `2` error.

```
$ mgd pending --since 7d
omp:01a206df-c17c  2026-11-23 19:26  ~/works/crash-triage  42 turns new (of 42)  signals: correction×5 recovered×11 rewound×1 aborted×10
zcode:23fcfdb3-28c5  2026-11-23 22:31  /tmp/test  6 turns new (of 6)  signals: recovered×2
-- 2 sessions with 48 unprocessed turns to read; read one: mgd read omp:01a206df-c17c
```

### What counts as pending

- A session is pending while it has more turns than the ledger says were read. Turns are counted, not bytes, so a session that is resumed later shows only its new turns.
- Left out: subagent sessions (their lesson is in the parent's), memory files, sessions an adapter marks as not the user's conversation, and sessions written less than `--idle` ago (default 10 minutes; they may still be going on).
- A fully read session that has not changed since is skipped without being opened; the first `mgd pending` over a long history opens every session once (in parallel).

### Signals

Cheap, deterministic hints computed from the transcript, shown by `pending` and `read` so that an agent can start where something probably happened. They select nothing by themselves (`mgd read --signals` does).

| Signal | Means |
|---|---|
| `correction` | A user line corrects the agent or states a rule (不要, 别用, 应该, 以后, always, never, don't, instead, prefer, remember, …) |
| `recovered` | A tool result failed and a later one in the same turn succeeded |
| `rewound` | A `[branch: …]` note: the user went back and abandoned a path |
| `aborted` | The user interrupted the agent |

## Memories

One Markdown file per memory, in `$MG_HOME/queue` until accepted, then in `$MG_HOME/memories`:

```
---
name: prefer-pnpm
description: "use pnpm in this repository, never npm"
type: feedback
project: "/home/dev/projects/shop-api"
evidence: [omp:01a180aa-27cc#1]
created: 2026-11-24T09:00:00Z
---

Use pnpm for every script.

**Why:** …
**How to apply:** …
```

- `type` is `user`, `feedback`, `project` or `reference`, as in Claude Code and ZCode.
- `project` is the working directory the memory applies to (that of its first evidence turn unless `--project` or `--global`); empty means every project. `mg -p` matches it.
- The file name is `<8 hex digits of sha256(project, name)>-<name>.md`: proposing the same name for the same project again replaces the memory instead of adding a near-duplicate.
- `mgd add` refuses a memory without evidence, or whose evidence is not an existing turn; a name that is not lower-case words joined by hyphens; an unknown type; a description that is not one line of 10–240 characters; a body outside 20–4000 characters. Description and body are redacted (`util.Redact`) before they are written.

`mg` reads the store through the `mem` adapter (`internal/sources/mem.go`): a memory is a one-turn session of `memory` lines ending with `[evidence: …]`, its time is `created`, its reference is `mem:<first 13 characters of the id>`.

```
$ mg -r memory pnpm -p shop-api
mem:3fa91c2e-pref#1  2026-11-24 09:00  /home/dev/projects/shop-api  (turn 1/1)
> MEMORY 3fa91c2e-prefer-pnpm.md [feedback]: use pnpm in this repository, never npm
> Use pnpm for every script.
```

## Safety

Distilling turns history into text that later sessions read, so a planted instruction in a file or web page an agent once read could become permanent. What stands against that:

- Nothing is searchable until it is accepted; the queue is the place to read it first.
- Every memory names its evidence, and `mg show <evidence>` shows where it came from.
- The skill restricts `user` / `feedback` memories to what the user wrote, and facts from tool output to what a later turn confirmed. This is a rule for the distilling agent, not something mgd can check.
- Secrets are redacted on the way in, and again by mg on the way out.

## State

Everything is in `$MG_HOME` (default `$XDG_DATA_HOME/mem-grep`, i.e. `~/.local/share/mem-grep`): `ledger.json` (session key → turns read, the file's modification time when fully read, when), `queue/`, `memories/`. Deleting `ledger.json` makes every session pending again; the memory files are plain text and can be edited or removed by hand. `MG_MEM_DIRS` points mg at other memory directories.

## Not there yet

- Accepted memories are found by `mg -r memory`, not yet shown to an agent at the start of a session (a `recall` command for a session-start hook is the next step).
- No timer or service: unattended runs are a cron entry around the headless command above.
- No deduplication beyond the name, and no expiry.
