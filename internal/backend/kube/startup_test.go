package kube

import (
	"context"
	"testing"
	"time"

	"github.com/ewhauser/cursor-controller/internal/backend"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBusyWorkerSurvivesStartupDeadline(t *testing.T) {
	for _, observedReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup probe before first poll", true: "observed ready then restart"}[observedReady], func(t *testing.T) {
			b, cs := newBackend(t, false)
			ctx := context.Background()
			now := time.Now()
			b.o.Now = func() time.Time { return now }
			if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-busy"}); err != nil {
				t.Fatal(err)
			}
			pods, _ := cs.CoreV1().Pods("cursord").List(ctx, metav1.ListOptions{})
			p := pods.Items[0]
			p.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Hour))
			p.Status.Phase = corev1.PodRunning
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-20 * time.Minute))}}
			started := true
			if observedReady {
				p.Status.Conditions[0].Status = corev1.ConditionTrue
			} else {
				p.Spec.Containers[0].StartupProbe = &corev1.Probe{}
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", Started: &started}}
			}
			if _, err := cs.CoreV1().Pods("cursord").Update(ctx, &p, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.ListWorkers(ctx); err != nil {
				t.Fatal(err)
			}
			if observedReady {
				latest, err := cs.CoreV1().Pods("cursord").Get(ctx, p.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				latest.Status.Conditions[0].Status = corev1.ConditionFalse
				if _, err := cs.CoreV1().Pods("cursord").Update(ctx, latest, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				b, err = New(b.o)
				if err != nil {
					t.Fatal(err)
				}
			}
			workers, err := b.ListWorkers(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(workers) != 1 || workers[0].StartupTimedOut || workers[0].Live == nil || !*workers[0].Live {
				t.Fatalf("busy worker treated as failed startup: %+v", workers)
			}
		})
	}
}

func TestStartupEvidenceBelongsToWorker(t *testing.T) {
	b, _ := newBackend(t, false)
	now := time.Now()
	b.o.Now = func() time.Time { return now }
	started := true
	p := b.BuildPod(backend.Spec{WorkerID: "cc-startup"})
	p.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", Started: &started}}
	if _, timedOut := b.podState(p); !timedOut {
		t.Fatal("Started without startup probe bypassed deadline")
	}
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", StartupProbe: &corev1.Probe{}})
	p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: "sidecar", Ready: true, Started: &started})
	if _, timedOut := b.podState(p); !timedOut {
		t.Fatal("sidecar bypassed worker deadline")
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now)}}
	if _, timedOut := b.podState(p); !timedOut {
		t.Fatal("condition change reset startup deadline")
	}
	p.Annotations[AnnStarted] = "true"
	p.Status.Phase = corev1.PodFailed
	if live, timedOut := b.podState(p); live || timedOut {
		t.Fatal("started annotation resurrected failed pod")
	}
	b.o.PodTemplate.Annotations = map[string]string{AnnStarted: "true"}
	if b.BuildPod(backend.Spec{WorkerID: "cc-wake", Kind: backend.KindWake}).Annotations[AnnStarted] != "" {
		t.Fatal("new pod inherited startup marker")
	}
}
