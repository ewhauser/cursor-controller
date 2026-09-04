#!/usr/bin/env bash
# Spawn hook for the `hook` backend. Same environment contract as
# `agent worker controller --spawn`, plus:
#   CURSOR_WAKE=1               this is a hibernation wake for an existing worker
#   CURSOR_WAKE_TIMEOUT_MS      how long Cursor will wait for the worker to reconnect
#   CURSOR_SPAWN_KIND           claim | wake | warm
#
# Exit 66 on a wake whose workspace no longer exists; the controller then
# releases the claim so Cursor re-queues the request for a fresh worker.
set -euo pipefail

: "${CURSOR_AGENT_WORKER_ID:?}"
: "${CURSOR_POOL:?}"

WORKSPACES="${WORKSPACES_DIR:-$HOME/cursor-workspaces}"
dir="$WORKSPACES/$CURSOR_AGENT_WORKER_ID"

if [[ "${CURSOR_WAKE:-0}" == "1" ]]; then
  if [[ ! -d "$dir" ]]; then
    echo "spawn: workspace $dir is gone; cannot wake" >&2
    exit 66
  fi
  echo "spawn: waking $CURSOR_AGENT_WORKER_ID (${CURSOR_WAKE_TIMEOUT_MS:-?}ms)" >&2
else
  mkdir -p "$dir"
  if [[ -n "${CURSOR_REPO_URL:-}" && ! -d "$dir/.git" ]]; then
    git clone --quiet "$CURSOR_REPO_URL" "$dir"
  fi
fi

# Detach the worker; the hook must return promptly.
nohup agent worker --pool "$CURSOR_POOL" --worker-dir "$dir" start \
  >"$dir/.worker.log" 2>&1 &
echo "spawn: started worker $CURSOR_AGENT_WORKER_ID pid $!" >&2
