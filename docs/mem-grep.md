# mg (mem-grep)

`mg` searches past coding-agent sessions the way `rg` searches files. There is no index, no model and no network: every call reads the session logs where the agents keep them, renders them through the adapters (`internal/sources` → `internal/ir`) and matches the transcript. It works on a fresh machine, offline, the moment it is installed.

It is built for AI agents first: results are turns with references the next command accepts, output is compact and line-oriented (or JSON Lines), the footer says what to do next, and exit codes follow rg. Time and output size can be bounded (`--timeout`, `--max-chars`), and an incomplete answer says so instead of passing for a complete one.

## How a search works

1. **Find sessions** of every supported agent in their default locations (`MG_<AGENT>_DIRS` moves them, see the [README](../README.md#configuration)), or only `--path` files / directories.
2. **Read files newest first**, one per CPU in parallel. Each file is read once into a reused buffer.
3. **Prefilter the file** on its raw bytes: a file without a literal term (or a regex's literal prefix) cannot match and is skipped without parsing. Files last written before `--since` are skipped too.
4. **Skim the records** that cannot match: a record (JSONL line) of the agent's work with none of the terms' needles, or with no line of a searched `--role`, is only scanned for the fields that structure the transcript (ids, parents, times, roles), not decoded. User messages and everything else that opens or shapes a turn are always decoded, so turn numbers never change.
5. **Render** the remaining records to transcript lines (injected context removed) and group them into **turns**: one user message plus everything the agent did in response.
6. **Match**: every term must occur somewhere in the same turn (`--any`: at least one), only in lines of the `--role`s given. Plain literal terms are matched without the regexp engine. When the terms allow it, matching runs on unredacted text and only candidate turns are redacted and matched again, so a secret is never found.
7. **Stop early**: with `-n N`, once N+1 matching turns are newer than every file not yet read (a file is written after each of its turns), older files cannot change the result and are left unread. The footer then says "at least".
8. **Print** the newest turns first; their sessions are read once more in full, so printed turns are complete.

A term is used to skip a file or record only when that is sound: it appears verbatim in JSON (no quotes, backslashes or control characters), has no whitespace, is not a bare number, and cannot overlap text the renderers write themselves (`sources.Synthetic`: `USER:`, `✓ tool:`, truncation and redaction markers, …). Otherwise every record is decoded. A test compares the result with a plain search (decode everything, regexp every line) on every query shape.

## Query syntax

```bash
mg TERM...                         # every term in the same turn
mg docker '网络|network'            # | separates alternatives inside a term
mg --any kubectl helm              # either term
mg -e 'github\.com/[\w.-]+/[\w.-]+' # RE2 regular expressions
mg -w cli                          # whole words: "cli", not "client"
mg -s HTTPError                    # case-sensitive (default: ignore case)
mg search show                     # a term that is also a command name
```

| Flag | Meaning |
|---|---|
| `-e, --regex` | Terms are RE2 regexes |
| `-s, --case-sensitive` | Match case (no smart case: a capital letter never silently hides matches) |
| `-w, --word` | Whole words (ASCII word boundaries) |
| `--any` | A turn matches when any term matches |
| `-r, --role R` | Only search and print lines of these roles: `user`, `assistant`, `tool` (calls), `result` (tool output), `shell` (commands the user ran), `task` (subagent tasks), `note` (branches, interrupts). Each turn's opening line is still printed as its anchor |
| `-a, --agent A` | Only these agents: `omp`, `claude`, `codex`, `gemini`, `qwen`, `opencode` |
| `-p, --project TEXT` | Sessions whose working directory contains TEXT |
| `--since WHEN`, `--until WHEN` | Turn time bounds: `36h`, `7d`, `2w`, `3m`, `1y`, `2026-10-01` |
| `-n, --limit N` | At most N results (default 20; 0 = all) |
| `-C, --context N` | Also print N lines around each matched line |
| `--full` | Print matching turns whole and uncut |
| `--width N` | Cut lines to about N characters around the match (default 240; 0 = never) |
| `--timeout D` | Stop searching after D (`2s`, `500ms`; default none). Files not yet read are left unread, newest first, and the footer says `TIMED OUT`. Finding nothing then is **inconclusive** (exit 3), not "no match" |
| `--max-chars N` | Stop printing once about N characters are out (search and `show`). The first result is always printed; the footer says how much is left (default: no limit) |
| `--top` | Count distinct matched strings instead of listing turns |
| `--path P` | Search these files / directories instead of the default locations |
| `--json` | JSON Lines |
| `--color` | `auto` (default; only on a terminal), `always`, `never` |

`-r`, `-a` and `--path` take several values, repeated or comma-separated.

## Commands

| Command | Does |
|---|---|
| `mg [flags] TERM...` (or `mg search …`) | Matching turns, newest first |
| `mg show REF... [-A N] [-B N] [-r ROLE] [--full]` | Turns of a session, uncut unless `--width` is given (`--full` forces it). REF: `omp:01a180aa-27cc#3`, `#3-5` for a range, no `#` for the whole session (first `-n` turns), or a file path. `-r` keeps lines of those roles and says how many others it left out |
| `mg sessions [TERM...]` | Sessions, most recent activity first, with their first user message; with terms, only sessions containing a matching turn, with hit counts |
| `mg version` | Build identity |

References use the first 13 characters of the session id: omp and Codex ids are UUIDv7, whose first 8 characters only change every ~65 seconds; 13 cover the full millisecond timestamp.

## Output

```
$ mg docker '网络|network' '修|fix|恢复|好了'
omp:01a181a6-b02b#2  2026-10-28 08:35  /home/dev/work/edge-proxy  (turn 2/3)
  USER: 是不是又是 iptables 的问题？上次换了内核之后就这样
> ASSISTANT: 是的：新内核下 iptables-nft 的 nat 表没有初始化，docker 建不了 NAT 链。切到 iptables-legacy 再重启 docker。
> → bash(command=sudo update-alternatives --set iptables /usr/sbin/iptables-legacy && sudo systemctl restart docker && docker compose up -d && docker compose exec api ping -c1 redis)
> ASSISTANT: 网络恢复了：根因是内核升级后 iptables-nft 的 nat 表缺失，docker 无法创建 bridge 网络的 NAT 链；切换到 iptables-legacy 并重启 docker 后 compose 网络重建成功，api 能 ping 通 redis。

-- 1 turn in 1 session matched, newest first; searched 2004 session files in 31ms
-- read it in full with the turn before and two after: mg show omp:01a181a6-b02b#2 -B 1 -A 2
```

- Header: `<ref>  <time>  <project>  (turn i/n)`, plus `subagent:<name>` for subagent sessions.
- The turn's first line (user message, subagent task or branch note) is always printed as an anchor; matched lines start with `> `, context lines with two spaces.
- Lines use the transcript notation: `USER:`, `ASSISTANT:`, `→ tool(args)`, `✓ tool: …`, `✗ tool failed: …`, `USER SHELL $ …`, `[branch: …]`. Newlines inside a line become ` ⏎ `, so one printed line is one transcript line.
- Footer lines start with `-- `: totals, whether results were cut, and the next command to run. With no match the footer suggests how to broaden the query. After an early stop the totals are lower bounds:

```
-- at least 143 turns in 13 sessions matched, newest first; examined 13 of 2004 session files in 10ms, newest first (older ones cannot outrank these)
-- showing 20; at least 123 more: raise -n, or narrow with -w (whole words), more terms, --since, -p, -r
```

With `--timeout`, a search that runs out of time says so, and says what a miss then means:

```
$ mg --timeout 100ms the error
-- inconclusive: nothing found in what was read, but TIMED OUT after 100ms: examined 195 of 2004 session files, newest first
-- this does not mean nothing matches: raise --timeout, or search less with --since, -p, -a, -r and distinctive terms
$ echo $?
3
```

With `--max-chars`:

```
$ mg --max-chars 2000 error
…
-- 22570 turns in 2001 sessions matched, newest first; searched 2004 session files in 1859ms
-- output budget of 2000 characters reached: showing 3; 22567 more: raise --max-chars, or narrow the query
```

A timeout is a bound on work, `--max-chars` a bound on output; neither changes which results come first. Always read the footer: an empty answer after `TIMED OUT` proves nothing.

`--top`:

```
$ mg -e 'github\.com/[\w.-]+/[\w.-]+' -r tool -r result --top
sessions     count  last              example                   match
       2         4  2026-10-28 08:35  omp:01a181a6-b0ad#2       github.com/charmbracelet/log
       2         4  2026-10-28 08:35  omp:01a181a6-b0ad#2       github.com/spf13/cobra
       1         3  2026-10-28 08:35  omp:01a181a6-b0ad#2       github.com/urfave/cli
       1         1  2026-10-28 08:35  omp:01a181a6-b0ab#1       github.com/dev/gh-sync
-- 4 distinct matches, ranked by sessions then count, from 3 turns in 2 sessions; searched 2004 session files in 35ms
-- see a use in context: mg show <example>
```

Ranked by the number of sessions, then occurrences; `example` is the newest turn using it.

### JSON Lines

Every line is an object with a `type`:

```json
{"type":"hit","ref":"omp:01a180aa-27cc#1","agent":"omp","session":"01a180aa-27cc-7000-…","turn":1,"turns":5,"ts":"2026-10-28T03:59:57.395Z","cwd":"/home/dev/projects/shop-api","path":"/…/main.jsonl","lines":[{"role":"user","text":"USER: …"},{"role":"result","text":"✗ bash failed: Error: connect ECONNREFUSED …","hit":true,"match":[[32,44]]}]}
{"type":"summary","matched":1,"shown":1,"sessions":1,"searched_files":2,"parsed_files":1,"skipped_files":1,"elapsed_ms":0,"truncated":false,"complete":true,"next":"mg show omp:01a180aa-27cc#1 -B 1 -A 2"}
```

- `match` holds `[start, end)` **byte** offsets into that line's `text` (after cutting).
- `top` objects: `match`, `count`, `sessions`, `last`, `example`. `mg show` emits a `session` object, then one `turn` object per turn (`ref`, `turn`, `ts`, `lines`). `mg sessions` emits `session` objects (`ref`, `agent`, `session`, `cwd`, `turns`, `first`, `last`, `hits`).
- A search always ends with a `summary` object, also when nothing matched. `complete: false` means it stopped early (the `-n` early stop, or `--timeout` with `timed_out: true`) and `matched` / `sessions` are lower bounds (`-n 0` reads everything). `budget_exhausted: true` means `--max-chars` stopped the printing (`truncated` is true, `shown` < `matched`). `mg show` ends each session with a `summary` object (`ref`, `from`, `to`, `turns`, `next`, `budget_exhausted`).

### Exit status

`0` something matched, `1` nothing matched (everything was searched), `2` an error (bad flag, unreadable path, ambiguous reference), `3` nothing matched in what was read, but `--timeout` stopped the search before it finished: the answer is unknown. A timeout that still found matches exits 0 with `TIMED OUT` in the footer and `timed_out: true` in the summary.

## Recipes

How did we fix it last time?

```bash
mg docker '网络|network' '修|fix|恢复|好了'          # the turn where it got fixed
mg show omp:01a181a6-b02b#2 -B 1 -A 2              # the problem before, the confirmation after
```

Which libraries / tools does the user prefer?

```bash
mg -e 'github\.com/[\w.-]+/[\w.-]+' -r tool -r result --top     # what actually got installed, most used first
mg -r user -w 'cobra|cli' 'log|日志'                              # what the user said about it
```

What did the user insist on or correct?

```bash
mg -r user '不要|别|不对|always|never|一律'
```

What happened in a project lately?

```bash
mg sessions -p edge-proxy --since 2w
mg show omp:01a181a6-b02b                           # first turns of a session
```

Tips: start broad and narrow down (`-r`, `-p`, `--since`, more terms); add the other language as an alternative (`'网络|network'`); use `-w` for short English words; read the footer before running the next command.

## Performance

Measured in a 4-core sandbox on 2004 omp sessions (79 MB) generated by omp itself: wall time of the whole command, best of three, before (the first version: full decode, one regexp per line) and now. About 50 ms of each is start-up and finding the files. "Spread" is a copy whose sessions are spread over five months, like a real history; in the original all were written within one minute, so an early stop cannot happen there.

| Query | Before | Now |
|---|---|---|
| `docker '网络\|network' '修\|fix\|恢复\|好了'` (the docker question) | 170 ms | 85 ms |
| `kubernetes` (no match; files skipped by the prefilter) | 190 ms | 90 ms |
| `-e 'github\.com/…' -r tool -r result --top` | 180 ms | 95 ms |
| `-r user -w 'cobra\|cli' 'log\|日志'` (common words, one role) | 1.8 s | 0.29 s |
| `error`, spread history (early stop after 13 of 2004 files) | 1.3 s | 0.06 s |
| `mg sessions` | 1.8 s | 0.28 s |
| `error timeout` (both in most files and records) | 1.5 s | 1.25 s |
| `-n 0 error` (22570 turns printed, 15 MB of output) | 1.6 s | 1.3 s |
| `-e '\d{4,}' -n 0` (regex without literal text: nothing can be skipped) | 1.8 s | 0.85 s |
| `the error` | 0.18 s | 0.72 s |

The last row got slower on purpose: "the" occurs in mg's own notes (`[branch: … on the previous path …]`), so a file can match without containing it, and the first version skipped such files wrongly. Words that can come from the notation (`the`, `user`, `tool`, `failed`, …) are never used to skip anything.

What made the difference, in the order of the profile: skimming records that cannot match instead of decoding them (and decoding the rest into a struct that skips thinking blocks, usage and tool result details); anchoring the raw-byte scan on the needle's rarest byte (`bytes.IndexByte` is vectorised, as in memchr); reading each file once into a reused buffer; collecting garbage less often (`GOGC=400` with a 1 GiB soft limit; the heap peaks around 15 MB); matching literals with `strings.Index` instead of the regexp engine; redacting only candidate turns; stopping early. With `-n 0` every hit is printed whole, so records are not skimmed then (a file with a hit would be parsed twice). The ideas are ripgrep's: do the cheapest possible check on raw bytes first and spend real work only on candidates. What still costs is decoding JSON when the terms are in almost every record, and Go's regexp engine for regexes without literal text.

The optimised search must return exactly what a plain search returns (decode everything, regexp every line): `TestSearchEqualsReference` checks that on the fixtures for every kind of query, `TestEarlyStopReturnsTheNewestHits` on sessions spread over time, and `MG_CORPUS=<dir> go test ./internal/grep -run OnCorpus` on a large generated corpus.

## Limits

- Matching is lexical. There is no stemming, and a synonym the query does not list is not found — list alternatives with `|` (semantic search is a different tool).
- Text is matched after rendering, so it sees what the transcript shows: tool outputs are cut (failures to ~700 characters, shell output to its last ~300), thinking is dropped, and secrets are redacted.
- Claude Code and Codex sessions go through skeleton adapters that have not been validated on real logs yet ([session-formats.md](session-formats.md)).
