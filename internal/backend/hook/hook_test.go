package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ewhauser/cursor-controller/internal/backend"
	"github.com/ewhauser/cursor-controller/internal/cursorapi"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHookLifecycle(t *testing.T) {
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env.txt")
	spawn := writeScript(t, dir, "spawn.sh", `env | grep ^CURSOR_ | sort > "`+envOut+`"
if [ "$CURSOR_WAKE" = "1" ] && [ "$CURSOR_AGENT_WORKER_ID" = "cc-missing" ]; then exit 66; fi
echo "spawned $CURSOR_AGENT_WORKER_ID"
`)
	disposed := filepath.Join(dir, "disposed.txt")
	dispose := writeScript(t, dir, "dispose.sh", `echo "$CURSOR_AGENT_WORKER_ID $CURSOR_DISPOSE_REASON $CURSOR_REQUEST_ID" >> "`+disposed+`"`)
	state := filepath.Join(dir, "state.json")

	b, err := New(Options{SpawnCommand: spawn, DisposeCommand: dispose, StateFile: state, WorkerIDPrefix: "cc", APIURL: "https://api.example", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := &cursorapi.PendingRequest{ID: "bc-1", RepoURL: "https://x/y", Labels: []cursorapi.Label{{Key: "pool", Value: "gpu"}}}
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-1", Pool: "gpu", Request: req}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(envOut)
	for _, want := range []string{"CURSOR_AGENT_WORKER_ID=cc-1", "CURSOR_POOL=gpu", "CURSOR_REQUEST_ID=bc-1", "CURSOR_API_KEY=k", "CURSOR_API_URL=https://api.example", "CURSOR_REPO_URL=https://x/y", "CURSOR_SPAWN_KIND=claim"} {
		if !strings.Contains(string(got), want+"\n") {
			t.Errorf("spawn env missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "CURSOR_WAKE=") {
		t.Errorf("fresh spawn must not set CURSOR_WAKE")
	}

	// Wake existing worker passes CURSOR_WAKE.
	if err := b.Wake(ctx, backend.Spec{WorkerID: "cc-1", Pool: "gpu", Request: req, WakeTimeout: 2000000000}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(envOut)
	if !strings.Contains(string(got), "CURSOR_WAKE=1\n") || !strings.Contains(string(got), "CURSOR_WAKE_TIMEOUT_MS=2000\n") {
		t.Errorf("wake env:\n%s", got)
	}

	// Unknown and missing-workspace wakes.
	if err := b.Wake(ctx, backend.Spec{WorkerID: "cc-nope"}); !errors.Is(err, backend.ErrUnknownWorker) {
		t.Errorf("unknown: %v", err)
	}
	if err := b.Spawn(ctx, backend.Spec{WorkerID: "cc-missing", Pool: "gpu"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Wake(ctx, backend.Spec{WorkerID: "cc-missing", Pool: "gpu"}); !errors.Is(err, backend.ErrWorkspaceMissing) {
		t.Errorf("missing: %v", err)
	}

	// State survives a restart.
	b2, err := New(Options{SpawnCommand: spawn, DisposeCommand: dispose, StateFile: state, WorkerIDPrefix: "cc"})
	if err != nil {
		t.Fatal(err)
	}
	workers, _ := b2.ListWorkers(ctx)
	if len(workers) != 2 || workers[0].ID != "cc-1" || workers[0].RequestID != "bc-1" || workers[0].Live != nil {
		t.Fatalf("workers = %+v", workers)
	}
	if err := b2.Dispose(ctx, "cc-1", "ttl"); err != nil {
		t.Fatal(err)
	}
	d, _ := os.ReadFile(disposed)
	if strings.TrimSpace(string(d)) != "cc-1 ttl bc-1" {
		t.Fatalf("dispose output = %q", d)
	}
	workers, _ = b2.ListWorkers(ctx)
	if len(workers) != 1 {
		t.Fatalf("workers after dispose = %+v", workers)
	}
}

func TestSpawnDoesNotRunHookWhenStateCannotBePersisted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	spawn := writeScript(t, dir, "spawn.sh", `touch "`+marker+`"`)

	b, err := New(Options{
		SpawnCommand:   spawn,
		StateFile:      filepath.Join(dir, "missing", "state.json"),
		WorkerIDPrefix: "cc",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = b.Spawn(context.Background(), backend.Spec{WorkerID: "cc-1", Pool: "gpu"})
	if err == nil {
		t.Fatal("expected state persistence failure")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("spawn hook ran before durable state was recorded: %v", statErr)
	}
}
