# caerus-framework-observability

[![CI](https://github.com/caerus-framework/caerus-framework-observability/actions/workflows/ci.yml/badge.svg)](https://github.com/caerus-framework/caerus-framework-observability/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/caerus-framework/caerus-framework-observability/graph/badge.svg)](https://codecov.io/gh/caerus-framework/caerus-framework-observability)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)


Caerus Framework — observability component.

Exposes:

- Kubernetes health-check endpoints aggregating every registered component that
  implements the optional
  [`caerusframework.HealthProvider`](https://github.com/caerus-framework/caerus-framework/blob/main/component.go)
  interface. Components that do not implement it are simply not included, so
  supporting health checks is entirely optional.
- A `/metrics` endpoint (Prometheus text format) with Go runtime metrics plus
  the live state of every registered component implementing the optional
  `cf.MetricsProvider` interface.
- OpenTelemetry tracing: when an OTLP endpoint is configured, the component
  builds a tracer provider, installs it as the global provider (components
  trace via `otel.Tracer`), and flushes it at `Shutdown`.

## Endpoints

| Endpoint | Probe | Behaviour |
|---|---|---|
| `/healthz`, `/livez` | liveness | `200 ok` while the process is alive; component health is deliberately excluded (a dependency outage should make a pod unready, not restartable). |
| `/readyz` | readiness (and startup) | `200 ok` when every registered `HealthProvider` component is healthy, `503` otherwise, with one `fail: <component>: <reason>` line per failing check. |
| `/metrics` | scrape | Prometheus text format: Go runtime + process metrics and one `caerus_<name>` sample per `MetricsProvider` component that has something to report. |

Kubernetes only inspects the status code: `2xx` = healthy, anything else =
unhealthy. Configure the probes in the pod spec, e.g.:

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 9090 }
  initialDelaySeconds: 3
  periodSeconds: 10
readinessProbe:
  httpGet: { path: /readyz, port: 9090 }
  initialDelaySeconds: 3
  periodSeconds: 5
```

Point a Prometheus scraper at `/metrics`, and a collector (e.g.
`otel-collector`) at the configured `trace_endpoint` for OTLP/gRPC spans.

## Usage

```go
fw := caerusframework.New() // observability is a built-in bootstrap stage
fw.AddComponent(cf_logs.New(cf_logs.WithWriter(os.Stdout)))
fw.AddComponent(cf_observability.New()) // health checks + metrics on :9090 by default
// ... register the rest of the components ...
fw.Run(ctx)
```

### Optional component health checks

A component opts in by implementing `cf.HealthProvider`; the observability
component discovers it and folds its health into `/readyz`:

```go
// Health implements cf.HealthProvider. nil = healthy.
func (c *CFMongoDB) Health(ctx context.Context) error {
    return c.pool.Ping(ctx) // or any other liveness/readiness signal
}
```

Every check is bounded by the health-check timeout (default `2s`); a component
that misses its deadline is reported as `timed out` so a hung check can never
hang the probe.

### Optional component metrics (lazy pickup)

A component opts in by implementing `cf.MetricsProvider`; observability
registers a collector for it and reads `Metrics()` on every `/metrics` scrape,
so the values are always live:

```go
// Metrics implements cf.MetricsProvider.
func (l *Logs) Metrics() []cf.Metric {
    return []cf.Metric{{
        Name:  "logs_info", // served as caerus_logs_info
        Value: 1,
        Labels: map[string]string{"format": l.format.String(), "level": l.Level().String()},
    }}
}
```

Return `nil` while the component is not initialized (or has nothing to report):
the collector then skips it, and the sample appears on the scrape after the
component initializes — a lazy pickup with no subscription. Components that do
not implement the interface contribute nothing.

### Tracing

With an endpoint configured, `Init` creates an OTLP/gRPC tracer provider
(`AlwaysSample`, `service.name` = `WithServiceName`) and installs it as the
global provider; components trace through `otel.Tracer`. `Shutdown` flushes
pending spans. The transport is insecure — run the collector in-cluster.

## Configuration

`ObservabilityConfig` is file/env-drivable: load it through the configuration
component and pass it via `WithConfig`:

```yaml
observability:
  health_checks: true          # enable the health-check endpoints
  metrics: true                # enable the /metrics endpoint
  tracing: true                # enable OTLP trace export (needs trace_endpoint)
  address: ":9090"             # bind address of the HTTP server
  health_check_timeout_sec: 2  # per-component health check deadline
  trace_endpoint: "otel-collector:4317"
  service_name: myapp          # service.name on exported spans
```

| Option | Default | Purpose |
|---|---|---|
| `WithHealthChecks(bool)` | `true` | Enable the Kubernetes health-check endpoints. |
| `WithMetrics(bool)` | `true` | Enable the `/metrics` endpoint. |
| `WithTracing(bool)` | `false` | Enable trace export (active once an endpoint is set). |
| `WithAddress(string)` | `":9090"` | Bind address of the HTTP server. |
| `WithHealthCheckTimeout(d)` | `2s` | Deadline for each component health check. |
| `WithTraceEndpoint(string)` | `""` (tracing latent) | OTLP/gRPC collector endpoint. |
| `WithServiceName(string)` | `"caerus"` | `service.name` attribute on exported spans. |
| `WithConfig(ObservabilityConfig)` | — | Loaded config; non-zero fields override the options. |
| `WithConfigSource(string)` | `""` | Bind a configuration source; `Init` applies its current value and `OnConfigReload` applies later changes live (tracing) or logs restart-required (bind/metrics/health toggles). |
| `WithLogger(*slog.Logger)` | framework `logs` logger (re-delivered on `logs` `Reconfigure`), falling back to `slog.Default()` | Explicit logger override. |

`health_checks`, `metrics` and `tracing` are `*bool` in `ObservabilityConfig`
so an explicit `false` in the file is honored (turning the feature off) instead
of being treated as "unset".

## Component contract

Implements `caerusframework.CaerusComponent`:

- `Name()` → `"observability"` (`cf_observability.ComponentName`)
- `GetInitOrderStage()` → `caerusframework.ObservabilityStage` (third bootstrap
  stage, after logs and configuration)
- `GetDependencies()` → `[logs]`
- `Init` sets up the Prometheus registry (Go/process collectors + one collector
  per registered `MetricsProvider`), builds and installs the tracer provider
  when tracing is enabled and an endpoint is configured, and binds the HTTP
  server (fail-fast on an unusable address) when health checks or metrics are
  enabled. With everything disabled it is a no-op.
- `Shutdown` stops the server, waiting for in-flight requests up to `ctx`, and
  flushes the tracer provider.
- `Address()` returns the bound address (empty when disabled) for building
  probe configs at runtime.
- `TracerProvider()` returns the configured provider (nil when tracing is
  inactive).

## Docs

- [ARCHITECTURE.md](https://github.com/caerus-framework/caerus-framework/blob/main/docs/ARCHITECTURE.md)
  — component model and stage ordering.
- [LIFECYCLE.md](https://github.com/caerus-framework/caerus-framework/blob/main/docs/LIFECYCLE.md)
  — lifecycle guarantees.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
