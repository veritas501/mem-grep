#!/usr/bin/env bash
# Regenerates test/fixtures/real/<agent>/ : sessions an agent wrote while talking to a REAL model
# (DeepSeek's OpenAI-compatible API) about a small coding task. Unlike the scripted fixtures next
# to it these are not deterministic, so the tests only assert what must hold for any conversation.
#
#   DEEPSEEK_API_KEY=… test/fixtures/real/gen.sh gemini|qwen|opencode [agent-binary]
#
# The key is only ever in the environment: it is handed to the agent (or to the Gemini translator,
# test/fixtures/gen/gemini-to-openai.mjs) and written nowhere. Check a fresh fixture with
# `grep -r "sk-" test/fixtures/real` before committing. The agent runs in a throwaway HOME in
# /tmp/mem-grep-gen/real-work, on a three-file Go module with a bug. Versions: see the other gen.sh.
set -euo pipefail

agent=${1:?usage: gen.sh gemini|qwen|opencode [agent-binary]}
bin=${2:-$agent}
: "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
base=${UPSTREAM_BASE_URL:-https://api.deepseek.com/v1}
model=${UPSTREAM_MODEL:-deepseek-flash}
here=$(cd "$(dirname "$0")" && pwd)
world=$(mktemp -d)
work=${GEN_WORKDIR:-/tmp/mem-grep-gen/real-work}
rm -rf "$work"
mkdir -p "$world/home" "$work"

cat > "$work/go.mod" <<'GO'
module example.com/calc

go 1.24
GO
cat > "$work/calc.go" <<'GO'
package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b
}
GO
cat > "$work/calc_test.go" <<'GO'
package calc

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2, 3) = %d, want 5", got)
	}
}
GO
p1="运行 go test ./...，找出失败的原因并修复，然后再运行一次确认测试通过。回答保持简短。"
p2="再加一个 Sub 函数和对应的测试，并运行测试确认通过。"
# The agents run in a cleaned environment, but behind an egress proxy they still need its settings
# (and Node's own fetch needs to be told to use them): pass those on, and nothing else.
netenv=(NODE_USE_ENV_PROXY=1)
for v in HTTPS_PROXY https_proxy NO_PROXY no_proxy SSL_CERT_FILE; do
  [ -n "${!v:-}" ] && netenv+=("$v=${!v}")
done
[ -n "${SSL_CERT_FILE:-}" ] && netenv+=("NODE_EXTRA_CA_CERTS=$SSL_CERT_FILE")
cleanenv=(env -i PATH="$PATH" HOME="$world/home" GOCACHE="$world/gocache" GOFLAGS=-mod=mod "${netenv[@]}")
pids=()
trap 'for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done' EXIT

case $agent in
qwen)
  run() { (cd "$work" && "${cleanenv[@]}" OPENAI_API_KEY="$DEEPSEEK_API_KEY" OPENAI_BASE_URL="$base" OPENAI_MODEL="$model" \
      timeout 300 "$bin" --auth-type openai --approval-mode yolo "$@" > "$world/run.log" 2>&1); }
  run "$p1"; run --continue "$p2"
  proj=$(ls -d "$world"/home/.qwen/projects/*/)
  out=$here/qwen/projects/$(basename "$proj"); rm -rf "$here/qwen"; mkdir -p "$out"
  cp -R "$proj/chats" "$out/chats"; find "$out" -name '*.runtime.json' -delete ;;
gemini)
  port=${PROXY_PORT:-18090}
  mkdir -p "$world/home/.gemini"
  cat > "$world/home/.gemini/settings.json" <<'JSON'
{ "security": { "auth": { "selectedType": "gemini-api-key" }, "folderTrust": { "enabled": false } },
  "general": { "enableAutoUpdate": false, "enableAutoUpdateNotification": false },
  "privacy": { "usageStatisticsEnabled": false } }
JSON
  env UPSTREAM_API_KEY="$DEEPSEEK_API_KEY" "${netenv[@]}" node "$here/../gen/gemini-to-openai.mjs" "$port" "$base" "$model" > "$world/proxy.out" 2>&1 &
  pids+=($!)
  for _ in $(seq 50); do grep -q "on http" "$world/proxy.out" && break; sleep 0.2; done
  run() { (cd "$work" && "${cleanenv[@]}" GEMINI_API_KEY=unused GOOGLE_GEMINI_BASE_URL="http://127.0.0.1:$port" \
      timeout 300 "$bin" --yolo "$@" > "$world/run.log" 2>&1); }
  run -p "$p1"; run --resume latest -p "$p2"
  proj=$(ls -d "$world"/home/.gemini/tmp/*/ | head -1)
  out=$here/gemini/tmp/$(basename "$proj"); rm -rf "$here/gemini"; mkdir -p "$out"
  cp -R "$proj/chats" "$out/chats"; cp "$proj/.project_root" "$out/.project_root" ;;
opencode)
  mkdir -p "$world"/{data,config,cache,state}
  cat > "$work/opencode.json" <<JSON
{
  "\$schema": "https://opencode.ai/config.json",
  "autoupdate": false, "share": "disabled",
  "model": "deepseek/$model",
  "permission": { "bash": "allow", "edit": "allow", "read": "allow", "webfetch": "deny" },
  "provider": { "deepseek": { "npm": "@ai-sdk/openai-compatible", "name": "DeepSeek",
    "options": { "baseURL": "$base", "apiKey": "{env:DEEPSEEK_API_KEY}" },
    "models": { "$model": { "name": "$model", "tool_call": true, "limit": { "context": 200000, "output": 8000 } } } } }
}
JSON
  run() { (cd "$work" && "${cleanenv[@]}" XDG_DATA_HOME="$world/data" XDG_CONFIG_HOME="$world/config" XDG_CACHE_HOME="$world/cache" \
      XDG_STATE_HOME="$world/state" DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_AUTOUPDATE=1 \
      timeout 300 "$bin" run --print-logs "$@" > "$world/run.log" 2>&1); }
  run "$p1"; run --continue "$p2"
  rm -rf "$here/opencode"; mkdir -p "$here/opencode"
  python3 -I - "$world/data/opencode/opencode.db" "$here/opencode/opencode.db" <<'PY'
import sqlite3, sys
src = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True)
src.execute("PRAGMA wal_checkpoint(TRUNCATE)")
dst = sqlite3.connect(sys.argv[2])
src.backup(dst)
dst.execute("PRAGMA journal_mode=DELETE")
dst.execute("VACUUM")
dst.close()
PY
  ;;
*) echo "unknown agent $agent" >&2; exit 2 ;;
esac

if grep -rIl -a "sk-" "$here/$agent" >/dev/null 2>&1 || grep -rl -a "$DEEPSEEK_API_KEY" "$here/$agent" >/dev/null 2>&1; then
  echo "REFUSING: something that looks like a key is in $here/$agent" >&2; exit 1
fi
echo "wrote $here/$agent (work dir was $work; go test result: $(cd "$work" && GOCACHE="$world/gocache" go test ./... 2>&1 | tail -1))"
