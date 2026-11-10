package wordpress

import "github.com/rezen/dug/sdk/registry"

// Provider describes both WordPress catalogs, preserving their independent
// registry identities. It derives them from the same directory definitions used
// by discovery and resolution so registration cannot invent a separate mapping.
func Provider() registry.Provider {
	provider := registry.Provider{ID: "wordpress"}
	for _, kind := range []Kind{Plugins, Themes} {
		provider.Registries = append(provider.Registries, registry.Definition{
			ID: kind.Registry(), Kind: string(kind), Stability: registry.Stable,
		})
	}
	return provider
}
