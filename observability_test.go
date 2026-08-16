package cf_observability

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	"go.opentelemetry.io/otel"
)

const testStage = cf.Stage("testdata")

func boolPtr(b bool) *bool        { return &b }
func floatPtr(f float64) *float64 { return &f }

func addComponent(t *testing.T, fw *cf.CaerusFramework, c cf.CaerusComponent) {
	t.Helper()
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent(%q): %v", c.Name(), err)
	}
}

func newTestFW(t *testing.T, comps ...cf.CaerusComponent) *cf.CaerusFramework {
	t.Helper()
	fw := cf.New()
	addComponent(t, fw, cf_logs.New(cf_logs.WithWriter(io.Discard)))
	for _, c := range comps {
		addComponent(t, fw, c)
	}
	return fw
}

func initFW(t *testing.T, fw *cf.CaerusFramework) {
	t.Helper()
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = fw.Shutdown(context.Background()) })
}

// startHTTP runs the observability Runnable until it binds, then cancels it
// on test cleanup. Call after initFW for tests that GET /metrics or probes.
func startHTTP(t *testing.T, o *Observability) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- o.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if o.Address() != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("observability Run did not bind an address")
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// testProvider is a test component implementing cf.HealthProvider.
type testProvider struct {
	name   string
	mu     sync.Mutex
	health func(ctx context.Context) error
}

func (p *testProvider) Name() string                                    { return p.name }
func (p *testProvider) GetInitOrderStage() cf.Stage                     { return testStage }
func (p *testProvider) GetDependencies() []string                       { return nil }
func (p *testProvider) Init(context.Context, *cf.CaerusFramework) error { return nil }
func (p *testProvider) Shutdown(context.Context) error                  { return nil }
func (p *testProvider) Health(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.health == nil {
		return nil
	}
	return p.health(ctx)
}

func (p *testProvider) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.health = nil
		return
	}
	p.health = func(context.Context) error { return err }
}

// plainComp is a test component that does NOT implement cf.HealthProvider. It
// must be ignored by the readiness aggregation.
type plainComp struct{ name string }

func (c *plainComp) Name() string                                    { return c.name }
func (c *plainComp) GetInitOrderStage() cf.Stage                     { return testStage }
func (c *plainComp) GetDependencies() []string                       { return nil }
func (c *plainComp) Init(context.Context, *cf.CaerusFramework) error { return nil }
func (c *plainComp) Shutdown(context.Context) error                  { return nil }

// testMetricsProvider is a test component implementing cf.MetricsProvider with
// a readiness flag, so the lazy pickup (nil before "init", samples after) can
// be exercised.
type testMetricsProvider struct {
	*plainComp
	mu    sync.Mutex
	ready bool
}

func (m *testMetricsProvider) setReady(ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = ready
}

func (m *testMetricsProvider) Metrics() []Metric {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return nil
	}
	return []Metric{{
		Name:   "app_info",
		Help:   "Test application state.",
		Value:  1,
		Labels: map[string]string{"mode": "prod"},
	}}
}

func TestComponentContract(t *testing.T) {
	o := New()
	if o.Name() != ComponentName {
		t.Fatalf("Name() = %q, want %q", o.Name(), ComponentName)
	}
	if o.GetInitOrderStage() != cf.ObservabilityStage {
		t.Fatalf("GetInitOrderStage() = %q, want %q", o.GetInitOrderStage(), cf.ObservabilityStage)
	}
	if deps := o.GetDependencies(); len(deps) != 1 || deps[0] != cf_logs.ComponentName {
		t.Fatalf("GetDependencies() = %v, want [logs]", deps)
	}
	if err := o.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Init: %v", err)
	}
	var _ cf.CaerusComponent = o
	var _ cf.Dependencies = o
	var _ cf.Runnable = o
}

func firstBind(o *Observability) string {
	if len(o.binds) == 0 {
		return ""
	}
	return o.binds[0]
}

