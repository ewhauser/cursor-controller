// Package fakeapi is an in-memory stand-in for the Cursor fleet API. It
// implements the /v0/private-workers and /v1/agents endpoints the controller
// uses with the same status codes, plus /fake/* admin endpoints to drive
// scenarios (inject requests, connect/disconnect workers, send follow-ups,
// archive agents) and inspect state. It serves both Go tests and the kind
// end-to-end harness.
package fakeapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ewhauser/cursor-controller/internal/cursorapi"
)

// Request is one agent request as tracked by the fake.
type Request struct {
	ID          string            `json:"id"`
	UserID      int64             `json:"userId,omitempty"`
	RepoOwner   string            `json:"repoOwner,omitempty"`
	RepoName    string            `json:"repoName,omitempty"`
	RepoURL     string            `json:"repoUrl,omitempty"`
	Labels      []cursorapi.Label `json:"labels"`
	CreatedAtMs int64             `json:"createdAtMs"`
	// Claim state.
	ClaimedWorkerID string `json:"claimedWorkerId,omitempty"`
	AwaitingWake    bool   `json:"awaitingWake,omitempty"`
	Status          string `json:"status"` // pending | claimed | expired
}

func (r *Request) pool() string {
	for _, l := range r.Labels {
		if l.Key == "pool" {
			return l.Value
		}
	}
	return "default"
}

// pendingView is what list/stream payloads expose.
func (r *Request) pendingView(wakeTimeout time.Duration) cursorapi.PendingRequest {
	p := cursorapi.PendingRequest{ID: r.ID, UserID: r.UserID, RepoOwner: r.RepoOwner, RepoName: r.RepoName, RepoURL: r.RepoURL, Labels: r.Labels, CreatedAtMs: r.CreatedAtMs}
	if r.AwaitingWake {
		p.ClaimedWorkerID = r.ClaimedWorkerID
		p.WakeTimeoutMs = wakeTimeout.Milliseconds()
	}
	return p
}

// Worker is a worker known to the fake (connected or not).
type Worker struct {
	ID          string `json:"workerId"`
	Name        string `json:"name,omitempty"`
	Pool        string `json:"pool"`
	Connected   bool   `json:"connected"`
	ConnectedAt int64  `json:"connectedAtMs,omitempty"`
	ActiveBcID  string `json:"activeBcId,omitempty"`
}

// Agent mirrors /v1/agents/{id}.
type Agent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Pool mirrors /v0/private-workers/pools entries.
type Pool struct {
	Scope                     string `json:"scope"`
	PoolName                  string `json:"poolName"`
	WorkerReadyTimeoutSeconds int    `json:"workerReadyTimeoutSeconds"`
	RepoOwner                 string `json:"repoOwner,omitempty"`
	RepoName                  string `json:"repoName,omitempty"`
	RepoURL                   string `json:"repoUrl,omitempty"`
}

// Event is one stream event with its sequence number.
type Event struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	Data string `json:"data"`
}

// ConnectRecord captures a fake worker connect call for assertions.
type ConnectRecord struct {
	WorkerID    string `json:"workerId"`
	Pool        string `json:"pool"`
	Wake        bool   `json:"wake"`
	MarkerFound bool   `json:"markerFound"`
	RequestID   string `json:"requestId,omitempty"`
	At          int64  `json:"atMs"`
}

// State is the inspectable snapshot returned by GET /fake/state.
type State struct {
	Requests []*Request      `json:"requests"`
	Workers  []*Worker       `json:"workers"`
	Agents   []*Agent        `json:"agents"`
	Pools    []*Pool         `json:"pools"`
	Claims   []string        `json:"claims"`   // "<request>@<worker>"
	Releases []string        `json:"releases"` // request ids
	Connects []ConnectRecord `json:"connects"`
	Events   []Event         `json:"events"`
}

// Options configures the fake.
type Options struct {
	APIKey    string
	Heartbeat time.Duration
	CursorTTL time.Duration
	Log       *slog.Logger
	Now       func() time.Time
}

// Server is the fake API.
type Server struct {
	o   Options
	mux *http.ServeMux

	mu       sync.Mutex
	requests map[string]*Request
	order    []string
	workers  map[string]*Worker
	agents   map[string]*Agent
	pools    map[string]*Pool
	events   []Event
	cursors  map[string]time.Time
	claims   []string
	releases []string
	connects []ConnectRecord
	seq      int64
	notify   chan struct{}
	nextID   int
}

