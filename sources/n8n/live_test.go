//go:build live

package n8n_test

import (
	"testing"

	"github.com/rezen/dug/sdk/registry"
	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/n8n"
)

// TestLiveNPM checks the npm packument and download-count APIs. The explicit
// package avoids the keyword search and ecosyste.ms enumeration.
func TestLiveNPM(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory: n8n.PluginFactory(livetest.Client()),
		Options: map[string]string{"packages": "n8n-nodes-puppeteer", "version_depth": "1"},
		Facts:   []string{livetest.FactDownloadsMonth, livetest.FactRepository, livetest.FactReleasedAt, livetest.FactUpdatedAt},
		Tags:    true, Author: true,
		// npm publishes SHA-512 integrity, not SHA-256; the source verifies it on download.
		Ref: func(t *testing.T, ref registry.Reference) {
			if ref.Attributes["integrity"] == "" {
				t.Error("npm integrity missing")
			}
		},
	})
}
