package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ewhauser/cursor-controller/internal/backend"
	"github.com/ewhauser/cursor-controller/internal/cursorapi"
	"github.com/ewhauser/cursor-controller/internal/metrics"
)

// fakeAPI scripts one list result and one stream of events.
type fakeAPI struct {
	mu              sync.Mutex
	listed          []cursorapi.PendingRequest
	cursor          string
	events          []cursorapi.Event
	claims          []string
	released        []string
	conflict        map[string]bool
	agents          map[string]*cursorapi.Agent
	workers         map[string]bool
	pools           map[string]*cursorapi.Pool
	streams         int
	stopAfterStream func()
}

func (f *fakeAPI) ListAllPendingRequests(context.Context, cursorapi.ListOptions) ([]cursorapi.PendingRequest, string, error) {
	return f.listed, f.cursor, nil
}
func (f *fakeAPI) StreamPendingRequests(ctx context.Context, _ cursorapi.StreamOptions, fn func(cursorapi.Event) error) error {
	f.mu.Lock()
	f.streams++
	f.mu.Unlock()
	for _, e := range f.events {
		if err := fn(e); err != nil {
			return err
		}
	}
	if f.stopAfterStream != nil {
		f.stopAfterStream()
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeAPI) ClaimRequest(_ context.Context, id, workerID string) (*cursorapi.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflict[id] {
		return nil, &cursorapi.APIError{Status: 409}
	}
	f.claims = append(f.claims, id+"@"+workerID)
	return &cursorapi.Claim{ID: id, WorkerID: workerID}, nil
}
func (f *fakeAPI) ReleaseClaim(_ context.Context, id string) (*cursorapi.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
	return &cursorapi.Claim{ID: id}, nil
}
func (f *fakeAPI) GetPool(_ context.Context, name string) (*cursorapi.Pool, error) {
	if p, ok := f.pools[name]; ok {
		return p, nil
	}
	return nil, &cursorapi.APIError{Status: 404}
}
func (f *fakeAPI) GetWorker(_ context.Context, id string) (*cursorapi.Worker, error) {
	if f.workers[id] {
		return &cursorapi.Worker{WorkerID: id}, nil
	}
	return nil, &cursorapi.APIError{Status: 404}
}
func (f *fakeAPI) GetAgent(_ context.Context, id string) (*cursorapi.Agent, error) {
	if a, ok := f.agents[id]; ok {
		return a, nil
	}
	return nil, &cursorapi.APIError{Status: 404}
}

type fakeBackend struct {
	mu       sync.Mutex
	prefix   string
	spawned  []backend.Spec
	woken    []backend.Spec
	claims   map[string]string
	disposed []string
	workers  []backend.WorkerInfo
	wakeErr  map[string]error
	spawnErr error
}

func (b *fakeBackend) Name() string        { return "fake" }
func (b *fakeBackend) Owns(id string) bool { return backend.HasPrefix(id, b.prefix) }
func (b *fakeBackend) Spawn(_ context.Context, s backend.Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spawnErr != nil {
		return b.spawnErr
	}
	b.spawned = append(b.spawned, s)
	return nil
}
func (b *fakeBackend) Wake(_ context.Context, s backend.Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err, ok := b.wakeErr[s.WorkerID]; ok {
		return err
	}
	b.woken = append(b.woken, s)
	return nil
}
func (b *fakeBackend) RecordClaim(_ context.Context, w, r string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claims == nil {
		b.claims = map[string]string{}
	}
	b.claims[w] = r
	return nil
}
func (b *fakeBackend) ListWorkers(context.Context) ([]backend.WorkerInfo, error) {
	return b.workers, nil
}
func (b *fakeBackend) Dispose(_ context.Context, id, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disposed = append(b.disposed, id)
	return nil
}

func newTestController(api API, be *fakeBackend, cfg Config) *Controller {
	cfg.WorkerIDPrefix = be.prefix
	return New(cfg, api, be, slog.New(slog.NewTextHandler(testWriter{}, nil)), metrics.New())
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

func runUntilStream(t *testing.T, c *Controller, api *fakeAPI) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api.stopAfterStream = cancel
	if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestClaimThenSpawnAndWake(t *testing.T) {
	api := &fakeAPI{
		listed: []cursorapi.PendingRequest{
			{ID: "r1", Labels: []cursorapi.Label{{Key: "pool", Value: "gpu"}}, RepoURL: "https://github.com/a/b"},
			{ID: "taken"},
			{ID: "r-offline", ClaimedWorkerID: "cc-aaaaaaaaaaaa", WakeTimeoutMs: 60000},
			{ID: "r-foreign", ClaimedWorkerID: "other-1"},
		},
		cursor:   "c0",
		conflict: map[string]bool{"taken": true},
		events: []cursorapi.Event{
			{Type: cursorapi.EventCreated, Data: `{"id":"r2"}`},
			{Type: cursorapi.EventClaimedOffline, Data: `{"id":"r3","claimedWorkerId":"cc-bbbbbbbbbbbb","wakeTimeoutMs":1000}`},
			{Type: cursorapi.EventClaimedOffline, Data: `{"id":"r4","claimedWorkerId":"cc-missing"}`},
			{Type: cursorapi.EventClaimed, Data: `{"id":"r5","workerId":"cc-cccccccccccc"}`},
			{Type: cursorapi.EventHeartbeat, Data: `{}`},
		},
	}
	be := &fakeBackend{prefix: "cc", wakeErr: map[string]error{"cc-missing": backend.ErrWorkspaceMissing}}
	c := newTestController(api, be, Config{Pools: []string{"gpu"}})
	runUntilStream(t, c, api)

	if len(api.claims) != 2 || api.claims[0][:3] != "r1@" || api.claims[1][:3] != "r2@" {
		t.Fatalf("claims = %v", api.claims)
	}
	if len(be.spawned) != 2 || be.spawned[0].Pool != "gpu" || be.spawned[0].Request.RepoURL != "https://github.com/a/b" || be.spawned[0].Kind != backend.KindClaim {
		t.Fatalf("spawned = %+v", be.spawned)
	}
	if !be.Owns(be.spawned[0].WorkerID) {
		t.Fatalf("minted worker id %q lacks prefix", be.spawned[0].WorkerID)
	}
	if len(be.woken) != 2 || be.woken[0].WorkerID != "cc-aaaaaaaaaaaa" || be.woken[0].WakeTimeout != time.Minute || be.woken[1].WorkerID != "cc-bbbbbbbbbbbb" || be.woken[1].Kind != backend.KindWake {
		t.Fatalf("woken = %+v", be.woken)
	}
	if len(api.released) != 1 || api.released[0] != "r4" {
		t.Fatalf("released = %v (want r4 for missing workspace)", api.released)
	}
	if be.claims["cc-cccccccccccc"] != "r5" {
		t.Fatalf("recorded claims = %v", be.claims)
	}
}

func TestSpawnFailureReleasesClaim(t *testing.T) {
	api := &fakeAPI{listed: []cursorapi.PendingRequest{{ID: "r1"}}, cursor: "c0"}
	be := &fakeBackend{prefix: "cc", spawnErr: fmt.Errorf("boom")}
	c := newTestController(api, be, Config{})
	runUntilStream(t, c, api)
	if len(api.claims) != 1 || len(api.released) != 1 || api.released[0] != "r1" {
		t.Fatalf("claims=%v released=%v", api.claims, api.released)
	}
}

func TestWarmIdleReconcile(t *testing.T) {
	api := &fakeAPI{pools: map[string]*cursorapi.Pool{"gpu": {PoolName: "gpu", ConnectedWorkerCount: 2, InUseWorkerCount: 1}}}
	live := true
	be := &fakeBackend{prefix: "cc", workers: []backend.WorkerInfo{
		{ID: "cc-1", Pool: "gpu", Live: &live, CreatedAt: time.Now()},                 // starting, counts
		{ID: "cc-2", Pool: "gpu", Live: &live, CreatedAt: time.Now().Add(-time.Hour)}, // old, already reflected in pool counts
		{ID: "cc-3", Pool: "gpu", Live: &live, RequestID: "r", CreatedAt: time.Now()}, // busy
		{ID: "cc-4", Pool: "other", Live: &live, CreatedAt: time.Now()},               // other pool
	}}
	c := newTestController(api, be, Config{Pools: []string{"gpu"}, WarmIdle: 4})
	c.reconcileWarm(context.Background(), "gpu", c.log)
	// target 4 - idle 1 - starting 1 = 2 spawns
	if len(be.spawned) != 2 || be.spawned[0].Kind != backend.KindWarm || be.spawned[0].Request != nil {
		t.Fatalf("spawned = %+v", be.spawned)
	}
}

func TestGCPolicy(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	live, dead := true, false
	api := &fakeAPI{
		agents: map[string]*cursorapi.Agent{
			"archived": {ID: "archived", Status: cursorapi.AgentStatusArchived},
			"active":   {ID: "active", Status: cursorapi.AgentStatusActive},
			"idle":     {ID: "idle", Status: cursorapi.AgentStatusIdle},
		},
		workers: map[string]bool{"cc-hook-live": true},
	}
	be := &fakeBackend{prefix: "cc", workers: []backend.WorkerInfo{
		{ID: "cc-live", RequestID: "archived", Live: &live, LastActivity: now.Add(-100 * time.Hour)},            // live: keep
		{ID: "cc-archived-fresh", RequestID: "archived", Live: &dead, LastActivity: now.Add(-10 * time.Minute)}, // archived but within grace: keep
		{ID: "cc-archived", RequestID: "archived", Live: &dead, LastActivity: now.Add(-2 * time.Hour)},          // archived past grace: dispose
		{ID: "cc-deleted", RequestID: "gone", Live: &dead, LastActivity: now.Add(-2 * time.Hour)},               // agent 404: dispose
		{ID: "cc-active", RequestID: "active", Live: &dead, LastActivity: now.Add(-2 * time.Hour)},              // active, waiting for wake: keep
		{ID: "cc-idle-old", RequestID: "idle", Live: &dead, LastActivity: now.Add(-8 * 24 * time.Hour)},         // idle past hard TTL: dispose
		{ID: "cc-unclaimed", Live: &dead, CreatedAt: now.Add(-2 * time.Hour)},                                   // warm worker never claimed: dispose
		{ID: "cc-hook-live", RequestID: "archived", LastActivity: now.Add(-2 * time.Hour)},                      // liveness unknown, API says connected: keep
		{ID: "cc-hook-dead", RequestID: "archived", LastActivity: now.Add(-2 * time.Hour)},                      // liveness unknown, API 404: dispose
		{ID: "other-1", RequestID: "archived", Live: &dead, LastActivity: now.Add(-100 * time.Hour)},            // not ours: keep
	}}
	c := newTestController(api, be, Config{
		DisposeAfter:          7 * 24 * time.Hour,
		DisposeArchivedAfter:  time.Hour,
		DisposeUnclaimedAfter: time.Hour,
		CheckAgent:            true,
	})
	c.now = func() time.Time { return now }
	if err := c.RunGC(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"cc-archived", "cc-deleted", "cc-idle-old", "cc-unclaimed", "cc-hook-dead"}
	if fmt.Sprint(be.disposed) != fmt.Sprint(want) {
		t.Fatalf("disposed = %v, want %v", be.disposed, want)
	}
}

func TestGCWithoutAgentChecks(t *testing.T) {
	now := time.Now()
	dead := false
	api := &fakeAPI{}
	be := &fakeBackend{prefix: "cc", workers: []backend.WorkerInfo{
		{ID: "cc-a", RequestID: "x", Live: &dead, LastActivity: now.Add(-2 * time.Hour)},
		{ID: "cc-b", RequestID: "y", Live: &dead, LastActivity: now.Add(-30 * time.Hour)},
	}}
	c := newTestController(api, be, Config{DisposeAfter: 24 * time.Hour, CheckAgent: false})
	if err := c.RunGC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(be.disposed) != "[cc-b]" {
		t.Fatalf("disposed = %v", be.disposed)
	}
}

func TestAuthErrorAborts(t *testing.T) {
	api := &authFailAPI{fakeAPI: &fakeAPI{}}
	be := &fakeBackend{prefix: "cc"}
	c := newTestController(api, be, Config{ExitOnAuthError: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

type authFailAPI struct{ *fakeAPI }

func (a *authFailAPI) ListAllPendingRequests(context.Context, cursorapi.ListOptions) ([]cursorapi.PendingRequest, string, error) {
	return nil, "", &cursorapi.APIError{Status: 401, Body: "Agent-scoped service-account API key required"}
}
