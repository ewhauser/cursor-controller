// Package metrics exposes Prometheus metrics and health endpoints.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every controller metric.
type Metrics struct {
	reg   *prometheus.Registry
	ready atomic.Bool

	APIRequests      *prometheus.CounterVec
	APILatency       *prometheus.HistogramVec
	Claims           *prometheus.CounterVec
	Spawns           *prometheus.CounterVec
	Wakes            *prometheus.CounterVec
	Releases         *prometheus.CounterVec
	Disposals        *prometheus.CounterVec
	StreamReconnects *prometheus.CounterVec
	ListRuns         *prometheus.CounterVec
	Workers          *prometheus.GaugeVec
	WarmDeficit      *prometheus.GaugeVec
	GCRuns           prometheus.Counter
	GCErrors         prometheus.Counter
}

// New registers all metrics on a fresh registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	ns := "cursor_controller"
	m := &Metrics{
		reg: reg,
		APIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "api_requests_total",
			Help: "Cursor API requests by method, path template, and HTTP status."}, []string{"method", "path", "status"}),
		APILatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "api_request_seconds",
			Help: "Cursor API request latency.", Buckets: prometheus.DefBuckets}, []string{"method", "path"}),
		Claims: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "claims_total",
			Help: "Claim attempts by pool and result (claimed, conflict, gone, error)."}, []string{"pool", "result"}),
		Spawns: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "spawns_total",
			Help: "Worker spawns by pool, kind (claim, wake, warm) and result."}, []string{"pool", "kind", "result"}),
		Wakes: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "wakes_total",
			Help: "Hibernation wake attempts by pool and result (woken, missing, foreign, error)."}, []string{"pool", "result"}),
		Releases: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "releases_total",
			Help: "Claims released back to Cursor by reason."}, []string{"reason"}),
		Disposals: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "disposals_total",
			Help: "Worker workspaces disposed by reason."}, []string{"reason", "result"}),
		StreamReconnects: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "stream_reconnects_total",
			Help: "SSE stream reconnects by pool and cause."}, []string{"pool", "cause"}),
		ListRuns: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "list_runs_total",
			Help: "Pending-request list calls by pool and result."}, []string{"pool", "result"}),
		Workers: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "workers",
			Help: "Workers known to the backend by liveness (live, idle, unknown)."}, []string{"state"}),
		WarmDeficit: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "warm_idle_deficit",
			Help: "Warm-idle target minus observed idle workers, per pool."}, []string{"pool"}),
		GCRuns:   prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "gc_runs_total", Help: "Garbage-collection passes."}),
		GCErrors: prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "gc_errors_total", Help: "Garbage-collection passes that hit an error."}),
	}
	reg.MustRegister(m.APIRequests, m.APILatency, m.Claims, m.Spawns, m.Wakes, m.Releases, m.Disposals,
		m.StreamReconnects, m.ListRuns, m.Workers, m.WarmDeficit, m.GCRuns, m.GCErrors)
	return m
}

// Gatherer exposes the registry so it can be bridged into OpenTelemetry.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// ObserveAPI is a cursorapi.Observer.
func (m *Metrics) ObserveAPI(method, path string, status int, d time.Duration) {
	m.APIRequests.WithLabelValues(method, path, strconv.Itoa(status)).Inc()
	m.APILatency.WithLabelValues(method, path).Observe(d.Seconds())
}

// SetReady flips the readiness probe.
func (m *Metrics) SetReady(v bool) { m.ready.Store(v) }

// Handler serves /metrics, /healthz, and /readyz.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.ready.Load() {
			http.Error(w, "not ready: no successful pending-request list yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Serve runs the management HTTP server until ctx is done.
func (m *Metrics) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
