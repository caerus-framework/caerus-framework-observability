package cf_observability

import (
	"log/slog"
	"sort"
	"strconv"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	"github.com/prometheus/client_golang/prometheus"
)

// MetricType distinguishes how a Metric sample is scraped. The zero value
// (MetricTypeGauge) preserves backward compatibility with existing samples.
type MetricType int

const (
	// MetricTypeGauge is the default. The sample is emitted as a Prometheus
	// gauge (current value, goes up and down).
	MetricTypeGauge MetricType = iota
	// MetricTypeCounter marks a monotonically increasing counter. The sample
	// is emitted as a Prometheus counter (only resets on process restart).
	// Components must ensure counter values only increase for the process
	// lifetime.
	MetricTypeCounter
)

// Metric is one runtime-state sample a component exposes for the /metrics
// endpoint. The observability component serves the sample under Name as-is
// (Name "logs_info" appears in /metrics as "logs_info").
type Metric struct {
	// Name is the metric name served on /metrics. It must not be empty for
	// the sample to be emitted.
	Name string
	// Help describes the metric for /metrics consumers.
	Help string
	// Value is the sample value. State/info samples typically use 1 and carry
	// their meaning in Labels.
	Value float64
	// Labels annotate the sample, e.g. {"format": "json", "level": "info"}.
	Labels map[string]string
	// Type selects how the sample is scraped. The zero value (MetricTypeGauge)
	// preserves the pre-existing behavior; set MetricTypeCounter for
	// monotonically increasing event counts.
	Type MetricType
}

// MetricsProvider is an optional interface for components that expose runtime
// state as metrics. The observability component discovers components
// implementing it and calls Metrics on every /metrics scrape, so the values
// are always live: a component that is not initialized yet returns nil and is
// skipped until it does — a lazy pickup that needs no subscription. A
// component that does not implement MetricsProvider contributes nothing to
// /metrics.
//
// Bootstrap components (logs, configuration) do not implement this interface
// to avoid import cycles; observability scrapes their state directly via
// cf.Get + exported state helpers.
type MetricsProvider interface {
	// Metrics returns the component's current runtime state. Return nil while
	// the component is not initialized or has nothing to report.
	Metrics() []Metric
}

// logsMetricsCollector scrapes the logs component's state for /metrics.
// Bootstrap components cannot implement MetricsProvider (import cycle), so
// observability reads their state directly.
type logsMetricsCollector struct {
	logs *cf_logs.Logs
}

func (c *logsMetricsCollector) Describe(ch chan<- *prometheus.Desc) {}

func (c *logsMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	defer recoverCollect("logs")
	ms := []Metric{{
		Name:  "logs_info",
		Help:  "Current logging configuration.",
		Value: 1,
		Labels: map[string]string{
			"format":        c.logs.Format().String(),
			"level":         c.logs.Level().String(),
			"report_caller": strconv.FormatBool(c.logs.ReportCaller()),
			"stack_traces":  strconv.FormatBool(c.logs.StackTraces()),
			"stack_level":   c.logs.StackLevel().String(),
		},
	}}
	for name, lv := range c.logs.Overrides() {
		ms = append(ms, Metric{
			Name:  "logs_component_level",
			Help:  "Per-component log level override (SetLevelFor).",
			Value: 1,
			Labels: map[string]string{
				"component": name,
				"level":     lv.String(),
			},
		})
	}
	for _, m := range ms {
		emitMetric(ch, m)
	}
}

// configurationMetricsCollector scrapes the configuration component's state
// for /metrics. Bootstrap components cannot implement MetricsProvider (import
// cycle), so observability reads their state directly.
type configurationMetricsCollector struct {
	conf *cf_configuration.Configuration
}

func (c *configurationMetricsCollector) Describe(ch chan<- *prometheus.Desc) {}

func (c *configurationMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	defer recoverCollect("configuration")
	for _, sample := range c.conf.MetricSamples() {
		emitMetric(ch, Metric{
			Name:   sample.Name,
			Help:   sample.Help,
			Value:  sample.Value,
			Labels: sample.Labels,
		})
	}
}

func emitMetric(ch chan<- prometheus.Metric, m Metric) {
	if m.Name == "" {
		return
	}
	defer recoverCollect(m.Name)
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

func recoverCollect(name string) {
	if rec := recover(); rec != nil {
		slog.Error("cf_observability: skipped bad metric sample", "collector", name, "panic", rec)
	}
}

var _ cf.CaerusComponent = (*cf_logs.Logs)(nil)
var _ cf.CaerusComponent = (*cf_configuration.Configuration)(nil)
