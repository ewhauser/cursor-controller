// Package telemetry wires optional OpenTelemetry export: OTLP traces from the
// controller's spans and OTLP metrics bridged from the existing Prometheus
// registry, so nothing is instrumented twice. Endpoint, headers, TLS, and
// export intervals come from the standard OTEL_EXPORTER_OTLP_* environment
// variables read by the SDK exporters.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config controls what is exported.
type Config struct {
	ServiceName    string
	ServiceVersion string
	// Protocol is grpc or http/protobuf. Empty reads
	// OTEL_EXPORTER_OTLP_PROTOCOL and falls back to grpc.
	Protocol string
	Traces   bool
	Metrics  bool
	// Gatherer is bridged into OTLP metrics (usually the Prometheus registry).
	Gatherer prometheus.Gatherer
	Log      *slog.Logger
}

// Shutdown flushes and stops the providers.
type Shutdown func(context.Context) error

// Exporter constructors, swappable in tests.
var (
	newSpanExporter   = defaultSpanExporter
	newMetricExporter = defaultMetricExporter
)

// Enabled reports whether an OTLP endpoint is configured in the environment,
// which is the auto-on signal when no explicit flag is given.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// Setup installs global tracer and meter providers and returns a shutdown.
func Setup(ctx context.Context, cfg Config) (Shutdown, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "cursor-controller"
	}
	protocol := resolveProtocol(cfg.Protocol)
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(cfg.ServiceName), semconv.ServiceVersion(cfg.ServiceVersion)),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { cfg.Log.Warn("otel export error", "err", err) }))

	var shutdowns []Shutdown
	if cfg.Traces {
		exp, err := newSpanExporter(ctx, protocol)
		if err != nil {
			return nil, fmt.Errorf("otel trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}
	if cfg.Metrics {
		exp, err := newMetricExporter(ctx, protocol)
		if err != nil {
			return nil, fmt.Errorf("otel metric exporter: %w", err)
		}
		opts := []sdkmetric.PeriodicReaderOption{}
		if cfg.Gatherer != nil {
			opts = append(opts, sdkmetric.WithProducer(prombridge.NewMetricProducer(prombridge.WithGatherer(cfg.Gatherer))))
		}
		reader := sdkmetric.NewPeriodicReader(exp, opts...)
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
	}
	cfg.Log.Info("opentelemetry export enabled", "protocol", protocol, "traces", cfg.Traces, "metrics", cfg.Metrics,
		"endpoint", firstEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"))
	return func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			if err := shutdowns[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}

func resolveProtocol(p string) string {
	if p == "" {
		p = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", "grpc":
		return "grpc"
	case "http", "http/protobuf", "http-protobuf", "httpprotobuf":
		return "http/protobuf"
	default:
		return "grpc"
	}
}

func defaultSpanExporter(ctx context.Context, protocol string) (sdktrace.SpanExporter, error) {
	if protocol == "http/protobuf" {
		return otlptracehttp.New(ctx)
	}
	return otlptracegrpc.New(ctx)
}

func defaultMetricExporter(ctx context.Context, protocol string) (sdkmetric.Exporter, error) {
	if protocol == "http/protobuf" {
		return otlpmetrichttp.New(ctx)
	}
	return otlpmetricgrpc.New(ctx)
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
