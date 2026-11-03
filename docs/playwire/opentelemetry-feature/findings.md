# OpenTelemetry Backend Findings

## Implementation

- The backend lives in [`metrics/opentelemetry`](../../../metrics/opentelemetry/opentelemetry.go) and implements the shared `metrics.MetricsEngine` interface. Auction instrumentation remains unchanged.
- [`metrics/config`](../../../metrics/config/metrics.go) adds OTel to the existing backend fan-out. The Prometheus port remains optional, so OTLP-only operation does not start a Prometheus listener.
- The backend owns an SDK `MeterProvider` and a periodic OTLP/HTTP protobuf exporter. Recording does not do network I/O; shutdown is bounded and attempts a final export.
- Metric names preserve the Prometheus namespace/subsystem prefix and metric names. Durations are measured in seconds; bid price remains unscaled CPM. Successful-request duration and adapter-time conditions follow the Prometheus implementation.
- Account attributes have a per-process limit and excess IDs map to `account="overflow"`. Existing disabled account/adapter settings are honored. Runtime metrics use the official OTel runtime instrumentation.
- OTel configuration is under `metrics.opentelemetry`; it is disabled by default and validates endpoint, protocol, interval, timeout, account limit, and temporality when enabled. Configuration and explicit resource attributes take precedence over standard OTel environment values for the settings they supply.
- Prometheus preloads label combinations at zero, while OTel emits only after a measurement. Dashboards must not assume unobserved label combinations exist as zero-valued series.
- Fixed instruments are created lazily and cached, avoiding duplicate metric-name catalogs. Module instruments are created at startup without recording measurements; a regression test verifies there are no phantom calls, failures, or timeouts.

## Lifecycle

- `NewMetricsEngine` returns an error so exporter/provider initialization failures can fail startup.
- `Router.Shutdown` shuts down the OTel provider with a timeout after request-serving shutdown. `serve` defers router cleanup and returns listener errors instead of calling `Fatalf`, preserving the cleanup path.
- Router initialization errors after creating the metrics engine also trigger provider shutdown.
- Exporter connectivity is asynchronous; an unavailable collector does not prevent request handling. Configuration/SDK initialization errors remain startup errors.

## Validation Findings

- `./validate.sh` runs formatting, every Go package test, and `go vet ./...`.
- Adding OTel v1.39.0 raises the selected Testify version to v1.11.1 through its module requirements. That version rejects `assert.NotSame` when arguments are not pointers. Existing tests had used it for values, maps, and slices, and had passed as no-op checks with the older version.
- Existing pointer assertions remain unchanged. Non-empty slice comparisons use backing-element pointers; map independence is verified through mutation. Meaningless scalar, struct-value, and empty-slice identity assertions were removed, along with the three compatibility helpers.
- The affected package tests pass, and the complete `./validate.sh` passes with `go vet` enabled.

## Relevant References

- [Implementation plan](../opentelemetry.md)
- [Metrics configuration and deployment guidance](../../developers/metrics-configuration.md)
- [OTel manual-reader and OTLP fake-collector tests](../../../metrics/opentelemetry/opentelemetry_test.go)