// New creates a fake server. Use Handler() or ServeHTTP.
func New(o Options) *Server {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	if o.CursorTTL <= 0 {
		o.CursorTTL = 5 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{o: o, mux: http.NewServeMux(), notify: make(chan struct{})}
	s.resetLocked()
	s.routes()
	return s
}

func (s *Server) resetLocked() {
	s.requests = map[string]*Request{}
	s.order = nil
	s.workers = map[string]*Worker{}
	s.agents = map[string]*Agent{}
	s.pools = map[string]*Pool{"default": {Scope: "team", PoolName: "default"}}
	s.events = nil
	s.cursors = map[string]time.Time{}
	s.claims, s.releases, s.connects = nil, nil, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	auth := s.requireAuth
	s.mux.HandleFunc("GET /v0/private-workers/pending-requests", auth(s.listPending))
	s.mux.HandleFunc("GET /v0/private-workers/pending-requests/stream", auth(s.stream))
	s.mux.HandleFunc("POST /v0/private-workers/claim", auth(s.claim))
	s.mux.HandleFunc("POST /v0/private-workers/claims/{id}/release", auth(s.release))
	s.mux.HandleFunc("GET /v0/private-workers/pools", auth(s.listPools))
	s.mux.HandleFunc("POST /v0/private-workers/pools", auth(s.registerPool))
	s.mux.HandleFunc("GET /v0/private-workers", auth(s.listWorkers))
	s.mux.HandleFunc("GET /v0/private-workers/summary", auth(s.workerSummary))
	s.mux.HandleFunc("GET /v0/private-workers/{id}", auth(s.getWorker))
	s.mux.HandleFunc("GET /v1/agents/{id}", auth(s.getAgent))
	s.mux.HandleFunc("POST /v1/agents", auth(s.createAgent))
	s.mux.HandleFunc("POST /v1/agents/{id}/archive", auth(s.archiveAgent))
	s.mux.HandleFunc("POST /v1/agents/{id}/followup", auth(s.followup))
	s.mux.HandleFunc("DELETE /v1/agents/{id}", auth(s.deleteAgent))

	// Admin endpoints: no auth, used by tests and the fake worker.
	s.mux.HandleFunc("POST /fake/requests", s.createAgent)
	s.mux.HandleFunc("POST /fake/requests/{id}/expire", s.expireRequest)
	s.mux.HandleFunc("POST /fake/workers/{id}/connect", s.connectWorker)
	s.mux.HandleFunc("POST /fake/workers/{id}/disconnect", s.disconnectWorker)
	s.mux.HandleFunc("POST /fake/agents/{id}/followup", s.followup)
	s.mux.HandleFunc("POST /fake/agents/{id}/archive", s.archiveAgent)
	s.mux.HandleFunc("DELETE /fake/agents/{id}", s.deleteAgent)
	s.mux.HandleFunc("POST /fake/pools", s.registerPool)
	s.mux.HandleFunc("GET /fake/state", s.state)
	s.mux.HandleFunc("POST /fake/reset", s.reset)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.o.APIKey == "" {
			next(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		ok := h == "Bearer "+s.o.APIKey
		if !ok {
			if u, _, hasBasic := r.BasicAuth(); hasBasic && u == s.o.APIKey {
				ok = true
			}
		}
		if !ok {
			http.Error(w, "Agent-scoped service-account API key required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}

// emitLocked appends an event and wakes stream subscribers.
func (s *Server) emitLocked(typ string, data any) {
	b, _ := json.Marshal(data)
	s.seq++
	s.events = append(s.events, Event{Seq: s.seq, Type: typ, Data: string(b)})
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *Server) wakeTimeoutLocked(pool string) time.Duration {
	if p, ok := s.pools[pool]; ok {
		return time.Duration(p.WorkerReadyTimeoutSeconds) * time.Second
	}
	return 0
}

func (s *Server) isPendingLocked(r *Request) bool {
	return r.Status == "pending" || (r.Status == "claimed" && r.AwaitingWake)
}

// --- /v0/private-workers ----------------------------------------------------

func (s *Server) listPending(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pool := q.Get("pool")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	start, _ := strconv.Atoi(q.Get("pageToken"))
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []cursorapi.PendingRequest
	for _, id := range s.order {
		req := s.requests[id]
		if !s.isPendingLocked(req) || (pool != "" && req.pool() != pool) {
			continue
		}
		all = append(all, req.pendingView(s.wakeTimeoutLocked(req.pool())))
	}
	end := min(start+limit, len(all))
	page := cursorapi.PendingRequestsPage{Requests: all[start:end]}
	if page.Requests == nil {
		page.Requests = []cursorapi.PendingRequest{}
	}
	if end < len(all) {
		page.NextPageToken = strconv.Itoa(end)
	}
	cursor := "c" + strconv.FormatInt(s.seq, 10)
	s.cursors[cursor] = s.o.Now()
	page.StreamCursor = cursor
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	pool := r.URL.Query().Get("pool")
	s.mu.Lock()
	issued, ok := s.cursors[cursor]
	if !ok || s.o.Now().Sub(issued) > s.o.CursorTTL {
		s.mu.Unlock()
		http.Error(w, "cursor expired", http.StatusGone)
		return
	}
	after, err := strconv.ParseInt(strings.TrimPrefix(cursor, "c"), 10, 64)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	hb := time.NewTicker(s.o.Heartbeat)
	defer hb.Stop()
	for {
		s.mu.Lock()
		var pending []Event
		for _, e := range s.events {
			if e.Seq > after {
				pending = append(pending, e)
			}
		}
		notify := s.notify
		s.mu.Unlock()
		for _, e := range pending {
			after = e.Seq
			if pool != "" && !eventMatchesPool(e, pool) {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: %s\nid: c%d\ndata: %s\n\n", e.Type, e.Seq, e.Data)
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-notify:
		case <-hb.C:
			_, _ = fmt.Fprintf(w, "event: heartbeat\nid: c%d\ndata: {}\n\n", after)
			flusher.Flush()
		}
	}
}

func eventMatchesPool(e Event, pool string) bool {
	var payload struct {
		Labels []cursorapi.Label `json:"labels"`
	}
	if err := json.Unmarshal([]byte(e.Data), &payload); err != nil || payload.Labels == nil {
		return true // claimed/expired payloads carry no labels; deliver them
	}
	return cursorapi.PendingRequest{Labels: payload.Labels}.Pool() == pool
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		WorkerID string `json:"workerId"`
	}
	if err := decode(r, &body); err != nil || body.ID == "" || body.WorkerID == "" {
		http.Error(w, "id and workerId required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[body.ID]
	if !ok || req.Status == "expired" {
		http.Error(w, "no pending request", http.StatusNotFound)
		return
	}
	if req.Status == "claimed" {
		http.Error(w, "request already has a live claim", http.StatusConflict)
		return
	}
	req.Status = "claimed"
	req.ClaimedWorkerID = body.WorkerID
	req.AwaitingWake = false
	if _, ok := s.workers[body.WorkerID]; !ok {
		s.workers[body.WorkerID] = &Worker{ID: body.WorkerID, Pool: req.pool()}
	}
	s.claims = append(s.claims, body.ID+"@"+body.WorkerID)
	s.emitLocked(cursorapi.EventClaimed, map[string]string{"id": req.ID, "workerId": body.WorkerID})
	s.o.Log.Info("fakeapi: claimed", "request", req.ID, "worker", body.WorkerID)
	writeJSON(w, http.StatusOK, cursorapi.Claim{ID: req.ID, WorkerID: body.WorkerID})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[id]
	if !ok || req.Status != "claimed" {
		http.Error(w, "no live claim", http.StatusNotFound)
		return
	}
	worker := req.ClaimedWorkerID
	req.Status = "pending"
	req.ClaimedWorkerID = ""
	req.AwaitingWake = false
	s.releases = append(s.releases, id)
	s.emitLocked(cursorapi.EventCreated, req.pendingView(0))
	s.o.Log.Info("fakeapi: released", "request", id, "worker", worker)
	writeJSON(w, http.StatusOK, cursorapi.Claim{ID: id, WorkerID: worker})
}

func (s *Server) listPools(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type view struct {
		Pool
		ConnectedWorkerCount int  `json:"connectedWorkerCount"`
		InUseWorkerCount     int  `json:"inUseWorkerCount"`
		IsStale              bool `json:"isStale"`
	}
	var out []view
	for _, p := range s.pools {
		v := view{Pool: *p}
		for _, wk := range s.workers {
			if wk.Pool == p.PoolName && wk.Connected {
				v.ConnectedWorkerCount++
				if wk.ActiveBcID != "" {
					v.InUseWorkerCount++
				}
			}
		}
		out = append(out, v)
	}
	if out == nil {
		out = []view{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func (s *Server) registerPool(w http.ResponseWriter, r *http.Request) {
	var p Pool
	if err := decode(r, &p); err != nil || p.PoolName == "" {
		http.Error(w, "poolName required", http.StatusBadRequest)
		return
	}
	if p.Scope == "" {
		p.Scope = "team"
	}
	s.mu.Lock()
	s.pools[p.PoolName] = &p
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"registered": true})
}

func (s *Server) workerView(wk *Worker) cursorapi.Worker {
	return cursorapi.Worker{WorkerID: wk.ID, Name: wk.Name, IsInUse: wk.ActiveBcID != "", ConnectedAtMs: wk.ConnectedAt, ActiveBcID: wk.ActiveBcID}
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	s.mu.Lock()
	defer s.mu.Unlock()
	page := cursorapi.WorkersPage{Workers: []cursorapi.Worker{}}
	for _, wk := range s.workers {
		if !wk.Connected {
			continue
		}
		inUse := wk.ActiveBcID != ""
		if (status == "in_use" && !inUse) || (status == "idle" && inUse) {
			continue
		}
		page.Workers = append(page.Workers, s.workerView(wk))
	}
	page.TotalCount = len(page.Workers)
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) workerSummary(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var connected, inUse int
	for _, wk := range s.workers {
		if wk.Connected {
			connected++
			if wk.ActiveBcID != "" {
				inUse++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"team": map[string]int{"totalConnected": connected, "inUse": inUse}})
}

func (s *Server) getWorker(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wk, ok := s.workers[r.PathValue("id")]
	if !ok || !wk.Connected {
		http.Error(w, "worker not connected", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.workerView(wk))
}

// --- /v1/agents ---------------------------------------------------------------

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[r.PathValue("id")]
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// createAgent serves both POST /v1/agents (Cursor shape) and POST /fake/requests.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		UserID int64  `json:"userId"`
		Pool   string `json:"pool"`
		Env    struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"env"`
		RepoURL   string `json:"repoUrl"`
		RepoOwner string `json:"repoOwner"`
		RepoName  string `json:"repoName"`
		Repos     []struct {
			URL string `json:"url"`
		} `json:"repos"`
		Labels []cursorapi.Label `json:"labels"`
	}
	if err := decode(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pool := body.Pool
	if pool == "" && body.Env.Type == "pool" {
		pool = body.Env.Name
	}
	if pool == "" {
		pool = "default"
	}
	if body.RepoURL == "" && len(body.Repos) > 0 {
		body.RepoURL = body.Repos[0].URL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := body.ID
	if id == "" {
		s.nextID++
		id = fmt.Sprintf("bc-%08d-0000-0000-0000-000000000000", s.nextID)
	}
	if _, exists := s.requests[id]; exists {
		http.Error(w, "duplicate id", http.StatusConflict)
		return
	}
	now := s.o.Now()
	labels := append([]cursorapi.Label{{Key: "pool", Value: pool}}, body.Labels...)
	req := &Request{ID: id, UserID: body.UserID, RepoOwner: body.RepoOwner, RepoName: body.RepoName, RepoURL: body.RepoURL, Labels: labels, CreatedAtMs: now.UnixMilli(), Status: "pending"}
	s.requests[id] = req
	s.order = append(s.order, id)
	name := body.Name
	if name == "" {
		name = "fake agent " + id
	}
	s.agents[id] = &Agent{ID: id, Name: name, Status: cursorapi.AgentStatusActive, CreatedAt: now, UpdatedAt: now}
	s.emitLocked(cursorapi.EventCreated, req.pendingView(0))
	s.o.Log.Info("fakeapi: request created", "request", id, "pool", pool)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "status": cursorapi.AgentStatusActive, "request": req})
}

func (s *Server) archiveAgent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[r.PathValue("id")]
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	a.Status = cursorapi.AgentStatusArchived
	a.UpdatedAt = s.o.Now()
	writeJSON(w, http.StatusOK, map[string]string{"id": a.ID})
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if _, ok := s.agents[id]; !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	delete(s.agents, id)
	if req, ok := s.requests[id]; ok {
		req.Status = "expired"
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// followup models a new turn on an existing agent. If the claimed worker is
// offline the request re-enters the pending list with claimedWorkerId and a
// claimed_offline event fires; if the claim was released it is re-queued.
func (s *Server) followup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	a, ok := s.agents[id]
	req := s.requests[id]
	if !ok || req == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if a.Status == cursorapi.AgentStatusArchived {
		http.Error(w, "agent is archived", http.StatusConflict)
		return
	}
	a.Status = cursorapi.AgentStatusActive
	a.UpdatedAt = s.o.Now()
	switch req.Status {
	case "claimed":
		wk := s.workers[req.ClaimedWorkerID]
		if wk != nil && wk.Connected {
			wk.ActiveBcID = req.ID
			s.o.Log.Info("fakeapi: followup delivered to connected worker", "request", id, "worker", wk.ID)
		} else {
			req.AwaitingWake = true
			s.emitLocked(cursorapi.EventClaimedOffline, req.pendingView(s.wakeTimeoutLocked(req.pool())))
			s.o.Log.Info("fakeapi: followup for offline worker", "request", id, "worker", req.ClaimedWorkerID)
		}
	default:
		req.Status = "pending"
		req.ClaimedWorkerID = ""
		req.AwaitingWake = false
		s.emitLocked(cursorapi.EventCreated, req.pendingView(0))
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": a.Status})
}

// --- /fake admin ---------------------------------------------------------------

func (s *Server) expireRequest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[r.PathValue("id")]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	req.Status = "expired"
	req.AwaitingWake = false
	s.emitLocked(cursorapi.EventExpired, map[string]string{"id": req.ID})
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) connectWorker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pool        string `json:"pool"`
		Name        string `json:"name"`
		Wake        bool   `json:"wake"`
		MarkerFound bool   `json:"markerFound"`
		RequestID   string `json:"requestId"`
	}
	if err := decode(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	wk, ok := s.workers[id]
	if !ok {
		wk = &Worker{ID: id}
		s.workers[id] = wk
	}
	if body.Pool != "" {
		wk.Pool = body.Pool
	}
	if wk.Pool == "" {
		wk.Pool = "default"
	}
	wk.Name = body.Name
	wk.Connected = true
	wk.ConnectedAt = s.o.Now().UnixMilli()
	wk.ActiveBcID = ""
	for _, req := range s.requests {
		if req.Status == "claimed" && req.ClaimedWorkerID == id {
			req.AwaitingWake = false
			wk.ActiveBcID = req.ID
			if a := s.agents[req.ID]; a != nil {
				a.Status = cursorapi.AgentStatusActive
			}
		}
	}
	s.connects = append(s.connects, ConnectRecord{WorkerID: id, Pool: wk.Pool, Wake: body.Wake, MarkerFound: body.MarkerFound, RequestID: body.RequestID, At: wk.ConnectedAt})
	s.o.Log.Info("fakeapi: worker connected", "worker", id, "wake", body.Wake, "marker", body.MarkerFound, "active", wk.ActiveBcID)
	writeJSON(w, http.StatusOK, wk)
}

func (s *Server) disconnectWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	wk, ok := s.workers[id]
	if !ok {
		http.Error(w, "unknown worker", http.StatusNotFound)
		return
	}
	wk.Connected = false
	if a := s.agents[wk.ActiveBcID]; a != nil && a.Status == cursorapi.AgentStatusActive {
		a.Status = cursorapi.AgentStatusIdle
		a.UpdatedAt = s.o.Now()
	}
	wk.ActiveBcID = ""
	s.o.Log.Info("fakeapi: worker disconnected", "worker", id)
	writeJSON(w, http.StatusOK, wk)
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

// Snapshot returns a copy of the fake's state for assertions.
func (s *Server) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Claims: append([]string{}, s.claims...), Releases: append([]string{}, s.releases...), Connects: append([]ConnectRecord{}, s.connects...), Events: append([]Event{}, s.events...)}
	for _, id := range s.order {
		cp := *s.requests[id]
		st.Requests = append(st.Requests, &cp)
	}
	for _, wk := range s.workers {
		cp := *wk
		st.Workers = append(st.Workers, &cp)
	}
	for _, a := range s.agents {
		cp := *a
		st.Agents = append(st.Agents, &cp)
	}
	for _, p := range s.pools {
		cp := *p
		st.Pools = append(st.Pools, &cp)
	}
	return st
}

func (s *Server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.resetLocked()
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Request returns the request with id (nil if absent).
func (s *Server) Request(id string) *Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.requests[id]; ok {
		cp := *r
		return &cp
	}
	return nil
}

// Worker returns the worker with id (nil if absent).
func (s *Server) Worker(id string) *Worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wk, ok := s.workers[id]; ok {
		cp := *wk
		return &cp
	}
	return nil
}
