// dug-wordpress runs the WordPress plugins directory as an independent Connect plugin.
package main

import (
	"net/http"

	"github.com/rezen/dug/sdk/plugin"
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/wordpress"
)

func main() {
	pluginhost.Main(pluginhost.Command{
		Provider:       wordpress.Provider(),
		Registry:       wordpress.Plugins.Registry(),
		DefaultAddress: "127.0.0.1:7781",
		Factory: func(client *http.Client) plugin.Factory {
			return wordpress.PluginFactory(client, wordpress.Plugins)
		},
	})
}
