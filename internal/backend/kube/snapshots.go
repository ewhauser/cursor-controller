package kube

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const AnnSeedSnapshot = "cursor-controller.dev/seed-snapshot"

var snapshotResource = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}

func (b *Backend) selectSnapshot(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	snapshots, err := b.o.SnapshotClient.Resource(snapshotResource).Namespace(b.o.Namespace).List(ctx, metav1.ListOptions{LabelSelector: b.o.SnapshotSelector})
	if err != nil {
		return fmt.Errorf("list seed snapshots: %w", err)
	}
	ready := make([]unstructured.Unstructured, 0, len(snapshots.Items))
	for _, snapshot := range snapshots.Items {
		usable, _, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse")
		if usable && snapshot.GetDeletionTimestamp() == nil {
			ready = append(ready, snapshot)
		}
	}
	if len(ready) == 0 {
		return fmt.Errorf("no ready seed VolumeSnapshot matches %q in namespace %s", b.o.SnapshotSelector, b.o.Namespace)
	}
	sort.Slice(ready, func(i, j int) bool {
		a, c := ready[i].GetCreationTimestamp(), ready[j].GetCreationTimestamp()
		if a.Equal(&c) {
			return ready[i].GetName() > ready[j].GetName()
		}
		return a.After(c.Time)
	})
	group := snapshotResource.Group
	pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VolumeSnapshot", Name: ready[0].GetName()}
	pvc.Annotations[AnnSeedSnapshot] = ready[0].GetName()
	return nil
}
