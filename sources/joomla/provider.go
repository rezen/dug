package joomla

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter.
// Kind names the catalog within the provider.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         Identity,
		Registries: []registry.Definition{{ID: Identity, Kind: "extensions", Stability: registry.Beta}},
	}
}
