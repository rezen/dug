package registry

import (
	"fmt"
	"slices"
)

// Provider describes an upstream ecosystem and the registries its adapter
// implements. A plugin endpoint serves exactly one of these registries; this
// list does not imply that the endpoint serves every registry in the provider.
type Provider struct {
	ID         string       `json:"id"`
	Registries []Definition `json:"registries"`
}

// Definition identifies a separately addressable catalog within a provider.
// ID is globally unique and remains the Reference.Registry/storage identity.
// Kind names the catalog within its provider, such as "plugins" or "themes".
// Provider and Kind are declared explicitly, never inferred by splitting IDs.
type Definition struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// Validate checks that every catalog has a distinct identity and kind.
func (p Provider) Validate() error {
	if p.ID == "" || len(p.Registries) == 0 {
		return fmt.Errorf("provider ID and at least one registry are required")
	}
	ids, kinds := make(map[string]bool), make(map[string]bool)
	for _, definition := range p.Registries {
		if definition.ID == "" || definition.Kind == "" {
			return fmt.Errorf("provider %q: registry ID and kind are required", p.ID)
		}
		if ids[definition.ID] || kinds[definition.Kind] {
			return fmt.Errorf("provider %q: duplicate registry ID or kind: %q/%q", p.ID, definition.ID, definition.Kind)
		}
		ids[definition.ID], kinds[definition.Kind] = true, true
	}
	return nil
}

// Registry looks up a catalog by its stable global ID.
func (p Provider) Registry(id string) (Definition, bool) {
	for _, definition := range p.Registries {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

// Clone returns independent metadata that callers may safely modify.
func (p Provider) Clone() Provider {
	p.Registries = slices.Clone(p.Registries)
	return p
}
