// dug-bitbucket runs the Bitbucket Cloud registry as an independent Connect plugin.
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/bitbucket"
)

// BITBUCKET_TOKEN is a repository, project or workspace access token. It stays
// in this process: it is not an option, attribute or manifest field.
func main() {
	pluginhost.Main(pluginhost.Command{
		Provider:       bitbucket.Provider(),
		Registry:       bitbucket.Identity,
		DefaultAddress: "127.0.0.1:7793",
		Factory: func(client *http.Client) plugin.Factory {
			token := os.Getenv("BITBUCKET_TOKEN")
			if token == "" {
				slog.Warn("BITBUCKET_TOKEN is not set; unauthenticated Bitbucket requests are limited to 60 per hour per IP and cannot see private repositories")
			}
			return bitbucket.PluginFactory(client, token)
		},
	})
}
