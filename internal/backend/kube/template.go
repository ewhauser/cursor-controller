package kube

import (
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// LoadPodTemplate reads a Pod manifest (YAML or JSON). Only spec and
// metadata.labels/annotations are used; name and namespace are overwritten.
func LoadPodTemplate(path string) (*corev1.Pod, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePodTemplate(b)
}

// ParsePodTemplate parses a Pod manifest from bytes.
func ParsePodTemplate(b []byte) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := yaml.UnmarshalStrict(b, &pod); err != nil {
		return nil, fmt.Errorf("parse pod template: %w", err)
	}
	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod template must declare at least one container")
	}
	return &pod, nil
}

// LoadPVCTemplate reads a PersistentVolumeClaim manifest. Name and namespace
// are overwritten per worker.
func LoadPVCTemplate(path string) (*corev1.PersistentVolumeClaim, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePVCTemplate(b)
}

// ParsePVCTemplate parses a PVC manifest from bytes.
func ParsePVCTemplate(b []byte) (*corev1.PersistentVolumeClaim, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := yaml.UnmarshalStrict(b, &pvc); err != nil {
		return nil, fmt.Errorf("parse pvc template: %w", err)
	}
	if len(pvc.Spec.AccessModes) == 0 {
		return nil, fmt.Errorf("pvc template must set spec.accessModes")
	}
	if pvc.Spec.Resources.Requests == nil {
		return nil, fmt.Errorf("pvc template must set spec.resources.requests.storage")
	}
	return &pvc, nil
}
