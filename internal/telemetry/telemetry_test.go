package telemetry

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// memSpans keeps exported spans across Shutdown (tracetest.InMemoryExporter
// resets on Shutdown, which the provider calls before we can read them).
type memSpans struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (m *memSpans) ExportSpans(_ context.Context, ss []sdktrace.ReadOnlySpan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, ss...)
	return nil
}
func (m *memSpans) Shutdown(context.Context) error { return nil }

type memMetrics struct {
	mu   sync.Mutex
	seen []string
}

func (m *memMetrics) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}
func (m *memMetrics) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}
func (m *memMetrics) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			m.seen = append(m.seen, mt.Name)
		}
	}
	return nil
}
func (m *memMetrics) ForceFlush(context.Context) error { return nil }
func (m *memMetrics) Shutdown(context.Context) error   { return nil }

func TestSetupBridgesPrometheusAndRecordsSpans(t *testing.T) {
	spans := &memSpans{}
	mem := &memMetrics{}
	newSpanExporter = func(context.Context, string) (sdktrace.SpanExporter, error) { return spans, nil }
	newMetricExporter = func(context.Context, string) (sdkmetric.Exporter, error) { return mem, nil }
	t.Cleanup(func() { newSpanExporter, newMetricExporter = defaultSpanExporter, defaultMetricExporter })

	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cursor_controller_claims_total", Help: "x"}, []string{"pool"})
	reg.MustRegister(c)
	c.WithLabelValues("gpu").Inc()

	ctx := context.Background()
	shutdown, err := Setup(ctx, Config{ServiceName: "t", Protocol: "http", Traces: true, Metrics: true, Gatherer: reg})
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(ctx, "controller.test")
	span.End()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := spans.spans; len(got) != 1 || got[0].Name() != "controller.test" {
		t.Fatalf("spans = %+v", got)
	}
	found := false
	for _, n := range mem.seen {
		if strings.HasPrefix(n, "cursor_controller_claims") {
			found = true
		}
	}
	if !found {
		t.Fatalf("bridged prometheus metric missing; exported names: %v", mem.seen)
	}
	_ = time.Second
}

func TestResolveProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	if p := resolveProtocol(""); p != "http/protobuf" {
		t.Fatalf("env protocol = %q", p)
	}
	if p := resolveProtocol("grpc"); p != "grpc" {
		t.Fatalf("explicit = %q", p)
	}
	if p := resolveProtocol("bogus"); p != "grpc" {
		t.Fatalf("fallback = %q", p)
	}
}
