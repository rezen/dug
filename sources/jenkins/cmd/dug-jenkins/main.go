// dug-jenkins runs the Jenkins registry as an independent Connect plugin.
package main

import (
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/jenkins"
)

// pluginhost owns --listen, --connections, --help, DUG_PLUGIN_TOKEN, signals,
// and the shared upstream HTTP client. Upstream credentials, when the registry
// needs any, are read from the environment in a Factory closure and handed to
// PluginFactory; source-specific flags use ConfigureFlags and Validate. See
// sources/github/cmd/dug-github for both.
func main() {
	pluginhost.Main(pluginhost.Command{
		Provider:       jenkins.Provider(),
		Registry:       jenkins.Identity,
		DefaultAddress: "127.0.0.1:7792",
		Factory:        jenkins.PluginFactory,
	})
}
