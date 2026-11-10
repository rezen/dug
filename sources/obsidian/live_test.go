//go:build live

package obsidian_test

import (
	"os"
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	ghsvc "github.com/rezen/dug/sdk/services/github"
	"github.com/rezen/dug/sources/obsidian"
)

// TestLiveCatalog checks the community catalog files and GitHub releases.
// It needs GITHUB_TOKEN: the release API is rate limited without one.
func TestLiveCatalog(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN is not set")
	}
	livetest.Run(t, livetest.Spec{
		Factory:  obsidian.PluginFactory(livetest.Client(), ghsvc.DefaultEndpoint, token),
		Options:  map[string]string{"ids": "obsidian-git", "version_depth": "1"},
		Facts:    []string{livetest.FactDownloadsTotal, livetest.FactVersionDownloads, livetest.FactRepository, livetest.FactReleasedAt},
		Metadata: []string{"plugin_name"},
		Author:   true,
		Digest:   true,
		Size:     true,
	})
}
