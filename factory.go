package cf_observability

import (
	cf "github.com/caerus-framework/caerus-framework"
)

func init() {
	cf.RegisterObservabilityFactory(func(settings *cf.ObservabilitySettings) (cf.CaerusComponent, error) {
		var opts []Option
		if settings != nil {
			if settings.Bind != "" {
				opts = append(opts, WithBind(settings.Bind))
			}
			if settings.HealthChecks != nil {
				opts = append(opts, WithHealthChecks(*settings.HealthChecks))
			}
			if settings.Metrics != nil {
				opts = append(opts, WithMetrics(*settings.Metrics))
			}
			if settings.Tracing != nil {
				opts = append(opts, WithTracing(*settings.Tracing))
			}
			if settings.TraceEndpoint != "" {
				opts = append(opts, WithTraceEndpoint(settings.TraceEndpoint))
			}
			if settings.TraceInsecure {
				opts = append(opts, WithTraceInsecure(true))
			}
			if settings.TraceSampleRatio != nil {
				opts = append(opts, WithTraceSampleRatio(*settings.TraceSampleRatio))
			}
			if settings.ServiceName != "" {
				opts = append(opts, WithServiceName(settings.ServiceName))
			}
			if settings.ConfigSource != "" {
				opts = append(opts, WithConfigSource(settings.ConfigSource))
			}
		}
		return New(opts...), nil
	})
}
