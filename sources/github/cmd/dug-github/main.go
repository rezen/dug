// dug-github runs the GitHub registry as an independent Connect plugin.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/rezen/dug/sdk/httpx"
	"github.com/rezen/dug/sdk/plugin"
	ghsvc "github.com/rezen/dug/sdk/services/github"
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/github"
)

func main() {
	var api string
	pluginhost.Main(pluginhost.Command{
		Provider:       github.Provider(),
		Registry:       "github",
		DefaultAddress: "127.0.0.1:7784",
		ConfigureFlags: func(flags *flag.FlagSet) {
			flags.StringVar(&api, "api", ghsvc.DefaultEndpoint, "GitHub REST API base URL (GitHub Enterprise Server: https://HOST/api/v3)")
		},
		Validate: func() error {
			if err := httpx.ValidateURL(api); err != nil {
				return fmt.Errorf("invalid api URL: %w", err)
			}
			return nil
		},
		Factory: func(client *http.Client) plugin.Factory {
			token := os.Getenv("GITHUB_TOKEN")
			if token == "" {
				slog.Warn("GITHUB_TOKEN is not set; unauthenticated GitHub requests are limited to 60 per hour and cannot see private repositories")
			}
			return github.PluginFactory(client, api, token)
		},
	})
}
