package cf_observability

import (
	"encoding/json"

	cf "github.com/caerus-framework/caerus-framework"
)

// frameworkSeed is the subset of cf.ObservabilitySettings this factory
// understands. Fields are copied with encoding/json so the factory compiles
// against both GitHub main (field Address, no trace seeds) and a core that
// has Bind / TraceEndpoint / TraceInsecure / TraceSampleRatio. Direct field
// access on *cf.ObservabilitySettings would not compile until that core is
// on the default branch CI clones.
type frameworkSeed struct {
	Bind             string
	Address          string
	HealthChecks     *bool
	Metrics          *bool
	Tracing          *bool
	TraceEndpoint    string
	TraceInsecure    bool
	TraceSampleRatio *float64
	ServiceName      string
	ConfigSource     string
}

func seedFromSettings(settings *cf.ObservabilitySettings) frameworkSeed {
	if settings == nil {
		return frameworkSeed{}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return frameworkSeed{}
	}
	var seed frameworkSeed
	_ = json.Unmarshal(raw, &seed)
	return seed
}

func init() {
	cf.RegisterObservabilityFactory(func(settings *cf.ObservabilitySettings) (cf.CaerusComponent, error) {
		seed := seedFromSettings(settings)
		var opts []Option
		bind := seed.Bind
		if bind == "" {
			bind = seed.Address
		}
		if bind != "" {
			opts = append(opts, WithBind(bind))
		}
		if seed.HealthChecks != nil {
			opts = append(opts, WithHealthChecks(*seed.HealthChecks))
		}
		if seed.Metrics != nil {
			opts = append(opts, WithMetrics(*seed.Metrics))
		}
		if seed.Tracing != nil {
			opts = append(opts, WithTracing(*seed.Tracing))
		}
		if seed.TraceEndpoint != "" {
			opts = append(opts, WithTraceEndpoint(seed.TraceEndpoint))
		}
		if seed.TraceInsecure {
			opts = append(opts, WithTraceInsecure(true))
		}
		if seed.TraceSampleRatio != nil {
			opts = append(opts, WithTraceSampleRatio(*seed.TraceSampleRatio))
		}
		if seed.ServiceName != "" {
			opts = append(opts, WithServiceName(seed.ServiceName))
		}
		if seed.ConfigSource != "" {
			opts = append(opts, WithConfigSource(seed.ConfigSource))
		}
		return New(opts...), nil
	})
}
