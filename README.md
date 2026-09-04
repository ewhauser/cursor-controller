# cursor-controller

An open-source controller for [Cursor self-hosted pool workers](https://cursor.com/docs/cloud-agent/self-hosted/pool)
on Kubernetes, written in Go.

It does what `agent worker controller --spawn` does (poll pending requests,
follow the SSE stream, claim, start a worker) and adds the pieces you need to
run agents on **retained, snapshot-seeded workspaces**:

- **Native Kubernetes backend.** Workers are plain Pods created through the
  API, not `kubectl` from a shell hook. One retained PersistentVolumeClaim per
  worker, created from a template (typically with a `VolumeSnapshot`
  `dataSource`, e.g. a fresh EBS snapshot of your monorepo), mounted into every
  incarnation of that worker.
- **Hibernation wake.** When Cursor reports `claimed_offline` for a worker this
  controller minted, it starts a new Pod for the *same* worker id on the *same*
  PVC. If the PVC is gone, it releases the claim so Cursor re-queues the
  request instead of leaving it stuck.
- **Workspace garbage collection.** A reconcile loop disposes workspaces when
  the agent is **archived or deleted** (via `GET /v1/agents/{id}`), after a
  hard **TTL**, or when a warm worker never received a request. This is the
  `--dispose` / `--dispose-after` hook the CLI controller lacks.
- **Hook backend** for non-Kubernetes hosts: `--spawn` and `--dispose` scripts
  with the same `CURSOR_*` environment as the official controller.
- Optional **warm-idle** capacity per pool, Prometheus metrics, health probes,
  and a Helm chart.

Roughly: list → claim → spawn, stream → wake, tick → dispose.

```
              Cursor fleet API (api.cursor.com)
    ┌──────────────────────────────────────────────────────┐
    │ GET  /v0/private-workers/pending-requests            │
    │ GET  /v0/private-workers/pending-requests/stream     │
    │ POST /v0/private-workers/claim                       │
    │ POST /v0/private-workers/claims/{id}/release         │
    │ GET  /v1/agents/{id}   (ARCHIVED? → dispose)         │
    └───────────────▲──────────────────────────────────────┘
                    │
            cursor-controller (1 replica)
     ┌──────────────┼─────────────────────────────┐
     │ watch loop   │  warm loop   │   gc loop     │
     └──────┬───────┴──────┬───────┴──────┬────────┘
            ▼              ▼              ▼
   Pod <worker>-xxxxx   Pod (warm)    delete PVC + pods
   PVC <worker>-ws  ◄── seeded from VolumeSnapshot, re-mounted on wake
```

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
5. The user sends a follow-up hours later. Cursor emits `claimed_offline` with
   `claimedWorkerId` and `wakeTimeoutMs`. The controller finds the PVC, deletes
   the terminal Pod, creates a new Pod for the same worker id with
   `CURSOR_WAKE=1`. The worker reconnects and the thread continues on the same
   checkout.
6. Eventually the user archives the agent. On the next GC tick the controller
   sees `status: ARCHIVED`, waits out `--dispose-archived-after`, and deletes
   the PVC and leftover Pods. Anything idle longer than `--dispose-after` is
   disposed regardless.

Set a non-zero `workerReadyTimeoutSeconds` on the pool so Cursor waits for the
wake (Pod scheduling + volume attach usually needs a couple of minutes):

```bash
curl -X POST https://api.cursor.com/v0/private-workers/pools -u "$CURSOR_API_KEY:" \
  -H 'Content-Type: application/json' \
  -d '{"scope":"team","poolName":"monorepo","workerReadyTimeoutSeconds":600}'
```

## Install with Helm

Prerequisites: a Cursor Enterprise team with self-hosted pools, a **team
service-account API key** with agent scope, a worker image with the `agent`
CLI and `git`, and (for retained workspaces) a CSI driver with snapshot
support.

```bash
kubectl create namespace cursord
kubectl -n cursord create secret generic cursor-api-key --from-literal=api-key='YOUR_SERVICE_ACCOUNT_KEY'

helm upgrade --install cursor-controller deploy/helm/cursor-controller -n cursord \
  --set auth.existingSecret=cursor-api-key \
  --set 'pools={monorepo}' \
  --set worker.image.repository=YOUR_REGISTRY/cursor-worker \
  --set worker.image.tag=YOUR_TAG \
  --set persistence.enabled=true \
  --set persistence.claimSpec.storageClassName=gp3 \
  --set persistence.claimSpec.resources.requests.storage=200Gi \
  --set persistence.claimSpec.dataSource.apiGroup=snapshot.storage.k8s.io \
  --set persistence.claimSpec.dataSource.kind=VolumeSnapshot \
  --set persistence.claimSpec.dataSource.name=monorepo-latest
```

The worker Pod the chart assembles runs:

```
agent worker --pool $(CURSOR_POOL) --idle-release-timeout 600 \
  --worker-dir /workspace --management-addr 0.0.0.0:8080 start
```

Set `worker.podTemplate` to a full Pod manifest instead if you need something
the structured values do not express. The controller only requires one
container; it overwrites name/namespace/labels, forces
`restartPolicy: Never`, injects the `CURSOR_*` env, and adds the `workspace`
volume. See [examples/pod-template.yaml](examples/pod-template.yaml) and
[examples/pvc-template.yaml](examples/pvc-template.yaml).

Keep the `VolumeSnapshot` name stable (`monorepo-latest`) and have your
snapshot pipeline re-point it every 30 minutes; every new claim then starts
from the newest snapshot while existing workers keep their own volume.

Check it:

```bash
kubectl -n cursord logs deploy/cursor-controller -f
kubectl -n cursord get pods,pvc -l app.kubernetes.io/managed-by=cursor-controller
```

### Chart values worth knowing

| Key | Default | Meaning |
| --- | --- | --- |
| `pools` | `[default]` | Pools to serve. Empty list watches every pool the key can see. |
| `controller.warmIdle` | `0` | Idle workers to keep connected per pool (needs `pools`). |
| `controller.workerIdPrefix` | `cc` | Prefix on minted worker ids; also ownership marker. Use a different prefix per cluster/controller. |
| `persistence.enabled` | `false` | Retained PVC per worker. |
| `persistence.claimSpec` | gp3 / 100Gi | Raw PVC spec, usually with a `VolumeSnapshot` dataSource. |
| `gc.disposeAfter` | `168h` | Hard TTL for offline workers. |
| `gc.disposeArchivedAfter` | `1h` | Grace after an agent is archived/deleted before its workspace goes. |
| `gc.disposeUnclaimedAfter` | `1h` | TTL for warm workers that exited without a request. |
| `gc.checkAgent` | `true` | Query `/v1/agents/{id}`. If the key lacks that scope, the controller logs once and falls back to TTL only. |
| `worker.*` | | Image, args, resources, probes, scheduling for worker Pods. |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator scrape config. |

RBAC is a namespaced Role: pods (create/get/list/watch/patch/delete) and, with
persistence, persistentvolumeclaims (same verbs).

## Run the binary directly

```
cursor-controller \
  --api-key "$CURSOR_API_KEY" \
  --pool monorepo \
  --backend kube --namespace cursord \
  --pod-template examples/pod-template.yaml \
  --pvc-template examples/pvc-template.yaml \
  --worker-api-key-secret cursor-api-key \
  --dispose-after 168h --dispose-archived-after 1h
```

Every flag has an environment-variable twin (`--help` lists them). Outside a
cluster it uses your kubeconfig; inside, the service account.

### Hook backend

For hosts that are not Kubernetes, or to reuse an existing `spawn.sh`:

```
cursor-controller --backend hook \
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
| `CURSOR_DISPOSE_REASON` | dispose | `ttl`, `agent_archived`, `agent_deleted`, `unclaimed`. |

Exit `66` from a wake to signal the workspace is gone; the controller releases
the claim. Liveness for hook workers is checked through
`GET /v0/private-workers/{id}` during GC. Examples in
[examples/hooks](examples/hooks).

## Operational notes

- **One controller per pool set.** Claims are atomic, so a second replica is
  merely wasteful for claiming, but warm-idle and GC assume a single actor.
  The chart uses `strategy: Recreate`.
- **Ownership.** The controller only wakes and disposes workers whose ids
  carry its `--worker-id-prefix`, and only touches Pods/PVCs labelled
  `cursor-controller.dev/worker-id`. Run several controllers (or clusters)
  against one pool with distinct prefixes.
- **Deleting workspaces by hand.** Kubernetes' pvc-protection finalizer keeps
  a PVC in `Terminating` while any scheduled pod references it, and a
  `Succeeded` worker pod still counts. Delete the finished pod first, or let
  the controller do it: `Dispose` removes pods before the claim. A wake that
  finds a terminating PVC releases the claim so Cursor re-queues the request.
- **Stuck wakes.** If a wake keeps failing (volume topology, quota), the
  request stays claimed until the pool's `workerReadyTimeoutSeconds` lapses
  and Cursor reassigns it. The controller re-attempts on every re-list.
- **Auth.** A `401`/`403` from the fleet API exits the process by default
  (`--exit-on-auth-error`) so a bad key shows up as a crash loop, not silence.
  Pool workers require a *team service-account* key; personal keys are rejected.
- **Metrics** on `--metrics-addr` (`:8080`): `cursor_controller_claims_total`,
  `spawns_total`, `wakes_total`, `releases_total`, `disposals_total`,
  `stream_reconnects_total`, `workers{state}`, `warm_idle_deficit`,
  `api_requests_total`, `api_request_seconds`. `/readyz` turns 200 after the
  first successful list.

## Testing

Three layers, cheapest first:

| Layer | Command | Needs | What it proves |
| --- | --- | --- | --- |
| Unit | `make test` | go | Parsers, claim/wake/GC logic, Pod/PVC construction (fake clientset), hook env contract, plus a component test that runs the real HTTP client and controller against the in-memory fake API. |
| Laptop e2e | `./hack/local-e2e.sh` | go, curl | Real processes: fake fleet API, controller (hook backend), fake worker. Claim, idle exit, follow-up wake with the workspace intact, missing workspace → release → re-claim, archive → dispose hook. About 40 seconds. |
| kind e2e | `make e2e-setup && make e2e` | docker with buildx, kind, kubectl, helm | Real cluster with the CSI hostpath driver and snapshot controller: PVC cloned from a `VolumeSnapshot`, Pod lifecycle, wake on the same PVC with the marker file present, PVC deletion on archive, controller restart without duplicate claims. `make e2e-teardown` removes the cluster. |

On macOS without Docker Desktop: `brew install colima docker docker-buildx kind helm`,
link the buildx plugin (`mkdir -p ~/.docker/cli-plugins && ln -sfn $(brew --prefix)/opt/docker-buildx/bin/docker-buildx ~/.docker/cli-plugins/docker-buildx`),
then `colima start --cpu 4 --memory 8`. The setup script is idempotent; rerun it after a failure.

The fakes live in [internal/fakeapi](internal/fakeapi) (fleet API with
`/fake/*` admin endpoints: inject requests, connect/disconnect workers,
follow-up, archive, inspect state) and [cmd/fake-worker](cmd/fake-worker)
(accepts `agent worker ... start` arguments, registers with the fake, keeps a
marker file in the workspace, exits after the idle timeout).

To run the controller by hand against the fake: `make fake-api` in one
terminal, then start the controller with `--api-url http://localhost:8081
--api-key dev`, and inject work with
`curl -X POST localhost:8081/fake/requests -d '{"pool":"default"}'`.

**Against real Cursor.** The payload shapes for `claimed_offline` and
`claimed` events were inferred from the docs. Run
`CURSOR_API_KEY=... POOL=<dev-pool> ./hack/capture-fixtures.sh` once against
a dev pool while starting an agent and sending a follow-up; it stores the
responses under `internal/cursorapi/testdata`, and the fixture tests then
validate the decoders against real data. For a full run, point the kind
setup or a dev namespace at the real API with a dedicated pool and a short
`idleReleaseTimeout`, trigger an agent with `POST /v1/agents`
(`env.type: pool`), follow up with `POST /v1/agents/{id}/followup`, and archive
with `POST /v1/agents/{id}/archive`.

## OpenTelemetry (optional)

Set `OTEL_EXPORTER_OTLP_ENDPOINT` (or pass `--otel`) and the controller exports
over OTLP:

- **Traces.** One span per claim (`controller.claim_and_spawn`), wake
  (`controller.wake`), release, warm reconcile, GC pass, and disposal, with
  `cursor.request_id`, `cursor.worker_id`, `cursor.pool`, `cursor.spawn_kind`,
  and `cursor.result` attributes. Every outbound call to the Cursor fleet API
  (`cursor-api POST /v0/private-workers/claim`, with ids collapsed to `{id}`)
  and to the Kubernetes API is a child span, so a slow wake shows exactly
  whether the time went into the claim, the PVC, or the pod.
- **Metrics.** The Prometheus registry is bridged and pushed over OTLP, so the
  same `cursor_controller_*` series land in your collector without a scrape.
  `/metrics` keeps working either way.

Flags and their env twins: `--otel` (`CONTROLLER_OTEL`, auto-on when an OTLP
endpoint variable is set), `--otel-traces`, `--otel-metrics`, `--otel-protocol`
(`grpc` default, or `http/protobuf`; also read from `OTEL_EXPORTER_OTLP_PROTOCOL`),
`--otel-service-name`. Headers, TLS, timeouts, export intervals, and resource
attributes follow the standard `OTEL_EXPORTER_OTLP_*`, `OTEL_METRIC_EXPORT_INTERVAL`,
and `OTEL_RESOURCE_ATTRIBUTES` variables.

Chart:

```yaml
otel:
  enabled: true
  endpoint: http://otel-collector.observability.svc:4317
  protocol: grpc
  resourceAttributes: deployment.environment=prod
```

Quick local check: run an OTel collector with the debug exporter, then
`OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 ./hack/local-e2e.sh` and
watch spans and metrics arrive.

## Development

```bash
make test            # go test -race ./...
make build           # bin/cursor-controller
make image           # docker build
make helm-template   # render the chart
./hack/local-e2e.sh  # laptop end-to-end with the fakes
```

Layout:

```
cmd/cursor-controller     flags, wiring
internal/cursorapi        fleet API client + SSE parser
internal/controller       watch / warm / gc loops
internal/backend          Backend interface, env contract
internal/backend/kube     Pods + PVCs via client-go
internal/backend/hook     spawn/dispose scripts + JSON state
internal/metrics          Prometheus + health endpoints
internal/fakeapi          in-memory Cursor fleet API for tests
cmd/fake-cursor-api       the fake as a binary
cmd/fake-worker           stand-in for `agent worker start`
test/e2e                  kind scenarios (build tag e2e)
hack/e2e                  kind setup, images, chart values
deploy/helm               chart
examples/                 pod/pvc templates, hook scripts
```

## License

Apache-2.0. Cursor is a trademark of Anysphere, Inc.; this project is not
affiliated with or endorsed by Anysphere.
