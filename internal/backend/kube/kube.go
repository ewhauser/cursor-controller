// Package kube is the Kubernetes backend: one Pod per worker incarnation and,
// optionally, one retained PersistentVolumeClaim per worker that survives
// hibernation and is re-mounted on wake.
package kube

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ewhauser/cursor-controller/internal/backend"
)

// Labels and annotations stamped on managed resources.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	ManagedBy      = "cursor-controller"

	LabelWorkerID = "cursor-controller.dev/worker-id"
	LabelPool     = "cursor-controller.dev/pool"

	AnnRequestID     = "cursor-controller.dev/request-id"
	AnnLastClaimedAt = "cursor-controller.dev/last-claimed-at"
	AnnSpawnKind     = "cursor-controller.dev/spawn-kind"
	AnnDisposeReason = "cursor-controller.dev/dispose-reason"

	ComponentWorker    = "worker"
	ComponentWorkspace = "workspace"

	// WorkspaceVolume is the pod volume name the retained PVC is mounted as.
	WorkspaceVolume = "workspace"
)

// Options configures the backend.
type Options struct {
	Client         kubernetes.Interface
	Namespace      string
	WorkerIDPrefix string
	PodTemplate    *corev1.Pod
	// PVCTemplate enables persistent, hibernation-safe workspaces. Nil means
	// workers are ephemeral pods only.
	PVCTemplate *corev1.PersistentVolumeClaim
	// MountPath is where the PVC is mounted in the worker container.
	MountPath string
	// WorkerContainer names the container that receives env/volume mounts.
	// Empty selects the first container.
	WorkerContainer string
	// APIKeySecretName/Key inject CURSOR_API_KEY via secretKeyRef when the
	// template does not already set it.
	APIKeySecretName string
	APIKeySecretKey  string
	APIURL           string
	// StartupTimeout is how long a non-ready pod may remain Pending, Running,
	// or Unknown before the controller treats the worker as failed startup.
	StartupTimeout time.Duration
	Log            *slog.Logger
	Now            func() time.Time
}

// Backend implements backend.Backend on Kubernetes.
type Backend struct {
	o Options
}

