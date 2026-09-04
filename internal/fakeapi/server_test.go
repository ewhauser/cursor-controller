package fakeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ewhauser/cursor-controller/internal/cursorapi"
)

func TestFakeLifecycle(t *testing.T) {
	srv := New(Options{APIKey: "k", Heartbeat: 50 * time.Millisecond})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	admin := NewClient(ts.URL)
	api := cursorapi.New(ts.URL, "k")

	if _, _, err := cursorapi.New(ts.URL, "wrong").ListAllPendingRequests(ctx, cursorapi.ListOptions{}); !cursorapi.IsAuthError(err) {
		t.Fatalf("expected auth error, got %v", err)
	}
	if err := admin.RegisterPool(ctx, Pool{PoolName: "gpu", WorkerReadyTimeoutSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	id, err := admin.CreateRequest(ctx, CreateRequestOptions{Pool: "gpu", RepoURL: "https://github.com/a/b"})
	if err != nil {
		t.Fatal(err)
	}
	reqs, cursor, err := api.ListAllPendingRequests(ctx, cursorapi.ListOptions{Pool: "gpu"})
	if err != nil || len(reqs) != 1 || reqs[0].ID != id || reqs[0].Pool() != "gpu" || cursor == "" {
		t.Fatalf("list: %v %+v %q", err, reqs, cursor)
	}
	if other, _, _ := api.ListAllPendingRequests(ctx, cursorapi.ListOptions{Pool: "other"}); len(other) != 0 {
		t.Fatalf("pool filter leaked: %+v", other)
	}

	// Claim, conflict, stream sees `claimed`.
	if _, err := api.ClaimRequest(ctx, id, "cc-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ClaimRequest(ctx, id, "cc-2"); !errors.Is(err, cursorapi.ErrConflict) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := api.ClaimRequest(ctx, "nope", "cc-2"); !errors.Is(err, cursorapi.ErrNotFound) {
		t.Fatalf("unknown claim: %v", err)
	}
	if reqs, _, _ := api.ListAllPendingRequests(ctx, cursorapi.ListOptions{Pool: "gpu"}); len(reqs) != 0 {
		t.Fatalf("claimed request still pending: %+v", reqs)
	}

	// Worker connects, is visible, agent active; disconnect -> idle.
	if _, err := api.GetWorker(ctx, "cc-1"); !errors.Is(err, cursorapi.ErrNotFound) {
		t.Fatalf("worker before connect: %v", err)
	}
	if err := admin.Connect(ctx, "cc-1", ConnectOptions{Pool: "gpu", RequestID: id}); err != nil {
		t.Fatal(err)
	}
	wk, err := api.GetWorker(ctx, "cc-1")
	if err != nil || !wk.IsInUse || wk.ActiveBcID != id {
		t.Fatalf("worker: %v %+v", err, wk)
	}
	if p, err := api.GetPool(ctx, "gpu"); err != nil || p.ConnectedWorkerCount != 1 || p.InUseWorkerCount != 1 || p.WorkerReadyTimeoutSeconds != 120 {
		t.Fatalf("pool: %v %+v", err, p)
	}
	if err := admin.Disconnect(ctx, "cc-1"); err != nil {
		t.Fatal(err)
	}
	if a, _ := api.GetAgent(ctx, id); a.Status != cursorapi.AgentStatusIdle {
		t.Fatalf("agent after disconnect: %+v", a)
	}

	// Follow-up to offline worker -> claimed_offline in list and stream.
	if err := admin.Followup(ctx, id); err != nil {
		t.Fatal(err)
	}
	reqs, cursor2, _ := api.ListAllPendingRequests(ctx, cursorapi.ListOptions{Pool: "gpu"})
	if len(reqs) != 1 || reqs[0].ClaimedWorkerID != "cc-1" || reqs[0].WakeTimeoutMs != 120000 {
		t.Fatalf("offline claim not listed: %+v", reqs)
	}
	var types []string
	var claimedData map[string]any
	sctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	err = api.StreamPendingRequests(sctx, cursorapi.StreamOptions{Pool: "gpu", Cursor: cursor}, func(e cursorapi.Event) error {
		if e.Type != cursorapi.EventHeartbeat {
			types = append(types, e.Type)
		}
		if e.Type == cursorapi.EventClaimed {
			if err := json.Unmarshal([]byte(e.Data), &claimedData); err != nil {
				t.Errorf("decode claimed payload: %v", err)
			}
		}
		if e.Type == cursorapi.EventClaimedOffline {
			r, err := cursorapi.DecodeRequest(e.Data)
			if err != nil || r.ClaimedWorkerID != "cc-1" {
				t.Errorf("bad claimed_offline payload: %v %+v", err, r)
			}
			cancel()
		}
		return nil
	})
	cancel()
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stream err: %v", err)
	}
	if len(types) != 2 || types[0] != cursorapi.EventClaimed || types[1] != cursorapi.EventClaimedOffline {
		t.Fatalf("stream types = %v", types)
	}
	if _, ok := claimedData["workerId"]; ok || claimedData["id"] != id {
		t.Fatalf("claimed payload must match Cursor's id-only contract: %+v", claimedData)
	}

	// Release re-queues and emits created; expired cursor -> 410.
	if _, err := api.ReleaseClaim(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ReleaseClaim(ctx, id); !errors.Is(err, cursorapi.ErrNotFound) {
		t.Fatalf("double release: %v", err)
	}
	st, _ := admin.State(ctx)
	if len(st.Releases) != 1 || st.Requests[0].Status != "pending" || st.Requests[0].ClaimedWorkerID != "" {
		t.Fatalf("state after release: %+v %+v", st.Releases, st.Requests[0])
	}
	if err := api.StreamPendingRequests(ctx, cursorapi.StreamOptions{Cursor: "c999999"}, nil); !errors.Is(err, cursorapi.ErrCursorExpired) {
		t.Fatalf("unknown cursor: %v", err)
	}
	_ = cursor2

	// Archive and delete.
	if err := admin.Archive(ctx, id); err != nil {
		t.Fatal(err)
	}
	if a, _ := api.GetAgent(ctx, id); a.Status != cursorapi.AgentStatusArchived {
		t.Fatalf("archived: %+v", a)
	}
	if err := admin.Followup(ctx, id); err == nil {
		t.Fatal("followup on archived agent should fail")
	}
	if err := admin.DeleteAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := api.GetAgent(ctx, id); !errors.Is(err, cursorapi.ErrNotFound) {
		t.Fatalf("deleted agent: %v", err)
	}
}

func TestCursorTTL(t *testing.T) {
	now := time.Now()
	srv := New(Options{CursorTTL: time.Minute, Now: func() time.Time { return now }})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	api := cursorapi.New(ts.URL, "")
	_, cursor, err := api.ListAllPendingRequests(context.Background(), cursorapi.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	err = api.StreamPendingRequests(context.Background(), cursorapi.StreamOptions{Cursor: cursor}, nil)
	if !errors.Is(err, cursorapi.ErrCursorExpired) {
		t.Fatalf("expected 410, got %v", err)
	}
}
