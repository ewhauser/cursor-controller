package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ewhauser/cursor-controller/internal/backend"
	"github.com/ewhauser/cursor-controller/internal/cursorapi"
)

const podYAML = `
apiVersion: v1
kind: Pod
metadata:
  labels:
    team: platform
spec:
  restartPolicy: Always
  containers:
    - name: worker
      image: example/worker:1
      args: ["worker", "--pool", "$(CURSOR_POOL)", "start"]
      env:
        - name: CURSOR_POOL
          value: stale
        - name: KEEP
          value: me
`

const pvcYAML = `
apiVersion: v1
kind: PersistentVolumeClaim
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: gp3
  resources:
    requests:
      storage: 100Gi
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: monorepo-seed
`

func newBackend(t *testing.T, persistent bool) (*Backend, *fake.Clientset) {
	t.Helper()
	pod, err := ParsePodTemplate([]byte(podYAML))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Namespace: "cursord", WorkerIDPrefix: "cc", PodTemplate: pod, APIKeySecretName: "cursor-key"}
	if persistent {
		pvc, err := ParsePVCTemplate([]byte(pvcYAML))
		if err != nil {
			t.Fatal(err)
		}
		opts.PVCTemplate = pvc
	}
	cs := fake.NewSimpleClientset()
	opts.Client = cs
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return b, cs
}

func envMap(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestSpawnCreatesPVCAndPod(t *testing.T) {
	b, cs := newBackend(t, true)
	ctx := context.Background()
	req := &cursorapi.PendingRequest{ID: "bc-1", UserID: 7, RepoURL: "https://github.com/acme/mono", RepoOwner: "acme", RepoName: "mono"}
	spec := backend.Spec{WorkerID: "cc-0123456789ab", Pool: "gpu", Kind: backend.KindClaim, Request: req}
	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("cursord").Get(ctx, "cc-0123456789ab-ws", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Labels[LabelWorkerID] != "cc-0123456789ab" || pvc.Annotations[AnnRequestID] != "bc-1" || pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Name != "monorepo-seed" {
		t.Fatalf("pvc = %+v", pvc)
	}
	pods, _ := cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("pods = %d", len(pods.Items))
	}
	p := pods.Items[0]
	if !strings.HasPrefix(p.Name, "cc-0123456789ab-") || p.Spec.RestartPolicy != corev1.RestartPolicyNever || p.Labels["team"] != "platform" || p.Labels[LabelWorkerID] != "cc-0123456789ab" {
		t.Fatalf("pod meta = %+v", p.ObjectMeta)
	}
	env := envMap(p.Spec.Containers[0])
	if env["CURSOR_POOL"].Value != "gpu" || env["CURSOR_AGENT_WORKER_ID"].Value != "cc-0123456789ab" || env["CURSOR_REQUEST_ID"].Value != "bc-1" ||
		env["CURSOR_USER_ID"].Value != "7" || env["CURSOR_REPO_URL"].Value != "https://github.com/acme/mono" || env["KEEP"].Value != "me" ||
		env["CURSOR_WORKSPACE_PATH"].Value != "/workspace" {
		t.Fatalf("env = %+v", env)
	}
	if _, wake := env["CURSOR_WAKE"]; wake {
		t.Fatal("CURSOR_WAKE must not be set on a fresh claim")
	}
	if env["CURSOR_API_KEY"].ValueFrom == nil || env["CURSOR_API_KEY"].ValueFrom.SecretKeyRef.Name != "cursor-key" {
		t.Fatalf("api key env = %+v", env["CURSOR_API_KEY"])
	}
	if len(p.Spec.Volumes) != 1 || p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "cc-0123456789ab-ws" {
		t.Fatalf("volumes = %+v", p.Spec.Volumes)
	}
	if m := p.Spec.Containers[0].VolumeMounts; len(m) != 1 || m[0].MountPath != "/workspace" {
		t.Fatalf("mounts = %+v", m)
	}
}

func TestWakeMissingPVC(t *testing.T) {
	b, _ := newBackend(t, true)
	err := b.Wake(context.Background(), backend.Spec{WorkerID: "cc-deadbeefdead", Pool: "gpu"})
	if !errors.Is(err, backend.ErrWorkspaceMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestWakeReplacesTerminalPod(t *testing.T) {
	b, cs := newBackend(t, true)
	ctx := context.Background()
	spec := backend.Spec{WorkerID: "cc-0123456789ab", Pool: "gpu", Request: &cursorapi.PendingRequest{ID: "bc-1"}}
	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pods, _ := cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	first := pods.Items[0]
	first.Status.Phase = corev1.PodSucceeded
	if _, err := cs.CoreV1().Pods("cursord").UpdateStatus(ctx, &first, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wake := backend.Spec{WorkerID: "cc-0123456789ab", Pool: "gpu", Request: &cursorapi.PendingRequest{ID: "bc-1", ClaimedWorkerID: "cc-0123456789ab"}, WakeTimeout: 90 * time.Second}
	if err := b.Wake(ctx, wake); err != nil {
		t.Fatal(err)
	}
	pods, _ = cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 || pods.Items[0].Name == first.Name {
		t.Fatalf("expected one fresh pod, got %+v", pods.Items)
	}
	env := envMap(pods.Items[0].Spec.Containers[0])
	if env["CURSOR_WAKE"].Value != "1" || env["CURSOR_WAKE_TIMEOUT_MS"].Value != "90000" || env["CURSOR_AGENT_WORKER_ID"].Value != "cc-0123456789ab" {
		t.Fatalf("wake env = %+v", env)
	}
	// Waking again while the new pod is live is a no-op.
	if err := b.Wake(ctx, wake); err != nil {
		t.Fatal(err)
	}
	pods, _ = cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("second wake should not create a pod, got %d", len(pods.Items))
	}
}

func TestWakeEphemeralUnknownWorker(t *testing.T) {
	b, _ := newBackend(t, false)
	err := b.Wake(context.Background(), backend.Spec{WorkerID: "cc-unknown", Pool: "gpu"})
	if !errors.Is(err, backend.ErrUnknownWorker) {
		t.Fatalf("err = %v", err)
	}
}

func TestListWorkersAndDispose(t *testing.T) {
	b, cs := newBackend(t, true)
	ctx := context.Background()
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-aaaaaaaaaaaa", Pool: "gpu", Request: &cursorapi.PendingRequest{ID: "bc-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-bbbbbbbbbbbb", Pool: "gpu", Kind: backend.KindWarm}); err != nil {
		t.Fatal(err)
	}
	// Mark worker b's pod as finished 2h ago.
	pods, _ := cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{LabelSelector: LabelWorkerID + "=cc-bbbbbbbbbbbb"})
	p := pods.Items[0]
	p.Status.Phase = corev1.PodSucceeded
	finished := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: finished}}}}
	if _, err := cs.CoreV1().Pods("cursord").UpdateStatus(ctx, &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordClaim(ctx, "cc-bbbbbbbbbbbb", "bc-2"); err != nil {
		t.Fatal(err)
	}
	workers, err := b.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 {
		t.Fatalf("workers = %+v", workers)
	}
	a, bw := workers[0], workers[1]
	if a.ID != "cc-aaaaaaaaaaaa" || a.RequestID != "bc-1" || a.Live == nil || !*a.Live {
		t.Fatalf("a = %+v", a)
	}
	if bw.ID != "cc-bbbbbbbbbbbb" || bw.RequestID != "bc-2" || bw.Live == nil || *bw.Live {
		t.Fatalf("b = %+v", bw)
	}
	if err := b.Dispose(ctx, "cc-bbbbbbbbbbbb", "ttl"); err != nil {
		t.Fatal(err)
	}
	pvcs, _ := cs.CoreV1().PersistentVolumeClaims("cursord").List(ctx, metav1.ListOptions{})
	pods, _ = cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
	if len(pvcs.Items) != 1 || pvcs.Items[0].Name != "cc-aaaaaaaaaaaa-ws" || len(pods.Items) != 1 {
		t.Fatalf("after dispose: pvcs=%d pods=%d", len(pvcs.Items), len(pods.Items))
	}
	// Disposing twice is fine.
	if err := b.Dispose(ctx, "cc-bbbbbbbbbbbb", "ttl"); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnRefusesForeignPVC(t *testing.T) {
	b, cs := newBackend(t, true)
	ctx := context.Background()
	_, err := cs.CoreV1().PersistentVolumeClaims("cursord").Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "cc-0123456789ab-ws", Namespace: "cursord", Labels: map[string]string{LabelWorkerID: "someone-else"}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-0123456789ab", Pool: "gpu"}); err == nil {
		t.Fatal("expected error for foreign pvc")
	}
}

