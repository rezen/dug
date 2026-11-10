//go:build live

package joomla_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/joomla"
)

// TestLiveJED checks the Joomla Extensions Directory pages and update manifests.
// Discovery walks the catalog until it has a couple of installable extensions.
func TestLiveJED(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory:  joomla.PluginFactory(livetest.Client()),
		Options:  map[string]string{"version_depth": "1"},
		MaxRefs:  2,
		Facts:    []string{livetest.FactUpdatedAt},
		Metadata: []string{"license"},
		Tags:     true, Author: true, Size: true,
	})
}