func TestConfigOverlay(t *testing.T) {
	// defaults: health + metrics enabled, tracing off, :9090, 2s, service caerus
	d := New()
	if !d.healthChecks || !d.metrics || d.tracing || firstBind(d) != ":9090" ||
		d.healthCheckTimeout != 2*time.Second || d.serviceName != "caerus" || d.traceEndpoint != "" {
		t.Fatalf("defaults = health:%v metrics:%v tracing:%v %q %v service:%q endpoint:%q",
			d.healthChecks, d.metrics, d.tracing, firstBind(d), d.healthCheckTimeout, d.serviceName, d.traceEndpoint)
	}
	if d.sampleRatio != 1 {
		t.Fatalf("default sample ratio = %v, want 1", d.sampleRatio)
	}

	// option-driven
	o := New(WithHealthChecks(false), WithMetrics(false), WithTracing(false),
		WithBind("127.0.0.1:9999"), WithHealthCheckTimeout(time.Second),
		WithTraceEndpoint("collector:4317"), WithServiceName("myapp"))
	if o.healthChecks || o.metrics || o.tracing || firstBind(o) != "127.0.0.1:9999" ||
		o.healthCheckTimeout != time.Second || o.traceEndpoint != "collector:4317" || o.serviceName != "myapp" {
		t.Fatalf("options not applied: health:%v metrics:%v tracing:%v %q %v %q %q",
			o.healthChecks, o.metrics, o.tracing, firstBind(o), o.healthCheckTimeout, o.traceEndpoint, o.serviceName)
	}

	// loaded config wins; explicit false values are honored
	c := New(WithConfig(ObservabilityConfig{
		HealthChecks:          boolPtr(false),
		Metrics:               boolPtr(false),
		Tracing:               boolPtr(false),
		Bind:                  Bind{"127.0.0.1:1234"},
		HealthCheckTimeoutSec: 5,
		TraceEndpoint:         "collector:4317",
		ServiceName:           "svc",
		TraceSampleRatio:      floatPtr(0.1),
	}))
	if c.healthChecks || c.metrics || c.tracing {
		t.Fatal("explicit false values must disable the features")
	}
	if firstBind(c) != "127.0.0.1:1234" || c.healthCheckTimeout != 5*time.Second ||
		c.traceEndpoint != "collector:4317" || c.serviceName != "svc" || c.sampleRatio != 0.1 {
		t.Fatalf("config overlay = %q %v %q %q", firstBind(c), c.healthCheckTimeout, c.traceEndpoint, c.serviceName)
	}

	// option-set values survive when the loaded config omits the field
	m := New(WithBind("127.0.0.1:7777"), WithServiceName("opt"), WithConfig(ObservabilityConfig{}))
	if firstBind(m) != "127.0.0.1:7777" || m.serviceName != "opt" {
		t.Fatalf("empty config must keep option-set values, got %q %q", firstBind(m), m.serviceName)
	}
}

func TestHealthChecksDisabled(t *testing.T) {
	for _, o := range []*Observability{
		New(WithHealthChecks(false), WithMetrics(false)),
		New(WithConfig(ObservabilityConfig{HealthChecks: boolPtr(false), Metrics: boolPtr(false)})),
	} {
		fw := newTestFW(t, o)
		initFW(t, fw)
		if addr := o.Address(); addr != "" {
			t.Fatalf("endpoints disabled but server bound on %q", addr)
		}
	}
}

func TestReadinessAggregatesHealthProviders(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	ok := &testProvider{name: "ok"}
	bad := &testProvider{name: "bad"}
	bad.setErr(errors.New("down"))
	plain := &plainComp{name: "plain"}
	fw := newTestFW(t, o, ok, bad, plain)
	initFW(t, fw)
	startHTTP(t, o)

	base := "http://" + o.Address()

	// liveness is independent of component health
	code, body := get(t, base+"/healthz")
	if code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("/healthz = %d %q, want 200 ok", code, body)
	}
	if code, _ := get(t, base+"/livez"); code != http.StatusOK {
		t.Fatalf("/livez = %d, want 200", code)
	}

	// readiness aggregates: bad fails, ok passes, plain is not included
	code, body = get(t, base+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with a failing provider = %d, want 503", code)
	}
	if !strings.Contains(body, "bad") || strings.Contains(body, "plain") {
		t.Fatalf("/readyz body = %q, want failure for bad only", body)
	}

	// recovery flips readiness back to 200
	bad.setErr(nil)
	code, body = get(t, base+"/readyz")
	if code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("/readyz after recovery = %d %q, want 200 ok", code, body)
	}
}

