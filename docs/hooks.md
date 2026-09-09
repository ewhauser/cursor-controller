# Hook backend

[README](../README.md)

Use the hook backend to provision workers through scripts outside Kubernetes.
Build the binary with `make build`, then run:

```
./bin/cursor-controller --backend hook \
  --api-key-file /path/to/cursor-api-key \
  --spawn /hooks/spawn.sh --dispose /hooks/dispose.sh \
  --state-file /var/lib/cursor-controller/state.json \
  --pool monorepo --dispose-after 24h
```

The spawn script receives the same environment as
`agent worker controller --spawn` (`CURSOR_REQUEST_ID`, `CURSOR_USER_ID`,
`CURSOR_REPO_URL/OWNER/NAME`, `CURSOR_REPO_URLS`, `CURSOR_POOL`,
`CURSOR_AGENT_WORKER_ID`, `CURSOR_WORKER_NAME`, `CURSOR_API_KEY`,
`CURSOR_API_URL`) plus:

| Variable | When | Meaning |
| --- | --- | --- |
| `CURSOR_WAKE=1` | wake | Restart an existing worker on its workspace. |
| `CURSOR_WAKE_TIMEOUT_MS` | wake | How long Cursor waits for it to reconnect. |
| `CURSOR_SPAWN_KIND` | always | `claim`, `wake`, or `warm`. |
| `CURSOR_DISPOSE_REASON` | dispose | `ttl`, `agent_archived`, `agent_deleted`, `unclaimed`, `startup_timeout`. |

Exit `66` from a wake to signal the workspace is gone; the controller releases
the claim. Liveness for hook workers is checked through
`GET /v0/private-workers/{id}` during GC. Examples in
[examples/hooks](../examples/hooks).
