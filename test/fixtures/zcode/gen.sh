#!/usr/bin/env bash
# Regenerates test/fixtures/zcode/cli/ (db/db.sqlite and memories/) with ZCode itself.
#
#   test/fixtures/zcode/gen.sh [zcode-binary]            (default: zcode on PATH)
#
# The ZCode CLI runs headless (`--prompt`) in a throwaway HOME against the scripted mock model of
# test/fixtures/gen/mock-model.mjs (OpenAI chat completions), so no account, credential or network
# model is involved. Fixtures made with zcode 0.18.0 (ZCode v4.0.0). Never copy a real database or
# memory directory: they contain the user's conversations.
#
# The CLI needs ZCode's built-in provider catalogue. A distribution that cannot find its own in a
# fresh HOME is pointed at one with ZCODE_BUILTIN_PROVIDER_CONFIG_FILE (config/provider/
# zcode-builtin.json of a ZCode checkout, or the copy an installed ZCode keeps under
# ~/.zcode/v2/runtime/provider/bundled); the personal provider file below is what selects the mock.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
bin=${1:-zcode}
port=${MOCK_PORT:-18085}
# fixed directories, so the paths recorded in the fixture are the same on every run
base=${GEN_BASE:-/tmp/mem-grep-gen}
work=${GEN_WORKDIR:-$base/work}
home=$base/zcode-home
rm -rf "$work" "$home"
mkdir -p "$work" "$home/.zcode/v2"
echo "pnpm test fails with ECONNREFUSED 127.0.0.1:5432" > "$work/notes.txt"

builtin=${ZCODE_BUILTIN_PROVIDER_CONFIG_FILE:-}
if [ -z "$builtin" ] && [ -f "$HOME/.zcode/v2/runtime/provider/bundled/zcode-builtin.json" ]; then
  builtin=$HOME/.zcode/v2/runtime/provider/bundled/zcode-builtin.json
fi

# The CLI keeps a project's memories in memories/projects/<slug>-<sha256(workspace path)[:16]>/memory
# (core/src/memory/project-root.ts); the mock model is told where, to write one there.
cli=$home/.zcode/cli
memory=$cli/memories/projects/$(basename "$work")-$(printf %s "$work" | sha256sum | cut -c1-16)/memory

MOCK_WORKDIR="$work" MOCK_MEMORY_DIR="$memory" MOCK_SUBAGENT_TYPE=Explore node "$here/../gen/mock-model.mjs" "$port" > "$base/zcode-mock.out" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break; sleep 0.2; done

cat > "$home/.zcode/v2/provider_config.json" <<JSON
{
  "schemaVersion": 1,
  "config": {
    "providerConfigRules": {
      "providerRules": [
        {
          "providerId": "mock",
          "providerName": "Mock",
          "config": {
            "group": "standard-personal",
            "access": { "type": "api-key", "apiKey": "mock" },
            "api": { "type": "openai-chat-completions", "baseUrl": "http://127.0.0.1:$port/v1" },
            "personalModelIds": ["mock-1"]
          }
        }
      ]
    },
    "modelConfigRules": { "providerModelRules": [], "manualProviderModelRules": [] },
    "defaultModelSelection": { "providerId": "mock", "modelId": "mock-1" }
  }
}
JSON

run() {
  (cd "$work" && env -i PATH="$PATH" HOME="$home" ${builtin:+ZCODE_BUILTIN_PROVIDER_CONFIG_FILE="$builtin"} \
    timeout 180 "$bin" --mode yolo --agent-always-background off --json "$@" > "$base/zcode-run.log" 2>&1) && grep -q '"sessionId"' "$base/zcode-run.log" && return 0
  echo "zcode run failed: $*" >&2
  tail -5 "$base/zcode-run.log" >&2
  return 1
}

# 1. a session of two turns: tools succeed, then a follow-up in the same session
run --prompt "SCENARIO list-files: 看看目录里有什么，用 pnpm"
run --continue --prompt "SCENARIO followup: 还有别的要改吗"
# 2. a failing command, recovered from
run --prompt "SCENARIO recover: 读一下 missing.txt"
# 3. a subagent session (the Agent tool, run in the foreground)
run --prompt "SCENARIO delegate: 让子任务看看"
# 4. a memory: the agent writes a memory file into the project's memory directory
run --prompt "SCENARIO remember: 记住集成测试不要 mock 数据库"

rm -rf "$here/cli"
mkdir -p "$here/cli/db" "$here/cli/memories"
# The database is copied as ZCode wrote it, except for what identifies the machine it ran on: the
# user and group names (in `ls -la` output), the host name and the kernel release (in the
# environment notes ZCode injects) are replaced by neutral ones.
python3 -I - "$cli/db/db.sqlite" "$here/cli/db/db.sqlite" "$(id -un)" "$(id -gn)" "$(hostname)" "$(uname -r)" <<'PY'
import sqlite3, sys
src = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
dst = sqlite3.connect(sys.argv[2])
src.backup(dst)
neutral = dict(zip(sys.argv[3:7], ["dev", "dev", "devbox", "6.0.0-generic"]))
for (table,) in dst.execute("SELECT name FROM sqlite_master WHERE type = 'table'").fetchall():
    for column in dst.execute('PRAGMA table_info("%s")' % table).fetchall():
        if column[2].lower() != "text":
            continue
        for old, new in neutral.items():
            if len(old) >= 3:
                dst.execute('UPDATE "%s" SET "%s" = replace("%s", ?, ?) WHERE instr("%s", ?) > 0' % (table, column[1], column[1], column[1]), (old, new, old))
dst.commit()
dst.execute("PRAGMA journal_mode=DELETE")
dst.execute("VACUUM")
dst.close()
PY
cp -R "$cli/memories/projects" "$here/cli/memories/projects"
echo "wrote $here/cli"
