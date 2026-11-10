//go:build live

package ansiblegalaxy_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	ansiblegalaxy "github.com/rezen/dug/sources/ansible-galaxy"
)

// TestLiveGalaxy checks the Galaxy v3 collections API still returns what the source maps.
func TestLiveGalaxy(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory: ansiblegalaxy.PluginFactory(livetest.Client()),
		Options: map[string]string{"collections": "community.general", "version_depth": "1"},
		Facts:   []string{livetest.FactDownloadsTotal, livetest.FactRepository, livetest.FactReleasedAt, livetest.FactUpdatedAt},
		Tags:    true, Author: true, Digest: true, Size: true,
	})
}
