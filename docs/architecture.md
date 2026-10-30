# Architecture

## Data flow

```
 session logs (omp, Gemini CLI, Qwen Code, opencode; Claude Code and Codex adapters are unverified)
        │  internal/sources: one adapter per agent → agent-neutral IR (internal/ir)
        ▼
 transcript lines  (roles, tool calls and results, branches, interrupts; secrets redacted on demand)
        │  internal/grep: group into turns, match, rank newest first
        ▼
 mg search / show / sessions  →  text or JSON Lines for the next agent call
```

Nothing is stored between calls: every `mg` call finds the files, reads what it needs and renders it again. That is deliberate (no index to build, to go stale or to leak; see [mem-grep.md](mem-grep.md#how-a-search-works) for how it stays fast).

Package map:

| Package | Role |
|---|---|
| `cmd/mg` | Entry point |
| `internal/cli/mg` | The commands (cobra): search, show, sessions, version; flags, printing, exit codes |
| `internal/grep` | The engine: query compilation, raw-byte prefilter, parallel turn matching, early stop, refs, snippets, `--top` ([mem-grep.md](mem-grep.md)) |
| `internal/ir` | The intermediate representation everything downstream sees |
| `internal/sources` | Adapters (`omp.go`, `gemini.go`, `qwen.go`, `opencode.go` checked against sessions the agents wrote; `claude.go`, `codex.go` unverified skeletons), the shared GenAI-parts rendering (`parts.go`) and the registry |
| `internal/config` | Where each agent keeps its sessions: defaults, overridden by `MG_<AGENT>_DIRS` |
| `internal/util`, `internal/version` | Redaction and text helpers, build identity |

## The IR and adapters

Every agent writes a different log format. Adapters (`sources.Adapter`) hide that: `Discover` finds files, `Sniff` recognises a file given by path, `Open` parses it into an `ir.Session`. Everything downstream sees only the IR. Rule of thumb: if the engine needs to know which agent wrote a session, the information belongs in the IR.

- `ir.Line` is one rendered transcript line: role, text, the id of the source record, whether it starts a turn, and the record's **byte range** in the (decompressed) file. The ranges let a caller resume or re-read a part of a file without knowing its format; mg uses them to skim records it can rule out.
- `ir.Line.Success` carries a tool result's structured outcome when the source has one (a failing shell command in Gemini CLI or opencode is a "successful" tool call with an exit code; the adapters turn that into a failure).
- Sessions need not be files: `SourceFile.Inner` names a session inside a shared database (opencode). Such an adapter builds the session's byte stream itself (`sources.RawReader`), offsets refer to it, and `Size` is taken from it once opened. `sources.Container` lists the sessions of a database given with `--path`.
- `ir.SourceFile.Key` identifies a session. It equals the path except where an agent moves files between representations (Codex compresses cold rollouts to `.jsonl.zst`, so its key is the plain `.jsonl` path) or keeps several sessions in one file (opencode: `<db>#<session id>`). mg identifies sessions by key, never by path.
- `ir.Session.SkipReason` marks sessions that are not the user's conversation (Codex memory consolidation, non-interactive `codex exec`); mg skips them.
- Searches read faster through `sources.OpenBytes` with `ScanOptions`: the bytes already read are parsed without reading the file again, records a `Keep` filter rejects are only skimmed for the tree's structure and come back as `ir.Line{Elided: true}` placeholders, and `Raw` skips redaction for the caller to redact candidates. `sources.Synthetic` lists the text renderers write themselves; a renderer that adds notation must add it there, or mg may skip files that match ([mem-grep.md](mem-grep.md#how-a-search-works)).

Format details for each agent: [session-formats.md](session-formats.md).

### Transcript notation

Adapters render the same notation:

```
USER: …                                  the user's message (injected context removed)
ASSISTANT: …                             text written by the agent
  → bash(command=pnpm test --filter order) a tool call with its key arguments
  ✓ bash: Tests: 42 passed               a successful result (shell output: the tail)
  ✗ bash failed: connect ECONNREFUSED …  a failed result
USER SHELL $ git status (exit 0)         a command the user ran by hand
[branch: rewound to earlier point #…]    the user went back; the abandoned path follows
  [assistant turn aborted by the user]   an interruption
[user ran /clear — …]                    a context reset
TASK for subagent "scout": …             the job handed to a subagent
```

Thinking, system and developer messages and compaction summaries (they repeat earlier content) are dropped.

## Safety

- **Redaction** (`util.Redact`) runs before a line is matched or printed: API keys, tokens, private keys, JWTs, `password=…`-style assignments. Matching may run on unredacted text for speed, but a candidate turn is redacted and matched again, so a secret is never found or shown.
- **History is data.** Everything mg prints was written by someone else at some point (earlier prompts, tool output, web pages an agent read). The skill says so explicitly; mg itself never executes anything it reads.
- **Offline.** mg opens files read-only (opencode's database too) and makes no network call.

## Design decisions

- **Search the logs directly instead of building an index.** An index goes stale, costs a build step, and holds secrets in a second place. Reading and skimming raw JSONL turned out fast enough (0.1–0.3 s over 2000 sessions) when the cheapest check runs on raw bytes first.
- **The turn is the unit.** A user message and everything the agent did in response answer "what did we do about X" better than single lines, and its first line says what the turn was for.
- **References the next command accepts.** An agent's search is a loop; every result carries a reference, and the footer names the next call, so a weak model can follow the output without inventing syntax.
- **One IR, many adapters.** A new agent is one adapter and a fixture; the engine, the skill and the output do not change.
- **Follow each agent's own reading of its logs.** Which user-role text is really the user is subtle (Codex injects AGENTS.md and environment context as user messages); the Codex adapter copies Codex's own filtering rather than guessing.
- **Lexical, not semantic.** Exact words are what an agent needs to find a command, an error message or a name; alternatives with `|` cover synonyms. Semantic search over transcripts is a different tool.