func TestReadinessTimesOutSlowProviders(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"), WithHealthCheckTimeout(100*time.Millisecond))
	slow := &testProvider{name: "slow"}
	slow.mu.Lock()
	slow.health = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return nil
		}
	}
	slow.mu.Unlock()
	fw := newTestFW(t, o, slow)
	initFW(t, fw)
	startHTTP(t, o)

	start := time.Now()
	code, body := get(t, "http://"+o.Address()+"/readyz")
	elapsed := time.Since(start)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with a hung provider = %d, want 503", code)
	}
	if !strings.Contains(body, "timed_out") {
		t.Fatalf("/readyz body = %q, want a timeout report", body)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("readiness took %v, want it bounded by the check timeout", elapsed)
	}
}

func TestShutdownStopsServer(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	startHTTP(t, o)

	base := "http://" + o.Address()
	if code, _ := get(t, base+"/healthz"); code != http.StatusOK {
		t.Fatalf("expected a healthy server before shutdown, got %d", code)
	}

	if err := o.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get(base + "/healthz"); err == nil {
		t.Fatal("expected requests to fail after Shutdown")
	}
	// idempotent
	if err := o.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestInitTwiceIsIdempotent(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	if o.Address() != "" {
		t.Fatal("Init must not bind a listen address")
	}
	if err := o.Init(context.Background(), fw); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if o.Address() != "" {
		t.Fatal("second Init must not bind a listen address")
	}
	startHTTP(t, o)
	addr := o.Address()
	if addr == "" {
		t.Fatal("Run must bind a listen address")
	}
	if err := o.Init(context.Background(), fw); err != nil {
		t.Fatalf("Init after Run: %v", err)
	}
	if o.Address() != addr {
		t.Fatal("Init after Run must not rebind the server")
	}
}

func TestLivenessDoesNotFailOnComponentHealth(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	bad := &testProvider{name: "bad"}
	bad.setErr(errors.New("down"))
	fw := newTestFW(t, o, bad)
	initFW(t, fw)
	startHTTP(t, o)

	code, body := get(t, "http://"+o.Address()+"/healthz")
	if code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("/healthz with an unhealthy component = %d %q, want 200", code, body)
	}
}

func TestMetricsEndpointLazyPickup(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	mp := &testMetricsProvider{plainComp: &plainComp{name: "app"}}
	fw := newTestFW(t, o, mp)
	initFW(t, fw)
	startHTTP(t, o)

	base := "http://" + o.Address()

	// not initialized: no component samples, but Go runtime metrics present
	code, body := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	if strings.Contains(body, "app_info") {
		t.Fatal("not-yet-initialized provider must be skipped")
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Fatal("Go runtime metrics must be present")
	}

	// initialized: the sample appears on the next scrape (lazy pickup)
	mp.setReady(true)
	code, body = get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics after pickup = %d, want 200", code)
	}
	if !strings.Contains(body, `app_info{mode="prod"} 1`) {
		t.Fatalf("/metrics after pickup body missing the app_info sample:\n%s", body)
	}
}

// testCounterProvider emits both gauge and counter metrics.
type testCounterProvider struct {
	*plainComp
}

func (m *testCounterProvider) Metrics() []Metric {
	return []Metric{
		{
			Name:   "app_info",
			Help:   "App state.",
			Value:  1,
			Labels: map[string]string{"mode": "prod"},
		},
		{
			Name:   "app_events_total",
			Help:   "Total events processed.",
			Value:  42,
			Labels: map[string]string{"kind": "request"},
			Type:   MetricTypeCounter,
		},
	}
}

