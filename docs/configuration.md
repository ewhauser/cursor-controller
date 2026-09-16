# Configuration

[README](../README.md)

Use a Helm values file for deployment-specific settings. The chart's
[values.yaml](../deploy/helm/cursor-controller/values.yaml) is the complete reference.

## Common Helm values

| Key | Default | Meaning |
| --- | --- | --- |
| `pools` | `[default]` | Pools to serve. Empty list watches every pool the key can see. |
| `controller.warmIdle` | `0` | Idle workers to keep connected per pool (needs `pools`). |
| `controller.workerIdPrefix` | `cc` | Prefix on minted worker ids; also ownership marker. Use a different prefix per cluster/controller. |
| `controller.apiTimeout` | `30s` | Deadline for ordinary Cursor JSON API calls; SSE is bounded by the resync interval. |
| `controller.wakeRetryInterval` | `2s` | Delay between transient wake retries inside Cursor's wake window. |
| `controller.workerStartupTimeout` | `10m` | Delete and release a worker that never reaches Kubernetes Ready. |
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
./bin/cursor-controller \
  --api-key-file /path/to/cursor-api-key \
  --pool monorepo \
  --backend kube --namespace cursord \
  --pod-template examples/pod-template.yaml \
  --pvc-template examples/pvc-template.yaml \
  --worker-api-key-secret cursor-api-key \
  --dispose-after 168h --dispose-archived-after 1h
```

Run `make build` first. Use `./bin/cursor-controller --help` to list flags and
their corresponding environment variables. Outside a cluster it uses your
kubeconfig; inside, it uses the service account.

## Worker templates

Set `worker.podTemplate` to a full Pod manifest when the structured chart values
are insufficient. The controller sets names, ownership labels, worker environment
variables, and `restartPolicy: Never`. With persistence enabled, it also mounts
the workspace volume. See the [Pod](../examples/pod-template.yaml) and
[PVC](../examples/pvc-template.yaml) examples.

### Controller Pod labels and worker container

Set `controller.podLabels` for controller Pod labels, including network-policy
selectors. The chart's required selector labels take precedence on collisions.
`controller.workerContainer` selects the container in a custom
`worker.podTemplate` that receives worker environment and workspace mounts;
empty preserves the default of selecting the first container.
