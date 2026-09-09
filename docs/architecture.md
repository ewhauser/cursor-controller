# Architecture

[README](../README.md)

## How a request flows

1. A user starts a cloud agent against your pool. Cursor records a pending
   request.
2. The controller sees it (list on startup / every `--resync-interval`, or a
   `created` event on the stream) and `POST`s a claim with a freshly minted
   worker id `<prefix>-<12 hex>`. Claims are atomic server-side; a `409` means
   another controller won.
3. **kube backend:** creates PVC `<worker>-ws` from the PVC template (if
   configured) and Pod `<worker>-<rand>` from the Pod template with
   `CURSOR_POOL`, `CURSOR_AGENT_WORKER_ID`, `CURSOR_REQUEST_ID`,
   `CURSOR_REPO_*`, and the PVC mounted at `--workspace-mount-path`.
   If Pod creation fails the claim is released.
4. The worker connects with that id, Cursor attaches the session, the agent
   runs. After `--idle-release-timeout` the worker exits 0, the Pod goes
   `Succeeded`, and Kubernetes detaches the volume. The PVC stays.
   Warm workers are associated with their request by reconciling Cursor's
   connected-worker `activeBcId`; the `claimed` stream event itself contains
   only the request id.
5. The user sends a follow-up hours later. Cursor emits `claimed_offline` with
   `claimedWorkerId` and `wakeTimeoutMs`. The controller finds the PVC, deletes
   the terminal Pod, creates a new Pod for the same worker id with
   `CURSOR_WAKE=1`. The worker reconnects and the thread continues on the same
   checkout.
6. Eventually the user archives the agent. On the next GC tick the controller
   sees `status: ARCHIVED`, waits out `--dispose-archived-after`, and deletes
   the PVC and leftover Pods. Anything idle longer than `--dispose-after` is
   disposed regardless.
