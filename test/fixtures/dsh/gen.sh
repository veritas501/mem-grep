#!/usr/bin/env bash
# Regenerates test/fixtures/dsh/sessions/ with DeepSeek Harness itself.
#
#   test/fixtures/dsh/gen.sh [dsh-binary]
#
# The default is the source CLI of a deepseek-harness checkout at $DSH_HARNESS_DIR (default
# ~/audit/deepseek-harness), launched through launch.mjs: tsx resolves from the harness repo while
# the process cwd stays the task directory, which the headless runner records as the session cwd.
# The built lib/ is not used — it goes stale against the workspace packages. A compiled `dsh` works
# too: pass it as the argument.
#
# The harness runs in a throwaway DSH_HOME against the scripted mock model of
# test/fixtures/gen/mock-model.mjs (the DeepSeek /v1/messages stream), so no account or credential
# is involved. The profile overlay is mock.patch.yml. Never copy a real session: it contains the
# user's conversations.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
harness=${DSH_HARNESS_DIR:-"$HOME/audit/deepseek-harness"}
world=$(mktemp -d)
port=${MOCK_PORT:-18083}
work=${GEN_WORKDIR:-/tmp/mem-grep-gen/dsh-work}   # fixed, so the encoded project directory is the same on every run
if [ $# -ge 1 ]; then
  bin=(timeout 180 "$1")
else
  bin=(timeout 180 node "$here/launch.mjs" "$harness" "$work" --)
fi
sessions=$world/sessions
rm -rf "$work"
mkdir -p "$world/home" "$work" "$sessions"
echo "pnpm test fails with ECONNREFUSED 127.0.0.1:5432" > "$work/notes.txt"

MOCK_WORKDIR="$work" node "$here/../gen/mock-model.mjs" "$port" --log "$world/mock.ndjson" > "$world/mock.out" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break; sleep 0.2; done

patch=$world/mock.patch.yml
sed -e "s/MOCK_PORT/$port/" -e "s#SESSION_ROOT#$sessions#" "$here/mock.patch.yml" > "$patch"

run() {
  for attempt in 1 2 3; do
    (cd "$work" && env -i PATH="$PATH" HOME="$world/home" DSH_HOME="$world/home/.dsh" \
      DEEPSEEK_API_KEY=mock DEEPSEEK_BASE_URL="http://127.0.0.1:$port" \
      "${bin[@]}" --profile headless --patch "$patch" --permission-mode danger-full-access \
      --approval-policy never "$@" > "$world/run.log" 2>&1) && return 0
    echo "dsh run failed (attempt $attempt): $*" >&2
    tail -20 "$world/run.log" >&2
  done
  return 1
}

# 1. two turns: tools succeed, then a follow-up in the same session
run "SCENARIO list-files: 看看目录里有什么"
sid=$(find "$sessions" -name 'session.v4.jsonl.zstd' -printf '%T@ %p\n' | sort -n | tail -1 | cut -d' ' -f2-)
sid=$(basename "$(dirname "$sid")")
run --session-id "$sid" "SCENARIO followup: 还有别的要改吗"
# 2. a failing command, recovered from (a fresh session)
run "SCENARIO recover: 读一下 missing.txt"

rm -rf "$here/sessions"
mkdir -p "$here/sessions"
cp -R "$sessions"/. "$here/sessions"/
find "$here/sessions" -name 'session.lock' -delete
echo "wrote $here/sessions"
