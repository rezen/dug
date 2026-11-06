package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net/http"
	"strings"
	"testing"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services"
)

type trackedTransport struct {
	*http.Transport
	closed bool
}

func (transport *trackedTransport) CloseIdleConnections() {
	transport.closed = true
	transport.Transport.CloseIdleConnections()
}

func TestRunConfiguresAndCleansUpClient(t *testing.T) {
	t.Setenv("DUG_PLUGIN_TOKEN", "rpc-secret")
	t.Setenv(services.EnvCacheTTL, "0") // the test inspects the raw transport
	for _, test := range []struct {
		name        string
		args        []string
		address     string
		connections int
		api         string
		serveErr    error
	}{
		{name: "defaults", address: "127.0.0.1:7781", connections: 128, api: "https://example.org"},
		{name: "overrides and server failure", args: []string{"--listen", "127.0.0.1:9999", "--connections", "7", "--api", "https://enterprise.example.org"}, address: "127.0.0.1:9999", connections: 7, api: "https://enterprise.example.org", serveErr: errors.New("server failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var api string
			var transport *trackedTransport
			validated, served := false, false
			command := Command{
				Registry: "test", DefaultAddress: "127.0.0.1:7781",
				Provider: registry.Provider{ID: "example", Registries: []registry.Definition{{ID: "test", Kind: "packages"}}},
				ConfigureFlags: func(flags *flag.FlagSet) {
					flags.StringVar(&api, "api", "https://example.org", "API endpoint")
				},
				Validate: func() error {
					if api != test.api {
						t.Fatalf("validation received api %q", api)
					}
					validated = true
					return nil
				},
				Factory: func(client *http.Client) plugin.Factory {
					if !validated {
						t.Fatal("factory constructed before source validation")
					}
					transport = &trackedTransport{Transport: client.Transport.(*http.Transport)}
					if transport.MaxConnsPerHost != test.connections || client.Timeout != 0 {
						t.Fatal("client lost connection settings or streaming timeout defaults")
					}
					client.Transport = transport
					return func(map[string]string) (registry.Registry, error) { return nil, nil }
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			err := command.run(ctx, test.args, &stderr, func(serverCtx context.Context, address string, options plugin.ServerOptions) error {
				served = true
				if serverCtx != ctx || address != test.address || options.Registry != "test" || options.Token != "rpc-secret" || options.Factory == nil {
					t.Fatal("server did not receive the configured context, address, identity, token, and factory")
				}
				if definition, ok := options.Provider.Registry(options.Registry); !ok || options.Provider.ID != "example" || definition.Kind != "packages" {
					t.Fatal("provider relationship was not forwarded to the server")
				}
				if transport.closed {
					t.Fatal("client closed before serving")
				}
				cancel()
				<-serverCtx.Done()
				return test.serveErr
			})
			if !errors.Is(err, test.serveErr) || !served || transport == nil || !transport.closed {
				t.Fatalf("err=%v served=%v transport=%+v", err, served, transport)
			}
		})
	}
}

func TestRunDoesNotStartOnHelpOrInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		validation error
		canceled   bool
		wantError  bool
	}{
		{name: "help", args: []string{"--help"}},
		{name: "unknown flag", args: []string{"--unknown"}, wantError: true},
		{name: "zero connections", args: []string{"--connections=0"}, wantError: true},
		{name: "negative connections", args: []string{"--connections=-1"}, wantError: true},
		{name: "positional argument", args: []string{"extra"}, wantError: true},
		{name: "empty listen address", args: []string{"--listen="}, wantError: true},
		{name: "source validation", validation: errors.New("invalid api"), wantError: true},
		{name: "canceled context", canceled: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := Command{
				Registry: "test", DefaultAddress: "127.0.0.1:7781",
				ConfigureFlags: func(flags *flag.FlagSet) { flags.String("api", "https://example.org", "API endpoint") },
				Provider:       registry.Provider{ID: "example", Registries: []registry.Definition{{ID: "test", Kind: "packages"}}},
				Validate:       func() error { return test.validation },
				Factory: func(*http.Client) plugin.Factory {
					t.Fatal("unexpected factory construction")
					return nil
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.canceled {
				cancel()
			}
			var stderr bytes.Buffer
			err := command.Run(ctx, test.args, &stderr)
			if (err != nil) != test.wantError {
				t.Fatalf("Run returned %v", err)
			}
			if test.validation != nil && !errors.Is(err, test.validation) {
				t.Fatalf("source validation error was lost: %v", err)
			}
			if test.canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error was lost: %v", err)
			}
			if !test.wantError {
				for _, flagName := range []string{"-listen", "-connections", "-api"} {
					if !strings.Contains(stderr.String(), flagName) {
						t.Errorf("help is missing %s", flagName)
					}
				}
			}
		})
	}
}

func TestRunRequiresDeclaredProviderRegistry(t *testing.T) {
	for _, provider := range []registry.Provider{
		{},
		{ID: "wordpress", Registries: []registry.Definition{{ID: "wordpress", Kind: "plugins"}}},
	} {
		command := Command{
			Registry: "wordpress-themes", Provider: provider, DefaultAddress: "127.0.0.1:7785",
			Factory: func(*http.Client) plugin.Factory {
				t.Fatal("factory created for an undeclared registry")
				return nil
			},
		}
		var stderr bytes.Buffer
		if err := command.Run(context.Background(), nil, &stderr); err == nil {
			t.Fatalf("accepted undeclared registry with provider %+v", provider)
		}
	}
}
