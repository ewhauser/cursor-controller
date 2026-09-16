# Persistent workspaces

[README](../README.md)

Pre-populated workspace volumes are the primary use case for this controller.
Prepare a volume containing cloned source code and create a snapshot of it. The
controller uses that snapshot to initialize a separate writable PVC for each
worker, avoiding a full repository clone on every worker startup.

The snapshot contains the source tree as it existed when prepared. Refresh it
through your own pipeline; workers may still need to fetch updates or check out
the revision required by a request.

Persistence creates one PVC per worker and reuses it when that worker resumes.
Workspace cleanup deletes the PVC according to the configured retention policy.
Review [operations](operations.md) before enabling cleanup for valuable data.

## Enable persistence

Create a values file with a StorageClass available in your cluster:

```yaml
persistence:
  enabled: true
  mountPath: /workspace
  claimSpec:
    accessModes: [ReadWriteOnce]
    storageClassName: your-storage-class
    resources:
      requests:
        storage: 100Gi
```

Pass the file with `--values workspace-values.yaml` alongside the installation
arguments in the [quick start](../README.md#quick-start). The chart defaults to
`gp3`; override it when your cluster uses a different StorageClass.

## Initialize from a snapshot

To seed new workspaces, add an existing VolumeSnapshot as the PVC data source:

```yaml
persistence:
  enabled: true
  claimSpec:
    storageClassName: your-storage-class
    dataSource:
      apiGroup: snapshot.storage.k8s.io
      kind: VolumeSnapshot
      name: workspace-seed
```

The snapshot must be available in the controller's namespace and compatible with
the selected CSI driver and requested volume size. Snapshot creation and rotation
are managed outside this controller. Update the configured snapshot name when
publishing a new seed; existing worker PVCs retain their data.

## Allow time for resume

Set the pool's `workerReadyTimeoutSeconds` to allow for scheduling, image pulls,
and volume attachment. For example, a value of 600 allows ten minutes:

```bash
curl --fail-with-body -X POST https://api.cursor.com/v0/private-workers/pools \
  -u "$CURSOR_API_KEY:" \
  -H 'Content-Type: application/json' \
  -d '{"scope":"team","poolName":"your-pool","workerReadyTimeoutSeconds":600}'
```

When a worker resumes, the controller mounts its retained PVC into a new Pod.
If the workspace is missing or terminating, it releases the request so another
worker can claim it.

### Append-only seed rotation

To resolve a seed when a new workspace is created, configure:

```yaml
persistence:
  enabled: true
  snapshotSelector:
    seed: cursor-workspace
```

Remove any `claimSpec.dataSource` or `dataSourceRef`: these are mutually exclusive
with selection. The CLI equivalent is `--workspace-snapshot-selector=seed=cursor-workspace`.
The controller lists matching `snapshot.storage.k8s.io/v1` VolumeSnapshots in its
worker namespace and chooses the newest creation timestamp with `readyToUse: true`
and no deletion timestamp. Equal timestamps are ordered by name, descending.
Missing matches or API errors fail the spawn before creating a PVC or Pod.
The chart grants snapshot list permission only when selection is enabled.

Publish uniquely named generations, wait for readiness, then prune older seeds.
Retained PVCs and wake operations keep their original generation, recorded on
both PVC and Pod in `cursor-controller.dev/seed-snapshot`. Cross-namespace sources
are not supported; mirror snapshots into the worker namespace.

Selection eliminates the delete-and-recreate gap, but is not a transaction with
an external pruner. Keep old snapshots until no pending PVC restore references
them, and leave a grace period for in-flight spawns. Deleting a selected snapshot
before its PVC is bound can still strand a restore.
