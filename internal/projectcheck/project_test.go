package projectcheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readRepositoryFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repositoryRoot(t), name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPublishedImageWaitsForEveryValidationJob(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Needs []string `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(readRepositoryFile(t, ".github/workflows/ci.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"test": false, "helm": false, "local-e2e": false, "kind-e2e": false}
	for _, dependency := range workflow.Jobs["image"].Needs {
		if _, ok := want[dependency]; ok {
			want[dependency] = true
		}
	}
	for dependency, found := range want {
		if !found {
			t.Errorf("image job does not wait for %s", dependency)
		}
	}
}

func TestMakeLintActuallyRunsLintChecks(t *testing.T) {
	makefile := string(readRepositoryFile(t, "Makefile"))
	if !strings.Contains(makefile, "lint: vet") || !strings.Contains(makefile, "gofmt -l") {
		t.Fatalf("lint target must run vet and reject unformatted Go files")
	}
}

func TestHelmChartExposesReliabilityTimeouts(t *testing.T) {
	values := string(readRepositoryFile(t, "deploy/helm/cursor-controller/values.yaml"))
	deployment := string(readRepositoryFile(t, "deploy/helm/cursor-controller/templates/deployment.yaml"))
	for _, value := range []string{"apiTimeout:", "wakeRetryInterval:", "workerStartupTimeout:"} {
		if !strings.Contains(values, value) {
			t.Errorf("values.yaml missing %s", value)
		}
	}
	for _, arg := range []string{"--api-timeout=", "--wake-retry-interval=", "--worker-startup-timeout="} {
		if !strings.Contains(deployment, arg) {
			t.Errorf("deployment missing %s", arg)
		}
	}
}

func TestBuildsUsePatchedGoToolchain(t *testing.T) {
	if goMod := string(readRepositoryFile(t, "go.mod")); !strings.Contains(goMod, "\ngo 1.26.6\n") {
		t.Error("go.mod must require Go 1.26.6 or newer to include current standard-library security fixes")
	}
	for _, dockerfile := range []string{"Dockerfile", "hack/e2e/Dockerfile"} {
		if body := string(readRepositoryFile(t, dockerfile)); !strings.Contains(body, "golang:1.26.6-alpine") {
			t.Errorf("%s does not pin the patched Go 1.26.6 builder", dockerfile)
		}
	}
}