// New validates options and returns a Backend.
func New(o Options) (*Backend, error) {
	if o.Client == nil {
		return nil, errors.New("kube: client is required")
	}
	if o.Namespace == "" {
		return nil, errors.New("kube: namespace is required")
	}
	if o.PodTemplate == nil || len(o.PodTemplate.Spec.Containers) == 0 {
		return nil, errors.New("kube: pod template with at least one container is required")
	}
	if err := backend.ValidatePrefix(o.WorkerIDPrefix); err != nil {
		return nil, err
	}
	if o.MountPath == "" {
		o.MountPath = "/workspace"
	}
	if o.APIKeySecretKey == "" {
		o.APIKeySecretKey = "api-key"
	}
	if o.StartupTimeout <= 0 {
		o.StartupTimeout = 10 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.WorkerContainer != "" {
		found := false
		for _, c := range o.PodTemplate.Spec.Containers {
			if c.Name == o.WorkerContainer {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("kube: worker container %q not in pod template", o.WorkerContainer)
		}
	}
	return &Backend{o: o}, nil
}

func (b *Backend) Name() string { return "kube" }

// Persistent reports whether workspaces are retained PVCs.
func (b *Backend) Persistent() bool { return b.o.PVCTemplate != nil }

func (b *Backend) Owns(workerID string) bool { return backend.HasPrefix(workerID, b.o.WorkerIDPrefix) }

// PVCName is the retained claim name for a worker.
func PVCName(workerID string) string { return workerID + "-ws" }

func podName(workerID string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var buf [5]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return workerID + "-" + string(buf[:])
}

func (b *Backend) workerSelector(workerID string) string {
	return labels.Set{LabelManagedBy: ManagedBy, LabelWorkerID: workerID}.String()
}

func (b *Backend) managedSelector() string {
	req, _ := labels.NewRequirement(LabelWorkerID, selection.Exists, nil)
	return labels.SelectorFromSet(labels.Set{LabelManagedBy: ManagedBy}).Add(*req).String()
}

// Spawn creates the PVC (if persistent) and a fresh pod.
func (b *Backend) Spawn(ctx context.Context, spec backend.Spec) error {
	if spec.Kind == "" {
		spec.Kind = backend.KindClaim
	}
	if b.Persistent() {
		if err := b.ensurePVC(ctx, spec); err != nil {
			return err
		}
	}
	return b.createPod(ctx, spec)
}

// Wake revives a hibernated worker on its retained PVC.
func (b *Backend) Wake(ctx context.Context, spec backend.Spec) error {
	spec.Kind = backend.KindWake
	pods, err := b.listPods(ctx, spec.WorkerID)
	if err != nil {
		return err
	}
	if b.Persistent() {
		pvc, err := b.o.Client.CoreV1().PersistentVolumeClaims(b.o.Namespace).Get(ctx, PVCName(spec.WorkerID), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pvc %s: %w", PVCName(spec.WorkerID), backend.ErrWorkspaceMissing)
		}
		if err != nil {
			return fmt.Errorf("get pvc: %w", err)
		}
		if pvc.DeletionTimestamp != nil {
			return fmt.Errorf("pvc %s is terminating: %w", pvc.Name, backend.ErrWorkspaceMissing)
		}
		if owner := pvc.Labels[LabelWorkerID]; owner != spec.WorkerID {
			return fmt.Errorf("pvc %s belongs to worker %q, refusing to reuse", pvc.Name, owner)
		}
		if err := b.annotatePVC(ctx, spec.WorkerID, spec.RequestID(), spec.Kind); err != nil {
			return err
		}
	} else if len(pods) == 0 {
		return backend.ErrUnknownWorker
	}
	for _, p := range pods {
		if isLive(&p) {
			b.o.Log.Info("wake: worker pod already live", "worker", spec.WorkerID, "pod", p.Name, "phase", p.Status.Phase)
			if spec.RequestID() != "" && p.Annotations[AnnRequestID] != spec.RequestID() {
				_ = b.annotatePod(ctx, p.Name, spec.RequestID())
			}
			return nil
		}
	}
	if err := b.createPod(ctx, spec); err != nil {
		return err
	}
	// Terminal incarnations are no longer useful; remove them in the background.
	for _, p := range pods {
		if err := b.deletePod(ctx, p.Name); err != nil {
			b.o.Log.Warn("wake: delete terminal pod", "pod", p.Name, "err", err)
		}
	}
	return nil
}

// RecordClaim stamps the request id onto the worker's PVC and live pods.
func (b *Backend) RecordClaim(ctx context.Context, workerID, requestID string) error {
	if b.Persistent() {
		if err := b.annotatePVC(ctx, workerID, requestID, backend.KindWarm); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	pods, err := b.listPods(ctx, workerID)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if isLive(&p) && p.Annotations[AnnRequestID] != requestID {
			if err := b.annotatePod(ctx, p.Name, requestID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ListWorkers merges pods and PVCs into per-worker state.
func (b *Backend) ListWorkers(ctx context.Context) ([]backend.WorkerInfo, error) {
	sel := b.managedSelector()
	byID := map[string]*backend.WorkerInfo{}
	get := func(id string) *backend.WorkerInfo {
		w, ok := byID[id]
		if !ok {
			w = &backend.WorkerInfo{ID: id}
			byID[id] = w
		}
		return w
	}
	if b.Persistent() {
		pvcs, err := b.o.Client.CoreV1().PersistentVolumeClaims(b.o.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, fmt.Errorf("list pvcs: %w", err)
		}
		for _, pvc := range pvcs.Items {
			if pvc.DeletionTimestamp != nil {
				continue
			}
			w := get(pvc.Labels[LabelWorkerID])
			w.Pool = pvc.Labels[LabelPool]
			if r := pvc.Annotations[AnnRequestID]; r != "" {
				w.RequestID = r
			}
			w.CreatedAt = earliest(w.CreatedAt, pvc.CreationTimestamp.Time)
			w.LastActivity = latest(w.LastActivity, pvc.CreationTimestamp.Time, parseTime(pvc.Annotations[AnnLastClaimedAt]))
		}
	}
	pods, err := b.o.Client.CoreV1().Pods(b.o.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		w := get(p.Labels[LabelWorkerID])
		if w.Pool == "" {
			w.Pool = p.Labels[LabelPool]
		}
		if r := p.Annotations[AnnRequestID]; r != "" && (w.RequestID == "" || parseTime(p.Annotations[AnnLastClaimedAt]).After(w.LastActivity)) {
			w.RequestID = r
		}
		w.CreatedAt = earliest(w.CreatedAt, p.CreationTimestamp.Time)
		w.LastActivity = latest(w.LastActivity, p.CreationTimestamp.Time, parseTime(p.Annotations[AnnLastClaimedAt]), podFinishedAt(p))
		live, startupTimedOut := b.podState(p)
		if w.Live == nil || live {
			w.Live = &live
		}
		if live {
			w.StartupTimedOut = false
		} else if startupTimedOut && (w.Live == nil || !*w.Live) {
			w.StartupTimedOut = true
		}
	}
	out := make([]backend.WorkerInfo, 0, len(byID))
	for _, w := range byID {
		if w.ID == "" {
			continue
		}
		if w.Live == nil {
			f := false
			w.Live = &f
		}
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Dispose deletes every pod for the worker and its PVC.
func (b *Backend) Dispose(ctx context.Context, workerID, reason string) error {
	pods, err := b.listPods(ctx, workerID)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if err := b.deletePod(ctx, p.Name); err != nil {
			return err
		}
	}
	if !b.Persistent() {
		return nil
	}
	pvcs := b.o.Client.CoreV1().PersistentVolumeClaims(b.o.Namespace)
	name := PVCName(workerID)
	pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get pvc %s: %w", name, err)
	}
	if owner := pvc.Labels[LabelWorkerID]; owner != workerID {
		return fmt.Errorf("pvc %s belongs to worker %q, refusing to delete", name, owner)
	}
	if reason != "" {
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{AnnDisposeReason: reason}}})
		_, _ = pvcs.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	err = pvcs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pvc.UID}})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc %s: %w", name, err)
	}
	b.o.Log.Info("disposed workspace", "worker", workerID, "pvc", name, "reason", reason)
	return nil
}

