package kube

import (
	"testing"

	"github.com/ewhauser/cursor-controller/internal/backend"
	corev1 "k8s.io/api/core/v1"
)

func TestWorkerDoesNotInheritFleetEndpoint(t *testing.T) {
	b, _ := newBackend(t, false)
	spec := backend.Spec{WorkerID: "cc-test", APIURL: "https://fleet.invalid"}
	for _, name := range []string{"CURSOR_API_URL", "CURSOR_API_ENDPOINT"} {
		if _, ok := envMap(b.BuildPod(spec).Spec.Containers[0])[name]; ok {
			t.Errorf("injected %s", name)
		}
	}
	b.o.PodTemplate.Spec.Containers[0].Env = append(b.o.PodTemplate.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "CURSOR_API_ENDPOINT", Value: "agent.example"},
		corev1.EnvVar{Name: "CURSOR_API_URL", Value: "custom.example"})
	env := envMap(b.BuildPod(spec).Spec.Containers[0])
	if env["CURSOR_API_ENDPOINT"].Value != "agent.example" || env["CURSOR_API_URL"].Value != "custom.example" {
		t.Fatalf("template endpoints overwritten: %+v", env)
	}
	b.o.WorkerAPIEndpoint = "override.example"
	env = envMap(b.BuildPod(spec).Spec.Containers[0])
	if env["CURSOR_API_ENDPOINT"].Value != "override.example" || env["CURSOR_API_URL"].Value != "custom.example" {
		t.Fatalf("explicit override: %+v", env)
	}
}
