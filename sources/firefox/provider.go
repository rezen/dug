package firefox

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         "firefox",
		Registries: []registry.Definition{{ID: "firefox", Kind: "addons", Stability: registry.Stable}},
	}
}
