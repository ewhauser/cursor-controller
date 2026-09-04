#!/usr/bin/env bash
# Dispose hook for the `hook` backend. Called once per worker when the
# controller decides its workspace can go. Environment:
#   CURSOR_AGENT_WORKER_ID   worker whose workspace should be removed
#   CURSOR_REQUEST_ID        agent id (if the worker ever got one)
#   CURSOR_POOL              pool name
#   CURSOR_DISPOSE_REASON    ttl | agent_archived | agent_deleted | unclaimed
set -euo pipefail

: "${CURSOR_AGENT_WORKER_ID:?}"
WORKSPACES="${WORKSPACES_DIR:-$HOME/cursor-workspaces}"
dir="$WORKSPACES/$CURSOR_AGENT_WORKER_ID"
echo "dispose: removing $dir (${CURSOR_DISPOSE_REASON:-unknown})" >&2
rm -rf -- "$dir"
