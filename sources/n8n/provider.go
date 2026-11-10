package n8n

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter: one catalog of
// npm packages, selected by keyword.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         Identity,
		Registries: []registry.Definition{{ID: Identity, Kind: "packages", Stability: registry.Beta}},
	}
}
