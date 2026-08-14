package cf_observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// ComponentName is the framework component name for the observability
// component. It is the identifier other components use in GetDependencies to
// require observability.
const ComponentName = "observability"

// ObservabilityConfig is the file/env-drivable observability configuration.
// Load it through the configuration component and pass it via WithConfig; both
// JSON and YAML tags are provided.
type ObservabilityConfig struct {
	// HealthChecks enables the Kubernetes health-check HTTP endpoints. It is a
	// *bool so an absent key is distinguishable from an explicit
	// health_checks: false, which is required to turn the endpoints off (the
	// component's default is enabled).
	HealthChecks *bool `json:"health_checks,omitempty" yaml:"health_checks,omitempty"`
	// Metrics enables the /metrics endpoint (Prometheus text format; default
	// enabled). It is a *bool so an explicit metrics: false is honored.
	Metrics *bool `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	// Tracing enables OpenTelemetry trace export over OTLP/gRPC (default off;
	// internal tracing mechanics stay dark until enabled). It is a *bool so an
	// explicit tracing: false is honored.
	Tracing *bool `json:"tracing,omitempty" yaml:"tracing,omitempty"`
	// Address is the bind address for the HTTP server (default ":9090").
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	// HealthCheckTimeoutSec bounds each component health check (default 2).
	HealthCheckTimeoutSec int `json:"health_check_timeout_sec,omitempty" yaml:"health_check_timeout_sec,omitempty"`
	// TraceEndpoint is the OTLP/gRPC collector endpoint (e.g.
	// "otel-collector:4317"). When set and tracing is enabled, spans are
	// exported to it (insecure transport).
	TraceEndpoint string `json:"trace_endpoint,omitempty" yaml:"trace_endpoint,omitempty"`
	// ServiceName is the OpenTelemetry service.name attribute attached to
	// exported spans (default "caerus").
	ServiceName string `json:"service_name,omitempty" yaml:"service_name,omitempty"`
}

// Option configures the observability component at construction time.
type Option func(*options)

type options struct {
	healthChecks       bool
	metrics            bool
	tracing            bool
	address            string
	healthCheckTimeout time.Duration
	traceEndpoint      string
	serviceName        string
	loaded             *ObservabilityConfig // set by WithConfig; overrides option-set defaults
	configSource       string               // configuration source for OnConfigReload ("" = none)
	logger             *slog.Logger
	loggerSet          bool // true when WithLogger was called explicitly
}

// WithHealthChecks enables (default) or disables the Kubernetes health-check
// HTTP endpoints.
func WithHealthChecks(enabled bool) Option {
	return func(o *options) { o.healthChecks = enabled }
}

// WithMetrics enables (default) or disables the /metrics endpoint.
func WithMetrics(enabled bool) Option {
	return func(o *options) { o.metrics = enabled }
}

// WithTracing enables (default off) OpenTelemetry trace export. Tracing is off
// by default and only active once enabled and an endpoint is set with
// WithTraceEndpoint (or the loaded config's trace_endpoint).
func WithTracing(enabled bool) Option {
	return func(o *options) { o.tracing = enabled }
}

// WithAddress sets the bind address for the HTTP server (default ":9090").
func WithAddress(addr string) Option {
	return func(o *options) { o.address = addr }
}

// WithHealthCheckTimeout sets the deadline for each component health check
// (default 2s). A component that misses its deadline is reported as not ready.
func WithHealthCheckTimeout(d time.Duration) Option {
	return func(o *options) { o.healthCheckTimeout = d }
}

// WithTraceEndpoint sets the OTLP/gRPC collector endpoint. The connection is
// insecure and only created when tracing is enabled.
func WithTraceEndpoint(endpoint string) Option {
	return func(o *options) { o.traceEndpoint = endpoint }
}

// WithServiceName sets the OpenTelemetry service.name attribute (default
// "caerus").
func WithServiceName(name string) Option {
	return func(o *options) { o.serviceName = name }
}

// WithConfig sets the configuration loaded from the configuration component.
// Non-zero fields of cfg override the values set by the convenience options,
// which act as in-code defaults:
//
//	cfg, _ := cf_configuration.Lookup[cf_observability.ObservabilityConfig](conf, "observability")
//	o := cf_observability.New(cf_observability.WithConfig(*cfg))
func WithConfig(cfg ObservabilityConfig) Option {
	return func(o *options) { o.loaded = &cfg }
}

// WithConfigSource names the configuration source (caerus-framework-
// configuration) whose ObservabilityConfig is applied to the component at Init
// and again on every validated reload via OnConfigReload. Prefer this over a
// WithConfig snapshot when the component is framework-managed: the source stays
// the live options plane. The component self-registers the source during argv
// absorption (default file config/<name>.json, owner cf_observability); an argv
// --<name> file-path override wins, and the app may also register its own
// Source[ObservabilityConfig] for a custom default. Until the source loads,
// construction-time defaults apply.
func WithConfigSource(name string) Option {
	return func(o *options) { o.configSource = name }
}

// WithLogger overrides the logger used for component diagnostics. By default
// the component logs through the framework logs component (declared in
// GetDependencies); WithLogger is an explicit override for tests and embedded
// use and wins over the framework logger. slog.Default() remains the fallback
// only when neither is available.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger; o.loggerSet = true }
}

// overlayConfig overlays non-zero fields of cfg onto o. It runs last, so a
// loaded config always wins over option-set defaults. The *bool fields are
// pointers so an explicit false in the loaded config is honored.
func overlayConfig(o *options, cfg ObservabilityConfig) {
	if cfg.HealthChecks != nil {
		o.healthChecks = *cfg.HealthChecks
	}
	if cfg.Metrics != nil {
		o.metrics = *cfg.Metrics
	}
	if cfg.Tracing != nil {
		o.tracing = *cfg.Tracing
	}
	if cfg.Address != "" {
		o.address = cfg.Address
	}
	if cfg.HealthCheckTimeoutSec != 0 {
		o.healthCheckTimeout = time.Duration(cfg.HealthCheckTimeoutSec) * time.Second
	}
	if cfg.TraceEndpoint != "" {
		o.traceEndpoint = cfg.TraceEndpoint
	}
	if cfg.ServiceName != "" {
		o.serviceName = cfg.ServiceName
	}
}

// Observability is the caerus-framework-observability component. It exposes:
//
//   - Kubernetes health-check endpoints (liveness /healthz + /livez, readiness
//     /readyz) that aggregate the health of every registered component
//     implementing cf.HealthProvider. Components that do not implement it are
//     simply not included, so supporting health checks is entirely optional.
//   - a /metrics endpoint (Prometheus text format) with Go runtime metrics plus
//     the live state of every registered component implementing
//     cf.MetricsProvider. State is read on every scrape, so a component that is
//     not initialized yet returns nil and is skipped until it does (lazy
//     pickup); components that do not implement the interface contribute
//     nothing.
//   - OpenTelemetry tracing: when an OTLP endpoint is configured, the component
//     builds a tracer provider, installs it as the global provider (components
//     trace via otel.Tracer), and flushes it at Shutdown.
type Observability struct {
	mu                 sync.Mutex
	fw                 *cf.CaerusFramework
	logger             *slog.Logger
	loggerSet          bool
	logsSub            *cf_logs.Subscription
	healthChecks       bool
	healthCheckTimeout time.Duration
	metrics            bool
	registry           *prometheus.Registry
	tracing            bool
	traceEndpoint      string
	serviceName        string
	tp                 *trace.TracerProvider
	bindAddress        string
	configSource       string
	providers          []namedHealth
	handler            http.Handler
	initialized        bool
	running            bool
	server             *http.Server
	addr               string
}

// httpDrainTimeout is how long Run waits for in-flight probe/scrape requests
// when the framework cancels the run context.
const httpDrainTimeout = 5 * time.Second

// namedHealth pairs a component's name with its HealthProvider.
type namedHealth struct {
	name string
	comp cf.HealthProvider
}

// New creates an observability component. Health checks and metrics are on by
// default; tracing is off until explicitly enabled with a config value or
// option. Init prepares collectors and the HTTP mux; Run (cf.Runnable) binds
// the listener when at least one of health checks or metrics is enabled.
// Jobs never start Runnables, so a migrate/seed process does not open the
// operator HTTP port.
func New(opts ...Option) *Observability {
	o := options{
		healthChecks:       true,
		metrics:            true,
		tracing:            false,
		address:            ":9090",
		healthCheckTimeout: 2 * time.Second,
		serviceName:        "caerus",
		logger:             slog.Default(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.loaded != nil {
		overlayConfig(&o, *o.loaded)
	}
	return &Observability{
		healthChecks:       o.healthChecks,
		healthCheckTimeout: o.healthCheckTimeout,
		metrics:            o.metrics,
		tracing:            o.tracing,
		traceEndpoint:      o.traceEndpoint,
		serviceName:        o.serviceName,
		bindAddress:        o.address,
		configSource:       o.configSource,
		logger:             o.logger,
		loggerSet:          o.loggerSet,
	}
}

// Name implements cf.CaerusComponent.
func (c *Observability) Name() string { return ComponentName }

// GetInitOrderStage implements cf.CaerusComponent. Observability is part of
// the bootstrap prefix and initializes after logs and configuration.
func (c *Observability) GetInitOrderStage() cf.Stage { return cf.ObservabilityStage }

// GetDependencies implements cf.Dependencies. The component logs through the
// framework logs component, and depends on configuration when WithConfigSource
// is set (it reads its own source through the configuration component).
func (c *Observability) GetDependencies() []string {
	deps := []string{cf_logs.ComponentName}
	if c.configSource != "" {
		deps = append(deps, cf_configuration.ComponentName)
	}
	return deps
}

// Init implements cf.CaerusComponent. It sets up metrics (Prometheus registry
// with Go runtime collectors and one collector per registered component
// implementing cf.MetricsProvider) and tracing (OTLP tracer provider, when an
// endpoint is configured and tracing is enabled). When health checks or
// metrics are enabled it builds the HTTP mux and discovers registered
// cf.HealthProvider components. It does not bind a listen address — that is
// Run's job, so a framework job (which never starts Runnables) does not
// expose /metrics or probes.
func (c *Observability) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.fw = fw
	if !c.loggerSet {
		if logs, ok := cf.Get[*cf_logs.Logs](fw); ok {
			c.logsSub = logs.OnReconfigureFor(c.Name(), func(l *slog.Logger) { c.logger = l })
		}
	}

	// Apply the observability source's current value (loaded before Init, since
	// configuration initializes in an earlier bootstrap stage). Later reloads
	// arrive via OnConfigReload.
	if c.configSource != "" {
		if conf, ok := cf.Get[*cf_configuration.Configuration](fw); ok {
			if cfg, err := cf_configuration.Lookup[ObservabilityConfig](conf, c.configSource); err == nil {
				c.applyConfigLocked(*cfg)
			}
		}
	}

	if c.metrics {
		registry := prometheus.NewRegistry()
		registry.MustRegister(collectors.NewGoCollector())
		registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		for _, comp := range fw.Components() {
			if mp, ok := comp.(MetricsProvider); ok {
				registry.MustRegister(&metricsCollector{name: comp.Name(), provider: mp})
			}
		}
		if logs, ok := cf.Get[*cf_logs.Logs](fw); ok {
			registry.MustRegister(&logsMetricsCollector{logs: logs})
		}
		if c.configSource != "" {
			// Configuration metrics are only meaningful when this component is
			// bound to a source; without one the dependency is not declared.
			if conf, ok := cf.Get[*cf_configuration.Configuration](fw); ok {
				registry.MustRegister(&configurationMetricsCollector{conf: conf})
			}
		}
		c.registry = registry
	}

	if c.tracing {
		if c.traceEndpoint == "" {
			c.logger.Info("cf_observability: tracing disabled (no trace endpoint configured)")
		} else if err := c.reconcileTracingLocked(); err != nil {
			return err
		}
	}

	if !c.healthChecks && !c.metrics {
		c.logger.Info("cf_observability: endpoints disabled")
		c.initialized = true
		return nil
	}

	mux := http.NewServeMux()
	if c.healthChecks {
		mux.HandleFunc("/healthz", c.livenessHandler)
		mux.HandleFunc("/livez", c.livenessHandler)
		mux.HandleFunc("/readyz", c.readinessHandler)
		for _, comp := range fw.Components() {
			if h, ok := comp.(cf.HealthProvider); ok {
				c.providers = append(c.providers, namedHealth{name: comp.Name(), comp: h})
			}
		}
	}
	if c.metrics {
		mux.Handle("/metrics", promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{}))
	}
	c.handler = mux
	c.initialized = true
	c.logger.Info("cf_observability: initialized",
		"health_checks", c.healthChecks,
		"metrics", c.metrics,
		"providers", len(c.providers),
		"bind_address", c.bindAddress,
	)
	return nil
}

// Run implements cf.Runnable. It binds the operator HTTP server (health
// probes and /metrics) and serves until ctx is canceled. Init only prepares
// the mux; claiming a listen address here means a job-only process — which
// never starts Runnables — does not expose those endpoints.
func (c *Observability) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return errors.New("cf_observability: Run before Init")
	}
	if c.running {
		c.mu.Unlock()
		return errors.New("cf_observability: Run already active")
	}
	if c.handler == nil {
		c.mu.Unlock()
		return nil
	}
	handler := c.handler
	bindAddress := c.bindAddress
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.addr = ""
		c.server = nil
		c.mu.Unlock()
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", bindAddress)
	if err != nil {
		return fmt.Errorf("cf_observability: listen %s: %w", bindAddress, err)
	}
	if err := ctx.Err(); err != nil {
		_ = ln.Close()
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	c.mu.Lock()
	c.server = server
	c.addr = ln.Addr().String()
	addr := c.addr
	providers := len(c.providers)
	healthChecks := c.healthChecks
	metrics := c.metrics
	c.mu.Unlock()
	c.logger.Info("cf_observability: HTTP server listening",
		"addr", addr,
		"health_checks", healthChecks,
		"metrics", metrics,
		"providers", providers,
	)

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()

	select {
	case err := <-errCh:
		return normalizeHTTPServerError(err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		serveErr := normalizeHTTPServerError(<-errCh)
		cancel()
		if shutdownErr != nil {
			c.logger.Error("cf_observability: graceful shutdown failed", "err", shutdownErr)
			return shutdownErr
		}
		return serveErr
	}
}

func normalizeHTTPServerError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// applyConfigLocked overlays a loaded ObservabilityConfig onto the runtime
// fields. Non-zero fields win; the *bool fields honor an explicit false.
// Callers must hold c.mu. Endpoint options that need a bound-server rebuild
// are not applied live; OnConfigReload logs them as restart-required.
func (c *Observability) applyConfigLocked(cfg ObservabilityConfig) {
	if cfg.HealthChecks != nil {
		c.healthChecks = *cfg.HealthChecks
	}
	if cfg.Metrics != nil {
		c.metrics = *cfg.Metrics
	}
	if cfg.Tracing != nil {
		c.tracing = *cfg.Tracing
	}
	if cfg.Address != "" {
		c.bindAddress = cfg.Address
	}
	if cfg.HealthCheckTimeoutSec != 0 {
		c.healthCheckTimeout = time.Duration(cfg.HealthCheckTimeoutSec) * time.Second
	}
	if cfg.TraceEndpoint != "" {
		c.traceEndpoint = cfg.TraceEndpoint
	}
	if cfg.ServiceName != "" {
		c.serviceName = cfg.ServiceName
	}
}

// reconcileTracingLocked starts or stops the tracer provider to match the
// current tracing config. Callers must hold c.mu. A failed provider build
// returns the error and keeps the previous state (last-good).
func (c *Observability) reconcileTracingLocked() error {
	if c.tracing && c.traceEndpoint != "" {
		if c.tp != nil {
			return nil
		}
		tp, err := newTracerProvider(context.Background(), c.traceEndpoint, c.serviceName)
		if err != nil {
			return err
		}
		c.tp = tp
		otel.SetTracerProvider(tp)
		c.logger.Info("cf_observability: tracing enabled",
			"endpoint", c.traceEndpoint,
			"service", c.serviceName,
		)
		return nil
	}
	if c.tp != nil {
		old := c.tp
		c.tp = nil
		// Reset the global so late otel.Tracer calls do not hit a shut-down
		// provider; a sampler-only provider drops spans cheaply.
		otel.SetTracerProvider(trace.NewTracerProvider(trace.WithSampler(trace.NeverSample())))
		c.logger.Info("cf_observability: tracing disabled")
		_ = old.Shutdown(context.Background())
	}
	return nil
}

// OnConfigReload implements cf.ConfigReloader. It applies a freshly loaded
// ObservabilityConfig from the source named by WithConfigSource. Tracing
// toggle/endpoint changes take effect immediately (provider swap); HTTP
// endpoint changes (bind address, health-check/metrics toggles) are logged as
// restart-required and the last-good server keeps running. The initial value
// delivered by configuration before this component's Init is ignored (Init
// reads the source itself and must not build providers early).
func (c *Observability) OnConfigReload(source string, cfg any) {
	if source != c.configSource {
		return
	}
	oc, ok := cfg.(*ObservabilityConfig)
	if !ok {
		return
	}
	c.mu.Lock()
	if c.fw == nil {
		// Pre-Init notification from the configuration stage: Init applies the
		// source value itself. Do not build providers/server yet.
		c.mu.Unlock()
		return
	}
	prevTracing, prevEndpoint := c.tracing, c.traceEndpoint
	prevAddr, prevHealth, prevMetrics := c.bindAddress, c.healthChecks, c.metrics
	c.applyConfigLocked(*oc)
	tracingChanged := c.tracing != prevTracing || c.traceEndpoint != prevEndpoint
	var tracingErr error
	if tracingChanged {
		tracingErr = c.reconcileTracingLocked()
	}
	restart := c.bindAddress != prevAddr || c.healthChecks != prevHealth || c.metrics != prevMetrics
	newAddr, newHealth, newMetrics := c.bindAddress, c.healthChecks, c.metrics
	c.mu.Unlock()

	if tracingErr != nil {
		c.logger.Error("cf_observability: tracing reload rejected; keeping previous provider", "err", tracingErr)
	}
	if restart {
		c.logger.Warn("cf_observability: HTTP endpoint changes need a restart to apply",
			"address", newAddr,
			"health_checks", newHealth,
			"metrics", newMetrics,
		)
	}
}

// Shutdown implements cf.CaerusComponent. It stops the HTTP server if Run
// (or an in-flight serve) left one running, waiting for in-flight requests up
// to ctx, and flushes the tracer provider. Safe to call even if Init never
// ran or the features are disabled.
func (c *Observability) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if c.logsSub != nil {
		c.logsSub.Unsubscribe()
		c.logsSub = nil
	}
	server := c.server
	c.server = nil
	c.addr = ""
	c.handler = nil
	c.providers = nil
	c.initialized = false
	c.fw = nil
	tp := c.tp
	c.tp = nil
	c.mu.Unlock()

	var errs []error
	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cf_observability: shutdown HTTP server: %w", err))
		} else {
			c.logger.Info("cf_observability: HTTP server stopped")
		}
	}
	if tp != nil {
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cf_observability: shutdown tracer provider: %w", err))
		} else {
			c.logger.Info("cf_observability: tracing stopped")
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Address returns the bound address of the HTTP server, or "" if neither
// health checks nor metrics are enabled, Init has not run, or Run has not
// bound yet. It is useful for building Kubernetes probe configs at runtime
// after the serve path has started.
func (c *Observability) Address() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addr
}

// TracerProvider returns the OpenTelemetry tracer provider configured at Init,
// or nil when tracing is disabled or no endpoint was configured. Components
// trace through otel.Tracer (the provider is installed globally); the accessor
// is for embedding and for building composite providers.
func (c *Observability) TracerProvider() oteltrace.TracerProvider {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tp == nil {
		return nil
	}
	return c.tp
}

// newTracerProvider builds an OTLP/gRPC-backed tracer provider that always
// samples and tags every span with the service name.
func newTracerProvider(ctx context.Context, endpoint, serviceName string) (*trace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("cf_observability: create OTLP trace exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("cf_observability: create trace resource: %w", err)
	}
	return trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithSampler(trace.AlwaysSample()),
		trace.WithResource(res),
	), nil
}

// metricsCollector bridges a MetricsProvider into the Prometheus registry.
// It is an unchecked collector: the metrics and their descriptors are only
// known at Collect time, so Describe emits nothing (client_golang treats a
// collector yielding no descriptor as unchecked). On every /metrics scrape it
// reads the component's live state, so a component that is not initialized yet
// returns nil and is skipped — a lazy pickup that needs no subscription.
type metricsCollector struct {
	name     string
	provider MetricsProvider
}

func (mc *metricsCollector) Describe(ch chan<- *prometheus.Desc) {}

func (mc *metricsCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range mc.provider.Metrics() {
		if m.Name == "" {
			continue
		}
		labelNames := make([]string, 0, len(m.Labels))
		for name := range m.Labels {
			labelNames = append(labelNames, name)
		}
		sort.Strings(labelNames)
		labelValues := make([]string, 0, len(labelNames))
		for _, name := range labelNames {
			labelValues = append(labelValues, m.Labels[name])
		}
		desc := prometheus.NewDesc(m.Name, m.Help, labelNames, nil)
		vt := prometheus.GaugeValue
		if m.Type == MetricTypeCounter {
			vt = prometheus.CounterValue
		}
		ch <- prometheus.MustNewConstMetric(desc, vt, m.Value, labelValues...)
	}
}

// livenessHandler reports that the process is alive. Kubernetes uses it for
// the liveness probe: it returns 200 as long as the server is up, so a wedged
// or dead process fails the probe. Component health is deliberately excluded —
// a dependency outage should make a pod unready, not restartable.
func (c *Observability) livenessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readinessHandler aggregates the health of every registered component that
// implements cf.HealthProvider. It returns 200 when all are healthy and 503
// otherwise, which Kubernetes uses for the readiness probe (and typically the
// startup probe). Components that do not implement cf.HealthProvider are not
// included; each included check is bounded by the health-check timeout.
func (c *Observability) readinessHandler(w http.ResponseWriter, r *http.Request) {
	var failures []string
	for _, p := range c.providers {
		checkCtx, cancel := context.WithTimeout(r.Context(), c.healthCheckTimeout)
		err := p.comp.Health(checkCtx)
		expired := checkCtx.Err() == context.DeadlineExceeded
		cancel()
		if err == nil {
			continue
		}
		if expired {
			failures = append(failures, fmt.Sprintf("%s: timed out", p.name))
		} else {
			failures = append(failures, fmt.Sprintf("%s: %v", p.name, err))
		}
	}
	if len(failures) == 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	for _, f := range failures {
		fmt.Fprintln(w, "fail:", f)
	}
}

var _ cf.CaerusComponent = (*Observability)(nil)
var _ cf.Dependencies = (*Observability)(nil)
var _ cf.ConfigReloader = (*Observability)(nil)
var _ cf.Runnable = (*Observability)(nil)
