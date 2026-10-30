#!/usr/bin/env bash
# Regenerates test/fixtures/opencode/opencode.db with opencode itself.
#
#   test/fixtures/opencode/gen.sh [opencode-binary]      (default: opencode on PATH)
#
# opencode runs in a throwaway HOME / XDG tree against the scripted mock model of
# test/fixtures/gen/mock-model.mjs, so no account, credential or network model is involved.
# Install the CLI beforehand, e.g. `npm install --prefix <dir> opencode-ai` (the postinstall links
# the platform binary; node_modules/.bin/opencode is the entry point). Fixtures made with
# opencode-ai 1.19.5. Never copy a real database: it contains the user's conversations.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
bin=${1:-opencode}
world=$(mktemp -d)
port=${MOCK_PORT:-18080}
# a fixed working directory, so the fixture's session `directory` is the same on every run
work=${GEN_WORKDIR:-/tmp/mem-grep-gen/work}
rm -rf "$work"
mkdir -p "$world/home" "$world/data" "$world/config" "$world/cache" "$world/state" "$work"
echo "pnpm test fails with ECONNREFUSED 127.0.0.1:5432" > "$work/notes.txt"

MOCK_WORKDIR="$work" node "$here/../gen/mock-model.mjs" "$port" > "$world/mock.out" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break; sleep 0.2; done

cat > "$work/opencode.json" <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  "autoupdate": false,
  "share": "disabled",
  "model": "mock/mock-1",
  "permission": { "bash": "allow", "edit": "allow", "read": "allow", "task": "allow", "webfetch": "deny" },
  "provider": {
    "mock": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Mock",
      "options": { "baseURL": "http://127.0.0.1:$port/v1", "apiKey": "mock" },
      "models": { "mock-1": { "name": "Mock 1", "tool_call": true, "limit": { "context": 100000, "output": 4000 } } }
    }
  }
}
JSON

run() {
  # opencode now and then stalls right after start-up; a stalled run wrote nothing, so run it again
  for attempt in 1 2 3; do
    (cd "$work" && env -i PATH="$PATH" HOME="$world/home" XDG_DATA_HOME="$world/data" XDG_CONFIG_HOME="$world/config" \
      XDG_CACHE_HOME="$world/cache" XDG_STATE_HOME="$world/state" MOCK_WORKDIR="$work" \
      OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_AUTOUPDATE=1 timeout 60 "$bin" run --print-logs "$@" > "$world/run.log" 2>&1) && return 0
    echo "opencode run stalled (attempt $attempt): $*" >&2
  done
  return 1
}

# 1. a session of two turns: tools succeed, then a follow-up in the same session
run "SCENARIO list-files: 看看目录里有什么，用 pnpm"
run --continue "SCENARIO followup: 还有别的要改吗"
# 2. a failing tool, recovered from
run "SCENARIO recover: 读一下 missing.txt"
# 3. a subagent session (the task tool)
run "SCENARIO delegate: 让子任务看看"

db=$world/data/opencode/opencode.db
python3 -I - "$db" "$here/opencode.db" <<'PY'
import sqlite3, sys
src = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
src.execute("PRAGMA wal_checkpoint(TRUNCATE)")
dst = sqlite3.connect(sys.argv[2])
src.backup(dst)
dst.execute("PRAGMA journal_mode=DELETE")
dst.execute("VACUUM")
dst.close()
PY
echo "wrote $here/opencode.db"