func (b *Backend) ensurePVC(ctx context.Context, spec backend.Spec) error {
	pvcs := b.o.Client.CoreV1().PersistentVolumeClaims(b.o.Namespace)
	name := PVCName(spec.WorkerID)
	pvc := b.o.PVCTemplate.DeepCopy()
	pvc.TypeMeta = metav1.TypeMeta{}
	pvc.ObjectMeta = metav1.ObjectMeta{
		Name:        name,
		Namespace:   b.o.Namespace,
		Labels:      mergeMaps(b.o.PVCTemplate.Labels, b.labels(spec, ComponentWorkspace)),
		Annotations: mergeMaps(b.o.PVCTemplate.Annotations, b.annotations(spec)),
	}
	_, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{})
	if err == nil {
		b.o.Log.Info("created workspace pvc", "worker", spec.WorkerID, "pvc", name)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pvc %s: %w", name, err)
	}
	existing, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get pvc %s: %w", name, err)
	}
	if owner := existing.Labels[LabelWorkerID]; owner != spec.WorkerID {
		return fmt.Errorf("pvc %s already exists for worker %q", name, owner)
	}
	return b.annotatePVC(ctx, spec.WorkerID, spec.RequestID(), spec.Kind)
}

func (b *Backend) annotatePVC(ctx context.Context, workerID, requestID string, kind backend.SpawnKind) error {
	ann := map[string]string{AnnLastClaimedAt: b.o.Now().UTC().Format(time.RFC3339), AnnSpawnKind: string(kind)}
	if requestID != "" {
		ann[AnnRequestID] = requestID
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	_, err := b.o.Client.CoreV1().PersistentVolumeClaims(b.o.Namespace).Patch(ctx, PVCName(workerID), types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("annotate pvc: %w", err)
	}
	return nil
}

func (b *Backend) annotatePod(ctx context.Context, name, requestID string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		AnnRequestID: requestID, AnnLastClaimedAt: b.o.Now().UTC().Format(time.RFC3339)}}})
	_, err := b.o.Client.CoreV1().Pods(b.o.Namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("annotate pod: %w", err)
	}
	return nil
}

func (b *Backend) createPod(ctx context.Context, spec backend.Spec) error {
	pod := b.BuildPod(spec)
	created, err := b.o.Client.CoreV1().Pods(b.o.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create pod %s: %w", pod.Name, err)
	}
	b.o.Log.Info("created worker pod", "worker", spec.WorkerID, "pod", created.Name, "pool", spec.Pool, "kind", spec.Kind, "request", spec.RequestID())
	return nil
}

