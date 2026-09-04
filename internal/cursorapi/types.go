// Package cursorapi is a small client for the Cursor Cloud Agents fleet API:
// the /v0/private-workers endpoints used by pool controllers, plus the
// /v1/agents endpoints needed to decide when a workspace can be disposed.
package cursorapi

import "time"

// DefaultBaseURL is the public fleet API base.
const DefaultBaseURL = "https://api.cursor.com"

// Label is a routing label attached to a pending request.
type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// PendingRequest is one queued (or claimed-but-offline) private-worker request.
// Unknown fields are ignored so new API fields never break the controller.
type PendingRequest struct {
	ID               string   `json:"id"`
	UserID           int64    `json:"userId,omitempty"`
	UserEmail        string   `json:"userEmail,omitempty"`
	ServiceAccountID string   `json:"serviceAccountId,omitempty"`
	RepoOwner        string   `json:"repoOwner,omitempty"`
	RepoName         string   `json:"repoName,omitempty"`
	RepoURL          string   `json:"repoUrl,omitempty"`
	RepoURLs         []string `json:"repoUrls,omitempty"`
	Labels           []Label  `json:"labels,omitempty"`
	CreatedAtMs      int64    `json:"createdAtMs,omitempty"`
	// ClaimedWorkerID is set when the request was claimed by a worker that is
	// currently offline (hibernation). The controller must start a worker with
	// this exact id before WakeTimeoutMs elapses.
	ClaimedWorkerID string `json:"claimedWorkerId,omitempty"`
	WakeTimeoutMs   int64  `json:"wakeTimeoutMs,omitempty"`
}

// Label returns the value of the first label with the given key.
func (r PendingRequest) Label(key string) string {
	for _, l := range r.Labels {
		if l.Key == key {
			return l.Value
		}
	}
	return ""
}

// Pool returns the pool the request is routed to, following the same rule as
// the official CLI controller: the `pool` label, else "default".
func (r PendingRequest) Pool() string {
	if p := r.Label("pool"); p != "" {
		return p
	}
	return "default"
}

// IsOfflineClaim reports whether this request is waiting for a hibernated
// worker to come back rather than for a fresh claim.
func (r PendingRequest) IsOfflineClaim() bool { return r.ClaimedWorkerID != "" }

// WakeTimeout converts WakeTimeoutMs to a duration (zero when unset).
func (r PendingRequest) WakeTimeout() time.Duration {
	return time.Duration(r.WakeTimeoutMs) * time.Millisecond
}

// PendingRequestsPage is one page of GET /v0/private-workers/pending-requests.
type PendingRequestsPage struct {
	Requests      []PendingRequest `json:"requests"`
	NextPageToken string           `json:"nextPageToken,omitempty"`
	StreamCursor  string           `json:"streamCursor,omitempty"`
}

// Pool is one entry of GET /v0/private-workers/pools.
type Pool struct {
	Scope                     string `json:"scope"`
	OwnerID                   int64  `json:"ownerId,omitempty"`
	PoolName                  string `json:"poolName"`
	ConnectedWorkerCount      int    `json:"connectedWorkerCount"`
	InUseWorkerCount          int    `json:"inUseWorkerCount"`
	FirstSeenAtMs             int64  `json:"firstSeenAtMs,omitempty"`
	LastSeenAtMs              int64  `json:"lastSeenAtMs,omitempty"`
	IsStale                   bool   `json:"isStale,omitempty"`
	RepoOwner                 string `json:"repoOwner,omitempty"`
	RepoName                  string `json:"repoName,omitempty"`
	RepoURL                   string `json:"repoUrl,omitempty"`
	WorkerReadyTimeoutSeconds int    `json:"workerReadyTimeoutSeconds,omitempty"`
}

// IdleWorkerCount is connected minus in-use, floored at zero.
func (p Pool) IdleWorkerCount() int {
	if n := p.ConnectedWorkerCount - p.InUseWorkerCount; n > 0 {
		return n
	}
	return 0
}

// Worker is one entry of GET /v0/private-workers.
type Worker struct {
	WorkerID          string `json:"workerId"`
	IsInUse           bool   `json:"isInUse"`
	RepoOwner         string `json:"repoOwner,omitempty"`
	RepoName          string `json:"repoName,omitempty"`
	RepoURL           string `json:"repoUrl,omitempty"`
	WorkspaceRootPath string `json:"workspaceRootPath,omitempty"`
	ConnectedAtMs     int64  `json:"connectedAtMs,omitempty"`
	UserID            int64  `json:"userId,omitempty"`
	TeamID            int64  `json:"teamId,omitempty"`
	ServiceAccountID  string `json:"serviceAccountId,omitempty"`
	ActiveBcID        string `json:"activeBcId,omitempty"`
	Name              string `json:"name,omitempty"`
}

// WorkersPage is one page of GET /v0/private-workers.
type WorkersPage struct {
	Workers       []Worker `json:"workers"`
	TotalCount    int      `json:"totalCount"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

// Claim is the response of POST /v0/private-workers/claim and the release call.
type Claim struct {
	ID       string `json:"id"`
	WorkerID string `json:"workerId"`
}

// Agent status values from GET /v1/agents/{id}.
const (
	AgentStatusActive   = "ACTIVE"
	AgentStatusIdle     = "IDLE"
	AgentStatusArchived = "ARCHIVED"
)

// Agent is the subset of GET /v1/agents/{id} the controller uses.
type Agent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Status    string    `json:"status"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// Event types on the pending-requests SSE stream.
const (
	EventCreated        = "created"
	EventClaimed        = "claimed"
	EventClaimedOffline = "claimed_offline"
	EventExpired        = "expired"
	EventHeartbeat      = "heartbeat"
)

// Event is one server-sent event from the pending-requests stream.
type Event struct {
	Type string
	ID   string
	Data string
}
