#!/usr/bin/env bash
# Regenerates test/fixtures/qwen/projects/ with Qwen Code itself.
#
#   test/fixtures/qwen/gen.sh [qwen-binary]              (default: qwen on PATH)
#
# Qwen Code runs in a throwaway HOME against the scripted mock model of
# test/fixtures/gen/mock-model.mjs (OpenAI-compatible), so no account or credential is involved.
# Install the CLI beforehand, e.g. `npm install --prefix <dir> @qwen-code/qwen-code`. Fixtures made
# with @qwen-code/qwen-code 0.26.3. Never copy a real session: it contains the user's conversations.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
bin=${1:-qwen}
world=$(mktemp -d)
port=${MOCK_PORT:-18082}
work=${GEN_WORKDIR:-/tmp/mem-grep-gen/work}   # fixed, so the project directory name is the same on every run
rm -rf "$work"
mkdir -p "$world/home" "$work"
echo "pnpm test fails with ECONNREFUSED 127.0.0.1:5432" > "$work/notes.txt"

MOCK_WORKDIR="$work" node "$here/../gen/mock-model.mjs" "$port" > "$world/mock.out" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break; sleep 0.2; done

run() {
  for attempt in 1 2 3; do
    (cd "$work" && env -i PATH="$PATH" HOME="$world/home" OPENAI_API_KEY=mock OPENAI_BASE_URL="http://127.0.0.1:$port/v1" \
      OPENAI_MODEL=mock-1 timeout 90 "$bin" --auth-type openai --approval-mode yolo "$@" > "$world/run.log" 2>&1) && return 0
    echo "qwen run failed (attempt $attempt): $*" >&2
  done
  return 1
}

# 1. a session of two turns: tools succeed (and a relative path is refused), then a follow-up
run "SCENARIO list-files: 看看目录里有什么"
run --continue "SCENARIO followup: 还有别的要改吗"
# 2. a failing command, recovered from (a fresh session: no --continue)
run "SCENARIO recover: 读一下 missing.txt"
# (Qwen Code 0.26.3 offers no subagent tool to a headless run, so there is no subagent session here.)

proj=$(ls -d "$world"/home/.qwen/projects/*/)
rm -rf "$here/projects"
mkdir -p "$here/projects/$(basename "$proj")"
for d in chats subagents; do
  [ -d "$proj/$d" ] && cp -R "$proj/$d" "$here/projects/$(basename "$proj")/$d"
done
find "$here/projects" -name '*.runtime.json' -delete
echo "wrote $here/projects"
