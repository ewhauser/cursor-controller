package fakeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client drives a fake server's admin endpoints (tests, e2e, fake worker).
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient creates an admin client.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: http.DefaultClient}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateRequestOptions describes an injected request.
type CreateRequestOptions struct {
	ID        string `json:"id,omitempty"`
	Pool      string `json:"pool,omitempty"`
	UserID    int64  `json:"userId,omitempty"`
	RepoURL   string `json:"repoUrl,omitempty"`
	RepoOwner string `json:"repoOwner,omitempty"`
	RepoName  string `json:"repoName,omitempty"`
}

// CreateRequest injects a pending request and returns its id.
func (c *Client) CreateRequest(ctx context.Context, o CreateRequestOptions) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/fake/requests", o, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ConnectOptions is the fake worker's connect payload.
type ConnectOptions struct {
	Pool        string `json:"pool,omitempty"`
	Name        string `json:"name,omitempty"`
	Wake        bool   `json:"wake"`
	MarkerFound bool   `json:"markerFound"`
	RequestID   string `json:"requestId,omitempty"`
}

// Connect marks a worker connected.
func (c *Client) Connect(ctx context.Context, workerID string, o ConnectOptions) error {
	return c.do(ctx, http.MethodPost, "/fake/workers/"+workerID+"/connect", o, nil)
}

// Disconnect marks a worker offline.
func (c *Client) Disconnect(ctx context.Context, workerID string) error {
	return c.do(ctx, http.MethodPost, "/fake/workers/"+workerID+"/disconnect", nil, nil)
}

// Followup sends a new turn to an agent.
func (c *Client) Followup(ctx context.Context, agentID string) error {
	return c.do(ctx, http.MethodPost, "/fake/agents/"+agentID+"/followup", nil, nil)
}

// Archive archives an agent.
func (c *Client) Archive(ctx context.Context, agentID string) error {
	return c.do(ctx, http.MethodPost, "/fake/agents/"+agentID+"/archive", nil, nil)
}

// DeleteAgent permanently deletes an agent.
func (c *Client) DeleteAgent(ctx context.Context, agentID string) error {
	return c.do(ctx, http.MethodDelete, "/fake/agents/"+agentID, nil, nil)
}

// Expire expires a request.
func (c *Client) Expire(ctx context.Context, requestID string) error {
	return c.do(ctx, http.MethodPost, "/fake/requests/"+requestID+"/expire", nil, nil)
}

// RegisterPool sets a pool's wake timeout.
func (c *Client) RegisterPool(ctx context.Context, p Pool) error {
	return c.do(ctx, http.MethodPost, "/fake/pools", p, nil)
}

// State fetches the fake's state.
func (c *Client) State(ctx context.Context) (*State, error) {
	var st State
	if err := c.do(ctx, http.MethodGet, "/fake/state", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Reset clears all state.
func (c *Client) Reset(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/fake/reset", nil, nil)
}
