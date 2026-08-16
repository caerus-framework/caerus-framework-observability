package cf_observability

import (
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
		Sample:    ObservabilityConfig{},
	}}, nil
}
