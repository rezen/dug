package helm

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter.
// Kind names the catalog within the provider; Artifact Hub's helm kind is a
// package catalog.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         Identity,
		Registries: []registry.Definition{{ID: Identity, Kind: "packages", Stability: registry.Beta}},
	}
}
