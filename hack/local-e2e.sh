#!/usr/bin/env bash
# Laptop end-to-end with no cluster: fake Cursor API + controller (hook
# backend) + fake worker as real processes. Exercises claim -> spawn, idle
# exit, follow-up -> wake with the workspace intact, missing workspace ->
# release -> re-claim, and archive -> dispose. Requires go and curl.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${PORT:-18081}
API="http://127.0.0.1:$PORT"
KEY=local-e2e
WORK=$(mktemp -d)
trap '{ kill $(jobs -p) 2>/dev/null; wait; } 2>/dev/null; rm -rf "$WORK"' EXIT
export WORKSPACES_DIR="$WORK/workspaces"
mkdir -p "$WORKSPACES_DIR" "$WORK/bin"

log() { printf '\n==> %s\n' "$*" >&2; }
fake() { curl -fsS -X "${1}" "$API/fake/${2}" -H 'Content-Type: application/json' ${3:+-d "$3"}; }
state() { curl -fsS "$API/fake/state"; }
jsonq() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_for() { # wait_for <desc> <seconds> <python expr over state d>
  local desc=$1 secs=$2 expr=$3 i
  for i in $(seq 1 "$secs"); do
    if [ "$(state | jsonq "$expr")" = "True" ]; then log "ok: $desc"; return 0; fi
    sleep 1
  done
  echo "TIMEOUT waiting for: $desc" >&2; state | python3 -m json.tool >&2; return 1
}

log "building binaries"
go build -o "$WORK/bin/" ./cmd/...

log "starting fake API on $API"
"$WORK/bin/fake-cursor-api" --addr "127.0.0.1:$PORT" --api-key "$KEY" --heartbeat 2s >"$WORK/fake-api.log" 2>&1 &
for i in $(seq 1 50); do curl -fsS "$API/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
fake POST pools '{"poolName":"local","workerReadyTimeoutSeconds":120}' >/dev/null

# Spawn hook: launches fake-worker detached, like a real spawn script would
# launch `agent worker start`. Exits 66 on wake when the workspace is gone.
cat > "$WORK/spawn.sh" <<HOOK
#!/usr/bin/env bash
set -euo pipefail
dir="\$WORKSPACES_DIR/\$CURSOR_AGENT_WORKER_ID"
if [ "\${CURSOR_WAKE:-0}" = "1" ] && [ ! -d "\$dir" ]; then echo "workspace missing" >&2; exit 66; fi
mkdir -p "\$dir"
export FAKE_API_URL="$API"
nohup "$WORK/bin/fake-worker" worker --pool "\$CURSOR_POOL" --worker-dir "\$dir" --idle-release-timeout 3 start >"\$dir.log" 2>&1 &
HOOK
cat > "$WORK/dispose.sh" <<HOOK
#!/usr/bin/env bash
echo "dispose \$CURSOR_AGENT_WORKER_ID reason=\$CURSOR_DISPOSE_REASON" >> "$WORK/disposed.txt"
rm -rf "\$WORKSPACES_DIR/\$CURSOR_AGENT_WORKER_ID"
HOOK
chmod +x "$WORK"/*.sh

log "starting controller (hook backend)"
"$WORK/bin/cursor-controller" --backend hook --api-url "$API" --api-key "$KEY" --pool local \
  --spawn "$WORK/spawn.sh" --dispose "$WORK/dispose.sh" --state-file "$WORK/state.json" \
  --worker-id-prefix local --resync-interval 5s --gc-interval 2s --dispose-archived-after 1s --dispose-after 1h \
  --metrics-addr 127.0.0.1:0 --log-format text --log-level debug >"$WORK/controller.log" 2>&1 &
sleep 1

log "1. inject request -> expect claim, spawn, worker connect"
REQ=$(fake POST requests '{"pool":"local","repoUrl":"https://github.com/acme/mono","userId":7}' | jsonq 'd["id"]')
wait_for "request $REQ claimed" 20 "any(r['id']=='$REQ' and r['status']=='claimed' for r in d['requests'])"
WORKER=$(state | jsonq "[r for r in d['requests'] if r['id']=='$REQ'][0]['claimedWorkerId']")
wait_for "worker $WORKER connected (fresh, no marker)" 20 "any(c['workerId']=='$WORKER' and not c['wake'] and not c['markerFound'] for c in d['connects'])"
test -f "$WORKSPACES_DIR/$WORKER/.fake-worker/marker.json"

log "2. worker idles out -> expect disconnect, agent IDLE, workspace retained"
wait_for "worker offline" 20 "not [w for w in d['workers'] if w['workerId']=='$WORKER'][0]['connected']"
wait_for "agent idle" 5 "[a for a in d['agents'] if a['id']=='$REQ'][0]['status']=='IDLE'"
test -d "$WORKSPACES_DIR/$WORKER"

log "3. follow-up -> claimed_offline -> expect wake on same worker with marker found"
fake POST "agents/$REQ/followup" >/dev/null
wait_for "wake connect with marker" 20 "any(c['workerId']=='$WORKER' and c['wake'] and c['markerFound'] for c in d['connects'])"
wait_for "worker offline again" 20 "not [w for w in d['workers'] if w['workerId']=='$WORKER'][0]['connected']"

log "4. delete workspace, follow-up -> expect release, re-queue, new worker"
rm -rf "$WORKSPACES_DIR/$WORKER"
fake POST "agents/$REQ/followup" >/dev/null
wait_for "claim released" 20 "'$REQ' in d['releases']"
wait_for "re-claimed by a different worker" 20 "any(r['id']=='$REQ' and r['status']=='claimed' and r['claimedWorkerId']!='$WORKER' for r in d['requests'])"
WORKER2=$(state | jsonq "[r for r in d['requests'] if r['id']=='$REQ'][0]['claimedWorkerId']")
wait_for "new worker $WORKER2 connected fresh" 20 "any(c['workerId']=='$WORKER2' and not c['wake'] for c in d['connects'])"
wait_for "new worker offline" 20 "not [w for w in d['workers'] if w['workerId']=='$WORKER2'][0]['connected']"

log "5. archive agent -> expect dispose hook for both workers"
fake POST "agents/$REQ/archive" >/dev/null
for i in $(seq 1 30); do
  if [ -f "$WORK/disposed.txt" ] && grep -q "$WORKER2 reason=agent_archived" "$WORK/disposed.txt"; then break; fi
  sleep 1
done
grep -q "$WORKER2 reason=agent_archived" "$WORK/disposed.txt" || { echo "dispose hook did not fire"; cat "$WORK/controller.log"; exit 1; }
test ! -d "$WORKSPACES_DIR/$WORKER2"
log "disposed: $(cat "$WORK/disposed.txt" | tr '\n' ';')"

log "ALL SCENARIOS PASSED"
echo "controller log tail:" >&2; tail -5 "$WORK/controller.log" >&2
