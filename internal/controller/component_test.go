package controller

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ewhauser/cursor-controller/internal/backend"
	"github.com/ewhauser/cursor-controller/internal/cursorapi"
	"github.com/ewhauser/cursor-controller/internal/fakeapi"
	"github.com/ewhauser/cursor-controller/internal/metrics"
)

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestComponentAgainstFakeAPI wires the real HTTP client and controller to the
// in-memory fake API: claim -> spawn, worker lifecycle, follow-up -> wake,
// missing workspace -> release -> re-claim, archive -> GC dispose.
func TestComponentAgainstFakeAPI(t *testing.T) {
	fake := fakeapi.New(fakeapi.Options{APIKey: "k", Heartbeat: 100 * time.Millisecond})
	ts := httptest.NewServer(fake)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := fakeapi.NewClient(ts.URL)
	api := cursorapi.New(ts.URL, "k")
	if err := admin.RegisterPool(ctx, fakeapi.Pool{PoolName: "gpu", WorkerReadyTimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}

	be := &fakeBackend{prefix: "cc", wakeErr: map[string]error{}}
	c := New(Config{Pools: []string{"gpu"}, ResyncInterval: 200 * time.Millisecond, ReconnectDelay: 10 * time.Millisecond,
		DisposeArchivedAfter: 0, CheckAgent: true, ExitOnAuthError: true},
		api, be, slog.New(slog.NewTextHandler(testWriter{}, nil)), metrics.New())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// 1. Request arrives on the stream -> claimed and spawned.
	id, err := admin.CreateRequest(ctx, fakeapi.CreateRequestOptions{Pool: "gpu", RepoURL: "https://github.com/acme/mono", UserID: 9})
	if err != nil {
		t.Fatal(err)
	}
	var workerID string
	waitFor(t, "spawn", func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		if len(be.spawned) == 1 {
			workerID = be.spawned[0].WorkerID
			return true
		}
		return false
	})
	if r := fake.Request(id); r.Status != "claimed" || r.ClaimedWorkerID != workerID {
		t.Fatalf("request after spawn: %+v", r)
	}
	if be.spawned[0].Request.UserID != 9 || be.spawned[0].Pool != "gpu" {
		t.Fatalf("spawn spec: %+v", be.spawned[0])
	}

	// 2. Worker connects, finishes, exits.
	if err := admin.Connect(ctx, workerID, fakeapi.ConnectOptions{Pool: "gpu", RequestID: id}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Disconnect(ctx, workerID); err != nil {
		t.Fatal(err)
	}

	// 3. Follow-up while offline -> claimed_offline -> wake on the same id.
	if err := admin.Followup(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "wake", func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return len(be.woken) == 1 && be.woken[0].WorkerID == workerID && be.woken[0].WakeTimeout == time.Minute
	})
	if err := admin.Connect(ctx, workerID, fakeapi.ConnectOptions{Pool: "gpu", Wake: true, MarkerFound: true, RequestID: id}); err != nil {
		t.Fatal(err)
	}
	if r := fake.Request(id); r.AwaitingWake {
		t.Fatalf("still awaiting wake after connect: %+v", r)
	}
	if err := admin.Disconnect(ctx, workerID); err != nil {
		t.Fatal(err)
	}

	// 4. Workspace gone -> release -> Cursor re-queues -> fresh claim + spawn.
	be.mu.Lock()
	be.wakeErr[workerID] = backend.ErrWorkspaceMissing
	be.mu.Unlock()
	if err := admin.Followup(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-spawn after release", func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return len(be.spawned) == 2 && be.spawned[1].WorkerID != workerID
	})
	st := fake.Snapshot()
	if len(st.Releases) != 1 || st.Releases[0] != id || len(st.Claims) != 2 {
		t.Fatalf("releases=%v claims=%v", st.Releases, st.Claims)
	}
	newWorker := be.spawned[1].WorkerID

	// 5. Archive -> GC disposes the offline worker (agent check via fake API).
	if err := admin.Archive(ctx, id); err != nil {
		t.Fatal(err)
	}
	dead := false
	be.mu.Lock()
	be.workers = []backend.WorkerInfo{{ID: newWorker, Pool: "gpu", RequestID: id, Live: &dead, LastActivity: time.Now().Add(-time.Minute)}}
	be.mu.Unlock()
	if err := c.RunGC(ctx); err != nil {
		t.Fatal(err)
	}
	if len(be.disposed) != 1 || be.disposed[0] != newWorker {
		t.Fatalf("disposed = %v", be.disposed)
	}

	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("run: %v", err)
	}
}
