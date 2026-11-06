package github

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         "github",
		Registries: []registry.Definition{{ID: "github", Kind: "repositories"}},
	}
}
