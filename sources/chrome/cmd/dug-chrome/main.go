// dug-chrome runs the Chrome registry as an independent Connect plugin.
package main

import (
	"github.com/rezen/dug/sdk/services/pluginhost"
	"github.com/rezen/dug/sources/chrome"
)

func main() {
	pluginhost.Main(pluginhost.Command{
		Provider:       chrome.Provider(),
		Registry:       "chrome",
		DefaultAddress: "127.0.0.1:7782",
		Factory:        chrome.PluginFactory,
	})
}
