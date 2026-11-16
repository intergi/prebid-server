# OpenTelemetry Metrics Backend Plan

## Goal

Add an OpenTelemetry metrics backend that uses the OpenTelemetry Go SDK to send
Prebid Server metrics to an OpenTelemetry Collector over OTLP. Reuse existing
metric events without changing auction instrumentation, and allow Prometheus and
OpenTelemetry to run together.

This is a metrics backend, not a bidder adapter under `adapters/`.

## Existing Integration Points

- [MetricsEngine](../../metrics/metrics.go) defines the shared metric recording interface.
- [Metrics configuration](../../metrics/config/metrics.go) selects backends and fans events out through `MultiMetricsEngine`.
- [Prometheus implementation](../../metrics/prometheus/prometheus.go) provides the reference for metric semantics, labels, recording conditions, and histogram boundaries.
- [Application configuration](../../config/config.go) defines metrics settings, defaults, and validation.
- [Router](../../router/router.go) creates the metrics engine in `router.New` and releases resources in `Router.Shutdown`.
- [Server lifecycle](../../server/server.go) and [application startup](../../main.go) are integration points for cleanup after requests drain and on startup failure.

The backend should consume the same events as Prometheus rather than scrape or
translate the Prometheus registry. The current metrics interface has no shutdown
hook, so SDK lifecycle management needs explicit integration.

## Implementation Plan

### 1. Define the Metric Mapping

- Inventory Prometheus metrics, labels, recording conditions, units, and histogram boundaries.
- Map counters to OTel counters and distributions to OTel histograms.
- Preserve existing semantics, including conditional recording. For example, Prometheus records request duration only for successful requests.
- Keep Prometheus metric names, including the configured namespace and subsystem prefix, so existing dashboards and alerts continue to work. Document any differences introduced by the collector's Prometheus translation.
- Record durations in seconds, matching Prometheus.
- Record bid prices as unscaled CPM, matching `RecordAdapterPrice`. Choose price histogram boundaries deliberately; the current Prometheus price buckets (250 to 4000) do not fit typical CPM values and should not be copied without review.
- Prometheus preloads label combinations at zero; the OTel SDK reports a series only after its first measurement. Accept and document this difference, and identify dashboards or alerts that depend on zero-valued series.

### 2. Add Configuration

Extend application configuration with a `metrics.opentelemetry` section containing:

- Enablement, defaulting to disabled.
- Collector endpoint and protocol.
- Export interval and timeout. Name the interval consistently with the existing InfluxDB `metric_send_interval` setting.
- TLS settings and authentication headers.
- Resource attributes, including service name, version, and instance identity.
- Cardinality controls for account-level attributes.

Start with OTLP/HTTP protobuf; add gRPC only if required. Define configuration
precedence relative to standard `OTEL_*` environment variables. Validate enabled
configurations and ensure credentials are not exposed in logs.

### 3. Implement the Backend

- Add a package under `metrics/opentelemetry` implementing the complete `MetricsEngine` interface.
- Use the official OTel Go metrics SDK, OTLP exporter, `MeterProvider`, and `PeriodicReader`.
- Select dependency versions compatible with both the `go.mod` Go version and the deployment toolchain; `go.mod` also contains a Heroku Go version comment that may differ.
- Create instruments once and keep network export off request paths.
- Use an explicitly owned provider rather than replacing the global provider.
- Attach service identity through resource attributes rather than repeating it on every measurement.
- Include a compile-time interface assertion to detect missing recording methods.
- Create per-module instruments at startup from the module stage names, following `createModulesMetrics` in the Prometheus backend.

### 4. Control Parity and Cardinality

- Honor existing `DisabledMetrics` settings.
- Set explicit histogram boundaries.
- Default to cumulative temporality, matching Prometheus; offer delta only as a configuration option.
- Account metrics are recorded for every known publisher ID, and some, such as account requests, are not controlled by `DisabledMetrics`. Bound account attributes with a configurable limit or allowlist, and fold excess values into an overflow attribute value.
- Avoid introducing request IDs, user IDs, or other unbounded attributes.
- Prometheus registers a Go collector outside the shared interface, so event reuse does not cover runtime metrics. Use the official OTel Go contrib `runtime` instrumentation for equivalent coverage.

### 5. Wire Initialization and Shutdown

- Register the backend in the existing metrics factory and fan-out.
- Support OTel-only operation without enabling the Prometheus HTTP listener.
- Change `NewMetricsEngine` to return `(*DetailedMetricsEngine, error)` and update `router.New` and existing tests to propagate initialization errors.
- Add a `DetailedMetricsEngine` field that holds the OTel provider so it can be shut down.
- Shut the provider down with a bounded timeout from `Router.Shutdown`, which runs after HTTP requests drain.
- Clean up the provider when `router.New` fails after creating the metrics engine.
- `serve` in `main.go` calls `logger.Fatalf` when `server.Listen` fails, which exits before cleanup. Return the error instead so shutdown and final export run.
- Keep lifecycle management outside the recording interface unless extending that interface is necessary.
- Log export failures without disrupting auctions or exposing credentials.
- Ensure an unavailable collector does not prevent the server from serving requests; distinguish configuration or SDK initialization failures from transient export failures.

### 6. Test and Document

- Use an SDK manual reader for deterministic counter, attribute, histogram, and disabled-metric tests.
- Verify recording conditions, unit conversions, and histogram boundaries against Prometheus behavior.
- Test OTel-only, Prometheus-only, combined, and disabled configurations.
- Use a local fake OTLP endpoint to verify payloads, unavailable-collector behavior, and final export.
- Test configuration validation, startup cleanup, and bounded shutdown.
- Check concurrent recording and measure overhead on representative hot paths.
- Add a collector configuration example and deployment guidance covering TLS, authentication, environment-variable precedence, cardinality, and metric naming.

## Acceptance Criteria

- Existing request instrumentation feeds both backends without changes to auction call sites.
- Equivalent events produce matching counts and histogram observations after documented unit conversions.
- Collector outages do not block auction processing.
- Shutdown attempts a final export within a fixed timeout.
- Existing Prometheus behavior remains unchanged.
- Disabled metric settings are respected, and high-cardinality attributes are bounded.
- Runtime metric coverage and any deliberate differences from Prometheus, including absent zero-valued series, are documented.
- Invalid OTel configuration fails startup; an unreachable collector only produces logged export errors.

## Alternative: Collector Scrapes Prometheus

If the in-process SDK requirement is negotiable, configure an OpenTelemetry
Collector to scrape the existing Prometheus endpoint and export those metrics.
This is the lowest-change approach and avoids adding an SDK lifecycle to Prebid
Server. A native backend is appropriate when Prebid Server itself must push OTLP
through the OpenTelemetry SDK.