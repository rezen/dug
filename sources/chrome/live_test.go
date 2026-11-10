//go:build live

package chrome_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/chrome"
)

// TestLiveWebStore checks the CRX update service and the store page parser.
// uBlock Origin Lite is a long-lived, widely installed extension.
func TestLiveWebStore(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory:  chrome.PluginFactory(livetest.Client()),
		Options:  map[string]string{"ids": "ddkjiahejlhfcafbddmgiahcphecmpfh", "details": "true"},
		Facts:    []string{livetest.FactInstalls},
		Metadata: []string{"category", "num_ratings", "title"},
		Tags:     true, Author: true, Digest: true, Size: true,
	})
}
