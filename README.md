# mem-grep (`mg`)

`rg` for past coding-agent sessions. `mg` reads the session logs your agents already keep, renders them into one transcript notation and prints the **turns** (a user message and everything the agent did in response) that match, newest first. No index, no model, no network, no setup.

```bash
mg docker '网络|network' '修|fix|恢复|好了'                      # how did we fix the docker network last time?
mg show omp:01a181a6-b02b#2 -B 1 -A 2                         # that turn with the one before and two after
mg -e 'github\.com/[\w.-]+/[\w.-]+' -r tool -r result --top     # which Go modules get installed most?
mg -r user -w cli                                             # what the user said (whole word: not "client")
mg sessions -p edge-proxy --since 2w                          # recent sessions in a project
```

- **Every term must occur in the same turn**; `|` inside a term lists alternatives, so one call covers synonyms and both languages. Matching is literal and case-insensitive (`-e` regex, `-s` case-sensitive, `-w` whole words).
- **Made for agents.** Compact lines, `> ` on matched lines, a reference on every result (`omp:01a181a6-b02b#2`) that `mg show` opens, footer lines starting with `-- ` that say what to run next, `--json` for JSON Lines, rg's exit codes (0 match, 1 none, 2 error) plus 3 for "nothing found, but `--timeout` cut the search short". `--timeout` and `--max-chars` bound time and output size.
- **Fast without an index.** Files are read once, newest first, one per CPU; records that cannot match are skimmed instead of decoded; the search stops once older sessions cannot change the newest N. Typical queries over 2000 sessions take 0.1–0.3 s ([numbers](docs/mem-grep.md#performance)).
- **Safe to hand to an agent.** Secrets are redacted before they are matched or printed, and the bundled skill tells the agent that history is data, not instructions.
- Pure Go (no cgo), one static binary.

## Supported agents

| Agent | Where it reads | Status |
|---|---|---|
| omp / pi | `~/.omp/agent/sessions`, `~/.pi/agent/sessions` | Complete; tested on sessions written by omp itself |
| Gemini CLI 0.66 | `~/.gemini/tmp/<project>/chats` | Checked against sessions the CLI wrote (scripted and real model) |
| Qwen Code 0.26 | `~/.qwen/projects/<cwd>/chats` | Checked against sessions the CLI wrote (scripted and real model) |
| opencode 1.19 | `~/.local/share/opencode/opencode.db` (SQLite) | Checked against a database the CLI wrote (scripted and real model) |
| Claude Code | `~/.claude/projects` | Skeleton from one real session's structure; no fixture yet |
| Codex | `~/.codex/sessions`, `archived_sessions` (also `.jsonl.zst`) | Skeleton following Codex's own filtering rules; no fixture yet |

Formats come from each agent's source code and are written down, with their sources, in [docs/session-formats.md](docs/session-formats.md). Adding an agent is described there too.

## Install

Requires Go ≥ 1.24:

```bash
make install            # builds and installs ~/.local/bin/mg (override with PREFIX=…)
mg version              # mg v0.1.0-3-gabc1234 (commit=abc1234, built=2026-10-28T08:00:00Z)
```

`make build` alone puts the binary in `bin/`. Copy [`skill/mem-grep/SKILL.md`](skill/mem-grep/SKILL.md) to your agent's skills directory so it reaches for `mg` whenever the answer may be in earlier work.

## Everyday use

```bash
mg TERM...                      # search; TERM is "a|b" for alternatives, all TERMs in one turn
mg --any kubectl helm           # either term
mg -r user 'always|never|prefer'   # only lines of a role: user, assistant, tool, result, shell, task, note
mg --since 2w -p shop-api pnpm  # bound by time and project (substring of the working directory)
mg show REF -B 1 -A 2           # open a result with its neighbours; REF is agent:id#turn, #from-to, or a file path
mg sessions --since 1w          # list sessions, newest first
mg --top -e 'pip install \S+' -r tool    # count distinct matches instead of listing turns
```

Flags, output format, JSON lines, exit codes and recipes: [docs/mem-grep.md](docs/mem-grep.md).

## Configuration

mg has no configuration file: everything is a flag or an environment variable, and it works with none. `--path FILE_OR_DIR` (repeatable) searches only those files instead of the agents' directories. To read sessions from somewhere else, set the agent's variable (several directories separated like `PATH`, `:` on Unix); it replaces that agent's default.

| Variable | Default | Meaning |
|---|---|---|
| `MG_OMP_DIRS` | `~/.omp/agent/sessions` (plus `~/.pi/agent/sessions` if present) | omp / pi sessions; `PI_CODING_AGENT_DIR` is honoured |
| `MG_CLAUDE_DIRS` | `~/.claude/projects` | Claude Code transcripts |
| `MG_CODEX_DIRS` | `$CODEX_HOME/sessions`, `$CODEX_HOME/archived_sessions` | Codex rollouts (`CODEX_HOME` defaults to `~/.codex`) |
| `MG_GEMINI_DIRS` | `~/.gemini/tmp` | Gemini CLI (`GEMINI_CLI_HOME` replaces `~`) |
| `MG_QWEN_DIRS` | `~/.qwen/projects` | Qwen Code (`QWEN_HOME` replaces `~/.qwen`) |
| `MG_OPENCODE_DIRS` | `~/.local/share/opencode` | opencode; the directory holds `opencode.db` (`XDG_DATA_HOME` is honoured) |
| `MG_SUBAGENTS` | `1` | `0` leaves subagent sessions out |
| `MG_ADVISOR` | `0` | `1` includes omp advisor runs |

An invalid `MG_SUBAGENTS` / `MG_ADVISOR` value is an error, not a silent default.

## Limitations

- Matching is lexical: no stemming, and a synonym the query does not list is not found — list alternatives with `|`.
- Text is matched after rendering, so it sees what the transcript shows: tool outputs are cut, thinking is dropped, secrets are redacted.
- The Claude Code and Codex adapters are unverified skeletons; the others are checked against scripted and model-written sessions, not against real users' histories.
- `--timeout` can leave older sessions unread; a miss after `TIMED OUT` proves nothing (exit status 3).

## Documentation

- [docs/mem-grep.md](docs/mem-grep.md) — query syntax, output and JSON format, recipes, performance
- [docs/architecture.md](docs/architecture.md) — data flow, IR and adapters, transcript notation, design decisions
- [docs/session-formats.md](docs/session-formats.md) — each agent's session format: verified facts and where they come from
- [docs/development.md](docs/development.md) — build, test, version injection, releases, Docker / CI, fixtures
