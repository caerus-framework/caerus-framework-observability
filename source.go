package cf_observability

import (
	"fmt"

	cf "github.com/caerus-framework/caerus-framework"
)

var _ cf.CoreConfigSource = (*Observability)(nil)

// CoreConfigSource implements cf.CoreConfigSource. Observability imports the
// configuration module (Lookup at Init). Logs cannot (configuration imports
// logs); this package does. The framework still collects the declaration
// during argv absorption so ParseFlags sees --observability and OBSERVABILITY_
// before Init.
func (c *Observability) CoreConfigSource() ([]cf.ConfigSourceValue, error) {
	name := c.configSource
	if name == "" {
		return nil, nil
	}
	return []cf.ConfigSourceValue{{
		Name:      name,
		Path:      "config/" + name + ".json",
		Format:    "json",
		EnvPrefix: "OBSERVABILITY_",
		Owner:     ComponentName,
		Validate:  validateObservabilityConfigValue,
		Sample:    ObservabilityConfig{},
	}}, nil
}

func validateObservabilityConfigValue(v any) error {
	cfg, ok := v.(*ObservabilityConfig)
	if !ok {
		return fmt.Errorf("cf_observability: validate config: unexpected type %T", v)
	}
	if cfg.TraceSampleRatio != nil {
		if _, err := samplerForRatio(*cfg.TraceSampleRatio); err != nil {
			return err
		}
	}
	return nil
}
