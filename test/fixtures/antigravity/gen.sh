#!/usr/bin/env bash
# Regenerates test/fixtures/antigravity/antigravity-cli/ with Antigravity's CLI (agy) itself.
#
#   test/fixtures/antigravity/gen.sh [agy-binary]            (default: agy on PATH)
#
# agy runs headless (-p) with its whole data directory moved to a throwaway place (--gemini_dir)
# and a custom model in settings.json that points at the scripted mock model of
# test/fixtures/gen/mock-model.mjs (Gemini generateContent), so no conversation reaches a real
# model. Never copy a real ~/.gemini: it holds the user's conversations.
#
# agy refuses to start without a signed-in account, even for a custom model, and keeps the login
# in the data directory. The script therefore copies the login token into the throwaway directory
# for the run (AGY_TOKEN, default ~/.gemini/antigravity-cli/antigravity-oauth-token) and deletes
# it when it exits; agy talks to Google for the sign-in and its own bookkeeping, not for replies.
# The token never reaches the fixture: only the transcripts and conversation_summaries.db are kept.
#
# AGY_PROVIDER picks the custom model's apiProvider (default API_PROVIDER_GOOGLE_GEMINI); the
# settings keys come from the strings in the agy binary and are checked by the first run.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
bin=${1:-agy}
port=${MOCK_PORT:-18086}
token=${AGY_TOKEN:-$HOME/.gemini/antigravity-cli/antigravity-oauth-token}
provider=${AGY_PROVIDER:-API_PROVIDER_GOOGLE_GEMINI}
# fixed directories, so the paths recorded in the fixture are the same on every run
base=${GEN_BASE:-/tmp/mem-grep-gen}
work=${GEN_WORKDIR:-$base/work}
gemini=$base/agy-gemini
data=$gemini/antigravity-cli
rm -rf "$work" "$gemini"
mkdir -p "$work" "$data"
echo "pnpm test fails with ECONNREFUSED 127.0.0.1:5432" > "$work/notes.txt"

[ -f "$token" ] || { echo "no agy login at $token: run agy once and sign in, or set AGY_TOKEN" >&2; exit 1; }
install -m 600 "$token" "$data/antigravity-oauth-token"

MOCK_WORKDIR="$work" MOCK_SUBAGENT_TYPE=${MOCK_SUBAGENT_TYPE:-research} node "$here/../gen/mock-model.mjs" "$port" --log "$base/agy-mock.log" > "$base/agy-mock.out" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null || true; rm -f "$data/antigravity-oauth-token"' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break; sleep 0.2; done

cat > "$data/settings.json" <<JSON
{
  "customModels": {
    "mock": {
      "modelName": "mock-1",
      "apiProvider": "$provider",
      "baseUrl": "http://127.0.0.1:$port",
      "apiKey": "mock"
    }
  },
  "trustedWorkspaces": ["$work"],
  "showTips": false,
  "showFeedbackSurvey": false
}
JSON

run() {
  (cd "$work" && TERM=dumb timeout 180 "$bin" --gemini_dir="$gemini" --model mock --dangerously-skip-permissions \
    --output-format json "$@" < /dev/null > "$base/agy-run.log" 2>&1) && grep -q '"status":"SUCCESS"' "$base/agy-run.log" && return 0
  echo "agy run failed: $*" >&2
  tail -5 "$base/agy-run.log" >&2
  return 1
}

# 1. a conversation of two turns: tools succeed, then a follow-up in the same conversation
run -p "SCENARIO list-files: 看看目录里有什么，用 pnpm"
run -c -p "SCENARIO followup: 还有别的要改吗"
# 2. a failing command, recovered from
run -p "SCENARIO recover: 读一下 missing.txt"
# 3. a subagent conversation (invoke_subagent)
run -p "SCENARIO delegate: 让子任务看看"

out=$here/antigravity-cli
rm -rf "$out"
mkdir -p "$out/brain"
for t in "$data"/brain/*/.system_generated/logs/transcript_full.jsonl; do
  id=$(basename "$(dirname "$(dirname "$(dirname "$t")")")")
  mkdir -p "$out/brain/$id/.system_generated/logs"
  cp "$t" "$out/brain/$id/.system_generated/logs/"
done
# The transcripts and the summaries are kept as agy wrote them, except for what identifies the
# machine they were made on: the user and group names, the host name and the kernel release
# (in command output and the environment notes agy injects) are replaced by neutral ones.
python3 -I - "$data/conversation_summaries.db" "$out" "$(id -un)" "$(id -gn)" "$(hostname)" "$(uname -r)" <<'PY'
import glob, os, sqlite3, sys
src, out = sys.argv[1], sys.argv[2]
neutral = {k: v for k, v in zip(sys.argv[3:7], ["dev", "dev", "devbox", "6.0.0-generic"]) if len(k) >= 3}
for path in glob.glob(os.path.join(out, "brain", "*", ".system_generated", "logs", "transcript_full.jsonl")):
    text = open(path, encoding="utf-8").read()
    for old, new in neutral.items():
        text = text.replace(old, new)
    open(path, "w", encoding="utf-8").write(text)
dst = sqlite3.connect(os.path.join(out, "conversation_summaries.db"))
sqlite3.connect("file:%s?mode=ro" % src, uri=True).backup(dst)
for (table,) in dst.execute("SELECT name FROM sqlite_master WHERE type = 'table' AND sql NOT LIKE 'CREATE VIRTUAL%'").fetchall():
    for column in dst.execute('PRAGMA table_info("%s")' % table).fetchall():
        if column[2].lower() != "text":
            continue
        for old, new in neutral.items():
            dst.execute('UPDATE "%s" SET "%s" = replace("%s", ?, ?) WHERE instr("%s", ?) > 0' % (table, column[1], column[1], column[1]), (old, new, old))
dst.commit()
dst.execute("PRAGMA journal_mode=DELETE")
dst.execute("VACUUM")
dst.close()
PY
echo "wrote $out"
