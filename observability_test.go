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

func boolPtr(b bool) *bool { return &b }

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
}

func TestConfigOverlay(t *testing.T) {
	// defaults: health + metrics enabled, tracing off, :9090, 2s, service caerus
	d := New()
	if !d.healthChecks || !d.metrics || d.tracing || d.bindAddress != ":9090" ||
		d.healthCheckTimeout != 2*time.Second || d.serviceName != "caerus" || d.traceEndpoint != "" {
		t.Fatalf("defaults = health:%v metrics:%v tracing:%v %q %v service:%q endpoint:%q",
			d.healthChecks, d.metrics, d.tracing, d.bindAddress, d.healthCheckTimeout, d.serviceName, d.traceEndpoint)
	}

	// option-driven
	o := New(WithHealthChecks(false), WithMetrics(false), WithTracing(false),
		WithAddress("127.0.0.1:9999"), WithHealthCheckTimeout(time.Second),
		WithTraceEndpoint("collector:4317"), WithServiceName("myapp"))
	if o.healthChecks || o.metrics || o.tracing || o.bindAddress != "127.0.0.1:9999" ||
		o.healthCheckTimeout != time.Second || o.traceEndpoint != "collector:4317" || o.serviceName != "myapp" {
		t.Fatalf("options not applied: health:%v metrics:%v tracing:%v %q %v %q %q",
			o.healthChecks, o.metrics, o.tracing, o.bindAddress, o.healthCheckTimeout, o.traceEndpoint, o.serviceName)
	}

	// loaded config wins; explicit false values are honored
	c := New(WithConfig(ObservabilityConfig{
		HealthChecks:          boolPtr(false),
		Metrics:               boolPtr(false),
		Tracing:               boolPtr(false),
		Address:               "127.0.0.1:1234",
		HealthCheckTimeoutSec: 5,
		TraceEndpoint:         "collector:4317",
		ServiceName:           "svc",
	}))
	if c.healthChecks || c.metrics || c.tracing {
		t.Fatal("explicit false values must disable the features")
	}
	if c.bindAddress != "127.0.0.1:1234" || c.healthCheckTimeout != 5*time.Second ||
		c.traceEndpoint != "collector:4317" || c.serviceName != "svc" {
		t.Fatalf("config overlay = %q %v %q %q", c.bindAddress, c.healthCheckTimeout, c.traceEndpoint, c.serviceName)
	}

	// option-set values survive when the loaded config omits the field
	m := New(WithAddress("127.0.0.1:7777"), WithServiceName("opt"), WithConfig(ObservabilityConfig{}))
	if m.bindAddress != "127.0.0.1:7777" || m.serviceName != "opt" {
		t.Fatalf("empty config must keep option-set values, got %q %q", m.bindAddress, m.serviceName)
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
	o := New(WithAddress("127.0.0.1:0"))
	ok := &testProvider{name: "ok"}
	bad := &testProvider{name: "bad"}
	bad.setErr(errors.New("down"))
	plain := &plainComp{name: "plain"}
	fw := newTestFW(t, o, ok, bad, plain)
	initFW(t, fw)

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
	o := New(WithAddress("127.0.0.1:0"), WithHealthCheckTimeout(100*time.Millisecond))
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

	start := time.Now()
	code, body := get(t, "http://"+o.Address()+"/readyz")
	elapsed := time.Since(start)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with a hung provider = %d, want 503", code)
	}
	if !strings.Contains(body, "timed out") {
		t.Fatalf("/readyz body = %q, want a timeout report", body)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("readiness took %v, want it bounded by the check timeout", elapsed)
	}
}

func TestShutdownStopsServer(t *testing.T) {
	o := New(WithAddress("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)

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
	o := New(WithAddress("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	addr := o.Address()
	if err := o.Init(context.Background(), fw); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if o.Address() != addr {
		t.Fatal("second Init must not rebind the server")
	}
}

func TestLivenessDoesNotFailOnComponentHealth(t *testing.T) {
	o := New(WithAddress("127.0.0.1:0"))
	bad := &testProvider{name: "bad"}
	bad.setErr(errors.New("down"))
	fw := newTestFW(t, o, bad)
	initFW(t, fw)

	code, body := get(t, "http://"+o.Address()+"/healthz")
	if code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("/healthz with an unhealthy component = %d %q, want 200", code, body)
	}
}

func TestMetricsEndpointLazyPickup(t *testing.T) {
	o := New(WithAddress("127.0.0.1:0"))
	mp := &testMetricsProvider{plainComp: &plainComp{name: "app"}}
	fw := newTestFW(t, o, mp)
	initFW(t, fw)

	base := "http://" + o.Address()

	// not initialized: no component samples, but Go runtime metrics present
	code, body := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	if strings.Contains(body, "caerus_app_info") {
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
	if !strings.Contains(body, `caerus_app_info{mode="prod"} 1`) {
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
	o := New(WithAddress("127.0.0.1:0"))
	cp := &testCounterProvider{plainComp: &plainComp{name: "app"}}
	fw := newTestFW(t, o, cp)
	initFW(t, fw)

	base := "http://" + o.Address()
	code, body := get(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	if !strings.Contains(body, `caerus_app_info{mode="prod"} 1`) {
		t.Fatalf("/metrics missing gauge sample:\n%s", body)
	}
	if !strings.Contains(body, `caerus_app_events_total{kind="request"} 42`) {
		t.Fatalf("/metrics missing counter sample:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE caerus_app_events_total counter") {
		t.Fatalf("/metrics counter not declared as TYPE counter:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE caerus_app_info gauge") {
		t.Fatalf("/metrics gauge not declared as TYPE gauge:\n%s", body)
	}
}

func TestMetricsDisabled(t *testing.T) {
	o := New(WithAddress("127.0.0.1:0"), WithMetrics(false))
	fw := newTestFW(t, o)
	initFW(t, fw)

	if code, _ := get(t, "http://"+o.Address()+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("/metrics with metrics disabled = %d, want 404", code)
	}
	if code, _ := get(t, "http://"+o.Address()+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz unaffected by metrics being disabled = %d, want 200", code)
	}
}

func TestMetricsServedWithoutHealthChecks(t *testing.T) {
	o := New(WithAddress("127.0.0.1:0"), WithHealthChecks(false))
	fw := newTestFW(t, o)
	initFW(t, fw)

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
	o := New(WithAddress("127.0.0.1:0"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	if o.TracerProvider() != nil {
		t.Fatal("TracerProvider should be nil when no endpoint is configured")
	}
}

func TestTracingExplicitlyDisabled(t *testing.T) {
	o := New(WithAddress("127.0.0.1:0"), WithTracing(false), WithTraceEndpoint("127.0.0.1:4317"))
	fw := newTestFW(t, o)
	initFW(t, fw)
	if o.TracerProvider() != nil {
		t.Fatal("TracerProvider should be nil when tracing is disabled")
	}
}

func TestTracingEnabledWithEndpoint(t *testing.T) {
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	o := New(WithAddress("127.0.0.1:0"), WithTracing(true), WithTraceEndpoint("127.0.0.1:1"))
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
