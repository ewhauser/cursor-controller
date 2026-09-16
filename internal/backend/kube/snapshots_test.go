package kube

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ewhauser/cursor-controller/internal/backend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func seed(name, ns string, age time.Duration, ready bool) *unstructured.Unstructured {
	s := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"seed": "workspace"}},
		"status":   map[string]any{"readyToUse": ready},
	}}
	s.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(age)))
	return s
}

func TestSnapshotSelectionAndRetainedGeneration(t *testing.T) {
	b, cs := newBackend(t, true)
	b.o.PVCTemplate.Spec.DataSource = nil
	deleted := seed("deleting", "cursord", 5*time.Hour, true)
	ts := metav1.Now()
	deleted.SetDeletionTimestamp(&ts)
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{snapshotResource: "VolumeSnapshotList"},
		seed("old", "cursord", 0, true), seed("new", "cursord", time.Hour, true), seed("pending", "cursord", 2*time.Hour, false), seed("foreign", "other", 3*time.Hour, true), deleted)
	b.o.SnapshotClient = dc
	b.o.SnapshotSelector = "seed=workspace"
	ctx := context.Background()
	spec := backend.Spec{WorkerID: "cc-seed"}
	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("cursord").Get(ctx, PVCName(spec.WorkerID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource.Name != "new" || pvc.Annotations[AnnSeedSnapshot] != "new" {
		t.Fatalf("wrong seed: %+v", pvc)
	}
	pods, _ := cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 || pods.Items[0].Annotations[AnnSeedSnapshot] != "new" {
		t.Fatalf("pod seed: %+v", pods)
	}
	if err := cs.CoreV1().Pods("cursord").Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Resource(snapshotResource).Namespace("cursord").Create(ctx, seed("next", "cursord", 4*time.Hour, true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Wake(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pods, _ = cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 || pods.Items[0].Annotations[AnnSeedSnapshot] != "new" {
		t.Fatalf("wake changed seed: %+v", pods)
	}
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-next"}); err != nil {
		t.Fatal(err)
	}
	pvc, err = cs.CoreV1().PersistentVolumeClaims("cursord").Get(ctx, PVCName("cc-next"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource.Name != "next" {
		t.Fatal("new worker did not rotate seed")
	}
	// Retrying an existing workspace must not need any snapshot API calls.
	b.o.SnapshotSelector = "seed=missing"
	if err := b.ensurePVC(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-missing"}); err == nil {
		t.Fatal("expected no matching ready snapshot error")
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("cursord").Get(ctx, PVCName("cc-missing"), metav1.GetOptions{}); err == nil {
		t.Fatal("created an unseeded PVC")
	}
}

func TestSnapshotConfigurationRejectsConflicts(t *testing.T) {
	b, _ := newBackend(t, true)
	b.o.SnapshotSelector = "seed=workspace"
	b.o.SnapshotClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := New(b.o); err == nil {
		t.Fatal("accepted fixed dataSource and selector")
	}
	b.o.PVCTemplate.Spec.DataSource = nil
	b.o.SnapshotSelector = "invalid==="
	if _, err := New(b.o); err == nil {
		t.Fatal("accepted invalid selector")
	}
	b.o.SnapshotSelector = "seed=workspace"
	b.o.PVCTemplate = nil
	if _, err := New(b.o); err == nil {
		t.Fatal("accepted selector without persistence")
	}
}

func TestSnapshotErrorsAndTieBreak(t *testing.T) {
	b, _ := newBackend(t, true)
	b.o.PVCTemplate.Spec.DataSource = nil
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{snapshotResource: "VolumeSnapshotList"}, seed("a", "cursord", 0, true), seed("z", "cursord", 0, true))
	b.o.SnapshotClient = dc
	b.o.SnapshotSelector = "seed=workspace"
	pvc := b.o.PVCTemplate.DeepCopy()
	pvc.Annotations = map[string]string{}
	if err := b.selectSnapshot(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource.Name != "z" {
		t.Fatal("tie break not deterministic")
	}
	dc.PrependReactor("list", "volumesnapshots", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, fmt.Errorf("forbidden") })
	if err := b.Spawn(context.Background(), backend.Spec{WorkerID: "cc-forbidden"}); err == nil {
		t.Fatal("ignored snapshot API failure")
	}
}