func TestMetricsCounterTypeScrapedAsCounter(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	cp := &testCounterProvider{plainComp: &plainComp{name: "app"}}
	fw := newTestFW(t, o, cp)
	initFW(t, fw)
	startHTTP(t, o)

	base := "http://" + o.Address()
	code, body := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	if !strings.Contains(body, `app_info{mode="prod"} 1`) {
		t.Fatalf("/metrics missing gauge sample:\n%s", body)
	}
	if !strings.Contains(body, `app_events_total{kind="request"} 42`) {
		t.Fatalf("/metrics missing counter sample:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE app_events_total counter") {
		t.Fatalf("/metrics counter not declared as TYPE counter:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE app_info gauge") {
		t.Fatalf("/metrics gauge not declared as TYPE gauge:\n%s", body)
	}
}

func TestMetricsDisabled(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"), WithMetrics(false))
	fw := newTestFW(t, o)
	initFW(t, fw)
	startHTTP(t, o)

	if code, _ := get(t, "http://"+o.Address()+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("/metrics with metrics disabled = %d, want 404", code)
	}
	if code, _ := get(t, "http://"+o.Address()+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz unaffected by metrics being disabled = %d, want 200", code)
	}
}

func TestMetricsServedWithoutHealthChecks(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"), WithHealthChecks(false))
	fw := newTestFW(t, o)
	initFW(t, fw)
	startHTTP(t, o)

	if o.Address() == "" {
		t.Fatal("server should still bind for /metrics when health checks are off")
	}
	if code, body := get(t, "http://"+o.Address()+"/metrics"); code != http.StatusOK ||
		!strings.Contains(body, "go_goroutines") {
		t.Fatalf("/metrics = %d, want 200 with Go metrics", code)
	}
	if code, _ := get(t, "http://"+o.Address()+"/healthz"); code != http.StatusNotFound {
		t.Fatalf("/healthz with health checks disabled = %d, want 404", code)
	}
}

func TestTracingDisabledWithoutEndpoint(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	if o.TracerProvider() != nil {
		t.Fatal("TracerProvider should be nil when no endpoint is configured")
	}
}

func TestTracingExplicitlyDisabled(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"), WithTracing(false), WithTraceEndpoint("127.0.0.1:4317"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	if o.TracerProvider() != nil {
		t.Fatal("TracerProvider should be nil when tracing is disabled")
	}
}

func TestTracingEnabledWithEndpoint(t *testing.T) {
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	o := New(WithBind("127.0.0.1:0"), WithTracing(true), WithTraceEndpoint("127.0.0.1:1"), WithTraceInsecure(true))
	fw := newTestFW(t, o)
	initFW(t, fw)

	tp := o.TracerProvider()
	if tp == nil {
		t.Fatal("TracerProvider should be configured when tracing is enabled and an endpoint is set")
	}
	if otel.GetTracerProvider() != tp {
		t.Fatal("observability must install its tracer provider globally")
	}
}

func TestTracingRejectsInvalidSampleRatio(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"), WithTracing(true), WithTraceEndpoint("127.0.0.1:1"),
		WithTraceInsecure(true), WithTraceSampleRatio(1.5))
	fw := newTestFW(t, o)
	if err := fw.Initialize(context.Background()); err == nil {
		t.Fatal("Init should reject trace_sample_ratio outside 0–1")
	}
}

func TestSamplerForRatio(t *testing.T) {
	if _, err := samplerForRatio(-0.1); err == nil {
		t.Fatal("want error for negative ratio")
	}
	if _, err := samplerForRatio(1.1); err == nil {
		t.Fatal("want error for ratio > 1")
	}
	if _, err := samplerForRatio(0); err != nil {
		t.Fatalf("0 is valid: %v", err)
	}
	if _, err := samplerForRatio(1); err != nil {
		t.Fatalf("1 is valid: %v", err)
	}
}

func TestInitDoesNotListen(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	if addr := o.Address(); addr != "" {
		t.Fatalf("Init bound %q; listen belongs in Run", addr)
	}
}

func TestRunBeforeInit(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Run before Init") {
		t.Fatalf("Run before Init = %v, want Run before Init", err)
	}
}

// jobTarget is a one-shot JobRunner used to prove the job path Inits
// observability (bootstrap stage) but never starts its Runnable.
type jobTarget struct {
	*plainComp
	obs  *Observability
	addr string
}

func (j *jobTarget) RunJob(context.Context, string) error {
	j.addr = j.obs.Address()
	return nil
}

func TestJobPathDoesNotListen(t *testing.T) {
	o := New(WithBind("127.0.0.1:0"))
	target := &jobTarget{plainComp: &plainComp{name: "migrator"}, obs: o}
	fw := newTestFW(t, o, target)
	if err := fw.RunJob(context.Background(), "migrator", "migrate"); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if target.addr != "" {
		t.Fatalf("job path bound observability at %q; jobs must not start Runnables", target.addr)
	}
	if o.Address() != "" {
		t.Fatalf("observability still bound %q after job Shutdown", o.Address())
	}
}
