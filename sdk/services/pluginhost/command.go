// Package pluginhost runs standalone source plugins with shared CLI and lifecycle
// behavior. Sources supply their registry identity, address, and factory.
package pluginhost

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/services"
)

// version is the source's release version, stamped at build time with
//
//	-ldflags "-X github.com/rezen/dug/sdk/services/pluginhost.version=1.2.3"
//
// scripts/build.sh derives it from the module's sources/NAME/vX.Y.Z git tag.
var version string

// stdout receives --version output; tests replace it.
var stdout io.Writer = os.Stdout

// Version reports the stamped release version, falling back to the module
// version recorded by `go install`, and to "devel" for untagged local builds.
func Version() string {
	if version != "" {
		return strings.TrimPrefix(version, "v")
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "devel"
}

// Command describes a source executable. Provider must declare Registry.
// ConfigureFlags and Validate are optional
// hooks for source-specific settings; validation runs after all flags are parsed
// and before constructing the factory or starting the server.
type Command struct {
	Registry       string
	Provider       registry.Provider
	DefaultAddress string
	Factory        func(*http.Client) plugin.Factory
	ConfigureFlags func(*flag.FlagSet)
	Validate       func() error
}

// Main runs a source command using process arguments and a context canceled by
// SIGINT or SIGTERM. Resources are released before reporting errors and exiting.
func Main(command Command) {
	if err := runMain(command); err != nil {
		slog.Error("plugin failed", "error", err)
		os.Exit(1)
	}
}

func runMain(command Command) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return command.Run(ctx, os.Args[1:], os.Stderr)
}

// Run parses common and source-specific flags and serves until cancellation.
// --help returns successfully without starting a server. The command owns one
// upstream HTTP client, shared by its factories, and closes idle connections on
// return. DUG_PLUGIN_TOKEN supplies the optional incoming RPC bearer token.
func (command Command) Run(ctx context.Context, args []string, stderr io.Writer) error {
	return command.run(ctx, args, stderr, plugin.Serve)
}

func (command Command) run(ctx context.Context, args []string, stderr io.Writer, serve func(context.Context, string, plugin.ServerOptions) error) error {
	if command.Registry == "" || command.DefaultAddress == "" || command.Factory == nil {
		return fmt.Errorf("plugin command requires a registry, default address, and factory")
	}
	if err := command.Provider.Validate(); err != nil {
		return err
	}
	definition, ok := command.Provider.Registry(command.Registry)
	if !ok {
		return fmt.Errorf("registry %q is not declared by provider %q", command.Registry, command.Provider.ID)
	}
	if !definition.Stability.Valid() {
		return fmt.Errorf("registry %q must declare its stability (experimental, beta or stable)", command.Registry)
	}
	flags := flag.NewFlagSet("dug-"+command.Registry, flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "Print the source version and exit")
	address := flags.String("listen", command.DefaultAddress, "Connect plugin listen address")
	connections := flags.Int("connections", 128, "Maximum upstream HTTP connections per host")
	cacheTTL := flags.Duration("cache-ttl", services.CacheTTLFromEnv(), "Upstream GET cache lifetime, 0 disables (or "+services.EnvCacheTTL+")")
	cacheDir := flags.String("cache-dir", services.CacheDirFromEnv(), "Upstream GET cache directory, default user cache dir (or "+services.EnvCacheDir+")")
	if command.ConfigureFlags != nil {
		command.ConfigureFlags(flags)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		_, err := fmt.Fprintf(stdout, "dug-%s %s (%s)\n", command.Registry, Version(), definition.Stability)
		return err
	}
	if *connections < 1 || flags.NArg() != 0 {
		return fmt.Errorf("invalid connections or positional arguments")
	}
	if *address == "" {
		return fmt.Errorf("listen address is required")
	}
	if command.Validate != nil {
		if err := command.Validate(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client := services.NewDugHttpClient(services.WithConnections(*connections), services.WithCache(*cacheDir, *cacheTTL))
	defer client.CloseIdleConnections()
	return serve(ctx, *address, plugin.ServerOptions{
		Registry: command.Registry,
		Provider: command.Provider,
		Version:  Version(),
		Factory:  command.Factory(client),
		Token:    os.Getenv("DUG_PLUGIN_TOKEN"),
	})
}
