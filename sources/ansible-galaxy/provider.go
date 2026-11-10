package ansiblegalaxy

import "github.com/rezen/dug/sdk/registry"

// Provider describes the catalogs implemented by this adapter. Galaxy roles
// are a different catalog (API v1, hosted on GitHub) and are not served here.
func Provider() registry.Provider {
	return registry.Provider{
		ID:         Identity,
		Registries: []registry.Definition{{ID: Identity, Kind: "collections", Stability: registry.Beta}},
	}
}
