package cf_observability

import (
	cf "github.com/caerus-framework/caerus-framework"
)

// Compile-time assertion: observability is a CoreConfigSource.
var _ cf.CoreConfigSource = (*Observability)(nil)

// CoreConfigSource implements cf.CoreConfigSource. It declares the observability
// component's own configuration source; the observability module cannot import
// the configuration module (the configuration module imports it), so the
// framework discovers it among registered components during argv absorption and
// registers the declaration on the component's behalf.
//
// The source is owned by the component: default file config/<name>.json, owner
// cf_observability. An argv redeclaration wins: the --<name> file-path flag
// ParseFlags registers overrides where the file is read from, and the loaded
// value reaches the component through OnConfigReload (see WithConfigSource).
// No source is declared when WithConfigSource was not given.
func (c *Observability) CoreConfigSource() ([]cf.ConfigSourceValue, error) {
	name := c.configSource
	if name == "" {
		return nil, nil
	}
	return []cf.ConfigSourceValue{{
		Name:   name,
		Path:   "config/" + name + ".json",
		Format: "json",
		Owner:  ComponentName,
		Sample: ObservabilityConfig{},
	}}, nil
}
