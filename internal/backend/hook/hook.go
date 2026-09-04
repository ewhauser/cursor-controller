// Package hook is the script backend: it execs a spawn script (same contract
// as `agent worker controller --spawn`) and an optional dispose script, and
// tracks workers in a small JSON state file.
package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ewhauser/cursor-controller/internal/backend"
)

// ExitWorkspaceMissing is the spawn-script exit code that tells the controller
// the retained workspace for a wake is gone (EX_NOINPUT). The controller then
// releases the claim so Cursor re-queues the request.
const ExitWorkspaceMissing = 66

// Options configures the backend.
type Options struct {
	SpawnCommand   string
	DisposeCommand string
	// StateFile persists worker records across restarts. Empty keeps them in
	// memory only (disposal then works only for the current process life).
	StateFile      string
	WorkerIDPrefix string
	Timeout        time.Duration
	APIURL         string
	APIKey         string
	Log            *slog.Logger
	Now            func() time.Time
}

type record struct {
	ID           string    `json:"id"`
	Pool         string    `json:"pool"`
	RequestID    string    `json:"requestId,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	LastActivity time.Time `json:"lastActivity"`
}

// Backend implements backend.Backend with shell hooks.
type Backend struct {
	o     Options
	mu    sync.Mutex
	state map[string]*record
}

// New validates options and loads the state file.
func New(o Options) (*Backend, error) {
	if o.SpawnCommand == "" {
		return nil, errors.New("hook: spawn command is required")
	}
	if err := backend.ValidatePrefix(o.WorkerIDPrefix); err != nil {
		return nil, err
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	b := &Backend{o: o, state: map[string]*record{}}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Backend) Name() string { return "hook" }

func (b *Backend) Owns(workerID string) bool { return backend.HasPrefix(workerID, b.o.WorkerIDPrefix) }

func (b *Backend) Spawn(ctx context.Context, spec backend.Spec) error {
	if spec.Kind == "" {
		spec.Kind = backend.KindClaim
	}
	if err := b.run(ctx, b.o.SpawnCommand, spec.WorkerID, b.env(spec)); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.o.Now()
	b.state[spec.WorkerID] = &record{ID: spec.WorkerID, Pool: spec.Pool, RequestID: spec.RequestID(), CreatedAt: now, LastActivity: now}
	return b.saveLocked()
}

func (b *Backend) Wake(ctx context.Context, spec backend.Spec) error {
	spec.Kind = backend.KindWake
	b.mu.Lock()
	rec, ok := b.state[spec.WorkerID]
	b.mu.Unlock()
	if !ok {
		return backend.ErrUnknownWorker
	}
	if err := b.run(ctx, b.o.SpawnCommand, spec.WorkerID, b.env(spec)); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == ExitWorkspaceMissing {
			return fmt.Errorf("spawn hook exit %d: %w", ExitWorkspaceMissing, backend.ErrWorkspaceMissing)
		}
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec.LastActivity = b.o.Now()
	if r := spec.RequestID(); r != "" {
		rec.RequestID = r
	}
	return b.saveLocked()
}

func (b *Backend) RecordClaim(_ context.Context, workerID, requestID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.state[workerID]
	if !ok {
		return nil
	}
	rec.RequestID = requestID
	rec.LastActivity = b.o.Now()
	return b.saveLocked()
}

func (b *Backend) ListWorkers(context.Context) ([]backend.WorkerInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]backend.WorkerInfo, 0, len(b.state))
	for _, r := range b.state {
		out = append(out, backend.WorkerInfo{ID: r.ID, Pool: r.Pool, RequestID: r.RequestID, LastActivity: r.LastActivity, CreatedAt: r.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (b *Backend) Dispose(ctx context.Context, workerID, reason string) error {
	b.mu.Lock()
	rec, ok := b.state[workerID]
	b.mu.Unlock()
	if b.o.DisposeCommand != "" {
		env := map[string]string{
			"CURSOR_AGENT_WORKER_ID": workerID,
			"CURSOR_WORKER_NAME":     workerID,
			"CURSOR_DISPOSE_REASON":  reason,
		}
		if ok {
			env["CURSOR_POOL"] = rec.Pool
			if rec.RequestID != "" {
				env["CURSOR_REQUEST_ID"] = rec.RequestID
				env["CURSOR_BC_ID"] = rec.RequestID
			}
		}
		b.addCommon(env)
		if err := b.run(ctx, b.o.DisposeCommand, workerID, env); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, workerID)
	return b.saveLocked()
}

func (b *Backend) env(spec backend.Spec) map[string]string {
	if spec.APIURL == "" {
		spec.APIURL = b.o.APIURL
	}
	env := backend.Env(spec)
	b.addCommon(env)
	return env
}

func (b *Backend) addCommon(env map[string]string) {
	if b.o.APIKey != "" {
		env["CURSOR_API_KEY"] = b.o.APIKey
	}
	if b.o.APIURL != "" {
		if _, ok := env["CURSOR_API_URL"]; !ok {
			env["CURSOR_API_URL"] = b.o.APIURL
			env["CURSOR_API_ENDPOINT"] = b.o.APIURL
		}
	}
}

func (b *Backend) run(ctx context.Context, command, workerID string, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, b.o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = nil
	log := b.o.Log.With("hook", filepath.Base(command), "worker", workerID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", command, err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pipeLines(stdout, log, "stdout") }()
	go func() { defer wg.Done(); pipeLines(stderr, log, "stderr") }()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s timed out after %s", command, b.o.Timeout)
		}
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

func pipeLines(r io.Reader, log *slog.Logger, stream string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		log.Info(sc.Text(), "stream", stream)
	}
}

func (b *Backend) load() error {
	if b.o.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(b.o.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}
	var recs []*record
	if err := json.Unmarshal(data, &recs); err != nil {
		return fmt.Errorf("parse state file %s: %w", b.o.StateFile, err)
	}
	for _, r := range recs {
		b.state[r.ID] = r
	}
	return nil
}

func (b *Backend) saveLocked() error {
	if b.o.StateFile == "" {
		return nil
	}
	recs := make([]*record, 0, len(b.state))
	for _, r := range b.state {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.o.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return os.Rename(tmp, b.o.StateFile)
}
