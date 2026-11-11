package bitbucket

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         Identity,
		Registries: []registry.Definition{{ID: Identity, Kind: "repositories", Stability: registry.Experimental}},
	}
}
