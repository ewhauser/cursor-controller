package cursorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Observer is called once per API response (or transport failure, status 0).
type Observer func(method, path string, status int, d time.Duration)

// Client talks to the Cursor fleet API.
type Client struct {
	baseURL   string
	apiKey    string
	http      *http.Client
	userAgent string
	observe   Observer
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the default http.Client. The client used for the SSE
// stream must not set a global Timeout; use per-request contexts instead.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// WithObserver registers a metrics hook.
func WithObserver(o Observer) Option { return func(c *Client) { c.observe = o } }

// New creates a Client. baseURL defaults to DefaultBaseURL when empty.
func New(baseURL, apiKey string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		http:      &http.Client{},
		userAgent: "cursor-controller",
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// BaseURL returns the configured API base.
func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do performs the request and decodes a JSON body into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.report(method, path, 0, start)
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.report(method, path, resp.StatusCode, start)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(method, path, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *Client) report(method, path string, status int, start time.Time) {
	if c.observe != nil {
		c.observe(method, path, status, time.Since(start))
	}
}

func newAPIError(method, path string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
}

// ListOptions filters pending-request listings.
type ListOptions struct {
	Pool       string
	Repository string
	Limit      int
	PageToken  string
}

func (o ListOptions) query() url.Values {
	q := url.Values{}
	if o.Pool != "" {
		q.Set("pool", o.Pool)
	}
	if o.Repository != "" {
		q.Set("repository", o.Repository)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.PageToken != "" {
		q.Set("pageToken", o.PageToken)
	}
	return q
}

const pendingPath = "/v0/private-workers/pending-requests"

// ListPendingRequests returns one page of pending requests.
func (c *Client) ListPendingRequests(ctx context.Context, opts ListOptions) (*PendingRequestsPage, error) {
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	var page PendingRequestsPage
	if err := c.do(ctx, http.MethodGet, pendingPath, opts.query(), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// ListAllPendingRequests follows pagination and returns every pending request
// plus the stream cursor from the first page.
func (c *Client) ListAllPendingRequests(ctx context.Context, opts ListOptions) ([]PendingRequest, string, error) {
	var (
		all    []PendingRequest
		cursor string
	)
	for i := 0; i < 100; i++ {
		page, err := c.ListPendingRequests(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		if i == 0 {
			cursor = page.StreamCursor
		}
		all = append(all, page.Requests...)
		if page.NextPageToken == "" || page.NextPageToken == opts.PageToken {
			break
		}
		opts.PageToken = page.NextPageToken
	}
	return all, cursor, nil
}

// StreamOptions configures the SSE watch.
type StreamOptions struct {
	Pool       string
	Repository string
	Cursor     string
}

// StreamPendingRequests opens the SSE stream and calls fn for each event until
// the context is cancelled, the server closes the stream, or fn errors.
// Returns ErrCursorExpired (wrapped) on 410 so the caller can re-list.
func (c *Client) StreamPendingRequests(ctx context.Context, opts StreamOptions, fn func(Event) error) error {
	q := url.Values{}
	q.Set("cursor", opts.Cursor)
	if opts.Pool != "" {
		q.Set("pool", opts.Pool)
	}
	if opts.Repository != "" {
		q.Set("repository", opts.Repository)
	}
	path := pendingPath + "/stream"
	req, err := c.newRequest(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.report(http.MethodGet, path, 0, start)
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	c.report(http.MethodGet, path, resp.StatusCode, start)
	if resp.StatusCode != http.StatusOK {
		return newAPIError(http.MethodGet, path, resp)
	}
	err = ParseSSE(resp.Body, fn)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ClaimRequest claims a pending request for workerID. Returns ErrConflict when
// another worker already holds a live claim and ErrNotFound when the request
// is gone.
func (c *Client) ClaimRequest(ctx context.Context, requestID, workerID string) (*Claim, error) {
	var out Claim
	body := map[string]string{"id": requestID, "workerId": workerID}
	if err := c.do(ctx, http.MethodPost, "/v0/private-workers/claim", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseClaim releases the live claim on requestID so another worker can
// serve it. Returns ErrNotFound when no live claim exists.
func (c *Client) ReleaseClaim(ctx context.Context, requestID string) (*Claim, error) {
	var out Claim
	path := "/v0/private-workers/claims/" + url.PathEscape(requestID) + "/release"
	if err := c.do(ctx, http.MethodPost, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPools returns the pools visible to the API key.
func (c *Client) ListPools(ctx context.Context, scope string, includeStale bool) ([]Pool, error) {
	q := url.Values{}
	if scope != "" {
		q.Set("scope", scope)
	}
	if includeStale {
		q.Set("includeStale", "true")
	}
	var out struct {
		Pools []Pool `json:"pools"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/private-workers/pools", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Pools, nil
}

// GetPool finds a pool by name (any scope). Returns ErrNotFound if absent.
func (c *Client) GetPool(ctx context.Context, name string) (*Pool, error) {
	pools, err := c.ListPools(ctx, "", true)
	if err != nil {
		return nil, err
	}
	for i := range pools {
		if pools[i].PoolName == name {
			return &pools[i], nil
		}
	}
	return nil, fmt.Errorf("pool %q: %w", name, ErrNotFound)
}

// WorkerListOptions filters GET /v0/private-workers.
type WorkerListOptions struct {
	Status    string // all | in_use | idle
	Scope     string // all | team_pool | personal
	Limit     int
	PageToken string
}

// ListWorkers returns one page of connected workers.
func (c *Client) ListWorkers(ctx context.Context, opts WorkerListOptions) (*WorkersPage, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Scope != "" {
		q.Set("scope", opts.Scope)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.PageToken != "" {
		q.Set("pageToken", opts.PageToken)
	}
	var page WorkersPage
	if err := c.do(ctx, http.MethodGet, "/v0/private-workers", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// GetWorker returns a connected worker by id. Returns ErrNotFound when the
// worker is not connected.
func (c *Client) GetWorker(ctx context.Context, workerID string) (*Worker, error) {
	var out Worker
	path := "/v0/private-workers/" + url.PathEscape(workerID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAgent returns the cloud agent behind a request id. Returns ErrNotFound
// when the agent has been permanently deleted.
func (c *Client) GetAgent(ctx context.Context, agentID string) (*Agent, error) {
	var out Agent
	path := "/v1/agents/" + url.PathEscape(agentID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecodeRequest parses an SSE data payload into a PendingRequest. Payloads
// without an id are rejected.
func DecodeRequest(data string) (*PendingRequest, error) {
	var r PendingRequest
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, errors.New("event payload has no id")
	}
	return &r, nil
}

// ClaimEvent is the payload of `claimed` events.
type ClaimEvent struct {
	ID       string `json:"id"`
	WorkerID string `json:"workerId,omitempty"`
}

// DecodeClaimEvent parses a `claimed` payload; workerId may be absent.
func DecodeClaimEvent(data string) (*ClaimEvent, error) {
	var e ClaimEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, err
	}
	if e.ID == "" {
		return nil, errors.New("event payload has no id")
	}
	if e.WorkerID == "" {
		// Some payloads use the pending-request field name.
		var alt struct {
			ClaimedWorkerID string `json:"claimedWorkerId"`
		}
		_ = json.Unmarshal([]byte(data), &alt)
		e.WorkerID = alt.ClaimedWorkerID
	}
	return &e, nil
}
