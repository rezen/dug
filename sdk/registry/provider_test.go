package registry

import "testing"

func TestProviderCatalogs(t *testing.T) {
	provider := Provider{ID: "wordpress", Registries: []Definition{
		{ID: "wordpress", Kind: "plugins"}, {ID: "wordpress-themes", Kind: "themes"},
	}}
	if err := provider.Validate(); err != nil {
		t.Fatal(err)
	}
	definition, ok := provider.Registry("wordpress-themes")
	if !ok || definition.Kind != "themes" {
		t.Fatalf("theme registry: %+v, %v", definition, ok)
	}
	if _, ok := provider.Registry("themes"); ok {
		t.Fatal("kind must not be treated as a global registry ID")
	}
	copy := provider.Clone()
	copy.Registries[0].ID = "changed"
	if provider.Registries[0].ID != "wordpress" {
		t.Fatal("cloned provider shares mutable definitions")
	}
}

func TestInvalidProviders(t *testing.T) {
	for _, provider := range []Provider{
		{},
		{ID: "wordpress"},
		{Registries: []Definition{{ID: "wordpress", Kind: "plugins"}}},
		{ID: "wordpress", Registries: []Definition{{ID: "wordpress"}}},
		{ID: "wordpress", Registries: []Definition{{Kind: "plugins"}}},
		{ID: "wordpress", Registries: []Definition{{ID: "wordpress", Kind: "plugins"}, {ID: "wordpress", Kind: "themes"}}},
		{ID: "wordpress", Registries: []Definition{{ID: "wordpress", Kind: "plugins"}, {ID: "wordpress-themes", Kind: "plugins"}}},
		{ID: "wordpress", Registries: []Definition{{ID: "wordpress", Kind: "plugins", Stability: "mostly-done"}}},
	} {
		if err := provider.Validate(); err == nil {
			t.Errorf("accepted invalid provider %+v", provider)
		}
	}
}
