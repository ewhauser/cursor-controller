// Command cursor-controller claims Cursor self-hosted pool requests, runs one
// worker per claim on Kubernetes (or via hook scripts), wakes hibernated
// workers on their retained workspace, and garbage-collects workspaces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ewhauser/cursor-controller/internal/backend"
	"github.com/ewhauser/cursor-controller/internal/backend/hook"
	"github.com/ewhauser/cursor-controller/internal/backend/kube"
	"github.com/ewhauser/cursor-controller/internal/controller"
	"github.com/ewhauser/cursor-controller/internal/cursorapi"
	"github.com/ewhauser/cursor-controller/internal/metrics"
	"github.com/ewhauser/cursor-controller/internal/telemetry"
)

var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cursor-controller:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("cursor-controller", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "cursor-controller %s\n\nEvery flag can also be set with the environment variable named next to it.\n\n", version)
		fs.PrintDefaults()
	}

	var pools multiFlag
	if v := os.Getenv("CURSOR_POOL"); v != "" {
		_ = pools.Set(v)
	}
	var (
		apiURL           = fs.String("api-url", envOr("CURSOR_API_URL", envOr("CURSOR_API_ENDPOINT", cursorapi.DefaultBaseURL)), "Cursor fleet API base (CURSOR_API_URL)")
		apiTimeout       = fs.Duration("api-timeout", envDuration("CONTROLLER_API_TIMEOUT", 30*time.Second), "Timeout for ordinary Cursor API requests; SSE is excluded (CONTROLLER_API_TIMEOUT)")
		apiKey           = fs.String("api-key", os.Getenv("CURSOR_API_KEY"), "Team service-account API key (CURSOR_API_KEY)")
		apiKeyFile       = fs.String("api-key-file", os.Getenv("CURSOR_API_KEY_FILE"), "Read the API key from this file (CURSOR_API_KEY_FILE)")
		repository       = fs.String("repository", os.Getenv("CURSOR_REPOSITORY"), "Repository filter for repo-scoped keys (CURSOR_REPOSITORY)")
		backendKind      = fs.String("backend", envOr("CONTROLLER_BACKEND", "kube"), "Worker backend: kube or hook (CONTROLLER_BACKEND)")
		prefix           = fs.String("worker-id-prefix", envOr("CONTROLLER_WORKER_ID_PREFIX", "cc"), "Prefix for minted worker ids; also marks ownership (CONTROLLER_WORKER_ID_PREFIX)")
		warmIdle         = fs.Int("warm-idle", envInt("CONTROLLER_WARM_IDLE", 0), "Keep this many idle workers connected per pool; 0 = claim-then-spawn only (CONTROLLER_WARM_IDLE)")
		warmInterval     = fs.Duration("warm-interval", envDuration("CONTROLLER_WARM_INTERVAL", time.Minute), "Warm-idle reconcile period (CONTROLLER_WARM_INTERVAL)")
		resync           = fs.Duration("resync-interval", envDuration("CONTROLLER_RESYNC_INTERVAL", 4*time.Minute), "Re-list pending requests at least this often (CONTROLLER_RESYNC_INTERVAL)")
		reconnect        = fs.Duration("reconnect-delay", envDuration("CONTROLLER_RECONNECT_DELAY", 5*time.Second), "Pause after a stream error (CONTROLLER_RECONNECT_DELAY)")
		wakeRetry        = fs.Duration("wake-retry-interval", envDuration("CONTROLLER_WAKE_RETRY_INTERVAL", 2*time.Second), "Pause between transient wake retries (CONTROLLER_WAKE_RETRY_INTERVAL)")
		gcInterval       = fs.Duration("gc-interval", envDuration("CONTROLLER_GC_INTERVAL", 5*time.Minute), "Workspace garbage-collection period; 0 disables (CONTROLLER_GC_INTERVAL)")
		disposeAfter     = fs.Duration("dispose-after", envDuration("CONTROLLER_DISPOSE_AFTER", 7*24*time.Hour), "Hard TTL: dispose offline workers idle this long; 0 disables (CONTROLLER_DISPOSE_AFTER)")
		disposeArchived  = fs.Duration("dispose-archived-after", envDuration("CONTROLLER_DISPOSE_ARCHIVED_AFTER", time.Hour), "Dispose offline workers whose agent is archived or deleted once idle this long (CONTROLLER_DISPOSE_ARCHIVED_AFTER)")
		disposeUnclaimed = fs.Duration("dispose-unclaimed-after", envDuration("CONTROLLER_DISPOSE_UNCLAIMED_AFTER", time.Hour), "Dispose offline workers that never got a request after this long; 0 disables (CONTROLLER_DISPOSE_UNCLAIMED_AFTER)")
		checkAgent       = fs.Bool("dispose-check-agent", envBool("CONTROLLER_DISPOSE_CHECK_AGENT", true), "Query /v1/agents/{id} during GC to dispose archived agents early (CONTROLLER_DISPOSE_CHECK_AGENT)")
		exitOnAuth       = fs.Bool("exit-on-auth-error", envBool("CONTROLLER_EXIT_ON_AUTH_ERROR", true), "Exit when the API rejects the key (CONTROLLER_EXIT_ON_AUTH_ERROR)")
		metricsAddr      = fs.String("metrics-addr", envOr("CONTROLLER_METRICS_ADDR", ":8080"), "Listen address for /metrics, /healthz, /readyz; empty disables (CONTROLLER_METRICS_ADDR)")
		logLevel         = fs.String("log-level", envOr("CONTROLLER_LOG_LEVEL", "info"), "debug, info, warn, error (CONTROLLER_LOG_LEVEL)")
		logFormat        = fs.String("log-format", envOr("CONTROLLER_LOG_FORMAT", "json"), "json or text (CONTROLLER_LOG_FORMAT)")
		showVersion      = fs.Bool("version", false, "Print version and exit")

		// opentelemetry
		otelEnabled  = fs.Bool("otel", envBool("CONTROLLER_OTEL", telemetry.Enabled()), "Export traces/metrics over OTLP; defaults to on when OTEL_EXPORTER_OTLP_ENDPOINT is set (CONTROLLER_OTEL)")
		otelTraces   = fs.Bool("otel-traces", envBool("CONTROLLER_OTEL_TRACES", true), "Export traces when --otel is on (CONTROLLER_OTEL_TRACES)")
		otelMetrics  = fs.Bool("otel-metrics", envBool("CONTROLLER_OTEL_METRICS", true), "Export the Prometheus metrics over OTLP when --otel is on (CONTROLLER_OTEL_METRICS)")
		otelProtocol = fs.String("otel-protocol", envOr("CONTROLLER_OTEL_PROTOCOL", ""), "OTLP protocol: grpc or http/protobuf; empty reads OTEL_EXPORTER_OTLP_PROTOCOL, default grpc (CONTROLLER_OTEL_PROTOCOL)")
		otelService  = fs.String("otel-service-name", envOr("OTEL_SERVICE_NAME", "cursor-controller"), "service.name resource attribute (OTEL_SERVICE_NAME)")

		// kube backend
		kubeconfig       = fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig; empty uses in-cluster config (KUBECONFIG)")
		namespace        = fs.String("namespace", envOr("CONTROLLER_NAMESPACE", envOr("POD_NAMESPACE", "")), "Namespace for worker pods and PVCs (CONTROLLER_NAMESPACE)")
		podTemplate      = fs.String("pod-template", envOr("CONTROLLER_POD_TEMPLATE", ""), "Path to the worker Pod manifest (CONTROLLER_POD_TEMPLATE)")
		snapshotSelector = fs.String("workspace-snapshot-selector", os.Getenv("CONTROLLER_WORKSPACE_SNAPSHOT_SELECTOR"), "Label selector for the newest ready namespace-local seed snapshot (CONTROLLER_WORKSPACE_SNAPSHOT_SELECTOR)")
		pvcTemplate      = fs.String("pvc-template", envOr("CONTROLLER_PVC_TEMPLATE", ""), "Path to a PVC manifest; enables retained per-worker workspaces (CONTROLLER_PVC_TEMPLATE)")
		mountPath        = fs.String("workspace-mount-path", envOr("CONTROLLER_WORKSPACE_MOUNT_PATH", "/workspace"), "Where the workspace PVC is mounted in the worker container (CONTROLLER_WORKSPACE_MOUNT_PATH)")
		workerContainer  = fs.String("worker-container", envOr("CONTROLLER_WORKER_CONTAINER", ""), "Container in the pod template that receives env and the workspace mount; default first (CONTROLLER_WORKER_CONTAINER)")
		apiKeySecret     = fs.String("worker-api-key-secret", envOr("CONTROLLER_WORKER_API_KEY_SECRET", ""), "Secret name injected as CURSOR_API_KEY into worker pods (CONTROLLER_WORKER_API_KEY_SECRET)")
		apiKeySecretKey  = fs.String("worker-api-key-secret-key", envOr("CONTROLLER_WORKER_API_KEY_SECRET_KEY", "api-key"), "Key inside that Secret (CONTROLLER_WORKER_API_KEY_SECRET_KEY)")
		workerStartup    = fs.Duration("worker-startup-timeout", envDuration("CONTROLLER_WORKER_STARTUP_TIMEOUT", 10*time.Minute), "Dispose non-ready worker pods after this startup deadline (CONTROLLER_WORKER_STARTUP_TIMEOUT)")
		workerEndpoint   = fs.String("worker-api-endpoint", os.Getenv("CONTROLLER_WORKER_API_ENDPOINT"), "Override the worker agent backend endpoint; empty uses the agent default or Pod template (CONTROLLER_WORKER_API_ENDPOINT)")

		// hook backend
		spawnCmd    = fs.String("spawn", os.Getenv("CONTROLLER_SPAWN"), "hook backend: script run per spawn/wake (CONTROLLER_SPAWN)")
		disposeCmd  = fs.String("dispose", os.Getenv("CONTROLLER_DISPOSE"), "hook backend: script run per disposal (CONTROLLER_DISPOSE)")
		stateFile   = fs.String("state-file", os.Getenv("CONTROLLER_STATE_FILE"), "hook backend: JSON file tracking workers across restarts (CONTROLLER_STATE_FILE)")
		hookTimeout = fs.Duration("hook-timeout", envDuration("CONTROLLER_HOOK_TIMEOUT", 5*time.Minute), "hook backend: per-script timeout (CONTROLLER_HOOK_TIMEOUT)")
	)
	fs.Var(&pools, "pool", "Pool to serve; repeatable or comma-separated. Empty watches all pools (CURSOR_POOL)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	key := strings.TrimSpace(*apiKey)
	if *apiKeyFile != "" {
		b, err := os.ReadFile(*apiKeyFile)
		if err != nil {
			return fmt.Errorf("read api key file: %w", err)
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		return errors.New("--api-key (CURSOR_API_KEY) is required: a team service-account key with agent scope")
	}
	if err := backend.ValidatePrefix(*prefix); err != nil {
		return err
	}
	if *warmIdle > 0 && len(pools) == 0 {
		return errors.New("--warm-idle requires at least one --pool")
	}

	m := metrics.New()
	var otelShutdown telemetry.Shutdown
	if *otelEnabled {
		var err error
		otelShutdown, err = telemetry.Setup(context.Background(), telemetry.Config{
			ServiceName: *otelService, ServiceVersion: version, Protocol: *otelProtocol,
			Traces: *otelTraces, Metrics: *otelMetrics, Gatherer: m.Gatherer(), Log: log,
		})
		if err != nil {
			return err
		}
	}
	httpClient := &http.Client{Transport: http.DefaultTransport}
	if *otelEnabled && *otelTraces {
		httpClient.Transport = otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return "cursor-api " + r.Method + " " + cursorapi.RouteTemplate(r.URL.Path)
		}))
	}
	api := cursorapi.New(*apiURL, key, cursorapi.WithObserver(m.ObserveAPI), cursorapi.WithUserAgent("cursor-controller/"+version), cursorapi.WithHTTPClient(httpClient), cursorapi.WithRequestTimeout(*apiTimeout))

	var be backend.Backend
	switch *backendKind {
	case "kube":
		if *podTemplate == "" {
			return errors.New("--pod-template is required for the kube backend")
		}
		pod, err := kube.LoadPodTemplate(*podTemplate)
		if err != nil {
			return err
		}
		opts := kube.Options{
			Namespace:         *namespace,
			WorkerIDPrefix:    *prefix,
			PodTemplate:       pod,
			MountPath:         *mountPath,
			SnapshotSelector:  *snapshotSelector,
			WorkerContainer:   *workerContainer,
			APIKeySecretName:  *apiKeySecret,
			APIKeySecretKey:   *apiKeySecretKey,
			WorkerAPIEndpoint: *workerEndpoint,
			StartupTimeout:    *workerStartup,
			Log:               log.With("backend", "kube"),
		}
		if *pvcTemplate != "" {
			pvc, err := kube.LoadPVCTemplate(*pvcTemplate)
			if err != nil {
				return err
			}
			opts.PVCTemplate = pvc
		}
		client, snapshotClient, ns, err := kubeClient(*kubeconfig, *otelEnabled && *otelTraces)
		if err != nil {
			return err
		}
		if opts.Namespace == "" {
			opts.Namespace = ns
		}
		if opts.Namespace == "" {
			return errors.New("--namespace is required (could not infer from kubeconfig or service account)")
		}
		opts.Client = client
		opts.SnapshotClient = snapshotClient
		kb, err := kube.New(opts)
		if err != nil {
			return err
		}
		be = kb
		log.Info("kube backend ready", "namespace", opts.Namespace, "persistent", kb.Persistent(), "mount", opts.MountPath)
	case "hook":
		hb, err := hook.New(hook.Options{
			SpawnCommand:   *spawnCmd,
			DisposeCommand: *disposeCmd,
			StateFile:      *stateFile,
			WorkerIDPrefix: *prefix,
			Timeout:        *hookTimeout,
			APIURL:         *apiURL,
			APIKey:         key,
			Log:            log.With("backend", "hook"),
		})
		if err != nil {
			return err
		}
		be = hb
		log.Info("hook backend ready", "spawn", *spawnCmd, "dispose", *disposeCmd, "state", *stateFile)
	default:
		return fmt.Errorf("unknown backend %q (kube or hook)", *backendKind)
	}

	cfg := controller.Config{
		Pools:                 pools,
		Repository:            *repository,
		WorkerIDPrefix:        *prefix,
		WarmIdle:              *warmIdle,
		WarmInterval:          *warmInterval,
		ResyncInterval:        *resync,
		ReconnectDelay:        *reconnect,
		WakeRetryInterval:     *wakeRetry,
		GCInterval:            *gcInterval,
		DisposeAfter:          *disposeAfter,
		DisposeArchivedAfter:  *disposeArchived,
		DisposeUnclaimedAfter: *disposeUnclaimed,
		CheckAgent:            *checkAgent,
		ExitOnAuthError:       *exitOnAuth,
		APIURL:                *apiURL,
	}
	ctrl := controller.New(cfg, api, be, log, m)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("starting cursor-controller", "version", version, "api", *apiURL, "pools", pools, "backend", be.Name(),
		"warm_idle", *warmIdle, "gc_interval", *gcInterval, "dispose_after", *disposeAfter, "dispose_archived_after", *disposeArchived)

	g, gctx := errgroup.WithContext(ctx)
	if *metricsAddr != "" {
		g.Go(func() error { return m.Serve(gctx, *metricsAddr) })
	}
	g.Go(func() error { return ctrl.Run(gctx) })
	err = g.Wait()
	if otelShutdown != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if serr := otelShutdown(sctx); serr != nil {
			log.Warn("opentelemetry shutdown", "err", serr)
		}
		cancel()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("cursor-controller stopped")
	return nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q", format)
}

// kubeClient builds a clientset from kubeconfig or in-cluster config and
// returns the default namespace it implies.
func kubeClient(kubeconfig string, traced bool) (kubernetes.Interface, dynamic.Interface, string, error) {
	wrap := func(cfg *rest.Config) *rest.Config {
		cfg = rest.CopyConfig(cfg)
		if traced {
			cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
				return otelhttp.NewTransport(rt, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return "kube-api " + r.Method
				}))
			})
		}
		return cfg
	}
	if kubeconfig == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			cs, err := kubernetes.NewForConfig(wrap(cfg))
			if err != nil {
				return nil, nil, "", err
			}
			ns := ""
			if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
				ns = strings.TrimSpace(string(b))
			}
			dc, err := dynamic.NewForConfig(wrap(cfg))
			return cs, dc, ns, err
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, nil, "", fmt.Errorf("kubernetes config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(wrap(cfg))
	if err != nil {
		return nil, nil, "", err
	}
	ns, _, _ := cc.Namespace()
	dc, err := dynamic.NewForConfig(wrap(cfg))
	return cs, dc, ns, err
}
