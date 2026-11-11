//go:build live

package jenkins_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/jenkins"
)

// TestLiveUpdateCenter checks the update center still returns what the source maps.
func TestLiveUpdateCenter(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory:  jenkins.PluginFactory(livetest.Client()),
		Options:  map[string]string{"plugins": "git", "version_depth": "2"},
		Facts:    []string{livetest.FactInstalls, livetest.FactRepository, livetest.FactReleasedAt, livetest.FactUpdatedAt},
		Metadata: []string{"health_score", "required_core"},
		Tags:     true, Author: true, Digest: true, Size: true,
	})
}
