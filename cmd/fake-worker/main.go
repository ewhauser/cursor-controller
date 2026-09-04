// Command fake-worker stands in for `agent worker ... start` in integration
// tests. It accepts the same argument shape, reads the CURSOR_* environment
// the controller injects, registers with the fake fleet API, keeps a marker
// file in its workspace so wakes can prove the volume survived, serves the
// management endpoints, and exits 0 after the idle-release timeout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ewhauser/cursor-controller/internal/fakeapi"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fake-worker failed", "err", err)
		os.Exit(1)
	}
}

type marker struct {
	WorkerID   string   `json:"workerId"`
	RequestIDs []string `json:"requestIds"`
	Boots      int      `json:"boots"`
	CreatedAt  string   `json:"createdAt"`
}

func run(log *slog.Logger) error {
	args := parseArgs(os.Args[1:])
	workerID := os.Getenv("CURSOR_AGENT_WORKER_ID")
	if workerID == "" {
		return errors.New("CURSOR_AGENT_WORKER_ID is required")
	}
	pool := firstNonEmpty(args["pool"], os.Getenv("CURSOR_POOL"), "default")
	workerDir := firstNonEmpty(args["worker-dir"], os.Getenv("CURSOR_WORKSPACE_PATH"), "/workspace")
	mgmt := firstNonEmpty(args["management-addr"], os.Getenv("FAKE_WORKER_MANAGEMENT_ADDR"), "")
	idle := 30 * time.Second
	if v := firstNonEmpty(args["idle-release-timeout"], os.Getenv("CURSOR_WORKER_IDLE_RELEASE_TIMEOUT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("idle-release-timeout: %w", err)
		}
		idle = time.Duration(n) * time.Second
	}
	apiURL := firstNonEmpty(os.Getenv("FAKE_API_URL"), os.Getenv("CURSOR_API_URL"), os.Getenv("CURSOR_API_ENDPOINT"))
	if apiURL == "" {
		return errors.New("CURSOR_API_URL (or FAKE_API_URL) is required")
	}
	wake := os.Getenv("CURSOR_WAKE") == "1"
	requestID := os.Getenv("CURSOR_REQUEST_ID")

	// Workspace marker: proves the same volume came back on wake.
	markerPath := filepath.Join(workerDir, ".fake-worker", "marker.json")
	var m marker
	found := false
	if b, err := os.ReadFile(markerPath); err == nil {
		if json.Unmarshal(b, &m) == nil && m.WorkerID == workerID {
			found = true
		} else {
			log.Warn("marker belongs to another worker or is corrupt", "path", markerPath, "owner", m.WorkerID)
			m = marker{}
		}
	}
	if !found {
		m = marker{WorkerID: workerID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	m.Boots++
	if requestID != "" && !contains(m.RequestIDs, requestID) {
		m.RequestIDs = append(m.RequestIDs, requestID)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return fmt.Errorf("workspace not writable: %w", err)
	}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		if err := os.WriteFile(markerPath, b, 0o644); err != nil {
			return fmt.Errorf("write marker: %w", err)
		}
	}
	seed := ""
	if entries, err := os.ReadDir(workerDir); err == nil {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		seed = strings.Join(names, ",")
	}
	log.Info("fake-worker starting", "worker", workerID, "pool", pool, "dir", workerDir, "wake", wake, "marker_found", found, "boots", m.Boots, "request", requestID, "workspace_entries", seed)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	admin := fakeapi.NewClient(apiURL)
	connected := false
	if mgmt != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			if !connected {
				http.Error(w, "connecting", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Addr: mgmt, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.ListenAndServe() }()
		defer srv.Close()
	}

	// Retry connect for a while: the fake API may still be coming up.
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		err = admin.Connect(ctx, workerID, fakeapi.ConnectOptions{Pool: pool, Name: os.Getenv("CURSOR_WORKER_NAME"), Wake: wake, MarkerFound: found, RequestID: requestID})
		if err == nil {
			break
		}
		log.Warn("connect failed; retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	connected = true
	log.Info("connected; idling", "idle_release_timeout", idle)

	select {
	case <-ctx.Done():
		log.Info("signal received; disconnecting")
	case <-time.After(idle):
		log.Info("idle timeout elapsed; disconnecting")
	}
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.Disconnect(dctx, workerID); err != nil {
		log.Warn("disconnect failed", "err", err)
	}
	return nil
}

// parseArgs pulls --flag value pairs out of an `agent worker ... start` style
// argument list, ignoring bare words (worker, start) and repeated --label.
func parseArgs(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		key, val, hasEq := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !hasEq && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") && args[i+1] != "start" {
			val = args[i+1]
			i++
		}
		out[key] = val
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
