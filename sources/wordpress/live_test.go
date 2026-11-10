//go:build live

package wordpress_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/wordpress"
)

// TestLivePlugins checks the plugins info API and plugin SVN tags.
func TestLivePlugins(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory: wordpress.PluginFactory(livetest.Client(), wordpress.Plugins),
		Options: map[string]string{"slugs": "akismet", "version_depth": "1"},
	})
}

// TestLiveThemes checks the themes info API and theme SVN directories.
func TestLiveThemes(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory: wordpress.PluginFactory(livetest.Client(), wordpress.Themes),
		Options: map[string]string{"slugs": "twentytwentyfour", "version_depth": "1"},
	})
}
