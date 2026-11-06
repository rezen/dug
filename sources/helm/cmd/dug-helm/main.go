// dug-helm runs the Helm charts registry as an independent Connect plugin.
package main

import (
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/helm"
)

// pluginhost owns --listen, --connections, --help, DUG_PLUGIN_TOKEN, signals,
// and the shared upstream HTTP client. Upstream credentials, when the registry
// needs any, are read from the environment in a Factory closure and handed to
// PluginFactory; source-specific flags use ConfigureFlags and Validate. See
// sources/github/cmd/dug-github for both.
func main() {
	pluginhost.Main(pluginhost.Command{
		Provider:       helm.Provider(),
		Registry:       helm.Identity,
		DefaultAddress: "127.0.0.1:7789",
		Factory:        helm.PluginFactory,
	})
}
