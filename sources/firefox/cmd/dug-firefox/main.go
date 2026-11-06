// dug-firefox runs the Firefox registry as an independent Connect plugin.
package main

import (
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/firefox"
)

func main() {
	pluginhost.Main(pluginhost.Command{
		Provider:       firefox.Provider(),
		Registry:       "firefox",
		DefaultAddress: "127.0.0.1:7783",
		Factory:        firefox.PluginFactory,
	})
}
