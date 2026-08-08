package cf_observability

import (
	cf "github.com/caerus-framework/caerus-framework"
)

// Register the observability core factory so cf.New(FrameworkOptions) can build
// the always-on observability component. init() runs when the module is
// imported, which is guaranteed by any package that uses cf_observability
// symbols.
func init() {
	cf.RegisterObservabilityFactory(func(settings *cf.ObservabilitySettings) (cf.CaerusComponent, error) {
		var opts []Option
		if settings != nil {
			if settings.Address != "" {
				opts = append(opts, WithAddress(settings.Address))
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
