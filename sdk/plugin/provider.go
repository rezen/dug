package plugin

import (
	"fmt"

	registryv1 "github.com/rezen/dug/sdk/gen/dug/registry/v1"
	"github.com/rezen/dug/sdk/registry"
)

func validateProvider(provider registry.Provider, id string) error {
	// Metadata is additive; legacy plugins can still serve a registry by ID.
	if provider.ID == "" && len(provider.Registries) == 0 {
		return nil
	}
	if err := provider.Validate(); err != nil {
		return err
	}
	if _, ok := provider.Registry(id); !ok {
		return fmt.Errorf("registry %q is not declared by provider %q", id, provider.ID)
	}
	return nil
}

func providerToWire(provider registry.Provider) *registryv1.Provider {
	if provider.ID == "" {
		return nil
	}
	wire := &registryv1.Provider{Id: provider.ID}
	for _, definition := range provider.Registries {
		wire.Registries = append(wire.Registries, &registryv1.RegistryDefinition{Id: definition.ID, Kind: definition.Kind, Stability: string(definition.Stability)})
	}
	return wire
}

func providerFromWire(wire *registryv1.Provider) registry.Provider {
	provider := registry.Provider{ID: wire.GetId()}
	for _, definition := range wire.GetRegistries() {
		provider.Registries = append(provider.Registries, registry.Definition{ID: definition.GetId(), Kind: definition.GetKind(), Stability: registry.Stability(definition.GetStability())})
	}
	return provider
}
