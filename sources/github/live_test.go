//go:build live

package github_test

import (
	"os"
	"testing"

	"github.com/rezen/dug/sdk/registry/livetest"
	ghsvc "github.com/rezen/dug/sdk/services/github"
	"github.com/rezen/dug/sources/github"
)

// TestLiveREST checks the repository, release and codeload responses.
func TestLiveREST(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		Factory: github.PluginFactory(livetest.Client(), ghsvc.DefaultEndpoint, os.Getenv("GITHUB_TOKEN")),
		Options: map[string]string{"repos": "cli/cli", "version_depth": "1"},
	})
}
