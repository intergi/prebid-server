## Metrics configuration and how to import metrics of [Prebid server](https://docs.prebid.org/prebid-server/versions/pbs-versions-go.html) to [Grafana](https://grafana.com/) through [Prometheus](https://prometheus.io/) stack.
     
#### These are the parameters that are capable for metrics collection 

- PBS_METRICS_PROMETHEUS_PORT=9100 - default is 0.

If PBS_METRICS_PROMETHEUS_PORT is null, the Prometheus endpoint is disabled. Other configured metrics backends can still collect metrics.
  
- PBS_METRICS_PROMETHEUS_NAMESPACE=prebid - default is empty.
  
PBS_METRICS_PROMETHEUS_NAMESPACE - this is responsable for the primary prefix added to your metrics to ensure uniqueness within your cluster.
  
- PBS_METRICS_PROMETHEUS_SUBSYSTEM=server - default is empty.

PBS_METRICS_PROMETHEUS_SUBSYSTEM - this is a secondary prefix added to metrics to ensure uniqueness.
  
- PBS_METRICS_DISABLED_METRICS_ADAPTER_CONNECTIONS_METRICS=false - default is true.

PBS_METRICS_DISABLED_METRICS_ADAPTER_CONNECTIONS_METRICS - If this flag is set to true you won't get any bidder http connection adapter metrics (e.g. number of new vs reused connections) but you'll still get other adapter metrics.

#### If you're going to get metrics though [Prometheus](https://prometheus.io/) and [Prometheus](https://prometheus.io/) stack has been already installed, you have several options, please chose one:

- change environments into code (bad way).
 
- create pbs.yaml if it hasn't been already created (It works, but not flexible).

- input the parameters into global environments (Highly recommend you to use this way also it's reliable).

#### Finally, when you fill in your credentials of metrics, please, run:

 - build your app 
```bash
go build .
```
 - run the server
```bash
./prebid-server
```

#### Make sure that an application returns any metrics - `http://localhost:9100/`, if not, please check your names of environment and recompile again.

#### Add your host and port into prometheus.yml file on the instance of your [Prometheus](https://prometheus.io/).

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

  external_labels:
    monitor: 'docker-host-alpha'

rule_files:
  - "alert.rules"

scrape_configs:
  - job_name: 'prometheus'
    scrape_interval: 10s
    static_configs:
      - targets: [ 'prometheus-host:9090']

  - job_name: 'prebidserver-metrics'
    scrape_interval: 10s
    static_configs:
      - targets: ['prebidserver-host:9100' ]
alerting:
  alertmanagers:
    - scheme: http
      static_configs:
        - targets:
            - 'localhost:9093'
```

#### Import data source as [Prometheus](https://prometheus.io/) into [Grafana](https://grafana.com/)

Just fill in host and port of [Prometheus](https://prometheus.io/)

![img_grafana.png](images/img_grafana.png)

## Exporting metrics with OpenTelemetry

The OpenTelemetry backend pushes metrics to an OpenTelemetry Collector over
OTLP/HTTP protobuf. It can run by itself (leave the Prometheus port at `0`) or
alongside the Prometheus backend.

```yaml
metrics:
  opentelemetry:
    enabled: true
    endpoint: https://otel-collector.example:4318
    protocol: http/protobuf
    metric_send_interval: 30
    timeout_ms: 10000
    temporality: cumulative
    service_name: prebid-server
    service_version: "1.0.0"
    instance_id: "pbs-01"
    account_limit: 1000
    headers:
      Authorization: "Bearer replace-with-a-secret"
    resource_attributes:
      deployment.environment: production
    tls:
      ca_file: /etc/ssl/certs/otel-collector-ca.pem
```

`endpoint` may be an HTTP(S) URL or a host and port. Leave it empty to use the
standard `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` or
`OTEL_EXPORTER_OTLP_ENDPOINT` environment variable, then the OTLP default
`localhost:4318`. PBS configuration takes precedence over standard `OTEL_*`
exporter settings when the corresponding PBS value is configured. The interval
and timeout in PBS configuration take precedence over
`OTEL_METRIC_EXPORT_INTERVAL` and `OTEL_METRIC_EXPORT_TIMEOUT`. Standard
`OTEL_RESOURCE_ATTRIBUTES` are merged with configured resource attributes;
configured attributes take precedence for duplicate keys. Headers are not
written to logs.

Use TLS for remote collectors and supply a CA file when the collector uses a
private certificate authority. Authentication belongs in `headers`; avoid
committing tokens in configuration files. The backend accepts only
`http/protobuf` and uses cumulative temporality by default; `delta` is available
when the collector and downstream storage are configured for it.

Account-labelled measurements are capped per server instance by
`account_limit` (default `1000`). Accounts above the limit share the
`account="overflow"` series. No request or user identifiers are exported.
Unlike Prometheus, OpenTelemetry emits a series only after its first
measurement, so dashboards and alerts must not assume unobserved label
combinations exist with a zero value. Histograms use seconds for durations and
unscaled CPM for bid prices; the bid-price boundaries are chosen for typical
CPM values rather than copied from the Prometheus backend.

The backend also exports Go runtime metrics using the official OpenTelemetry
runtime instrumentation. OTLP-to-Prometheus translation may append counter
suffixes or normalize metric names; configure dashboards against the names
exposed by your Collector/exporter pipeline.

#### In that case [Prebid server](https://docs.prebid.org/prebid-server/versions/pbs-versions-go.html) uses [package](https://github.com/prometheus/client_golang) in our case it works as [Node exporter](https://github.com/prometheus/node_exporter). Therefore, here is described only how to connect [Prebid server](https://docs.prebid.org/prebid-server/versions/pbs-versions-go.html) connection with [Prometheus](https://prometheus.io/). Also, if you are interested in [Prometheus](https://prometheus.io/) and want to dig deep, follow [docs](https://prometheus.io/docs/introduction/overview/).
