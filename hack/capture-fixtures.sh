#!/usr/bin/env bash
# Capture real Cursor fleet API responses as test fixtures so the decoders are
# checked against actual payloads, not guesses from the docs.
#
#   CURSOR_API_KEY=... POOL=my-pool ./hack/capture-fixtures.sh
#
# Writes internal/cursorapi/testdata/*.json and stream.sse. Start an agent
# against the pool (dashboard, or POST /v1/agents with env.type=pool) while
# the stream capture runs (STREAM_SECONDS, default 60) to record `created`;
# with a hibernated worker, send a follow-up to record `claimed_offline`.
# Review the files before committing: they contain user ids and repo names.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${CURSOR_API_KEY:?}"
API=${CURSOR_API_URL:-https://api.cursor.com}
POOL=${POOL:-default}
OUT=internal/cursorapi/testdata
STREAM_SECONDS=${STREAM_SECONDS:-60}
mkdir -p "$OUT"
h=(-sS -H "Authorization: Bearer $CURSOR_API_KEY" -H "Accept: application/json")

curl "${h[@]}" "$API/v0/private-workers/pending-requests?pool=$POOL&limit=100" -o "$OUT/pending-requests.json"
curl "${h[@]}" "$API/v0/private-workers/pools?includeStale=true" -o "$OUT/pools.json"
curl "${h[@]}" "$API/v0/private-workers?limit=100" -o "$OUT/workers.json"
cursor=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("streamCursor",""))' "$OUT/pending-requests.json")
echo "captured list/pools/workers; streaming for ${STREAM_SECONDS}s with cursor=$cursor" >&2
curl -sS --no-buffer --max-time "$STREAM_SECONDS" -H "Authorization: Bearer $CURSOR_API_KEY" -H "Accept: text/event-stream" \
  "$API/v0/private-workers/pending-requests/stream?pool=$POOL&cursor=$cursor" -o "$OUT/stream.sse" || true
agent=$(python3 -c 'import json,sys;r=json.load(open(sys.argv[1])).get("requests",[]);print(r[0]["id"] if r else "")' "$OUT/pending-requests.json")
if [ -n "$agent" ]; then
  curl "${h[@]}" "$API/v1/agents/$agent" -o "$OUT/agent.json"
fi
ls -la "$OUT"
