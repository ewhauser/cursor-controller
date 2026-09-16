//go:build e2e

// End-to-end scenarios against a kind cluster prepared by hack/e2e/setup.sh.
//
//	go test -tags e2e ./test/e2e -v -count=1
//
// Environment: E2E_FAKE_API (default http://localhost:30080), E2E_NAMESPACE
// (default cursord), KUBECONFIG (default ~/.kube/config), E2E_POOL (e2e).
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ewhauser/cursor-controller/internal/backend/kube"
	"github.com/ewhauser/cursor-controller/internal/fakeapi"
)

type harness struct {
	t     *testing.T
	ctx   context.Context
	ns    string
	pool  string
	kube  kubernetes.Interface
	admin *fakeapi.Client
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	kubeconfig := envOr("KUBECONFIG", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, ns: envOr("E2E_NAMESPACE", "cursord"), pool: envOr("E2E_POOL", "e2e"),
		kube: cs, admin: fakeapi.NewClient(envOr("E2E_FAKE_API", "http://localhost:30080"))}
	if err := h.admin.RegisterPool(ctx, fakeapi.Pool{PoolName: h.pool, WorkerReadyTimeoutSeconds: 300}); err != nil {
		t.Fatalf("fake api unreachable: %v", err)
	}
	return h
}

func (h *harness) waitFor(what string, timeout time.Duration, cond func() (bool, string)) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			h.t.Logf("ok: %s %s", what, detail)
			return
		}
		last = detail
		time.Sleep(2 * time.Second)
	}
	h.dump()
	h.t.Fatalf("timed out waiting for %s (last: %s)", what, last)
}

