//go:build live

package helm_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/helm"
)

// TestLiveArtifactHub checks Artifact Hub's package API and the chart hosts.
func TestLiveArtifactHub(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory:  helm.PluginFactory(livetest.Client()),
		Options:  map[string]string{"charts": "ingress-nginx/ingress-nginx", "version_depth": "1"},
		Facts:    []string{livetest.FactRepository, livetest.FactReleasedAt, livetest.FactUpdatedAt},
		Metadata: []string{"category", "repository"},
		Tags:     true, Author: true, Digest: true,
	})
}
