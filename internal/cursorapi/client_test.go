package cursorapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListAllPendingRequestsPaginates(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v0/private-workers/pending-requests" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("pool") != "gpu" {
			t.Errorf("pool query = %q", r.URL.Query().Get("pool"))
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			_ = json.NewEncoder(w).Encode(PendingRequestsPage{Requests: []PendingRequest{{ID: "a"}}, NextPageToken: "p2", StreamCursor: "c0"})
		case "p2":
			_ = json.NewEncoder(w).Encode(PendingRequestsPage{Requests: []PendingRequest{{ID: "b", ClaimedWorkerID: "w1", WakeTimeoutMs: 1500}}, StreamCursor: "ignored"})
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "key")
	reqs, cursor, err := c.ListAllPendingRequests(context.Background(), ListOptions{Pool: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer key" {
		t.Errorf("auth header = %q", seenAuth)
	}
	if cursor != "c0" || len(reqs) != 2 || reqs[1].ClaimedWorkerID != "w1" || reqs[1].WakeTimeout() != 1500*time.Millisecond {
		t.Fatalf("unexpected result: cursor=%q reqs=%+v", cursor, reqs)
	}
}

func TestClaimErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["id"] {
		case "taken":
			http.Error(w, "already claimed", http.StatusConflict)
		case "gone":
			http.Error(w, "nope", http.StatusNotFound)
		case "badkey":
			http.Error(w, "personal key", http.StatusUnauthorized)
		default:
			_ = json.NewEncoder(w).Encode(Claim{ID: body["id"], WorkerID: body["workerId"]})
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "key")
	ctx := context.Background()
	if _, err := c.ClaimRequest(ctx, "taken", "w"); !errors.Is(err, ErrConflict) {
		t.Errorf("taken: %v", err)
	}
	if _, err := c.ClaimRequest(ctx, "gone", "w"); !errors.Is(err, ErrNotFound) {
		t.Errorf("gone: %v", err)
	}
	if _, err := c.ClaimRequest(ctx, "badkey", "w"); !IsAuthError(err) || StatusOf(err) != 401 {
		t.Errorf("badkey: %v", err)
	}
	cl, err := c.ClaimRequest(ctx, "ok", "w9")
	if err != nil || cl.WorkerID != "w9" {
		t.Errorf("ok: %v %+v", err, cl)
	}
}

func TestStreamCursorExpiredAndEvents(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept = %q", r.Header.Get("Accept"))
		}
		if r.URL.Query().Get("cursor") == "old" {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: created\nid: 1\ndata: {\"id\":\"r1\"}\n\nevent: claimed_offline\nid: 2\ndata: {\"id\":\"r2\",\"claimedWorkerId\":\"w1\"}\n\n"))
	}))
	defer srv.Close()
	c := New(srv.URL, "key")
	err := c.StreamPendingRequests(context.Background(), StreamOptions{Cursor: "old"}, func(Event) error { return nil })
	if !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("expected cursor expired, got %v", err)
	}
	var types []string
	err = c.StreamPendingRequests(context.Background(), StreamOptions{Cursor: "fresh", Pool: "p"}, func(e Event) error {
		types = append(types, e.Type)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 2 || types[0] != EventCreated || types[1] != EventClaimedOffline {
		t.Fatalf("types = %v", types)
	}
}

func TestGetAgentAndWorker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/bc-1":
			_ = json.NewEncoder(w).Encode(Agent{ID: "bc-1", Status: AgentStatusArchived})
		case "/v0/private-workers/w-1":
			_ = json.NewEncoder(w).Encode(Worker{WorkerID: "w-1", IsInUse: true})
		case "/v0/private-workers/pools":
			_, _ = w.Write([]byte(`{"pools":[{"scope":"team","poolName":"gpu","connectedWorkerCount":3,"inUseWorkerCount":1}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "key")
	ctx := context.Background()
	a, err := c.GetAgent(ctx, "bc-1")
	if err != nil || a.Status != AgentStatusArchived {
		t.Fatalf("agent: %v %+v", err, a)
	}
	if _, err := c.GetAgent(ctx, "bc-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing agent: %v", err)
	}
	wk, err := c.GetWorker(ctx, "w-1")
	if err != nil || !wk.IsInUse {
		t.Fatalf("worker: %v %+v", err, wk)
	}
	p, err := c.GetPool(ctx, "gpu")
	if err != nil || p.IdleWorkerCount() != 2 {
		t.Fatalf("pool: %v %+v", err, p)
	}
	if _, err := c.GetPool(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pool: %v", err)
	}
}

func TestDecodeClaimEvent(t *testing.T) {
	e, err := DecodeClaimEvent(`{"id":"r","claimedWorkerId":"w"}`)
	if err != nil || e.WorkerID != "w" {
		t.Fatalf("%v %+v", err, e)
	}
	if _, err := DecodeRequest(`{"labels":[]}`); err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestRouteTemplate(t *testing.T) {
	cases := map[string]string{
		"/v0/private-workers":                                "/v0/private-workers",
		"/v0/private-workers/pw_123":                         "/v0/private-workers/{id}",
		"/v0/private-workers/summary":                        "/v0/private-workers/summary",
		"/v0/private-workers/pools":                          "/v0/private-workers/pools",
		"/v0/private-workers/pending-requests":               "/v0/private-workers/pending-requests",
		"/v0/private-workers/pending-requests/stream":        "/v0/private-workers/pending-requests/stream",
		"/v0/private-workers/claim":                          "/v0/private-workers/claim",
		"/v0/private-workers/claims/bc-1/release":            "/v0/private-workers/claims/{id}/release",
		"/v1/agents/bc-00000000-0000-0000-0000-000000000001": "/v1/agents/{id}",
		"/v1/agents": "/v1/agents",
	}
	for in, want := range cases {
		if got := RouteTemplate(in); got != want {
			t.Errorf("RouteTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}
