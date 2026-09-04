// Package backend defines how the controller turns claims into running
// workers. Implementations: kube (native Kubernetes Pods + PVCs) and hook
// (exec spawn/dispose scripts, compatible with `agent worker controller`).
package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/ewhauser/cursor-controller/internal/cursorapi"
)

// SpawnKind distinguishes why a worker is being started.
type SpawnKind string

const (
	KindClaim SpawnKind = "claim" // fresh claim-then-spawn
	KindWake  SpawnKind = "wake"  // hibernated worker being revived
	KindWarm  SpawnKind = "warm"  // warm-idle capacity, no request yet
)

// Spec describes the worker to start.
type Spec struct {
	WorkerID string
	Pool     string
	Kind     SpawnKind
	// Request is the claimed request. Nil for warm spawns.
	Request *cursorapi.PendingRequest
	// WakeTimeout is how long Cursor will wait for the worker to reconnect
	// (only meaningful for KindWake; zero when unknown).
	WakeTimeout time.Duration
	// APIURL is passed through to the worker as CURSOR_API_URL/ENDPOINT.
	APIURL string
}

// RequestID returns the request id or "".
func (s Spec) RequestID() string {
	if s.Request == nil {
		return ""
	}
	return s.Request.ID
}

// Errors returned by Wake.
var (
	// ErrWorkspaceMissing means the retained workspace for the worker is gone;
	// the controller releases the claim so Cursor can re-queue the request.
	ErrWorkspaceMissing = errors.New("worker workspace is missing")
	// ErrUnknownWorker means the backend has no record of the worker at all.
	ErrUnknownWorker = errors.New("unknown worker")
)

// WorkerInfo is the backend's view of one worker it manages.
type WorkerInfo struct {
	ID        string
	Pool      string
	RequestID string
	// Live is true when a worker process/pod is running or starting. Nil means
	// the backend cannot tell; the controller then asks the Cursor API.
	Live *bool
	// LastActivity is the most recent claim, wake, or process exit.
	LastActivity time.Time
	CreatedAt    time.Time
}

// Backend starts, revives, tracks, and disposes workers.
type Backend interface {
	Name() string
	// Owns reports whether the worker id was minted by this controller.
	Owns(workerID string) bool
	// Spawn starts a brand-new worker (and its workspace, if persistent).
	Spawn(ctx context.Context, spec Spec) error
	// Wake restarts an existing worker on its retained workspace. It is
	// idempotent: if the worker is already live it returns nil.
	Wake(ctx context.Context, spec Spec) error
	// RecordClaim associates a request with a worker that was claimed by
	// Cursor directly (warm-idle workers).
	RecordClaim(ctx context.Context, workerID, requestID string) error
	// ListWorkers returns every worker the backend knows about.
	ListWorkers(ctx context.Context) ([]WorkerInfo, error)
	// Dispose deletes the worker's workspace and any leftover process/pod.
	Dispose(ctx context.Context, workerID, reason string) error
}

var prefixRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidatePrefix checks a worker-id prefix is DNS-1123 safe and short enough
// to leave room for the random suffix and derived resource names.
func ValidatePrefix(prefix string) error {
	if !prefixRE.MatchString(prefix) {
		return fmt.Errorf("worker id prefix %q must be lowercase alphanumeric/dashes, 1-40 chars", prefix)
	}
	return nil
}

// NewWorkerID mints a worker id of the form <prefix>-<12 hex>.
func NewWorkerID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// HasPrefix reports whether workerID was minted with prefix.
func HasPrefix(workerID, prefix string) bool {
	return len(workerID) > len(prefix)+1 && workerID[:len(prefix)+1] == prefix+"-"
}

// Env returns the CURSOR_* environment the official CLI controller passes to
// its spawn hook, plus wake metadata. The API key is deliberately not
// included; each backend injects it from its own secret source.
func Env(spec Spec) map[string]string {
	env := map[string]string{
		"CURSOR_POOL":            spec.Pool,
		"CURSOR_AGENT_WORKER_ID": spec.WorkerID,
		"CURSOR_WORKER_NAME":     spec.WorkerID,
		"CURSOR_SPAWN_KIND":      string(spec.Kind),
	}
	if spec.APIURL != "" {
		env["CURSOR_API_URL"] = spec.APIURL
		env["CURSOR_API_ENDPOINT"] = spec.APIURL
	}
	if r := spec.Request; r != nil {
		env["CURSOR_REQUEST_ID"] = r.ID
		env["CURSOR_BC_ID"] = r.ID
		if r.UserID != 0 {
			env["CURSOR_USER_ID"] = strconv.FormatInt(r.UserID, 10)
		}
		if r.RepoURL != "" {
			env["CURSOR_REPO_URL"] = r.RepoURL
		}
		if r.RepoOwner != "" {
			env["CURSOR_REPO_OWNER"] = r.RepoOwner
		}
		if r.RepoName != "" {
			env["CURSOR_REPO_NAME"] = r.RepoName
		}
		if len(r.RepoURLs) > 0 {
			if b, err := json.Marshal(r.RepoURLs); err == nil {
				env["CURSOR_REPO_URLS"] = string(b)
			}
		}
	}
	if spec.Kind == KindWake {
		env["CURSOR_WAKE"] = "1"
		if spec.WakeTimeout > 0 {
			env["CURSOR_WAKE_TIMEOUT_MS"] = strconv.FormatInt(spec.WakeTimeout.Milliseconds(), 10)
		}
	}
	return env
}
