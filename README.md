# cursor-controller

A Kubernetes controller for Cursor self-hosted workers with pre-populated workspace volumes.

cursor-controller gives each worker a PersistentVolumeClaim (PVC) initialized
from a prepared volume snapshot. The volume can already contain cloned source
code, so workers begin with a populated workspace instead of cloning the entire
repository at startup.

## Why this controller?

Cursor's built-in [`agent worker controller`](https://cursor.com/docs/cloud-agent/self-hosted/pool#worker-controller)
handles request claiming and warm worker capacity through a user-provided
`--spawn` script. That script can create Kubernetes Pods, containers, or processes.

This project exists to make pre-populated PVCs part of that provisioning process.
For a large repository, repeatedly cloning source code adds work before an agent
can begin. Prepare the source tree once, snapshot its volume, and configure the
controller to create each worker's PVC from that snapshot. Each worker receives
its own writable workspace containing the prepared source tree.

The Kubernetes backend manages Pod creation and snapshot-based PVC initialization
through Helm values and Pod/PVC templates. Your snapshot pipeline prepares and
refreshes the source data; the controller provisions worker volumes from it.
Workers may still need to fetch changes or check out the requested revision.

Workspace retention and cleanup complete the lifecycle: follow-up requests reuse
the same volume, and retention policies remove workspaces when they are no longer
needed. See [persistent workspaces](docs/workspaces.md) for setup.

Workers still run Cursor's `agent` CLI, and Cursor continues to provide agent
orchestration and model access. This project manages the worker infrastructure.

## Features

- Pre-populated workspace PVCs initialized from a prepared source-code snapshot.
- Worker and volume provisioning through the Kubernetes API.
- Worker resume and recovery when a workspace is unavailable.
- Configurable warm worker capacity and workspace retention.
- Helm deployment, Prometheus metrics, and optional OpenTelemetry export.
- A script-based backend for running workers outside Kubernetes.

## Project status

The project includes unit tests, component tests, and end-to-end tests using a
simulated Cursor API and a local Kubernetes cluster. Real Cursor API fixtures
are not included in the repository; validation against a dedicated Cursor pool
is still required before relying on the integration in production.

Run one controller per pool set. Warm capacity and workspace cleanup assume a
single controller. See [operations](docs/operations.md) for deployment constraints
and retention behavior.

## Requirements

- A Cursor team with self-hosted pools enabled and a team service-account API
  key with agent scope.
- A Kubernetes cluster, kubectl, and Helm 3.
- A worker image containing the Cursor `agent` CLI and Git.
- A container registry accessible to the cluster, and Docker to build the
  controller image.
- For retained workspaces, a suitable StorageClass. Snapshot initialization also
  requires a CSI driver with snapshot support and an existing VolumeSnapshot.

## Quick start

Run these commands in Bash. These instructions install the chart from a local
checkout. Replace the registry,
worker image, and pool values below with values for your environment.

Clone the repository and publish a controller image:

```bash
git clone https://github.com/ewhauser/cursor-controller.git
cd cursor-controller

export CONTROLLER_IMAGE=registry.example.com/your-team/cursor-controller
export CONTROLLER_VERSION=$(git rev-parse --short HEAD)
make image IMAGE="$CONTROLLER_IMAGE" VERSION="$CONTROLLER_VERSION"
docker push "$CONTROLLER_IMAGE:$CONTROLLER_VERSION"
```

Build for your cluster's architecture. Configure registry credentials if the
cluster needs them to pull either image.

Create the namespace and a Secret containing your Cursor API key:

```bash
kubectl create namespace cursord
read -r -s -p "Cursor API key: " CURSOR_API_KEY; printf '\n'
kubectl -n cursord create secret generic cursor-api-key \
  --from-literal=api-key="$CURSOR_API_KEY"
unset CURSOR_API_KEY
```

Install the controller:

```bash
helm upgrade --install cursor-controller deploy/helm/cursor-controller \
  --namespace cursord \
  --set image.repository="$CONTROLLER_IMAGE" \
  --set image.tag="$CONTROLLER_VERSION" \
  --set auth.existingSecret=cursor-api-key \
  --set 'pools={your-pool}' \
  --set worker.image.repository=registry.example.com/your-team/cursor-worker \
  --set worker.image.tag=your-worker-version \
  --wait
```

This minimal installation uses ephemeral workspaces. Follow the
[persistent workspace guide](docs/workspaces.md) to retain data between worker
sessions or initialize workspaces from snapshots.

Check the deployment:

```bash
kubectl -n cursord rollout status deployment/cursor-controller
kubectl -n cursord logs deployment/cursor-controller
```

## Documentation

| Guide | Contents |
| --- | --- |
| [Configuration](docs/configuration.md) | Helm values, worker templates, and running the binary |
| [Persistent workspaces](docs/workspaces.md) | Storage, snapshots, and resume configuration |
| [Architecture](docs/architecture.md) | Request handling and worker lifecycle |
| [Operations](docs/operations.md) | Ownership, retention, failure recovery, and health endpoints |
| [Observability](docs/observability.md) | Prometheus and OpenTelemetry |
| [Hook backend](docs/hooks.md) | Worker scripts and their environment contract |
| [Testing](docs/testing.md) | Local tests, Kubernetes tests, and real API fixture capture |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and pull request
guidelines. Run `make check` to build the project, run race tests, lint Go code,
and validate the Helm chart.

## License

[Apache License 2.0](LICENSE). Cursor is a trademark of Anysphere, Inc.
This project is not affiliated with or endorsed by Anysphere.
