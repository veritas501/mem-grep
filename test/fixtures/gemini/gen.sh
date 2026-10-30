#!/usr/bin/env bash
# Regenerates test/fixtures/gemini/tmp/ with Gemini CLI itself.
#
#   test/fixtures/gemini/gen.sh [gemini-binary]          (default: gemini on PATH)
#
# Gemini CLI runs in a throwaway HOME against the scripted mock model of
# test/fixtures/gen/mock-model.mjs (Gemini generateContent protocol, via GOOGLE_GEMINI_BASE_URL), so
# no account or credential is involved. Install the CLI beforehand, e.g.
# `npm install --prefix <dir> @google/gemini-cli`. Fixtures made with @google/gemini-cli 0.66.0.
# Never copy a real session: it contains the user's conversations.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
bin=${1:-gemini}
world=$(mktemp -d)
port=${MOCK_PORT:-18085}
work=${GEN_WORKDIR:-/tmp/mem-grep-gen/work}   # fixed, so the project root recorded in the fixture is the same on every run
rm -rf "$work"
mkdir -p "$world/home/.gemini" "$work"
echo "pnpm test fails with ECONNREFUSED 127.0.0.1:5432" > "$work/notes.txt"
cat > "$world/home/.gemini/settings.json" <<'JSON'
{ "security": { "auth": { "selectedType": "gemini-api-key" }, "folderTrust": { "enabled": false } },
  "general": { "enableAutoUpdate": false, "enableAutoUpdateNotification": false },
  "privacy": { "usageStatisticsEnabled": false } }
JSON

MOCK_WORKDIR="$work" node "$here/../gen/mock-model.mjs" "$port" > "$world/mock.out" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break; sleep 0.2; done

run() {
  for attempt in 1 2 3; do
    (cd "$work" && env -i PATH="$PATH" HOME="$world/home" GEMINI_API_KEY=mock GOOGLE_GEMINI_BASE_URL="http://127.0.0.1:$port" \
      timeout 90 "$bin" --yolo "$@" > "$world/run.log" 2>&1) && return 0
    echo "gemini run failed (attempt $attempt): $*" >&2
  done
  return 1
}

# 1. a session of two turns: tools succeed, then a follow-up in the same session
run -p "SCENARIO list-files: 看看目录里有什么"
run --resume latest -p "SCENARIO followup: 还有别的要改吗"
# 2. a failing command, recovered from (a fresh session)
run -p "SCENARIO recover: 读一下 missing.txt"
# 3. a subagent (the invoke_agent tool)
run -p "SCENARIO delegate: 让子任务看看"

proj=$(ls -d "$world"/home/.gemini/tmp/*/ | head -1)
rm -rf "$here/tmp"
mkdir -p "$here/tmp/$(basename "$proj")"
cp -R "$proj/chats" "$here/tmp/$(basename "$proj")/chats"
cp "$proj/.project_root" "$here/tmp/$(basename "$proj")/.project_root"
echo "wrote $here/tmp"