// BuildPod renders the pod manifest for a spec without creating it.
func (b *Backend) BuildPod(spec backend.Spec) *corev1.Pod {
	pod := b.o.PodTemplate.DeepCopy()
	pod.TypeMeta = metav1.TypeMeta{}
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:        podName(spec.WorkerID),
		Namespace:   b.o.Namespace,
		Labels:      mergeMaps(b.o.PodTemplate.Labels, b.labels(spec, ComponentWorker)),
		Annotations: mergeMaps(b.o.PodTemplate.Annotations, b.annotations(spec)),
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	c := b.workerContainer(pod)
	env := backend.Env(spec)
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	c.Env = dropEnv(c.Env, names...)
	for _, k := range names {
		c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: env[k]})
	}
	if b.o.APIKeySecretName != "" && !hasEnv(c.Env, "CURSOR_API_KEY") {
		c.Env = append(c.Env, corev1.EnvVar{Name: "CURSOR_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: b.o.APIKeySecretName}, Key: b.o.APIKeySecretKey}}})
	}
	if b.Persistent() {
		vol := corev1.Volume{Name: WorkspaceVolume, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName(spec.WorkerID)}}}
		replaced := false
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == WorkspaceVolume {
				pod.Spec.Volumes[i] = vol
				replaced = true
			}
		}
		if !replaced {
			pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == WorkspaceVolume {
				mounted = true
			}
		}
		if !mounted {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: WorkspaceVolume, MountPath: b.o.MountPath})
		}
		c.Env = dropEnv(c.Env, "CURSOR_WORKSPACE_PATH")
		c.Env = append(c.Env, corev1.EnvVar{Name: "CURSOR_WORKSPACE_PATH", Value: b.o.MountPath})
	}
	return pod
}

func (b *Backend) workerContainer(pod *corev1.Pod) *corev1.Container {
	if b.o.WorkerContainer != "" {
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == b.o.WorkerContainer {
				return &pod.Spec.Containers[i]
			}
		}
	}
	return &pod.Spec.Containers[0]
}

func (b *Backend) labels(spec backend.Spec, component string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedBy,
		LabelComponent: component,
		LabelWorkerID:  spec.WorkerID,
		LabelPool:      spec.Pool,
	}
}

func (b *Backend) annotations(spec backend.Spec) map[string]string {
	ann := map[string]string{
		AnnLastClaimedAt: b.o.Now().UTC().Format(time.RFC3339),
		AnnSpawnKind:     string(spec.Kind),
	}
	if r := spec.RequestID(); r != "" {
		ann[AnnRequestID] = r
	}
	return ann
}

func (b *Backend) listPods(ctx context.Context, workerID string) ([]corev1.Pod, error) {
	list, err := b.o.Client.CoreV1().Pods(b.o.Namespace).List(ctx, metav1.ListOptions{LabelSelector: b.workerSelector(workerID)})
	if err != nil {
		return nil, fmt.Errorf("list pods for %s: %w", workerID, err)
	}
	return list.Items, nil
}

func (b *Backend) deletePod(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationBackground
	err := b.o.Client.CoreV1().Pods(b.o.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return nil
}

// isLive is true for pods that are running or still coming up.
func isLive(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false
	}
	return true
}

func (b *Backend) podState(p *corev1.Pod) (live, startupTimedOut bool) {
	if !isLive(p) {
		return false, false
	}
	for _, condition := range p.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true, false
		}
	}
	started := p.CreationTimestamp.Time
	for _, condition := range p.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			started = latest(started, condition.LastTransitionTime.Time)
		}
	}
	if started.IsZero() || b.o.StartupTimeout <= 0 || b.o.Now().Sub(started) < b.o.StartupTimeout {
		return true, false
	}
	return false, true
}

func podFinishedAt(p *corev1.Pod) time.Time {
	var t time.Time
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			t = latest(t, cs.State.Terminated.FinishedAt.Time)
		}
	}
	if p.DeletionTimestamp != nil {
		t = latest(t, p.DeletionTimestamp.Time)
	}
	return t
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func earliest(cur, t time.Time) time.Time {
	if t.IsZero() {
		return cur
	}
	if cur.IsZero() || t.Before(cur) {
		return t
	}
	return cur
}

func mergeMaps(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	maps.Copy(out, base)
	maps.Copy(out, override)
	return out
}

func dropEnv(env []corev1.EnvVar, names ...string) []corev1.EnvVar {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := env[:0:0]
	for _, e := range env {
		if !drop[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}
