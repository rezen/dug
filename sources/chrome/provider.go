package chrome

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         "chrome",
		Registries: []registry.Definition{{ID: "chrome", Kind: "extensions"}},
	}
}
