package wordpress

import "testing"

func TestProviderDeclaresSeparateCatalogs(t *testing.T) {
	provider := Provider()
	if err := provider.Validate(); err != nil {
		t.Fatal(err)
	}
	if provider.ID != "wordpress" || len(provider.Registries) != 2 {
		t.Fatalf("unexpected provider: %+v", provider)
	}
	for _, kind := range []Kind{Plugins, Themes} {
		definition, ok := provider.Registry(kind.Registry())
		if !ok || definition.Kind != string(kind) {
			t.Fatalf("catalog %q does not match adapter identity: %+v", kind, definition)
		}
	}
	if Plugins.Registry() != "wordpress" || Themes.Registry() != "wordpress-themes" {
		t.Fatal("persisted registry identities must remain stable")
	}
	provider.Registries[0].ID = "changed"
	if Provider().Registries[0].ID != "wordpress" {
		t.Fatal("provider metadata must not be shared mutable state")
	}
}
