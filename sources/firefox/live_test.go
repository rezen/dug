//go:build live

package firefox_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/firefox"
)

// TestLiveAMO checks the AMO v5 API still returns what the source maps.
func TestLiveAMO(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory:  firefox.PluginFactory(livetest.Client()),
		Options:  map[string]string{"slugs": "ublock-origin", "version_depth": "1"},
		Facts:    []string{livetest.FactInstalls, livetest.FactDownloadsWeek, livetest.FactReleasedAt, livetest.FactUpdatedAt},
		Metadata: []string{"num_ratings", "rating"},
		Tags:     true, Author: true, Raw: true, Digest: true, Size: true,
	})
}
