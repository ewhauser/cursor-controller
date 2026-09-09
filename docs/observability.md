# Observability

[README](../README.md)

## OpenTelemetry

Set `OTEL_EXPORTER_OTLP_ENDPOINT` (or pass `--otel`) and the controller exports
over OTLP:

- **Traces.** One span per claim (`controller.claim_and_spawn`), wake
  (`controller.wake`), release, warm reconcile, GC pass, and disposal, with
  `cursor.request_id`, `cursor.worker_id`, `cursor.pool`, `cursor.spawn_kind`,
  and `cursor.result` attributes. Every outbound call to the Cursor fleet API
  (`cursor-api POST /v0/private-workers/claim`, with ids collapsed to `{id}`)
  and to the Kubernetes API is a child span, so a slow wake shows exactly
  whether the time went into the claim, the PVC, or the pod.
- **Metrics.** The Prometheus registry is bridged and pushed over OTLP, so the
  same `cursor_controller_*` series land in your collector without a scrape.
  `/metrics` keeps working either way.

Flags and their env twins: `--otel` (`CONTROLLER_OTEL`, auto-on when an OTLP
endpoint variable is set), `--otel-traces`, `--otel-metrics`, `--otel-protocol`
(`grpc` default, or `http/protobuf`; also read from `OTEL_EXPORTER_OTLP_PROTOCOL`),
`--otel-service-name`. Headers, TLS, timeouts, export intervals, and resource
attributes follow the standard `OTEL_EXPORTER_OTLP_*`, `OTEL_METRIC_EXPORT_INTERVAL`,
and `OTEL_RESOURCE_ATTRIBUTES` variables.

Chart:

```yaml
otel:
  enabled: true
  endpoint: http://otel-collector.observability.svc:4317
  protocol: grpc
  resourceAttributes: deployment.environment=prod
```

Quick local check: run an OTel collector with the debug exporter, then
`OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 ./hack/local-e2e.sh` and
watch spans and metrics arrive.


## Prometheus

The management server exposes `/metrics`, `/healthz`, and `/readyz` on port 8080
by default. Enable `metrics.serviceMonitor.enabled` when using Prometheus Operator.
See [operations](operations.md) for health endpoint behavior.
