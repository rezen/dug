//go:build live

package bitbucket_test

import (
	"os"
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	"github.com/rezen/dug/sources/bitbucket"
)

// TestLiveTags checks the repository, tag and archive routes against a small
// public repository from the atlassian workspace.
func TestLiveTags(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory: bitbucket.PluginFactory(livetest.Client(), os.Getenv("BITBUCKET_TOKEN")),
		Options: map[string]string{"repos": "atlassian/aui", "version_depth": "1"},
	})
}
