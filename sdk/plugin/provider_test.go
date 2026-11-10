package plugin_test

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"connectrpc.com/connect"
	registryv1 "github.com/rezen/dug/sdk/gen/dug/registry/v1"
	"github.com/rezen/dug/sdk/gen/dug/registry/v1/registryv1connect"
	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
)

func TestProviderHandshakePreservesCatalogIdentity(t *testing.T) {
	for _, id := range []string{"wordpress", "wordpress-themes"} {
		t.Run(id, func(t *testing.T) {
			provider := registry.Provider{ID: "wordpress", Registries: []registry.Definition{
				{ID: "wordpress", Kind: "plugins", Stability: registry.Stable}, {ID: "wordpress-themes", Kind: "themes", Stability: registry.Beta},
			}}
			expected := provider.Clone()
			handler, err := plugin.NewHandler(plugin.ServerOptions{
				Registry: id, Provider: provider, Version: "1.2.3",
				Factory: func(map[string]string) (registry.Registry, error) { return nil, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			provider.Registries[0].Kind = "changed"
			server := httptest.NewServer(handler)
			defer server.Close()
			client, err := plugin.NewClient(context.Background(), server.Client(), server.URL, plugin.ClientOptions{Registry: id})
			if err != nil {
				t.Fatal(err)
			}
			if actual := client.Provider(); !reflect.DeepEqual(actual, expected) {
				t.Fatalf("provider metadata changed during handshake: %+v", actual)
			}
			if client.SourceVersion() != "1.2.3" {
				t.Fatalf("source version %q", client.SourceVersion())
			}
			metadata := client.Provider()
			metadata.Registries[0].Kind = "changed"
			if !reflect.DeepEqual(client.Provider(), expected) {
				t.Fatal("client exposes mutable handshake state")
			}
			other := "wordpress"
			if other == id {
				other = "wordpress-themes"
			}
			if _, err := plugin.NewClient(context.Background(), server.Client(), server.URL, plugin.ClientOptions{Registry: other}); err == nil {
				t.Fatal("sharing a provider must not make different registries interchangeable")
			}
		})
	}
}

func TestHandlerRejectsUndeclaredRegistry(t *testing.T) {
	_, err := plugin.NewHandler(plugin.ServerOptions{
		Registry: "wordpress-themes",
		Provider: registry.Provider{ID: "wordpress", Registries: []registry.Definition{{ID: "wordpress", Kind: "plugins"}}},
		Factory:  func(map[string]string) (registry.Registry, error) { return nil, nil },
	})
	if err == nil {
		t.Fatal("handler accepted a registry missing from its provider")
	}
}

type providerInfoServer struct {
	registryv1connect.UnimplementedRegistryServiceHandler
	provider *registryv1.Provider
}

func (server providerInfoServer) Info(context.Context, *connect.Request[registryv1.InfoRequest]) (*connect.Response[registryv1.InfoResponse], error) {
	return connect.NewResponse(&registryv1.InfoResponse{Registry: "test", ProtocolVersion: plugin.ProtocolVersion, Provider: server.provider}), nil
}

func TestClientValidatesOptionalProviderMetadata(t *testing.T) {
	for _, test := range []struct {
		name      string
		provider  *registryv1.Provider
		wantError bool
	}{
		{name: "legacy plugin"},
		{name: "empty metadata", provider: &registryv1.Provider{}, wantError: true},
		{name: "wrong registry", provider: &registryv1.Provider{Id: "example", Registries: []*registryv1.RegistryDefinition{{Id: "other", Kind: "packages"}}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, handler := registryv1connect.NewRegistryServiceHandler(providerInfoServer{provider: test.provider})
			server := httptest.NewServer(handler)
			defer server.Close()
			client, err := plugin.NewClient(context.Background(), server.Client(), server.URL, plugin.ClientOptions{Registry: "test"})
			if (err != nil) != test.wantError {
				t.Fatalf("handshake error: %v", err)
			}
			if !test.wantError && client.Provider().ID != "" {
				t.Fatal("legacy provider must remain unknown, not inferred from registry ID")
			}
		})
	}
}
