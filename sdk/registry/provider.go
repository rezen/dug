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
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Stability Stability `json:"stability,omitempty"`
}

// Stability says how far callers can rely on a catalog's adapter: its options,
// the facts it fills, and the shape of what it enumerates.
type Stability string

const (
	// Experimental adapters may change options, facts or coverage without notice.
	Experimental Stability = "experimental"
	// Beta adapters work end to end but are young or have known coverage gaps.
	Beta Stability = "beta"
	// Stable adapters keep their options and facts compatible between releases.
	Stable Stability = "stable"
)

// Valid reports whether s is a declared level. Empty is not valid here; older
// plugins that predate the field are tolerated by Provider.Validate instead.
func (s Stability) Valid() bool { return s == Experimental || s == Beta || s == Stable }

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
		if definition.Stability != "" && !definition.Stability.Valid() {
			return fmt.Errorf("provider %q: registry %q has unknown stability %q", p.ID, definition.ID, definition.Stability)
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
