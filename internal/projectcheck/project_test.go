package projectcheck

import (
	"os"
	"path/filepath"
	"regexp"
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
	want := map[string]bool{"build": false, "test": false, "lint": false, "helm": false, "local-e2e": false, "kind-e2e": false}
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

func TestMakeTargetsMatchCIEntryPoints(t *testing.T) {
	makefile := string(readRepositoryFile(t, "Makefile"))
	for _, required := range []string{
		"GO_PACKAGES := ./...",
		"GOLANGCI_LINT_VERSION ?= v2.11.3",
		"github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)",
		"$(GOLANGCI_LINT) run ./...",
		"$(GOLANGCI_LINT) run --new-from-rev=HEAD ./...",
		"go build $(GO_PACKAGES)",
		"go test $(RACE) $(GO_PACKAGES)",
		"check: build test lint helm-lint",
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("Makefile missing gbash-style entry point %q", required)
		}
	}
}

func TestGolangCILintAndPreCommitArePinned(t *testing.T) {
	lintConfig := string(readRepositoryFile(t, ".golangci.yml"))
	for _, required := range []string{`version: "2"`, "goimports", "staticcheck", "modernize", "errorlint", "contextcheck"} {
		if !strings.Contains(lintConfig, required) {
			t.Errorf(".golangci.yml missing %q", required)
		}
	}
	preCommit := string(readRepositoryFile(t, ".pre-commit-config.yaml"))
	if !strings.Contains(preCommit, "entry: make lint-new") || !strings.Contains(preCommit, "pass_filenames: false") {
		t.Error("pre-commit hook must run the pinned changed-code lint target")
	}
	renovate := string(readRepositoryFile(t, ".renovaterc.json5"))
	if !strings.Contains(renovate, "helpers:pinGitHubActionDigests") || !strings.Contains(renovate, `"minimumReleaseAge": "3 days"`) {
		t.Error("Renovate must maintain immutable action pins with a release cooldown")
	}
}

func TestCIUsesCanonicalMakeTargetsAndLeastPrivilege(t *testing.T) {
	workflowBytes := readRepositoryFile(t, ".github/workflows/ci.yaml")
	workflow := string(workflowBytes)
	for _, command := range []string{"run: make build", "run: make test", "run: make lint", "run: make helm-lint", "run: make local-e2e"} {
		if !strings.Contains(workflow, command) {
			t.Errorf("CI does not use canonical target %q", command)
		}
	}
	var permissions struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowBytes, &permissions); err != nil {
		t.Fatal(err)
	}
	if permissions.Permissions["contents"] != "read" || permissions.Permissions["packages"] != "" {
		t.Errorf("workflow permissions are not read-only: %+v", permissions.Permissions)
	}
	if permissions.Jobs["image"].Permissions["packages"] != "write" {
		t.Errorf("image job lacks scoped package write permission: %+v", permissions.Jobs["image"].Permissions)
	}
}

func TestGitHubActionsUseImmutablePins(t *testing.T) {
	workflows, err := filepath.Glob(filepath.Join(repositoryRoot(t), ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`@[0-9a-f]{40}(?:\s+#\s+v\S+)?$`)
	for _, workflow := range workflows {
		body, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatal(err)
		}
		for lineNumber, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "uses:") && !pinned.MatchString(line) {
				t.Errorf("%s:%d action is not pinned to a full commit SHA with a version comment: %s", filepath.Base(workflow), lineNumber+1, line)
			}
		}
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

func TestReleaseWaitsForImageAndIsTagOnly(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Needs []string `yaml:"needs"`
			If    string   `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(readRepositoryFile(t, ".github/workflows/ci.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	release := workflow.Jobs["release"]
	if len(release.Needs) != 1 || release.Needs[0] != "image" {
		t.Fatal("release must wait for validated image publication")
	}
	if release.If != "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')" {
		t.Fatal("release must only publish version tags")
	}
}
