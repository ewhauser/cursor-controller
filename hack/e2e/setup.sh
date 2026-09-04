#!/usr/bin/env bash
# Create a kind cluster with CSI snapshot support, seed a snapshot, build and
# load images, deploy the fake Cursor API, and install the controller chart.
# Requires: docker, kind, kubectl, helm, git.
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER=${CLUSTER:-cursor-e2e}
NS=cursord
SNAPSHOTTER_VERSION=${SNAPSHOTTER_VERSION:-v8.2.0}
HOSTPATH_VERSION=${HOSTPATH_VERSION:-v1.15.0}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-kindest/node:v1.31.4}

log() { printf '\n==> %s\n' "$*" >&2; }

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster $CLUSTER"
  kind create cluster --config hack/e2e/kind-config.yaml --image "$KIND_NODE_IMAGE" --wait 120s
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

log "installing VolumeSnapshot CRDs and snapshot-controller ($SNAPSHOTTER_VERSION)"
kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=$SNAPSHOTTER_VERSION"
kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=$SNAPSHOTTER_VERSION"

if kubectl -n default get statefulset csi-hostpathplugin >/dev/null 2>&1; then
  log "csi-driver-host-path already installed; skipping"
else
log "installing csi-driver-host-path ($HOSTPATH_VERSION)"
tmp=$(mktemp -d)
git clone -q --depth 1 --branch "$HOSTPATH_VERSION" https://github.com/kubernetes-csi/csi-driver-host-path.git "$tmp/hostpath"
# deploy.sh picks manifests for the cluster's Kubernetes version and installs
# the driver plus the csi-hostpath-sc StorageClass and snapshot class.
(cd "$tmp/hostpath" && ./deploy/kubernetes-latest/deploy.sh)
kubectl apply -f "$tmp/hostpath/examples/csi-storageclass.yaml"
kubectl apply -f "$tmp/hostpath/examples/csi-snapshotclass.yaml" 2>/dev/null || kubectl apply -f - <<'YAML'
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: csi-hostpath-snapclass
driver: hostpath.csi.k8s.io
deletionPolicy: Delete
YAML
kubectl -n default rollout status statefulset/csi-hostpathplugin --timeout=180s
fi
kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=180s

log "building images"
export DOCKER_BUILDKIT=1
docker build -t cursor-controller:dev .
docker build -t cursor-controller-e2e:dev -f hack/e2e/Dockerfile .
kind load docker-image --name "$CLUSTER" cursor-controller:dev cursor-controller-e2e:dev

log "seeding workspace snapshot"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f hack/e2e/seed.yaml
kubectl -n "$NS" wait --for=condition=complete job/workspace-seed-writer --timeout=180s
for i in $(seq 1 60); do
  ready=$(kubectl -n "$NS" get volumesnapshot monorepo-latest -o jsonpath='{.status.readyToUse}' 2>/dev/null || true)
  [ "$ready" = "true" ] && break
  sleep 2
done
[ "$ready" = "true" ] || { echo "snapshot never became ready" >&2; kubectl -n "$NS" describe volumesnapshot monorepo-latest; exit 1; }

log "deploying fake Cursor API"
kubectl apply -f hack/e2e/fake-api.yaml
kubectl -n "$NS" rollout status deploy/fake-cursor-api --timeout=120s

log "installing controller chart"
helm upgrade --install cursor-controller deploy/helm/cursor-controller -n "$NS" -f hack/e2e/values.yaml
kubectl -n "$NS" rollout status deploy/cursor-controller --timeout=120s

log "ready. run: make e2e   (fake API at http://localhost:30080)"
