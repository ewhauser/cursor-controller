# Operations

[README](../README.md)

- **One controller per pool set.** Claims are atomic, so a second replica is
  unnecessary for claim processing, but warm-idle and GC assume a single actor.
  The chart uses `strategy: Recreate`.
- **Ownership.** The controller only wakes and disposes workers whose ids
  carry its `--worker-id-prefix`, and only touches Pods/PVCs labelled
  `cursor-controller.dev/worker-id`. Run several controllers (or clusters)
  against one pool with distinct prefixes.
- **Deleting workspaces by hand.** Kubernetes' pvc-protection finalizer keeps
  a PVC in `Terminating` while any scheduled pod references it, and a
  `Succeeded` worker pod still counts. Delete the finished pod first, or let
  the controller do it: `Dispose` removes pods before the PVC. A wake that
  finds a terminating PVC releases the claim so Cursor re-queues the request.
- **Stuck wakes.** If a wake keeps failing (volume topology, quota), the
  controller retries inside the pool's `workerReadyTimeoutSeconds` window,
  then re-attempts if Cursor lists the request again. A worker Pod that never
  becomes Ready is removed after `--worker-startup-timeout`; its claim is
  released so the request can be reassigned.
- **Auth.** A `401`/`403` from the fleet API exits the process by default
  (`--exit-on-auth-error`) so the deployment reports an authentication failure.
  Pool workers require a *team service-account* key; personal keys are rejected.
- **Worker credential scope.** Worker Pods currently receive the service-account
  key. A repository-scoped key with `--repository` can reduce its blast radius,
  but does not remove credential exposure inside workers. See the
  [managed worker-token proposal](proposals/worker-tokens.md) for the API
  dependency and implementation requirements.
- **Metrics** on `--metrics-addr` (`:8080`): `cursor_controller_claims_total`,
  `spawns_total`, `wakes_total`, `releases_total`, `disposals_total`,
  `stream_reconnects_total`, `workers{state}`, `warm_idle_deficit`,
  `api_requests_total`, `api_request_seconds`. `/readyz` turns 200 after the
  first successful list.

### Startup versus busy readiness

The startup deadline is measured from Pod creation, including scheduling and
workspace restore. It stops applying after the worker passes its startup probe
or is observed ready. That fact is recorded on the Pod as
`cursor-controller.dev/started`, survives controller restarts, and resets for
new Pods on wake. Later `/readyz` failures during a session do not trigger
startup cleanup. Terminal Pods are still treated as stopped.

The chart and example use `/healthz` for the startup probe and `/readyz` for
readiness. Custom templates should also set a startup probe: polling can miss
a brief ready period before a session starts. `containerStatuses.started` is
only trusted when that container has a startup probe. For existing Pods without
one, keep the `/healthz` readiness workaround until workers are recreated with
the updated template. Increase both the controller deadline and startup probe
budget for slow restores. With probes disabled there is no durable startup
signal unless the controller observes readiness.