func TestListWorkersMarksStalledPodForStartupCleanup(t *testing.T) {
	b, cs := newBackend(t, true)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	b.o.Now = func() time.Time { return now }
	b.o.StartupTimeout = 10 * time.Minute
	ctx := context.Background()
	if err := b.Spawn(ctx, backend.Spec{
		WorkerID: "cc-stalled000000",
		Pool:     "gpu",
		Request:  &cursorapi.PendingRequest{ID: "bc-stalled"},
	}); err != nil {
		t.Fatal(err)
	}
	pods, err := cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{LabelSelector: LabelWorkerID + "=cc-stalled000000"})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods: %v %+v", err, pods.Items)
	}
	p := pods.Items[0]
	p.CreationTimestamp = metav1.NewTime(now.Add(-11 * time.Minute))
	p.Status.Phase = corev1.PodPending
	if _, err := cs.CoreV1().Pods("cursord").Update(ctx, &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	workers, err := b.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || !workers[0].StartupTimedOut || workers[0].Live == nil || *workers[0].Live {
		t.Fatalf("stalled worker must be non-live and marked timed out: %+v", workers)
	}
}

func TestPodStartupDeadlineAppliesUntilReady(t *testing.T) {
	b, _ := newBackend(t, false)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	b.o.Now = func() time.Time { return now }
	b.o.StartupTimeout = 10 * time.Minute
	old := metav1.NewTime(now.Add(-11 * time.Minute))
	recent := metav1.NewTime(now.Add(-time.Minute))

	tests := []struct {
		name     string
		pod      corev1.Pod
		live     bool
		timedOut bool
	}{
		{name: "old pending", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: old}, Status: corev1.PodStatus{Phase: corev1.PodPending}}, timedOut: true},
		{name: "old running but unready", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: old}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, timedOut: true},
		{name: "recent pending", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: recent}, Status: corev1.PodStatus{Phase: corev1.PodPending}}, live: true},
		{name: "ready", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: old}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}, live: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live, timedOut := b.podState(&tt.pod)
			if live != tt.live || timedOut != tt.timedOut {
				t.Fatalf("state=(live=%v timedOut=%v), want (%v %v)", live, timedOut, tt.live, tt.timedOut)
			}
		})
	}
}