func (h *harness) dump() {
	pods, _ := h.kube.CoreV1().Pods(h.ns).List(h.ctx, metav1.ListOptions{LabelSelector: kube.LabelManagedBy + "=" + kube.ManagedBy})
	for _, p := range pods.Items {
		h.t.Logf("pod %s phase=%s worker=%s req=%s", p.Name, p.Status.Phase, p.Labels[kube.LabelWorkerID], p.Annotations[kube.AnnRequestID])
	}
	pvcs, _ := h.kube.CoreV1().PersistentVolumeClaims(h.ns).List(h.ctx, metav1.ListOptions{LabelSelector: kube.LabelManagedBy + "=" + kube.ManagedBy})
	for _, p := range pvcs.Items {
		h.t.Logf("pvc %s phase=%s worker=%s", p.Name, p.Status.Phase, p.Labels[kube.LabelWorkerID])
	}
	if st, err := h.admin.State(h.ctx); err == nil {
		h.t.Logf("fake: claims=%v releases=%v connects=%+v", st.Claims, st.Releases, st.Connects)
		for _, r := range st.Requests {
			h.t.Logf("fake request %s status=%s worker=%s awaitingWake=%v", r.ID, r.Status, r.ClaimedWorkerID, r.AwaitingWake)
		}
	}
	if pods, err := h.kube.CoreV1().Pods(h.ns).List(h.ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=controller"}); err == nil {
		for _, p := range pods.Items {
			raw, err := h.kube.CoreV1().Pods(h.ns).GetLogs(p.Name, &corev1.PodLogOptions{TailLines: ptr(int64(60))}).DoRaw(h.ctx)
			if err == nil {
				h.t.Logf("controller logs (%s):\n%s", p.Name, raw)
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

func (h *harness) pods(workerID string) []corev1.Pod {
	sel := kube.LabelManagedBy + "=" + kube.ManagedBy
	if workerID != "" {
		sel += "," + kube.LabelWorkerID + "=" + workerID
	}
	list, err := h.kube.CoreV1().Pods(h.ns).List(h.ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		h.t.Fatal(err)
	}
	return list.Items
}

func (h *harness) pvc(workerID string) *corev1.PersistentVolumeClaim {
	p, err := h.kube.CoreV1().PersistentVolumeClaims(h.ns).Get(h.ctx, kube.PVCName(workerID), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return p
}

// workerFor waits until the fake shows the request claimed and returns the worker id.
func (h *harness) workerFor(reqID string) string {
	var worker string
	h.waitFor("claim of "+reqID, 60*time.Second, func() (bool, string) {
		st, err := h.admin.State(h.ctx)
		if err != nil {
			return false, err.Error()
		}
		for _, r := range st.Requests {
			if r.ID == reqID && r.Status == "claimed" {
				worker = r.ClaimedWorkerID
				return true, "worker " + worker
			}
		}
		return false, "not claimed"
	})
	return worker
}

func (h *harness) connectsFor(worker string) []fakeapi.ConnectRecord {
	st, err := h.admin.State(h.ctx)
	if err != nil {
		return nil
	}
	var out []fakeapi.ConnectRecord
	for _, c := range st.Connects {
		if c.WorkerID == worker {
			out = append(out, c)
		}
	}
	return out
}

func (h *harness) waitConnects(worker string, n int, timeout time.Duration) []fakeapi.ConnectRecord {
	var out []fakeapi.ConnectRecord
	h.waitFor(fmt.Sprintf("%d connect(s) from %s", n, worker), timeout, func() (bool, string) {
		out = h.connectsFor(worker)
		return len(out) >= n, fmt.Sprintf("%d connects", len(out))
	})
	return out
}

func (h *harness) waitPodsTerminal(worker string, timeout time.Duration) {
	h.waitFor("pods of "+worker+" terminal", timeout, func() (bool, string) {
		for _, p := range h.pods(worker) {
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				return false, p.Name + " " + string(p.Status.Phase)
			}
		}
		return true, ""
	})
}

func TestClaimSpawnHibernateWakeDispose(t *testing.T) {
	h := newHarness(t)
	reqID, err := h.admin.CreateRequest(h.ctx, fakeapi.CreateRequestOptions{Pool: h.pool, RepoURL: "https://github.com/acme/mono", UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("injected request %s", reqID)

	// Claim -> PVC from snapshot + Pod -> worker connects with seed present.
	worker := h.workerFor(reqID)
	h.waitFor("pvc bound", 120*time.Second, func() (bool, string) {
		p := h.pvc(worker)
		if p == nil {
			return false, "no pvc"
		}
		return p.Status.Phase == corev1.ClaimBound, string(p.Status.Phase)
	})
	if pvc := h.pvc(worker); pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Name != "monorepo-latest" || pvc.Annotations[kube.AnnSeedSnapshot] != "monorepo-latest" {
		t.Fatalf("workspace did not use selected seed: %+v", pvc)
	}
	first := h.waitConnects(worker, 1, 120*time.Second)[0]
	for _, pod := range h.pods(worker) {
		if pod.Annotations[kube.AnnSeedSnapshot] != "monorepo-latest" {
			t.Fatalf("pod missing seed generation: %+v", pod.Annotations)
		}
	}
	if first.Wake || first.MarkerFound || first.RequestID != reqID {
		t.Fatalf("first connect = %+v", first)
	}
	if n := len(h.pods(worker)); n != 1 {
		t.Fatalf("expected 1 pod, got %d", n)
	}

	// Idle timeout -> pod Succeeded, PVC retained.
	h.waitPodsTerminal(worker, 90*time.Second)
	if h.pvc(worker) == nil {
		t.Fatal("pvc disposed while agent still live")
	}

	// Follow-up -> claimed_offline -> new pod on the same PVC, marker survives.
	if err := h.admin.Followup(h.ctx, reqID); err != nil {
		t.Fatal(err)
	}
	second := h.waitConnects(worker, 2, 120*time.Second)[1]
	if !second.Wake || !second.MarkerFound {
		t.Fatalf("wake connect = %+v (want wake=true markerFound=true)", second)
	}
	h.waitFor("old pod removed", 60*time.Second, func() (bool, string) {
		pods := h.pods(worker)
		return len(pods) == 1, fmt.Sprintf("%d pods", len(pods))
	})
	h.waitPodsTerminal(worker, 90*time.Second)

	// Archive -> GC deletes PVC and pods.
	if err := h.admin.Archive(h.ctx, reqID); err != nil {
		t.Fatal(err)
	}
	h.waitFor("workspace disposed", 120*time.Second, func() (bool, string) {
		return h.pvc(worker) == nil && len(h.pods(worker)) == 0, "still present"
	})
}

func TestMissingWorkspaceReleasesClaim(t *testing.T) {
	h := newHarness(t)
	reqID, err := h.admin.CreateRequest(h.ctx, fakeapi.CreateRequestOptions{Pool: h.pool})
	if err != nil {
		t.Fatal(err)
	}
	worker := h.workerFor(reqID)
	h.waitConnects(worker, 1, 120*time.Second)
	h.waitPodsTerminal(worker, 90*time.Second)

	// Simulate an operator cleaning up by hand: the finished pod first (the
	// pvc-protection finalizer blocks PVC deletion while any scheduled pod,
	// even a Succeeded one, still references the claim), then the PVC.
	for _, p := range h.pods(worker) {
		if err := h.kube.CoreV1().Pods(h.ns).Delete(h.ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.kube.CoreV1().PersistentVolumeClaims(h.ns).Delete(h.ctx, kube.PVCName(worker), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.waitFor("pvc gone", 60*time.Second, func() (bool, string) { return h.pvc(worker) == nil, "pvc exists" })
	if err := h.admin.Followup(h.ctx, reqID); err != nil {
		t.Fatal(err)
	}
	// Controller releases; fake re-queues; controller claims again with a new worker.
	var newWorker string
	h.waitFor("release and re-claim", 90*time.Second, func() (bool, string) {
		st, err := h.admin.State(h.ctx)
		if err != nil {
			return false, err.Error()
		}
		released := false
		for _, r := range st.Releases {
			if r == reqID {
				released = true
			}
		}
		for _, r := range st.Requests {
			if r.ID == reqID && r.Status == "claimed" && r.ClaimedWorkerID != worker {
				newWorker = r.ClaimedWorkerID
				return released, "new worker " + newWorker
			}
		}
		return false, fmt.Sprintf("released=%v", released)
	})
	c := h.waitConnects(newWorker, 1, 120*time.Second)[0]
	if c.Wake || c.MarkerFound {
		t.Fatalf("fresh worker connect = %+v", c)
	}
	if err := h.admin.Archive(h.ctx, reqID); err != nil {
		t.Fatal(err)
	}
	h.waitFor("cleanup", 120*time.Second, func() (bool, string) {
		return h.pvc(newWorker) == nil && len(h.pods(newWorker)) == 0 && len(h.pods(worker)) == 0, "still present"
	})
}

func TestControllerRestartDoesNotDuplicate(t *testing.T) {
	h := newHarness(t)
	reqID, err := h.admin.CreateRequest(h.ctx, fakeapi.CreateRequestOptions{Pool: h.pool})
	if err != nil {
		t.Fatal(err)
	}
	worker := h.workerFor(reqID)
	// Kill the controller while the worker is coming up.
	pods, err := h.kube.CoreV1().Pods(h.ns).List(h.ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=controller"})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("controller pod: %v", err)
	}
	if err := h.kube.CoreV1().Pods(h.ns).Delete(h.ctx, pods.Items[0].Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.waitConnects(worker, 1, 180*time.Second)
	time.Sleep(30 * time.Second) // give a restarted controller time to (wrongly) act
	st, _ := h.admin.State(h.ctx)
	claims := 0
	for _, c := range st.Claims {
		if strings.HasPrefix(c, reqID+"@") {
			claims++
		}
	}
	if claims != 1 || len(h.pods(worker)) != 1 {
		t.Fatalf("after restart: claims=%d pods=%d", claims, len(h.pods(worker)))
	}
	if err := h.admin.Archive(h.ctx, reqID); err != nil {
		t.Fatal(err)
	}
	h.waitFor("cleanup", 120*time.Second, func() (bool, string) {
		return h.pvc(worker) == nil && len(h.pods(worker)) == 0, "still present"
	})
}
